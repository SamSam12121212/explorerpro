package worker

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"explorer/internal/doccmd"
	"explorer/internal/preparedinput"
	"explorer/internal/threadcmd"
	"explorer/internal/threadstore"
)

// maxCitationsPerTurn caps the number of store_citation calls the main
// thread may emit in a single turn. Each call spawns its own locator
// child thread with its own one-shot OpenAI session, so the cap is a
// real cost control, not a structural limit.
const maxCitationsPerTurn = 20

// citationLocatorRequest is the decoded store_citation tool call.
type citationLocatorRequest struct {
	DocumentID    int64  `json:"document_id"`
	Pages         []int  `json:"pages"`
	Instruction   string `json:"instruction"`
	IncludeImages bool   `json:"include_images"`
}

type citationLocatorCall struct {
	CallID  string
	Request citationLocatorRequest
}

// citationRoundCall is what gets encoded into spawn.ParentCallID so the
// barrier-close path can correlate child threads back to parent store_citation
// call_ids (and re-fire the finalize RPC with the right inputs). Filled in
// incrementally: the core fields before spawn, ChildThreadID after we know it.
type citationRoundCall struct {
	CallID        string `json:"call_id"`
	DocumentID    int64  `json:"document_id"`
	Pages         []int  `json:"pages"`
	Instruction   string `json:"instruction"`
	IncludeImages bool   `json:"include_images,omitempty"`
	ChildThreadID int64  `json:"child_thread_id,omitempty"`
}

func decodeCitationLocatorRequest(arguments string) (citationLocatorRequest, error) {
	var req citationLocatorRequest
	if err := json.Unmarshal([]byte(arguments), &req); err != nil {
		return citationLocatorRequest{}, fmt.Errorf("decode %s arguments: %w", doccmd.ToolNameStoreCitation, err)
	}
	if req.DocumentID <= 0 {
		return citationLocatorRequest{}, fmt.Errorf("%s requires a positive document_id", doccmd.ToolNameStoreCitation)
	}
	if err := validateCitationLocatorPages(req.Pages); err != nil {
		return citationLocatorRequest{}, err
	}
	if strings.TrimSpace(req.Instruction) == "" {
		return citationLocatorRequest{}, fmt.Errorf("%s requires a non-empty instruction", doccmd.ToolNameStoreCitation)
	}
	return req, nil
}

// validateCitationLocatorPages enforces the structural invariant for citations:
// 1 or 2 pages, positive, consecutive (N, N+1) when 2. Same rule the
// documenthandler applies downstream — we check here too so bad calls surface
// as per-call errors on the main thread rather than spawning doomed children.
func validateCitationLocatorPages(pages []int) error {
	switch len(pages) {
	case 1:
		if pages[0] <= 0 {
			return fmt.Errorf("%s pages[0] must be positive, got %d", doccmd.ToolNameStoreCitation, pages[0])
		}
	case 2:
		if pages[0] <= 0 || pages[1] <= 0 {
			return fmt.Errorf("%s pages must be positive, got %v", doccmd.ToolNameStoreCitation, pages)
		}
		if pages[1] != pages[0]+1 {
			return fmt.Errorf("%s pages must be consecutive (N, N+1), got %v", doccmd.ToolNameStoreCitation, pages)
		}
	default:
		return fmt.Errorf("%s requires 1 or 2 pages, got %d", doccmd.ToolNameStoreCitation, len(pages))
	}
	return nil
}

func encodeCitationRoundCalls(calls []citationRoundCall) (string, error) {
	if len(calls) == 0 {
		return "", fmt.Errorf("citation round requires at least one call")
	}
	payload, err := json.Marshal(map[string]any{
		"kind":  "citation_locator_round",
		"calls": calls,
	})
	if err != nil {
		return "", fmt.Errorf("marshal citation round calls: %w", err)
	}
	return string(payload), nil
}

func decodeCitationRoundCalls(raw string) ([]citationRoundCall, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var payload struct {
		Kind  string              `json:"kind"`
		Calls []citationRoundCall `json:"calls"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return nil, fmt.Errorf("decode citation round calls: %w", err)
	}
	if payload.Kind != "citation_locator_round" {
		return nil, fmt.Errorf("spawn group ParentCallID is not a citation_locator_round (kind=%q)", payload.Kind)
	}
	return payload.Calls, nil
}

// startCitationLocatorGroup fans out N locator child threads, one per
// store_citation call on the parent's turn. Mirrors startDocumentQueryGroup
// but simpler: 1:1 parent_call_id → child thread (no merging by document).
// Each child is one-shot — single response.create via tool_choice:emit_bboxes,
// then socket releases.
func (a *threadActor) startCitationLocatorGroup(parentMeta threadstore.ThreadMeta, calls []citationLocatorCall) (int64, error) {
	if a.threadDocs == nil {
		return 0, fmt.Errorf("document store not available")
	}
	if len(calls) == 0 {
		return 0, fmt.Errorf("citation locator round requires at least one tool call")
	}
	if len(calls) > maxCitationsPerTurn {
		return 0, fmt.Errorf("store_citation per-turn cap exceeded: %d > %d", len(calls), maxCitationsPerTurn)
	}

	allDocumentIDs := make([]int64, 0, len(calls))
	for _, call := range calls {
		allDocumentIDs = append(allDocumentIDs, call.Request.DocumentID)
	}
	attached, err := a.threadDocs.FilterAttached(a.ctx, parentMeta.ID, allDocumentIDs)
	if err != nil {
		return 0, fmt.Errorf("validate attached documents: %w", err)
	}
	attachedSet := make(map[int64]bool, len(attached))
	for _, id := range attached {
		attachedSet[id] = true
	}
	var missing []int64
	for _, id := range allDocumentIDs {
		if !attachedSet[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		return 0, fmt.Errorf("%w: documents not attached to thread: %v", errCommandPrecond, missing)
	}

	stableKey := calls[0].CallID
	spawnMeta, err := a.store.LoadOrCreateSpawnGroup(a.ctx, threadstore.SpawnGroupMeta{
		ParentThreadID: parentMeta.ID,
		GroupKind:      "citation_locator",
		StableKey:      stableKey,
		Expected:       len(calls),
		Status:         threadstore.SpawnGroupStatusWaiting,
		CreatedAt:      time.Now().UTC(),
		UpdatedAt:      time.Now().UTC(),
	})
	if err != nil {
		return 0, err
	}
	spawnGroupID := spawnMeta.ID

	roundCalls := make([]citationRoundCall, 0, len(calls))
	childCommands := make([]threadcmd.Command, 0, len(calls))
	childThreadIDs := make([]int64, 0, len(calls))

	for _, call := range calls {
		requestID := stableCitationSpawnPreparedInputID(parentMeta.ID, call.CallID)
		resp, err := a.preparedInputs.PrepareInput(a.ctx, doccmd.PrepareInputRequest{
			RequestID:     requestID,
			Kind:          doccmd.PrepareKindStoreCitationSpawn,
			ThreadID:      parentMeta.ID,
			DocumentID:    call.Request.DocumentID,
			Pages:         call.Request.Pages,
			Instruction:   call.Request.Instruction,
			IncludeImages: call.Request.IncludeImages,
			CallID:        call.CallID,
		})
		if err != nil {
			return 0, fmt.Errorf("prepare citation locator spawn input for call %s: %w", call.CallID, err)
		}
		if resp.Status != doccmd.PrepareStatusOK {
			return 0, fmt.Errorf("prepare citation locator spawn input for call %s failed: %s", call.CallID, resp.Error)
		}

		threadID, startCmd, err := a.buildCitationLocatorChildStartCommand(parentMeta, spawnGroupID, call, resp.PreparedInputRef)
		if err != nil {
			return 0, err
		}
		childCommands = append(childCommands, startCmd)
		childThreadIDs = append(childThreadIDs, threadID)

		roundCalls = append(roundCalls, citationRoundCall{
			CallID:        call.CallID,
			DocumentID:    call.Request.DocumentID,
			Pages:         call.Request.Pages,
			Instruction:   call.Request.Instruction,
			IncludeImages: call.Request.IncludeImages,
			ChildThreadID: threadID,
		})

		a.logger.Info("spawning child thread",
			appendThreadGraphAttrs([]any{
				"spawn_group_id", spawnGroupID,
				"child_thread_id", threadID,
				"child_kind", "citation_locator",
				"document_id", call.Request.DocumentID,
				"call_id", call.CallID,
				"pages", call.Request.Pages,
			}, parentMeta)...,
		)
	}

	encodedParentCallID, err := encodeCitationRoundCalls(roundCalls)
	if err != nil {
		return 0, err
	}

	spawnMeta.ParentCallID = encodedParentCallID
	spawnMeta.Expected = len(childCommands)
	spawnMeta.GroupKind = "citation_locator"
	spawnMeta.StableKey = stableKey
	if err := a.store.CreateSpawnGroup(a.ctx, spawnMeta, childThreadIDs); err != nil {
		return 0, err
	}

	a.logger.Info("opened child barrier",
		appendThreadGraphAttrs([]any{
			"spawn_group_id", spawnGroupID,
			"child_source", "citation_locator",
			"expected_children", len(childCommands),
			"function_call_count", len(calls),
		}, parentMeta)...,
	)

	for _, cmd := range childCommands {
		if err := a.publish(a.ctx, threadcmd.DispatchStartSubject, cmd); err != nil {
			return 0, err
		}
	}

	return spawnGroupID, nil
}

// buildCitationLocatorChildStartCommand constructs the thread.start command
// for a single locator child. Forces the emit_bboxes tool call, marks the
// thread one_shot, pins model/reasoning to the locator defaults.
func (a *threadActor) buildCitationLocatorChildStartCommand(parentMeta threadstore.ThreadMeta, spawnGroupID int64, call citationLocatorCall, preparedInputRef string) (int64, threadcmd.Command, error) {
	cmdID, err := a.store.LoadOrCreateCommandID(
		a.ctx,
		"thread",
		string(threadcmd.KindThreadStart),
		fmt.Sprintf("citation_locator-%d-%s", parentMeta.ID, call.CallID),
	)
	if err != nil {
		return 0, threadcmd.Command{}, err
	}

	toolsJSON, err := json.Marshal([]any{doccmd.EmitBboxesToolDefinition()})
	if err != nil {
		return 0, threadcmd.Command{}, fmt.Errorf("marshal locator tools: %w", err)
	}
	toolChoiceJSON, err := json.Marshal(map[string]any{
		"type": "function",
		"name": doccmd.ToolNameEmitBboxes,
	})
	if err != nil {
		return 0, threadcmd.Command{}, fmt.Errorf("marshal locator tool_choice: %w", err)
	}

	metadataMap := map[string]string{
		"spawn_mode":  "citation_locator",
		"document_id": formatDocumentIDLocal(call.Request.DocumentID),
		"call_id":     call.CallID,
	}
	metadataJSON, err := json.Marshal(metadataMap)
	if err != nil {
		return 0, threadcmd.Command{}, fmt.Errorf("marshal locator metadata: %w", err)
	}

	toolsAny, err := rawJSONToAny(toolsJSON)
	if err != nil {
		return 0, threadcmd.Command{}, fmt.Errorf("decode locator tools: %w", err)
	}
	toolChoiceAny, err := rawJSONToAny(toolChoiceJSON)
	if err != nil {
		return 0, threadcmd.Command{}, fmt.Errorf("decode locator tool_choice: %w", err)
	}
	metadataAny, err := rawJSONToAny(metadataJSON)
	if err != nil {
		return 0, threadcmd.Command{}, fmt.Errorf("decode locator metadata: %w", err)
	}

	startBody := map[string]any{
		"model":              "gpt-5.4-mini",
		"instructions":       doccmd.CitationLocatorInstructions,
		"reasoning":          map[string]any{"effort": "medium"},
		"tools":              toolsAny,
		"tool_choice":        toolChoiceAny,
		"store":              true,
		"one_shot":           true,
		"prepared_input_ref": preparedInputRef,
		"metadata":           metadataAny,
	}

	body, err := json.Marshal(startBody)
	if err != nil {
		return 0, threadcmd.Command{}, fmt.Errorf("marshal locator start body: %w", err)
	}

	threadID, err := a.store.ReserveThreadID(a.ctx)
	if err != nil {
		return 0, threadcmd.Command{}, err
	}

	now := time.Now().UTC()
	if err := a.store.CreateThreadIfAbsent(a.ctx, threadstore.ThreadMeta{
		ID:             threadID,
		RootThreadID:   parentMeta.RootThreadID,
		ParentThreadID: parentMeta.ID,
		ParentCallID:   call.CallID,
		Depth:          parentMeta.Depth + 1,
		Status:         threadstore.ThreadStatusNew,
		Model:          "gpt-5.4-mini",
		Instructions:   doccmd.CitationLocatorInstructions,
		MetadataJSON:   string(metadataJSON),
		ChildKind:      "citation_locator",
		DocumentID:     call.Request.DocumentID,
		OneShot:        true,
		CreatedAt:      now,
		UpdatedAt:      now,
	}); err != nil {
		return 0, threadcmd.Command{}, err
	}

	return threadID, threadcmd.Command{
		CmdID:        cmdID,
		Kind:         threadcmd.KindThreadStart,
		ThreadID:     threadID,
		RootThreadID: parentMeta.RootThreadID,
		CausationID:  parentMeta.LastResponseID,
		Body:         body,
	}, nil
}

// aggregateCitationLocatorOutputs fires PrepareInput(store_citation_finalize)
// for each completed locator child, unwraps each resulting function_call_output
// artifact, and returns the concatenated items as the parent's turn-2 input.
// Per-child failures (missing args, finalize RPC error, malformed artifact)
// surface as error-shaped function_call_output so the model can retry instead
// of the whole turn dying.
func (a *threadActor) aggregateCitationLocatorOutputs(spawn threadstore.SpawnGroupMeta, results []threadstore.SpawnChildResult) (json.RawMessage, error) {
	callBindings, err := decodeCitationRoundCalls(spawn.ParentCallID)
	if err != nil {
		return nil, err
	}
	if len(callBindings) == 0 {
		return nil, fmt.Errorf("citation spawn group %d has no parent call bindings", spawn.ID)
	}

	resultsByThread := make(map[int64]threadstore.SpawnChildResult, len(results))
	for _, r := range results {
		resultsByThread[r.ChildThreadID] = r
	}

	store, err := preparedinput.NewStore(a.blob)
	if err != nil {
		return nil, err
	}

	outputItems := make([]map[string]any, 0, len(callBindings))
	for _, binding := range callBindings {
		result, ok := resultsByThread[binding.ChildThreadID]
		if !ok {
			outputItems = append(outputItems, errorFunctionCallOutput(binding.CallID, "citation locator child missing from results"))
			continue
		}
		if result.Status != "completed" {
			msg := fmt.Sprintf("citation locator child status=%s", result.Status)
			if result.ErrorRef != "" {
				msg = fmt.Sprintf("%s error_ref=%s", msg, result.ErrorRef)
			}
			outputItems = append(outputItems, errorFunctionCallOutput(binding.CallID, msg))
			continue
		}
		if strings.TrimSpace(result.ToolCallArgsJSON) == "" {
			outputItems = append(outputItems, errorFunctionCallOutput(binding.CallID, "citation locator emitted no tool call arguments"))
			continue
		}

		requestID := stableCitationFinalizePreparedInputID(spawn.ParentThreadID, binding.ChildThreadID, binding.CallID)
		resp, err := a.preparedInputs.PrepareInput(a.ctx, doccmd.PrepareInputRequest{
			RequestID:        requestID,
			Kind:             doccmd.PrepareKindStoreCitationFinalize,
			ThreadID:         spawn.ParentThreadID,
			DocumentID:       binding.DocumentID,
			Pages:            binding.Pages,
			Instruction:      binding.Instruction,
			CallID:           binding.CallID,
			ToolCallArgsJSON: result.ToolCallArgsJSON,
			ChildThreadID:    binding.ChildThreadID,
		})
		if err != nil {
			a.logger.Warn("citation locator finalize rpc failed",
				"call_id", binding.CallID,
				"child_thread_id", binding.ChildThreadID,
				"error", err,
			)
			outputItems = append(outputItems, errorFunctionCallOutput(binding.CallID, fmt.Sprintf("finalize rpc failed: %v", err)))
			continue
		}
		if strings.TrimSpace(resp.Status) != doccmd.PrepareStatusOK {
			msg := strings.TrimSpace(resp.Error)
			if msg == "" {
				msg = fmt.Sprintf("citation finalize returned status %q", resp.Status)
			}
			outputItems = append(outputItems, errorFunctionCallOutput(binding.CallID, msg))
			continue
		}

		artifact, err := store.Read(a.ctx, resp.PreparedInputRef)
		if err != nil {
			outputItems = append(outputItems, errorFunctionCallOutput(binding.CallID, fmt.Sprintf("read finalize artifact: %v", err)))
			continue
		}

		var artifactItems []map[string]any
		if err := json.Unmarshal(artifact.Input, &artifactItems); err != nil || len(artifactItems) != 1 {
			outputItems = append(outputItems, errorFunctionCallOutput(binding.CallID, "citation finalize artifact malformed"))
			continue
		}
		outputItems = append(outputItems, artifactItems[0])
	}

	payload, err := json.Marshal(outputItems)
	if err != nil {
		return nil, fmt.Errorf("marshal citation locator outputs: %w", err)
	}
	return payload, nil
}

func stableCitationSpawnPreparedInputID(parentThreadID int64, callID string) string {
	return fmt.Sprintf("citspawn_%d_%s", parentThreadID, callID)
}

func stableCitationFinalizePreparedInputID(parentThreadID, childThreadID int64, callID string) string {
	return fmt.Sprintf("citfinal_%d_%d_%s", parentThreadID, childThreadID, callID)
}
