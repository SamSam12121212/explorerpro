package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"explorer/internal/agentcmd"
	"explorer/internal/config"
	"explorer/internal/docstore"
	"explorer/internal/idgen"
	"explorer/internal/platform"
	"explorer/internal/postgresstore"
	"explorer/internal/threaddocstore"
	"explorer/internal/threadhistory"
	"explorer/internal/threadstore"

	"github.com/nats-io/nats.go"
)

type commandAPI struct {
	cfg     config.Config
	logger  *slog.Logger
	runtime *platform.Runtime
	store   *postgresstore.Store
	docs    *docstore.Store
	links   *threaddocstore.Store
	history eventHistoryStore
}

type eventHistoryStore interface {
	ListEvents(ctx context.Context, threadID string, options threadstore.ListOptions) ([]threadstore.EventRecord, error)
}

type createThreadRequest struct {
	Model               string          `json:"model"`
	Instructions        string          `json:"instructions,omitempty"`
	Input               json.RawMessage `json:"input"`
	Metadata            json.RawMessage `json:"metadata,omitempty"`
	Include             json.RawMessage `json:"include,omitempty"`
	Tools               json.RawMessage `json:"tools,omitempty"`
	ToolChoice          json.RawMessage `json:"tool_choice,omitempty"`
	Reasoning           json.RawMessage `json:"reasoning,omitempty"`
	Store               *bool           `json:"store,omitempty"`
	PreviousResponseID  string          `json:"previous_response_id,omitempty"`
	AttachedDocumentIDs []string        `json:"attached_document_ids,omitempty"`
}

type submitCommandRequest struct {
	CmdID                    string          `json:"cmd_id,omitempty"`
	Kind                     agentcmd.Kind   `json:"kind"`
	RootThreadID             string          `json:"root_thread_id,omitempty"`
	CausationID              string          `json:"causation_id,omitempty"`
	CorrelationID            string          `json:"correlation_id,omitempty"`
	ExpectedStatus           string          `json:"expected_status,omitempty"`
	ExpectedLastResponseID   string          `json:"expected_last_response_id,omitempty"`
	ExpectedSocketGeneration uint64          `json:"expected_socket_generation,omitempty"`
	Attempt                  int             `json:"attempt,omitempty"`
	Body                     json.RawMessage `json:"body"`
}

type threadRoute struct {
	ThreadID    string
	Resource    string
	ResourceID  string
	Subresource string
}

type listQuery struct {
	Limit  int64
	After  string
	Before string
}

func newCommandAPI(cfg config.Config, logger *slog.Logger, runtime *platform.Runtime) *commandAPI {
	store := postgresstore.New(runtime.Postgres().Pool())
	return &commandAPI{
		cfg:     cfg,
		logger:  logger,
		runtime: runtime,
		store:   store,
		docs:    docstore.New(runtime.Postgres().Pool()),
		links:   threaddocstore.New(runtime.Postgres().Pool()),
		history: threadhistory.New(runtime.NATS().JetStream()),
	}
}

func (a *commandAPI) handleThreads(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.handleListThreads(w, r)
	case http.MethodPost:
		a.handleCreateThread(w, r)
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

func (a *commandAPI) handleThreadRoutes(w http.ResponseWriter, r *http.Request) {
	route, ok := parseThreadRoute(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}

	switch route.Resource {
	case "":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		a.handleGetThread(w, r, route.ThreadID)
	case "commands":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		a.handleSubmitCommand(w, r, route.ThreadID)
	case "items":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		a.handleListItems(w, r, route.ThreadID)
	case "events":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		a.handleListEvents(w, r, route.ThreadID)
	case "responses":
		if route.ResourceID == "" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		a.handleGetResponse(w, r, route.ThreadID, route.ResourceID)
	case "spawn-groups":
		switch {
		case route.ResourceID == "":
			if r.Method != http.MethodGet {
				methodNotAllowed(w, http.MethodGet)
				return
			}
			a.handleListSpawnGroups(w, r, route.ThreadID)
		case route.Subresource == "":
			if r.Method != http.MethodGet {
				methodNotAllowed(w, http.MethodGet)
				return
			}
			a.handleGetSpawnGroup(w, r, route.ThreadID, route.ResourceID)
		case route.Subresource == "results":
			if r.Method != http.MethodGet {
				methodNotAllowed(w, http.MethodGet)
				return
			}
			a.handleListSpawnGroupResults(w, r, route.ThreadID, route.ResourceID)
		default:
			http.NotFound(w, r)
		}
	default:
		http.NotFound(w, r)
	}
}

func (a *commandAPI) handleCreateThread(w http.ResponseWriter, r *http.Request) {
	var req createThreadRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeErrorJSON(w, http.StatusBadRequest, err.Error())
		return
	}

	if strings.TrimSpace(req.Model) == "" {
		writeErrorJSON(w, http.StatusBadRequest, "model is required")
		return
	}
	if strings.TrimSpace(req.PreviousResponseID) != "" && req.Store != nil && !*req.Store {
		writeErrorJSON(w, http.StatusBadRequest, "previous_response_id requires store=true")
		return
	}

	initialInput, err := normalizeResponseInput(req.Input)
	if err != nil {
		writeErrorJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	include, err := agentcmd.NormalizeInclude(req.Include)
	if err != nil {
		writeErrorJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	metadata, err := agentcmd.NormalizeMetadata(req.Metadata)
	if err != nil {
		writeErrorJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	toolChoice, err := agentcmd.NormalizeToolChoice(req.ToolChoice)
	if err != nil {
		writeErrorJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	tools, err := agentcmd.NormalizeTools(req.Tools)
	if err != nil {
		writeErrorJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	reasoning, err := agentcmd.NormalizeReasoning(req.Reasoning)
	if err != nil {
		writeErrorJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	attachedDocumentIDs, err := normalizeAttachedDocumentIDs(req.AttachedDocumentIDs)
	if err != nil {
		writeErrorJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := a.ensureDocumentsExist(r.Context(), attachedDocumentIDs); err != nil {
		if errors.Is(err, docstore.ErrDocumentNotFound) {
			writeErrorJSON(w, http.StatusNotFound, err.Error())
			return
		}
		writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("validate attached documents: %v", err))
		return
	}

	threadID, err := idgen.New("thread")
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, err.Error())
		return
	}

	cmdID, err := idgen.New("cmd")
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, err.Error())
		return
	}

	body, err := json.Marshal(agentcmd.StartBody{
		InitialInput:       initialInput,
		Model:              req.Model,
		Instructions:       req.Instructions,
		Metadata:           metadata,
		Include:            include,
		Tools:              tools,
		ToolChoice:         toolChoice,
		Reasoning:          reasoning,
		Store:              req.Store,
		PreviousResponseID: req.PreviousResponseID,
	})
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("marshal thread.start body: %v", err))
		return
	}

	now := time.Now().UTC()
	if err := a.store.CreateThreadIfAbsent(r.Context(), threadstore.ThreadMeta{
		ID:             threadID,
		RootThreadID:   threadID,
		Status:         threadstore.ThreadStatusNew,
		Model:          req.Model,
		Instructions:   req.Instructions,
		MetadataJSON:   string(metadata),
		IncludeJSON:    string(include),
		ToolsJSON:      string(tools),
		ToolChoiceJSON: string(toolChoice),
		ReasoningJSON:  string(reasoning),
		CreatedAt:      now,
		UpdatedAt:      now,
	}); err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("create thread record: %v", err))
		return
	}
	if err := a.links.AddDocuments(r.Context(), threadID, attachedDocumentIDs); err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("attach thread documents: %v", err))
		return
	}

	cmd := agentcmd.Command{
		CmdID:        cmdID,
		Kind:         agentcmd.KindThreadStart,
		ThreadID:     threadID,
		RootThreadID: threadID,
		CreatedAt:    now.Format(time.RFC3339),
		Body:         body,
	}

	a.logger.Info("create thread request received",
		append(agentcmd.LogAttrs(cmd),
			"attached_document_count", len(attachedDocumentIDs),
			"has_tools", len(tools) > 0,
		)...,
	)

	subject := agentcmd.DispatchStartSubject
	if err := a.publishCommand(r.Context(), subject, cmd); err != nil {
		writeErrorJSON(w, http.StatusServiceUnavailable, err.Error())
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]any{
		"status":         "accepted",
		"kind":           cmd.Kind,
		"thread_id":      threadID,
		"root_thread_id": threadID,
		"cmd_id":         cmdID,
		"subject":        subject,
	})
}

func (a *commandAPI) handleListThreads(w http.ResponseWriter, r *http.Request) {
	limit := int64(100)
	if rawLimit := strings.TrimSpace(r.URL.Query().Get("limit")); rawLimit != "" {
		parsed, err := parsePositiveInt64(rawLimit)
		if err != nil {
			writeErrorJSON(w, http.StatusBadRequest, fmt.Sprintf("invalid limit: %v", err))
			return
		}
		limit = parsed
	}
	if limit > 200 {
		limit = 200
	}

	threads, err := a.store.ListRootThreads(r.Context(), limit)
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("list threads: %v", err))
		return
	}

	presented := make([]map[string]any, 0, len(threads))
	for _, thread := range threads {
		presented = append(presented, presentThreadListEntry(thread))
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"threads": presented,
		"count":   len(presented),
	})
}

func (a *commandAPI) handleSubmitCommand(w http.ResponseWriter, r *http.Request, threadID string) {
	meta, err := a.store.LoadThread(r.Context(), threadID)
	if err != nil {
		if errors.Is(err, threadstore.ErrThreadNotFound) {
			writeErrorJSON(w, http.StatusNotFound, fmt.Sprintf("thread %s not found", threadID))
			return
		}
		writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("load thread %s: %v", threadID, err))
		return
	}

	var req submitCommandRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeErrorJSON(w, http.StatusBadRequest, err.Error())
		return
	}

	if req.Kind == "" {
		writeErrorJSON(w, http.StatusBadRequest, "kind is required")
		return
	}

	if req.Kind == agentcmd.KindThreadStart {
		writeErrorJSON(w, http.StatusBadRequest, "use POST /threads for thread.start")
		return
	}

	if isTerminalThreadStatus(meta.Status) && requiresActiveThread(req.Kind) {
		writeErrorJSON(w, http.StatusConflict, fmt.Sprintf("thread %s is in terminal status %s", threadID, meta.Status))
		return
	}

	body, err := normalizeCommandBody(req.Kind, req.Body)
	if err != nil {
		writeErrorJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	attachedDocumentIDs, err := attachedDocumentIDsForCommand(req.Kind, body)
	if err != nil {
		writeErrorJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := a.ensureDocumentsExist(r.Context(), attachedDocumentIDs); err != nil {
		if errors.Is(err, docstore.ErrDocumentNotFound) {
			writeErrorJSON(w, http.StatusNotFound, err.Error())
			return
		}
		writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("validate attached documents: %v", err))
		return
	}
	if err := a.links.AddDocuments(r.Context(), threadID, attachedDocumentIDs); err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("attach thread documents: %v", err))
		return
	}

	cmdID := strings.TrimSpace(req.CmdID)
	if cmdID == "" {
		cmdID, err = idgen.New("cmd")
		if err != nil {
			writeErrorJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	rootThreadID := strings.TrimSpace(req.RootThreadID)
	if rootThreadID == "" {
		rootThreadID = meta.RootThreadID
	}
	if rootThreadID == "" {
		rootThreadID = threadID
	}

	cmd := agentcmd.Command{
		CmdID:                    cmdID,
		Kind:                     req.Kind,
		ThreadID:                 threadID,
		RootThreadID:             rootThreadID,
		CausationID:              req.CausationID,
		CorrelationID:            req.CorrelationID,
		ExpectedStatus:           req.ExpectedStatus,
		ExpectedLastResponseID:   req.ExpectedLastResponseID,
		ExpectedSocketGeneration: req.ExpectedSocketGeneration,
		Attempt:                  req.Attempt,
		CreatedAt:                time.Now().UTC().Format(time.RFC3339),
		Body:                     body,
	}

	a.logger.Info("submit command request received",
		append(agentcmd.LogAttrs(cmd),
			"thread_status", meta.Status,
			"attached_document_count", len(attachedDocumentIDs),
		)...,
	)

	subject, routedDirect, owner, err := a.resolveCommandSubject(r.Context(), meta.ID, cmd.Kind)
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("resolve command route: %v", err))
		return
	}
	if routedDirect && cmd.ExpectedSocketGeneration == 0 {
		cmd.ExpectedSocketGeneration = owner.SocketGeneration
	}

	if err := a.publishCommand(r.Context(), subject, cmd); err != nil {
		writeErrorJSON(w, http.StatusServiceUnavailable, err.Error())
		return
	}

	response := map[string]any{
		"status":         "accepted",
		"kind":           cmd.Kind,
		"thread_id":      cmd.ThreadID,
		"root_thread_id": cmd.RootThreadID,
		"cmd_id":         cmd.CmdID,
		"subject":        subject,
		"routed_direct":  routedDirect,
	}
	if routedDirect {
		response["owner_worker_id"] = owner.WorkerID
		response["socket_generation"] = owner.SocketGeneration
	}

	writeJSON(w, http.StatusAccepted, response)
}

func (a *commandAPI) handleGetThread(w http.ResponseWriter, r *http.Request, threadID string) {
	meta, err := a.loadThreadForRead(r.Context(), threadID)
	if err != nil {
		if errors.Is(err, threadstore.ErrThreadNotFound) {
			writeErrorJSON(w, http.StatusNotFound, fmt.Sprintf("thread %s not found", threadID))
			return
		}
		writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("load thread %s: %v", threadID, err))
		return
	}

	response := map[string]any{
		"thread":             presentThreadMeta(meta),
		"attached_documents": []map[string]any{},
	}

	attachedDocuments, err := a.links.ListDocuments(r.Context(), threadID, 200)
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("list attached documents for thread %s: %v", threadID, err))
		return
	}
	if len(attachedDocuments) > 0 {
		response["attached_documents"] = presentAttachedDocuments(attachedDocuments)
	}

	if owner, err := a.store.LoadOwner(r.Context(), threadID); err == nil {
		response["owner"] = presentOwner(owner)
	} else if !errors.Is(err, threadstore.ErrThreadNotFound) {
		writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("load owner for thread %s: %v", threadID, err))
		return
	}

	if meta.ActiveSpawnGroupID != "" {
		spawn, err := a.loadSpawnGroupForRead(r.Context(), meta.ActiveSpawnGroupID)
		if err != nil && !errors.Is(err, threadstore.ErrThreadNotFound) {
			writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("load spawn group %s: %v", meta.ActiveSpawnGroupID, err))
			return
		}
		if err == nil {
			response["active_spawn_group"] = presentSpawnGroup(spawn)
		}
	}

	writeJSON(w, http.StatusOK, response)
}

func (a *commandAPI) handleListItems(w http.ResponseWriter, r *http.Request, threadID string) {
	if _, err := a.loadThreadForRead(r.Context(), threadID); err != nil {
		if errors.Is(err, threadstore.ErrThreadNotFound) {
			writeErrorJSON(w, http.StatusNotFound, fmt.Sprintf("thread %s not found", threadID))
			return
		}
		writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("load thread %s: %v", threadID, err))
		return
	}

	query, err := parseListQuery(r)
	if err != nil {
		writeErrorJSON(w, http.StatusBadRequest, err.Error())
		return
	}

	items, err := a.listItemsForRead(r.Context(), threadID, query)
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("list items for thread %s: %v", threadID, err))
		return
	}

	presented := make([]map[string]any, 0, len(items))
	for _, item := range items {
		presented = append(presented, presentItem(item))
	}

	pageBounds := itemPageBounds(items)
	writeJSON(w, http.StatusOK, map[string]any{
		"thread_id": threadID,
		"items":     presented,
		"page":      presentPage(query, pageBounds, len(items)),
	})
}

func (a *commandAPI) handleListEvents(w http.ResponseWriter, r *http.Request, threadID string) {
	if _, err := a.loadThreadForRead(r.Context(), threadID); err != nil {
		if errors.Is(err, threadstore.ErrThreadNotFound) {
			writeErrorJSON(w, http.StatusNotFound, fmt.Sprintf("thread %s not found", threadID))
			return
		}
		writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("load thread %s: %v", threadID, err))
		return
	}

	query, err := parseListQuery(r)
	if err != nil {
		writeErrorJSON(w, http.StatusBadRequest, err.Error())
		return
	}

	events, err := a.listEventsForRead(r.Context(), threadID, query)
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("list events for thread %s: %v", threadID, err))
		return
	}

	presented := make([]map[string]any, 0, len(events))
	for _, event := range events {
		presented = append(presented, presentEvent(event))
	}

	pageBounds := eventPageBounds(events)
	writeJSON(w, http.StatusOK, map[string]any{
		"thread_id": threadID,
		"events":    presented,
		"page":      presentPage(query, pageBounds, len(events)),
	})
}

func (a *commandAPI) handleGetResponse(w http.ResponseWriter, r *http.Request, threadID, responseID string) {
	if _, err := a.loadThreadForRead(r.Context(), threadID); err != nil {
		if errors.Is(err, threadstore.ErrThreadNotFound) {
			writeErrorJSON(w, http.StatusNotFound, fmt.Sprintf("thread %s not found", threadID))
			return
		}
		writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("load thread %s: %v", threadID, err))
		return
	}

	linked, err := a.threadHasResponseForRead(r.Context(), threadID, responseID)
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("check response index for thread %s: %v", threadID, err))
		return
	}
	if !linked {
		writeErrorJSON(w, http.StatusNotFound, fmt.Sprintf("response %s not found for thread %s", responseID, threadID))
		return
	}

	raw, err := a.loadResponseRawForRead(r.Context(), responseID)
	if err != nil {
		if errors.Is(err, threadstore.ErrThreadNotFound) {
			writeErrorJSON(w, http.StatusNotFound, fmt.Sprintf("raw response %s not found", responseID))
			return
		}
		writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("load raw response %s: %v", responseID, err))
		return
	}

	decoded, err := decodeRawJSON(raw)
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("decode raw response %s: %v", responseID, err))
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"thread_id":   threadID,
		"response_id": responseID,
		"response":    decoded,
	})
}

func (a *commandAPI) handleListSpawnGroups(w http.ResponseWriter, r *http.Request, threadID string) {
	if _, err := a.loadThreadForRead(r.Context(), threadID); err != nil {
		if errors.Is(err, threadstore.ErrThreadNotFound) {
			writeErrorJSON(w, http.StatusNotFound, fmt.Sprintf("thread %s not found", threadID))
			return
		}
		writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("load thread %s: %v", threadID, err))
		return
	}

	groups, err := a.listSpawnGroupsByParentForRead(r.Context(), threadID)
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("list spawn groups for thread %s: %v", threadID, err))
		return
	}

	presented := make([]map[string]any, 0, len(groups))
	for _, group := range groups {
		presented = append(presented, presentSpawnGroup(group))
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"thread_id":    threadID,
		"spawn_groups": presented,
		"count":        len(presented),
	})
}

func (a *commandAPI) handleGetSpawnGroup(w http.ResponseWriter, r *http.Request, threadID, spawnGroupID string) {
	if _, err := a.loadThreadForRead(r.Context(), threadID); err != nil {
		if errors.Is(err, threadstore.ErrThreadNotFound) {
			writeErrorJSON(w, http.StatusNotFound, fmt.Sprintf("thread %s not found", threadID))
			return
		}
		writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("load thread %s: %v", threadID, err))
		return
	}

	spawn, err := a.loadSpawnGroupForRead(r.Context(), spawnGroupID)
	if err != nil {
		if errors.Is(err, threadstore.ErrThreadNotFound) {
			writeErrorJSON(w, http.StatusNotFound, fmt.Sprintf("spawn group %s not found", spawnGroupID))
			return
		}
		writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("load spawn group %s: %v", spawnGroupID, err))
		return
	}
	if spawn.ParentThreadID != threadID {
		writeErrorJSON(w, http.StatusNotFound, fmt.Sprintf("spawn group %s not found for thread %s", spawnGroupID, threadID))
		return
	}

	childThreadIDs, err := a.loadSpawnGroupChildThreadIDsForRead(r.Context(), spawnGroupID)
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("load child thread ids for spawn group %s: %v", spawnGroupID, err))
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"thread_id":        threadID,
		"spawn_group":      presentSpawnGroup(spawn),
		"child_thread_ids": childThreadIDs,
	})
}

func (a *commandAPI) handleListSpawnGroupResults(w http.ResponseWriter, r *http.Request, threadID, spawnGroupID string) {
	if _, err := a.loadThreadForRead(r.Context(), threadID); err != nil {
		if errors.Is(err, threadstore.ErrThreadNotFound) {
			writeErrorJSON(w, http.StatusNotFound, fmt.Sprintf("thread %s not found", threadID))
			return
		}
		writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("load thread %s: %v", threadID, err))
		return
	}

	spawn, err := a.loadSpawnGroupForRead(r.Context(), spawnGroupID)
	if err != nil {
		if errors.Is(err, threadstore.ErrThreadNotFound) {
			writeErrorJSON(w, http.StatusNotFound, fmt.Sprintf("spawn group %s not found", spawnGroupID))
			return
		}
		writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("load spawn group %s: %v", spawnGroupID, err))
		return
	}
	if spawn.ParentThreadID != threadID {
		writeErrorJSON(w, http.StatusNotFound, fmt.Sprintf("spawn group %s not found for thread %s", spawnGroupID, threadID))
		return
	}

	results, err := a.listSpawnResultsForRead(r.Context(), spawnGroupID)
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("list results for spawn group %s: %v", spawnGroupID, err))
		return
	}

	presented := make([]map[string]any, 0, len(results))
	for _, result := range results {
		presented = append(presented, presentSpawnResult(result))
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"thread_id":      threadID,
		"spawn_group_id": spawnGroupID,
		"results":        presented,
		"count":          len(presented),
	})
}

func (a *commandAPI) loadThreadForRead(ctx context.Context, threadID string) (threadstore.ThreadMeta, error) {
	return a.store.LoadThread(ctx, threadID)
}

func (a *commandAPI) loadSpawnGroupForRead(ctx context.Context, spawnGroupID string) (threadstore.SpawnGroupMeta, error) {
	return a.store.LoadSpawnGroup(ctx, spawnGroupID)
}

func (a *commandAPI) listItemsForRead(ctx context.Context, threadID string, query listQuery) ([]threadstore.ItemRecord, error) {
	options := threadstore.ListOptions{
		Limit:  query.Limit,
		After:  query.After,
		Before: query.Before,
	}
	return a.store.ListItems(ctx, threadID, options)
}

func (a *commandAPI) listEventsForRead(ctx context.Context, threadID string, query listQuery) ([]threadstore.EventRecord, error) {
	options := threadstore.ListOptions{
		Limit:  query.Limit,
		After:  query.After,
		Before: query.Before,
	}
	return a.history.ListEvents(ctx, threadID, options)
}

func (a *commandAPI) threadHasResponseForRead(ctx context.Context, threadID, responseID string) (bool, error) {
	return a.store.ThreadHasResponse(ctx, threadID, responseID)
}

func (a *commandAPI) loadResponseRawForRead(ctx context.Context, responseID string) (json.RawMessage, error) {
	return a.store.LoadResponseRaw(ctx, responseID)
}

func (a *commandAPI) listSpawnGroupsByParentForRead(ctx context.Context, parentThreadID string) ([]threadstore.SpawnGroupMeta, error) {
	return a.store.ListSpawnGroupsByParent(ctx, parentThreadID)
}

func (a *commandAPI) loadSpawnGroupChildThreadIDsForRead(ctx context.Context, spawnGroupID string) ([]string, error) {
	return a.store.LoadSpawnGroupChildThreadIDs(ctx, spawnGroupID)
}

func (a *commandAPI) listSpawnResultsForRead(ctx context.Context, spawnGroupID string) ([]threadstore.SpawnChildResult, error) {
	return a.store.ListSpawnResults(ctx, spawnGroupID)
}

func (a *commandAPI) resolveCommandSubject(ctx context.Context, threadID string, kind agentcmd.Kind) (string, bool, threadstore.OwnerRecord, error) {
	switch kind {
	case agentcmd.KindThreadStart, agentcmd.KindThreadAdopt:
		return agentcmd.DispatchSubject(kind), false, threadstore.OwnerRecord{}, nil
	}

	owner, err := a.store.LoadOwner(ctx, threadID)
	if err != nil {
		if errors.Is(err, threadstore.ErrThreadNotFound) {
			return agentcmd.DispatchSubject(kind), false, threadstore.OwnerRecord{}, nil
		}
		return "", false, threadstore.OwnerRecord{}, err
	}

	if strings.TrimSpace(owner.WorkerID) == "" || !owner.LeaseUntil.After(time.Now().UTC()) {
		return agentcmd.DispatchSubject(kind), false, owner, nil
	}

	return agentcmd.WorkerCommandSubject(owner.WorkerID, kind), true, owner, nil
}

func (a *commandAPI) publishCommand(ctx context.Context, subject string, cmd agentcmd.Command) error {
	payload, err := json.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("marshal command: %w", err)
	}

	msg := &nats.Msg{
		Subject: subject,
		Header:  nats.Header{},
		Data:    payload,
	}
	msg.Header.Set("Nats-Msg-Id", cmd.CmdID)

	if _, err := a.runtime.NATS().JetStream().PublishMsg(msg); err != nil {
		return fmt.Errorf("publish command to %s: %w", subject, err)
	}

	a.logger.Info("published command",
		append(agentcmd.LogAttrs(cmd),
			"subject", subject,
		)...,
	)

	return nil
}

func decodeJSONBody(r *http.Request, dst any) error {
	defer r.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(r.Body, (4<<20)+1))
	if err != nil {
		return fmt.Errorf("read request body: %w", err)
	}
	if len(payload) == 0 {
		return fmt.Errorf("request body is required")
	}
	if len(payload) > 4<<20 {
		return fmt.Errorf("request body exceeds 4194304 bytes")
	}

	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(dst); err != nil {
		return fmt.Errorf("decode json body: %w", err)
	}

	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("request body must contain a single JSON object")
	}

	return nil
}

func methodNotAllowed(w http.ResponseWriter, methods ...string) {
	w.Header().Set("Allow", strings.Join(methods, ", "))
	writeErrorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
}

func parseThreadRoute(path string) (threadRoute, bool) {
	trimmed := strings.Trim(strings.TrimPrefix(path, "/threads/"), "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) == 0 || parts[0] == "" || len(parts) > 4 {
		return threadRoute{}, false
	}

	route := threadRoute{ThreadID: parts[0]}
	if len(parts) == 2 {
		route.Resource = parts[1]
	}
	if len(parts) == 3 {
		if parts[2] == "" {
			return threadRoute{}, false
		}
		route.Resource = parts[1]
		route.ResourceID = parts[2]
	}
	if len(parts) == 4 {
		if parts[1] != "spawn-groups" || parts[2] == "" || parts[3] == "" {
			return threadRoute{}, false
		}
		route.Resource = parts[1]
		route.ResourceID = parts[2]
		route.Subresource = parts[3]
	}

	if route.ResourceID != "" {
		switch route.Resource {
		case "responses":
			if route.Subresource != "" {
				return threadRoute{}, false
			}
		case "spawn-groups":
		default:
			return threadRoute{}, false
		}
	}

	return route, true
}

func normalizeCommandBody(kind agentcmd.Kind, raw json.RawMessage) (json.RawMessage, error) {
	switch kind {
	case agentcmd.KindThreadResume:
		return normalizeResumeBody(raw)
	case agentcmd.KindThreadSubmitToolOutput:
		cmd := agentcmd.Command{Kind: kind, Body: raw}
		if _, err := cmd.SubmitToolOutputBody(); err != nil {
			return nil, err
		}
		return raw, nil
	case agentcmd.KindThreadCancel:
		return normalizeOptionalBody(raw)
	case agentcmd.KindThreadDisconnectSocket:
		return normalizeOptionalBody(raw)
	case agentcmd.KindThreadAdopt:
		return normalizeOptionalBody(raw)
	case agentcmd.KindThreadChildCompleted, agentcmd.KindThreadChildFailed:
		cmd := agentcmd.Command{Kind: kind, Body: raw}
		if _, err := cmd.ChildResultBody(); err != nil {
			return nil, err
		}
		return raw, nil
	case agentcmd.KindThreadRotateSocket, agentcmd.KindThreadReconcile:
		return normalizeOptionalBody(raw)
	default:
		return nil, fmt.Errorf("unsupported command kind %q", kind)
	}
}

func attachedDocumentIDsForCommand(kind agentcmd.Kind, raw json.RawMessage) ([]string, error) {
	if kind != agentcmd.KindThreadResume {
		return nil, nil
	}
	return extractAttachedDocumentIDs(raw)
}

func normalizeResumeBody(raw json.RawMessage) (json.RawMessage, error) {
	body, err := normalizeObjectBody(raw)
	if err != nil {
		return nil, err
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode resume body: %w", err)
	}

	if rawPreparedInputRef, ok := payload["prepared_input_ref"]; ok && !isBlankJSON(rawPreparedInputRef) {
		return nil, fmt.Errorf("prepared_input_ref is internal-only and is not accepted by the public API")
	}

	if rawInputItems, ok := payload["input_items"]; ok && !isBlankJSON(rawInputItems) {
		inputItems, err := normalizeResponseInput(rawInputItems)
		if err != nil {
			return nil, fmt.Errorf("normalize input_items: %w", err)
		}
		payload["input_items"] = inputItems
	}

	if reasoningRaw, ok := payload["reasoning"]; ok {
		reasoning, err := agentcmd.NormalizeReasoning(reasoningRaw)
		if err != nil {
			return nil, fmt.Errorf("normalize reasoning: %w", err)
		}
		if len(reasoning) == 0 {
			delete(payload, "reasoning")
		} else {
			payload["reasoning"] = reasoning
		}
	}

	normalized, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal resume body: %w", err)
	}

	cmd := agentcmd.Command{Kind: agentcmd.KindThreadResume, Body: normalized}
	if _, err := cmd.ResumeBody(); err != nil {
		return nil, err
	}

	return normalized, nil
}

func normalizeOptionalBody(raw json.RawMessage) (json.RawMessage, error) {
	if isBlankJSON(raw) {
		return json.RawMessage("{}"), nil
	}
	return normalizeObjectBody(raw)
}

func extractAttachedDocumentIDs(raw json.RawMessage) ([]string, error) {
	if isBlankJSON(raw) {
		return nil, nil
	}

	var payload struct {
		AttachedDocumentIDs []string `json:"attached_document_ids"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("decode attached_document_ids: %w", err)
	}

	return normalizeAttachedDocumentIDs(payload.AttachedDocumentIDs)
}

func normalizeAttachedDocumentIDs(rawIDs []string) ([]string, error) {
	if len(rawIDs) == 0 {
		return nil, nil
	}

	seen := make(map[string]struct{}, len(rawIDs))
	ids := make([]string, 0, len(rawIDs))
	for _, rawID := range rawIDs {
		id := strings.TrimSpace(rawID)
		if id == "" {
			return nil, fmt.Errorf("attached_document_ids cannot contain blank values")
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}

	return ids, nil
}

func (a *commandAPI) ensureDocumentsExist(ctx context.Context, documentIDs []string) error {
	for _, documentID := range documentIDs {
		if _, err := a.docs.Get(ctx, documentID); err != nil {
			if errors.Is(err, docstore.ErrDocumentNotFound) {
				return fmt.Errorf("%w: %s", docstore.ErrDocumentNotFound, documentID)
			}
			return err
		}
	}
	return nil
}

func normalizeObjectBody(raw json.RawMessage) (json.RawMessage, error) {
	if isBlankJSON(raw) {
		return nil, fmt.Errorf("command body is required")
	}

	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, fmt.Errorf("command body must be a JSON object")
	}

	var probe map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &probe); err != nil {
		return nil, fmt.Errorf("decode command body: %w", err)
	}

	return json.RawMessage(trimmed), nil
}

func normalizeResponseInput(raw json.RawMessage) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("input is required")
	}

	switch trimmed[0] {
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return nil, fmt.Errorf("decode input array: %w", err)
		}
		return json.RawMessage(trimmed), nil
	case '{':
		return wrapSingleInputItem(trimmed)
	case '"':
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return nil, fmt.Errorf("decode input text: %w", err)
		}
		return marshalInputText(text)
	default:
		return nil, fmt.Errorf("input must be an array, an item object, or a JSON string")
	}
}

func wrapSingleInputItem(raw json.RawMessage) (json.RawMessage, error) {
	var item map[string]any
	if err := json.Unmarshal(raw, &item); err != nil {
		return nil, fmt.Errorf("decode input item: %w", err)
	}

	payload, err := json.Marshal([]map[string]any{item})
	if err != nil {
		return nil, fmt.Errorf("marshal wrapped input item: %w", err)
	}

	return payload, nil
}

func marshalInputText(text string) (json.RawMessage, error) {
	payload, err := json.Marshal([]map[string]any{{
		"type": "message",
		"role": "user",
		"content": []map[string]any{{
			"type": "input_text",
			"text": text,
		}},
	}})
	if err != nil {
		return nil, fmt.Errorf("marshal input text: %w", err)
	}

	return payload, nil
}

func isBlankJSON(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return trimmed == "" || trimmed == "null"
}

func parseListQuery(r *http.Request) (listQuery, error) {
	query := listQuery{
		Limit:  100,
		After:  strings.TrimSpace(r.URL.Query().Get("after")),
		Before: strings.TrimSpace(r.URL.Query().Get("before")),
	}

	if query.After != "" && query.Before != "" {
		return listQuery{}, fmt.Errorf("after and before cannot both be set")
	}

	if rawLimit := strings.TrimSpace(r.URL.Query().Get("limit")); rawLimit != "" {
		var err error
		query.Limit, err = parsePositiveInt64(rawLimit)
		if err != nil {
			return listQuery{}, fmt.Errorf("invalid limit: %w", err)
		}
	}

	if err := validateListCursor(query.After); err != nil {
		return listQuery{}, fmt.Errorf("invalid after cursor: %w", err)
	}
	if err := validateListCursor(query.Before); err != nil {
		return listQuery{}, fmt.Errorf("invalid before cursor: %w", err)
	}

	if query.Limit > 500 {
		query.Limit = 500
	}

	return query, nil
}

func parsePositiveInt64(raw string) (int64, error) {
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, err
	}
	if value <= 0 {
		return 0, fmt.Errorf("must be greater than zero")
	}
	return value, nil
}

func validateListCursor(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}

	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return fmt.Errorf("cursor must be a numeric sequence")
	}
	if value <= 0 {
		return fmt.Errorf("cursor must be greater than zero")
	}

	return nil
}

func presentThreadMeta(meta threadstore.ThreadMeta) map[string]any {
	response := map[string]any{
		"id":                    meta.ID,
		"root_thread_id":        meta.RootThreadID,
		"parent_thread_id":      meta.ParentThreadID,
		"parent_call_id":        meta.ParentCallID,
		"depth":                 meta.Depth,
		"status":                meta.Status,
		"model":                 meta.Model,
		"instructions":          meta.Instructions,
		"owner_worker_id":       meta.OwnerWorkerID,
		"socket_generation":     meta.SocketGeneration,
		"last_response_id":      meta.LastResponseID,
		"active_response_id":    meta.ActiveResponseID,
		"active_spawn_group_id": meta.ActiveSpawnGroupID,
		"created_at":            meta.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":            meta.UpdatedAt.UTC().Format(time.RFC3339),
	}

	if !meta.SocketExpiresAt.IsZero() {
		response["socket_expires_at"] = meta.SocketExpiresAt.UTC().Format(time.RFC3339)
	}

	if decoded, err := decodeOptionalJSONObject(meta.MetadataJSON); err == nil && decoded != nil {
		response["metadata"] = decoded
	}
	if decoded, err := decodeOptionalJSONObject(meta.ToolsJSON); err == nil && decoded != nil {
		response["tools"] = decoded
	}
	if decoded, err := decodeOptionalJSONObject(meta.ToolChoiceJSON); err == nil && decoded != nil {
		response["tool_choice"] = decoded
	}

	return response
}

func presentThreadListEntry(entry postgresstore.ThreadListEntry) map[string]any {
	label := entry.FirstMessageText
	if strings.TrimSpace(label) == "" {
		label = "New thread"
	}
	preview := entry.LatestMessageText
	response := map[string]any{
		"id":             entry.Meta.ID,
		"root_thread_id": entry.Meta.RootThreadID,
		"status":         entry.Meta.Status,
		"model":          entry.Meta.Model,
		"label":          label,
		"preview_text":   preview,
		"created_at":     entry.Meta.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":     entry.Meta.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if !entry.LatestMessageAt.IsZero() {
		response["latest_message_at"] = entry.LatestMessageAt.UTC().Format(time.RFC3339)
	}
	if preview != "" {
		response["preview_role"] = "user"
		if entry.LatestMessageIsOut {
			response["preview_role"] = "assistant"
		}
	}
	return response
}

func presentAttachedDocuments(documents []docstore.Document) []map[string]any {
	presented := make([]map[string]any, 0, len(documents))
	for _, document := range documents {
		presented = append(presented, map[string]any{
			"id":         document.ID,
			"filename":   document.Filename,
			"status":     document.Status,
			"page_count": document.PageCount,
		})
	}
	return presented
}

func presentOwner(owner threadstore.OwnerRecord) map[string]any {
	response := map[string]any{
		"worker_id":         owner.WorkerID,
		"socket_generation": owner.SocketGeneration,
	}
	if !owner.LeaseUntil.IsZero() {
		response["lease_until"] = owner.LeaseUntil.UTC().Format(time.RFC3339)
	}
	if !owner.ClaimedAt.IsZero() {
		response["claimed_at"] = owner.ClaimedAt.UTC().Format(time.RFC3339)
	}
	if !owner.UpdatedAt.IsZero() {
		response["updated_at"] = owner.UpdatedAt.UTC().Format(time.RFC3339)
	}
	return response
}

func presentSpawnGroup(spawn threadstore.SpawnGroupMeta) map[string]any {
	response := map[string]any{
		"id":                     spawn.ID,
		"parent_thread_id":       spawn.ParentThreadID,
		"parent_call_id":         spawn.ParentCallID,
		"expected":               spawn.Expected,
		"completed":              spawn.Completed,
		"failed":                 spawn.Failed,
		"cancelled":              spawn.Cancelled,
		"status":                 spawn.Status,
		"aggregate_cmd_id":       spawn.AggregateCmdID,
		"created_at":             spawn.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":             spawn.UpdatedAt.UTC().Format(time.RFC3339),
		"aggregate_submitted_at": "",
	}
	if !spawn.AggregateSubmittedAt.IsZero() {
		response["aggregate_submitted_at"] = spawn.AggregateSubmittedAt.UTC().Format(time.RFC3339)
	}
	return response
}

func presentSpawnResult(result threadstore.SpawnChildResult) map[string]any {
	response := map[string]any{
		"child_thread_id": result.ChildThreadID,
		"status":          result.Status,
	}
	if result.ChildResponseID != "" {
		response["child_response_id"] = result.ChildResponseID
	}
	if result.AssistantText != "" {
		response["assistant_text"] = result.AssistantText
	}
	if result.ResultRef != "" {
		response["result_ref"] = result.ResultRef
	}
	if result.SummaryRef != "" {
		response["summary_ref"] = result.SummaryRef
	}
	if result.ErrorRef != "" {
		response["error_ref"] = result.ErrorRef
	}
	if !result.UpdatedAt.IsZero() {
		response["updated_at"] = result.UpdatedAt.UTC().Format(time.RFC3339)
	}
	return response
}

func presentItem(item threadstore.ItemRecord) map[string]any {
	response := map[string]any{
		"cursor":      strconv.FormatInt(item.Seq, 10),
		"seq":         item.Seq,
		"response_id": item.ResponseID,
		"item_type":   item.ItemType,
		"direction":   item.Direction,
		"created_at":  item.CreatedAt.UTC().Format(time.RFC3339),
	}
	if decoded, err := decodeRawJSON(item.Payload); err == nil && decoded != nil {
		response["payload"] = decoded
	}
	return response
}

func presentEvent(event threadstore.EventRecord) map[string]any {
	response := map[string]any{
		"cursor":            strconv.FormatInt(event.EventSeq, 10),
		"event_seq":         event.EventSeq,
		"socket_generation": event.SocketGeneration,
		"event_type":        event.EventType,
		"response_id":       event.ResponseID,
		"created_at":        event.CreatedAt.UTC().Format(time.RFC3339),
	}
	if decoded, err := decodeRawJSON(event.Payload); err == nil && decoded != nil {
		response["payload"] = decoded
	}
	return response
}

type pageBounds struct {
	FirstCursor string
	LastCursor  string
}

func presentPage(query listQuery, bounds pageBounds, count int) map[string]any {
	page := map[string]any{
		"limit":  query.Limit,
		"after":  query.After,
		"before": query.Before,
		"count":  count,
	}
	if bounds.FirstCursor != "" {
		page["first_cursor"] = bounds.FirstCursor
	}
	if bounds.LastCursor != "" {
		page["last_cursor"] = bounds.LastCursor
	}
	return page
}

func decodeOptionalJSONObject(raw string) (any, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	return decodeRawJSON(json.RawMessage(raw))
}

func decodeRawJSON(raw json.RawMessage) (any, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, nil
	}

	var decoded any
	if err := json.Unmarshal(trimmed, &decoded); err != nil {
		return string(trimmed), nil
	}

	return decoded, nil
}

func itemPageBounds(items []threadstore.ItemRecord) pageBounds {
	if len(items) == 0 {
		return pageBounds{}
	}
	return pageBounds{
		FirstCursor: strconv.FormatInt(items[0].Seq, 10),
		LastCursor:  strconv.FormatInt(items[len(items)-1].Seq, 10),
	}
}

func eventPageBounds(events []threadstore.EventRecord) pageBounds {
	if len(events) == 0 {
		return pageBounds{}
	}
	return pageBounds{
		FirstCursor: strconv.FormatInt(events[0].EventSeq, 10),
		LastCursor:  strconv.FormatInt(events[len(events)-1].EventSeq, 10),
	}
}

func isTerminalThreadStatus(status threadstore.ThreadStatus) bool {
	switch status {
	case threadstore.ThreadStatusCompleted, threadstore.ThreadStatusFailed, threadstore.ThreadStatusCancelled, threadstore.ThreadStatusIncomplete:
		return true
	default:
		return false
	}
}

func requiresActiveThread(kind agentcmd.Kind) bool {
	switch kind {
	case agentcmd.KindThreadResume, agentcmd.KindThreadSubmitToolOutput:
		return true
	default:
		return false
	}
}
