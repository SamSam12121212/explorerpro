package worker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"explorer/internal/blobstore"
	"explorer/internal/doccmd"
	"explorer/internal/docstore"
	"explorer/internal/openaiws"
	"explorer/internal/preparedinput"
	"explorer/internal/threadcmd"
	"explorer/internal/threadstore"

	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
)

func tid(name string) int64 {
	var sum int64
	for _, r := range name {
		sum = sum*131 + int64(r)
	}
	if sum < 0 {
		sum = -sum
	}
	if sum == 0 {
		return 1
	}
	return sum
}

func stableDocumentChildThreadID(parentThreadID, parentCallID string, documentID int64, phase string) int64 {
	return tid(fmt.Sprintf("%s_doc_%d", parentThreadID, documentID))
}

func stableDocumentSpawnGroupID(parentThreadID int64, parentCallID string) int64 {
	return tid(fmt.Sprintf("sg_doc_%d_%s", parentThreadID, parentCallID))
}

func TestWrapRawItemAsArray(t *testing.T) {
	raw, err := wrapRawItemAsArray([]byte(`{"type":"function_call_output","call_id":"call_123"}`))
	if err != nil {
		t.Fatalf("wrapRawItemAsArray() error = %v", err)
	}

	if got := string(raw); got != `[{"type":"function_call_output","call_id":"call_123"}]` {
		t.Fatalf("wrapRawItemAsArray() = %s", got)
	}
}

func TestStableDocumentChildThreadIDIgnoresInvocationAndPhase(t *testing.T) {
	t.Parallel()

	warmupID := stableDocumentChildThreadID("thread_root", "call_1", 1, "warmup")
	queryID := stableDocumentChildThreadID("thread_root", "call_2", 1, "query")

	if warmupID != queryID {
		t.Fatalf("stableDocumentChildThreadID() = %d and %d, want same stable id", warmupID, queryID)
	}
}

func TestAggregateSpawnOutputItem(t *testing.T) {
	raw, err := aggregateSpawnOutputItem(threadstore.SpawnGroupMeta{
		ID:           tid("sg_123"),
		ParentCallID: "call_parent",
	}, []threadstore.SpawnChildResult{
		{
			ChildThreadID:   tid("thread_child_1"),
			Status:          "completed",
			ChildResponseID: "resp_1",
			AssistantText:   "Auth paths look healthy.",
			ResultRef:       "blob://child-results/1.json",
		},
		{
			ChildThreadID: tid("thread_child_2"),
			Status:        "failed",
			ErrorRef:      "blob://child-results/2-error.json",
		},
	})
	if err != nil {
		t.Fatalf("aggregateSpawnOutputItem() error = %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	if decoded["type"] != "function_call_output" {
		t.Fatalf("type = %v, want function_call_output", decoded["type"])
	}

	output, ok := decoded["output"].(string)
	if !ok {
		t.Fatalf("output = %#v, want string", decoded["output"])
	}

	var parsed map[string]any
	if err := json.Unmarshal([]byte(output), &parsed); err != nil {
		t.Fatalf("json.Unmarshal(output) error = %v", err)
	}

	if parsed["spawn_group_id"] != float64(tid("sg_123")) {
		t.Fatalf("spawn_group_id = %v, want %d", parsed["spawn_group_id"], tid("sg_123"))
	}

	children, ok := parsed["children"].([]any)
	if !ok || len(children) != 2 {
		t.Fatalf("children = %#v, want 2 entries", parsed["children"])
	}

	firstChild, ok := children[0].(map[string]any)
	if !ok {
		t.Fatalf("first child = %#v, want object", children[0])
	}
	if firstChild["assistant_text"] != "Auth paths look healthy." {
		t.Fatalf("assistant_text = %v, want %q", firstChild["assistant_text"], "Auth paths look healthy.")
	}
}

func TestAggregateSpawnOutputItemSkipsPendingChildren(t *testing.T) {
	t.Parallel()

	raw, err := aggregateSpawnOutputItem(threadstore.SpawnGroupMeta{
		ID:           tid("sg_123"),
		ParentCallID: "call_parent",
	}, []threadstore.SpawnChildResult{
		{
			ChildThreadID: tid("thread_warmup"),
			Status:        "pending",
		},
		{
			ChildThreadID:   tid("thread_query"),
			Status:          "completed",
			ChildResponseID: "resp_query",
		},
	})
	if err != nil {
		t.Fatalf("aggregateSpawnOutputItem() error = %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	output, ok := decoded["output"].(string)
	if !ok {
		t.Fatalf("output = %#v, want string", decoded["output"])
	}

	var parsed map[string]any
	if err := json.Unmarshal([]byte(output), &parsed); err != nil {
		t.Fatalf("json.Unmarshal(output) error = %v", err)
	}

	children, ok := parsed["children"].([]any)
	if !ok || len(children) != 1 {
		t.Fatalf("children = %#v, want 1 terminal entry", parsed["children"])
	}

	child, ok := children[0].(map[string]any)
	if !ok {
		t.Fatalf("child = %#v, want object", children[0])
	}
	if child["thread_id"] != float64(tid("thread_query")) {
		t.Fatalf("thread_id = %v, want %d", child["thread_id"], tid("thread_query"))
	}
}

func TestAggregateSpawnOutputItemForMultipleDocumentQueryCalls(t *testing.T) {
	t.Parallel()

	parentCallID, err := encodeDocQueryRoundCalls([]docQueryRoundCall{
		{CallID: "call_parent_1", DocumentID: 1, Task: "Summarize doc 1"},
		{CallID: "call_parent_2", DocumentID: 2, Task: "Summarize doc 2"},
	})
	if err != nil {
		t.Fatalf("encodeDocQueryRoundCalls() error = %v", err)
	}

	raw, err := aggregateSpawnOutputItem(threadstore.SpawnGroupMeta{
		ID:             tid("sg_doc_round"),
		ParentThreadID: tid("thread_parent"),
		ParentCallID:   parentCallID,
	}, []threadstore.SpawnChildResult{
		{
			ChildThreadID:   stableDocumentChildThreadID("thread_parent", "call_parent_1", 1, "query"),
			DocumentID:      1,
			Status:          "completed",
			ChildResponseID: "resp_doc_1",
			AssistantText:   "Doc 1 summary",
		},
		{
			ChildThreadID:   stableDocumentChildThreadID("thread_parent", "call_parent_2", 2, "query"),
			DocumentID:      2,
			Status:          "failed",
			ChildResponseID: "resp_doc_2",
			ErrorRef:        "blob://errors/doc-2.json",
		},
	})
	if err != nil {
		t.Fatalf("aggregateSpawnOutputItem() error = %v", err)
	}

	var decoded []map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if len(decoded) != 2 {
		t.Fatalf("len(decoded) = %d, want 2", len(decoded))
	}
	if decoded[0]["call_id"] != "call_parent_1" {
		t.Fatalf("decoded[0].call_id = %v, want call_parent_1", decoded[0]["call_id"])
	}
	if decoded[1]["call_id"] != "call_parent_2" {
		t.Fatalf("decoded[1].call_id = %v, want call_parent_2", decoded[1]["call_id"])
	}

	for i, wantThreadID := range []int64{
		stableDocumentChildThreadID("thread_parent", "call_parent_1", 1, "query"),
		stableDocumentChildThreadID("thread_parent", "call_parent_2", 2, "query"),
	} {
		output, ok := decoded[i]["output"].(string)
		if !ok {
			t.Fatalf("decoded[%d].output = %#v, want string", i, decoded[i]["output"])
		}

		var payload map[string]any
		if err := json.Unmarshal([]byte(output), &payload); err != nil {
			t.Fatalf("json.Unmarshal(output) error = %v", err)
		}

		children, ok := payload["children"].([]any)
		if !ok || len(children) != 1 {
			t.Fatalf("payload[%d].children = %#v, want 1 entry", i, payload["children"])
		}
		child, ok := children[0].(map[string]any)
		if !ok {
			t.Fatalf("payload[%d].children[0] = %#v, want object", i, children[0])
		}
		if child["thread_id"] != float64(wantThreadID) {
			t.Fatalf("payload[%d].children[0].thread_id = %v, want %d", i, child["thread_id"], wantThreadID)
		}
	}
}

func TestResponseCreateInputKind(t *testing.T) {
	tests := []struct {
		name    string
		payload map[string]any
		want    string
	}{
		{
			name: "prepared input",
			payload: map[string]any{
				"prepared_input_ref": "blob://prepared/input.json",
			},
			want: "prepared_input",
		},
		{
			name: "user message",
			payload: map[string]any{
				"input": json.RawMessage(`[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]`),
			},
			want: "user_message",
		},
		{
			name: "function call output",
			payload: map[string]any{
				"input": json.RawMessage(`[{"type":"function_call_output","call_id":"call_123","output":"{}"}]`),
			},
			want: "function_call_output",
		},
		{
			name:    "no input",
			payload: map[string]any{},
			want:    "none",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := responseCreateInputKind(tt.payload); got != tt.want {
				t.Fatalf("responseCreateInputKind() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAppendThreadGraphAttrs(t *testing.T) {
	meta := threadstore.ThreadMeta{
		ID:                 tid("thread_child"),
		RootThreadID:       tid("thread_root"),
		ParentThreadID:     tid("thread_parent"),
		ParentCallID:       "call_parent",
		Depth:              2,
		ActiveSpawnGroupID: tid("sg_123"),
	}

	got := appendThreadGraphAttrs([]any{"event", "start"}, meta)
	want := []any{
		"event", "start",
		"root_thread_id", tid("thread_root"),
		"parent_thread_id", tid("thread_parent"),
		"parent_call_id", "call_parent",
		"depth", 2,
		"spawn_group_id", tid("sg_123"),
	}

	if fmt.Sprintf("%#v", got) != fmt.Sprintf("%#v", want) {
		t.Fatalf("appendThreadGraphAttrs() = %#v, want %#v", got, want)
	}
}

func TestAppendThreadGraphAttrsSkipsExistingKeys(t *testing.T) {
	meta := threadstore.ThreadMeta{
		ID:                 tid("thread_child"),
		RootThreadID:       tid("thread_root"),
		ParentThreadID:     tid("thread_parent"),
		ParentCallID:       "call_parent",
		Depth:              2,
		ActiveSpawnGroupID: tid("sg_meta"),
	}

	got := appendThreadGraphAttrs([]any{
		"spawn_group_id", tid("sg_explicit"),
		"depth", 9,
		"root_thread_id", "thread_explicit_root",
	}, meta)
	want := []any{
		"spawn_group_id", tid("sg_explicit"),
		"depth", 9,
		"root_thread_id", "thread_explicit_root",
		"parent_thread_id", tid("thread_parent"),
		"parent_call_id", "call_parent",
	}

	if fmt.Sprintf("%#v", got) != fmt.Sprintf("%#v", want) {
		t.Fatalf("appendThreadGraphAttrs() with explicit keys = %#v, want %#v", got, want)
	}
}

func TestAppendCommandLifecycleGraphAttrs(t *testing.T) {
	store := newFakeActorStore(t)
	store.threads[tid("thread_child")] = threadstore.ThreadMeta{
		ID:                 tid("thread_child"),
		RootThreadID:       tid("thread_root"),
		ParentThreadID:     tid("thread_parent"),
		ParentCallID:       "call_parent",
		Depth:              1,
		ActiveSpawnGroupID: tid("sg_123"),
	}

	actor := newActorRecoveryHarness(t, store, nil)
	got := actor.appendCommandLifecycleGraphAttrs([]any{
		"cmd_id", int64(123),
		"kind", threadcmd.KindThreadStart,
	}, threadcmd.Command{
		CmdID:        123,
		Kind:         threadcmd.KindThreadStart,
		ThreadID:     tid("thread_child"),
		RootThreadID: tid("thread_root"),
	})

	want := []any{
		"cmd_id", int64(123),
		"kind", threadcmd.KindThreadStart,
		"root_thread_id", tid("thread_root"),
		"parent_thread_id", tid("thread_parent"),
		"parent_call_id", "call_parent",
		"depth", 1,
		"spawn_group_id", tid("sg_123"),
	}

	if fmt.Sprintf("%#v", got) != fmt.Sprintf("%#v", want) {
		t.Fatalf("appendCommandLifecycleGraphAttrs() = %#v, want %#v", got, want)
	}
}

func TestAppendCommandLifecycleGraphAttrsFallsBackToCommandRootThreadID(t *testing.T) {
	actor := newActorRecoveryHarness(t, newFakeActorStore(t), nil)
	got := actor.appendCommandLifecycleGraphAttrs([]any{
		"cmd_id", int64(123),
		"kind", threadcmd.KindThreadResume,
	}, threadcmd.Command{
		CmdID:        123,
		Kind:         threadcmd.KindThreadResume,
		ThreadID:     tid("thread_missing"),
		RootThreadID: tid("thread_root"),
	})

	want := []any{
		"cmd_id", int64(123),
		"kind", threadcmd.KindThreadResume,
		"root_thread_id", tid("thread_root"),
		"depth", 0,
	}

	if fmt.Sprintf("%#v", got) != fmt.Sprintf("%#v", want) {
		t.Fatalf("appendCommandLifecycleGraphAttrs() fallback = %#v, want %#v", got, want)
	}
}

func TestValidateCommandPreconditions(t *testing.T) {
	t.Parallel()

	meta := threadstore.ThreadMeta{
		ID:               tid("thread_123"),
		Status:           threadstore.ThreadStatusWaitingTool,
		LastResponseID:   "resp_123",
		SocketGeneration: 7,
	}

	if err := validateCommandPreconditions(threadcmd.Command{
		ExpectedStatus:           "waiting_tool",
		ExpectedLastResponseID:   "resp_123",
		ExpectedSocketGeneration: 7,
	}, meta); err != nil {
		t.Fatalf("validateCommandPreconditions() unexpected error: %v", err)
	}

	err := validateCommandPreconditions(threadcmd.Command{
		ExpectedSocketGeneration: 8,
	}, meta)
	if !errors.Is(err, errCommandPrecond) {
		t.Fatalf("expected errCommandPrecond, got %v", err)
	}
}

func TestExtractResponseCreatePayload(t *testing.T) {
	t.Parallel()

	payload, err := extractResponseCreatePayload([]byte(`{
		"type":"response.create",
		"event_id":"cmd_123",
		"model":"gpt-5.4",
		"previous_response_id":"resp_prev",
		"store":true
	}`))
	if err != nil {
		t.Fatalf("extractResponseCreatePayload() error = %v", err)
	}

	if payload["model"] != "gpt-5.4" {
		t.Fatalf("model = %v, want gpt-5.4", payload["model"])
	}
	if payload["previous_response_id"] != "resp_prev" {
		t.Fatalf("previous_response_id = %v, want resp_prev", payload["previous_response_id"])
	}
}

func TestExtractResponseCreatePayloadLegacyNestedShape(t *testing.T) {
	t.Parallel()

	payload, err := extractResponseCreatePayload([]byte(`{
		"type":"response.create",
		"event_id":"cmd_legacy",
		"response":{
			"model":"gpt-5.4",
			"store":true
		}
	}`))
	if err != nil {
		t.Fatalf("extractResponseCreatePayload() error = %v", err)
	}

	if payload["model"] != "gpt-5.4" {
		t.Fatalf("model = %v, want gpt-5.4", payload["model"])
	}
}

func TestBuildResponseCreatePayloadMergesThreadToolState(t *testing.T) {
	t.Parallel()

	payloadJSON, err := buildResponseCreatePayload(threadstore.ThreadMeta{
		MetadataJSON:   `{"tenant":"acme"}`,
		IncludeJSON:    `["reasoning.encrypted_content"]`,
		ToolsJSON:      `[{"type":"function","name":"spawn_threads"}]`,
		ToolChoiceJSON: `{"type":"function","name":"spawn_threads"}`,
	}, map[string]any{
		"model": "gpt-5.4",
		"input": json.RawMessage(`[{"type":"message","role":"user"}]`),
	})
	if err != nil {
		t.Fatalf("buildResponseCreatePayload() error = %v", err)
	}

	payload, err := decodeResponseCreatePayloadObject(payloadJSON)
	if err != nil {
		t.Fatalf("decodeResponseCreatePayloadObject() error = %v", err)
	}

	if payload["metadata"] == nil {
		t.Fatalf("metadata missing from payload")
	}
	if payload["include"] == nil {
		t.Fatalf("include missing from payload")
	}
	if payload["tools"] == nil {
		t.Fatalf("tools missing from payload")
	}
	if payload["tool_choice"] == nil {
		t.Fatalf("tool_choice missing from payload")
	}
}

func TestBuildResponseCreatePayloadStripsReasoningSummaryFields(t *testing.T) {
	t.Parallel()

	payloadJSON, err := buildResponseCreatePayload(threadstore.ThreadMeta{}, map[string]any{
		"model":     "gpt-5.4",
		"reasoning": json.RawMessage(`{"effort":"high","summary":"detailed","generate_summary":"concise"}`),
	})
	if err != nil {
		t.Fatalf("buildResponseCreatePayload() error = %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	reasoning, ok := payload["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("reasoning = %#v, want map[string]any", payload["reasoning"])
	}
	if reasoning["effort"] != "high" {
		t.Fatalf("effort = %v, want high", reasoning["effort"])
	}
	if _, exists := reasoning["summary"]; exists {
		t.Fatalf("summary should be omitted, got %#v", reasoning["summary"])
	}
	if _, exists := reasoning["generate_summary"]; exists {
		t.Fatalf("generate_summary should be omitted, got %#v", reasoning["generate_summary"])
	}
}

func TestBuildResponseCreatePayloadNormalizesStoredReasoning(t *testing.T) {
	t.Parallel()

	payloadJSON, err := buildResponseCreatePayload(threadstore.ThreadMeta{
		ReasoningJSON: `{"effort":"medium","summary":"concise","generate_summary":"detailed"}`,
	}, map[string]any{
		"model": "gpt-5.4",
	})
	if err != nil {
		t.Fatalf("buildResponseCreatePayload() error = %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	reasoning, ok := payload["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("reasoning = %#v, want map[string]any", payload["reasoning"])
	}
	if reasoning["effort"] != "medium" {
		t.Fatalf("effort = %v, want medium", reasoning["effort"])
	}
	if _, exists := reasoning["summary"]; exists {
		t.Fatalf("summary should be omitted, got %#v", reasoning["summary"])
	}
	if _, exists := reasoning["generate_summary"]; exists {
		t.Fatalf("generate_summary should be omitted, got %#v", reasoning["generate_summary"])
	}
}

func TestBuildResponseCreatePayloadPreservesToolChoiceMode(t *testing.T) {
	t.Parallel()

	payloadJSON, err := buildResponseCreatePayload(threadstore.ThreadMeta{}, map[string]any{
		"model":       "gpt-5.4",
		"tool_choice": "required",
	})
	if err != nil {
		t.Fatalf("buildResponseCreatePayload() error = %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	if payload["tool_choice"] != "required" {
		t.Fatalf("tool_choice = %v, want required", payload["tool_choice"])
	}
}

func TestBuildResponseCreatePayloadNormalizesStoredToolChoiceMode(t *testing.T) {
	t.Parallel()

	payloadJSON, err := buildResponseCreatePayload(threadstore.ThreadMeta{
		ToolChoiceJSON: `"auto"`,
	}, map[string]any{
		"model": "gpt-5.4",
	})
	if err != nil {
		t.Fatalf("buildResponseCreatePayload() error = %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	if payload["tool_choice"] != "auto" {
		t.Fatalf("tool_choice = %v, want auto", payload["tool_choice"])
	}
}

func TestBuildResponseCreatePayloadNormalizesMetadataValuesToStrings(t *testing.T) {
	t.Parallel()

	payloadJSON, err := buildResponseCreatePayload(threadstore.ThreadMeta{}, map[string]any{
		"model": "gpt-5.4",
		"metadata": json.RawMessage(`{
			"tenant":"acme",
			"branch_index":2,
			"enabled":true,
			"filters":{"region":"eu"},
			"tags":["alpha","beta"],
			"empty":null
		}`),
	})
	if err != nil {
		t.Fatalf("buildResponseCreatePayload() error = %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	metadata, ok := payload["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("metadata = %#v, want map[string]any", payload["metadata"])
	}
	if metadata["tenant"] != "acme" {
		t.Fatalf("tenant = %v, want acme", metadata["tenant"])
	}
	if metadata["branch_index"] != "2" {
		t.Fatalf("branch_index = %v, want 2", metadata["branch_index"])
	}
	if metadata["enabled"] != "true" {
		t.Fatalf("enabled = %v, want true", metadata["enabled"])
	}
	if metadata["filters"] != `{"region":"eu"}` {
		t.Fatalf("filters = %v, want %q", metadata["filters"], `{"region":"eu"}`)
	}
	if metadata["tags"] != `["alpha","beta"]` {
		t.Fatalf("tags = %v, want %q", metadata["tags"], `["alpha","beta"]`)
	}
	if metadata["empty"] != "null" {
		t.Fatalf("empty = %v, want null", metadata["empty"])
	}
}

func TestBuildResponseCreatePayloadNormalizesStoredMetadata(t *testing.T) {
	t.Parallel()

	payloadJSON, err := buildResponseCreatePayload(threadstore.ThreadMeta{
		MetadataJSON: `{"tenant":"acme","branch_index":2}`,
	}, map[string]any{
		"model": "gpt-5.4",
	})
	if err != nil {
		t.Fatalf("buildResponseCreatePayload() error = %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	metadata, ok := payload["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("metadata = %#v, want map[string]any", payload["metadata"])
	}
	if metadata["branch_index"] != "2" {
		t.Fatalf("branch_index = %v, want 2", metadata["branch_index"])
	}
}

func TestApplyDocumentRuntimeContextUpdatesInstructionsAndTools(t *testing.T) {
	t.Parallel()

	actor := &threadActor{
		ctx: context.Background(),
		threadDocs: &fakeThreadDocumentStore{
			documentsByThread: map[int64][]docstore.Document{
				tid("thread_123"): {
					{ID: 1, Filename: "report.pdf"},
				},
			},
		},
		docRuntime: fakeDocRuntimeContextClient(func(_ context.Context, req doccmd.RuntimeContextRequest) (doccmd.RuntimeContextResponse, error) {
			if req.ThreadID != tid("thread_123") {
				t.Fatalf("ThreadID = %d, want %d", req.ThreadID, tid("thread_123"))
			}
			if req.Instructions != "Be concise." {
				t.Fatalf("Instructions = %q, want %q", req.Instructions, "Be concise.")
			}
			return doccmd.RuntimeContextResponse{
				RequestID:    req.RequestID,
				Status:       doccmd.PrepareStatusOK,
				Instructions: "Be concise.\n\n<available_documents>\n<document id=\"1\" name=\"report.pdf\" />\n</available_documents>",
				Tools:        json.RawMessage(`[{"type":"function","name":"lookup"},{"type":"function","name":"query_document"}]`),
			}, nil
		}),
	}

	payload := map[string]any{
		"instructions": "Be concise.",
		"tools":        []any{map[string]any{"type": "function", "name": "lookup"}},
	}

	if err := actor.applyDocumentRuntimeContext(threadstore.ThreadMeta{ID: tid("thread_123")}, payload); err != nil {
		t.Fatalf("applyDocumentRuntimeContext() error = %v", err)
	}

	if got := payload["instructions"]; got != "Be concise.\n\n<available_documents>\n<document id=\"1\" name=\"report.pdf\" />\n</available_documents>" {
		t.Fatalf("instructions = %v, want runtime-augmented instructions", got)
	}

	tools, ok := payload["tools"].([]responses.ToolUnionParam)
	if !ok || len(tools) != 2 {
		t.Fatalf("tools = %#v, want 2 tools", payload["tools"])
	}
	if toolParamName(tools[1]) != doccmd.ToolNameQueryDocument {
		t.Fatalf("tool name = %q, want %q", toolParamName(tools[1]), doccmd.ToolNameQueryDocument)
	}
}

func TestApplyDocumentRuntimeContextSkipsWhenNoDocumentsAttached(t *testing.T) {
	t.Parallel()

	actor := &threadActor{
		ctx: context.Background(),
		threadDocs: &fakeThreadDocumentStore{
			documentsByThread: map[int64][]docstore.Document{},
		},
		docRuntime: fakeDocRuntimeContextClient(func(_ context.Context, _ doccmd.RuntimeContextRequest) (doccmd.RuntimeContextResponse, error) {
			t.Fatal("RuntimeContext should not be called when no documents are attached")
			return doccmd.RuntimeContextResponse{}, nil
		}),
	}

	payload := map[string]any{
		"instructions": "Be concise.",
		"tools":        []any{map[string]any{"type": "function", "name": "lookup"}},
	}

	if err := actor.applyDocumentRuntimeContext(threadstore.ThreadMeta{ID: tid("thread_123")}, payload); err != nil {
		t.Fatalf("applyDocumentRuntimeContext() error = %v", err)
	}

	if got := payload["instructions"]; got != "Be concise." {
		t.Fatalf("instructions = %v, want original instructions", got)
	}

	tools, ok := payload["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %#v, want original tools", payload["tools"])
	}
}

func TestApplyDocumentRuntimeContextFallsBackLocallyOnClientError(t *testing.T) {
	t.Parallel()

	actor := &threadActor{
		ctx: context.Background(),
		threadDocs: &fakeThreadDocumentStore{
			documentsByThread: map[int64][]docstore.Document{
				tid("thread_123"): {
					{ID: 1, Filename: `Quarterly "Report" <Draft>.pdf`},
				},
			},
		},
		docRuntime: fakeDocRuntimeContextClient(func(_ context.Context, _ doccmd.RuntimeContextRequest) (doccmd.RuntimeContextResponse, error) {
			return doccmd.RuntimeContextResponse{}, errors.New(`runtime context response missing request_id (body="{}")`)
		}),
	}

	payload := map[string]any{
		"instructions": "Be concise.",
		"tools":        []any{map[string]any{"type": "function", "name": "lookup"}},
	}

	if err := actor.applyDocumentRuntimeContext(threadstore.ThreadMeta{ID: tid("thread_123")}, payload); err != nil {
		t.Fatalf("applyDocumentRuntimeContext() error = %v", err)
	}

	instructions, _ := payload["instructions"].(string)
	if !strings.Contains(instructions, `<document id="1" name="Quarterly &quot;Report&quot; &lt;Draft&gt;.pdf" />`) {
		t.Fatalf("instructions = %q, want local available_documents block", instructions)
	}

	tools, ok := payload["tools"].([]responses.ToolUnionParam)
	if !ok || len(tools) != 3 {
		t.Fatalf("tools = %#v, want lookup + query_document + read_document_page", payload["tools"])
	}
	if toolParamName(tools[1]) != doccmd.ToolNameQueryDocument {
		t.Fatalf("tool name[1] = %q, want %q", toolParamName(tools[1]), doccmd.ToolNameQueryDocument)
	}
	if toolParamName(tools[2]) != doccmd.ToolNameReadDocumentPage {
		t.Fatalf("tool name[2] = %q, want %q", toolParamName(tools[2]), doccmd.ToolNameReadDocumentPage)
	}
}

func TestApplyDocumentRuntimeContextLocalFallbackOmitsReadDocumentPageForChildThreads(t *testing.T) {
	t.Parallel()

	actor := &threadActor{
		ctx: context.Background(),
		threadDocs: &fakeThreadDocumentStore{
			documentsByThread: map[int64][]docstore.Document{
				tid("thread_child"): {
					{ID: 1, Filename: "report.pdf"},
				},
			},
		},
		docRuntime: fakeDocRuntimeContextClient(func(_ context.Context, _ doccmd.RuntimeContextRequest) (doccmd.RuntimeContextResponse, error) {
			return doccmd.RuntimeContextResponse{}, errors.New("force fallback")
		}),
	}

	payload := map[string]any{
		"instructions": "Be concise.",
		"tools":        []any{map[string]any{"type": "function", "name": "lookup"}},
	}

	childMeta := threadstore.ThreadMeta{ID: tid("thread_child"), ParentThreadID: tid("thread_parent")}
	if err := actor.applyDocumentRuntimeContext(childMeta, payload); err != nil {
		t.Fatalf("applyDocumentRuntimeContext() error = %v", err)
	}

	tools, ok := payload["tools"].([]responses.ToolUnionParam)
	if !ok || len(tools) != 2 {
		t.Fatalf("tools = %#v, want lookup + query_document only (no read_document_page on child threads)", payload["tools"])
	}
	for _, tool := range tools {
		if toolParamName(tool) == doccmd.ToolNameReadDocumentPage {
			t.Fatalf("child thread should not receive %q tool", doccmd.ToolNameReadDocumentPage)
		}
	}
}

func TestEnsureRequiredResponseIncludeAddsEncryptedContent(t *testing.T) {
	t.Parallel()

	payload := map[string]any{
		"model": "gpt-5.4",
	}

	if err := ensureRequiredResponseInclude(payload); err != nil {
		t.Fatalf("ensureRequiredResponseInclude() error = %v", err)
	}

	include, ok := payload["include"].([]responses.ResponseIncludable)
	if !ok {
		t.Fatalf("include = %#v, want []responses.ResponseIncludable", payload["include"])
	}
	if len(include) != 1 || include[0] != responses.ResponseIncludableReasoningEncryptedContent {
		t.Fatalf("include = %v, want [%q]", include, responses.ResponseIncludableReasoningEncryptedContent)
	}
}

func TestBuildThreadResponseCreatePayloadAddsRequiredIncludeAndDocumentTool(t *testing.T) {
	t.Parallel()

	actor := &threadActor{
		ctx: context.Background(),
		threadDocs: &fakeThreadDocumentStore{
			documentsByThread: map[int64][]docstore.Document{
				tid("thread_123"): {
					{ID: 1, Filename: "report.pdf"},
				},
			},
		},
		docRuntime: fakeDocRuntimeContextClient(func(_ context.Context, req doccmd.RuntimeContextRequest) (doccmd.RuntimeContextResponse, error) {
			return doccmd.RuntimeContextResponse{
				RequestID:    req.RequestID,
				Status:       doccmd.PrepareStatusOK,
				Instructions: req.Instructions,
				Tools:        json.RawMessage(`[{"type":"function","name":"query_document"}]`),
			}, nil
		}),
	}

	payload, err := actor.buildThreadResponseCreatePayload(threadstore.ThreadMeta{
		ID: tid("thread_123"),
	}, map[string]any{
		"model": "gpt-5.4",
		"input": json.RawMessage(`[{"type":"message","role":"user"}]`),
	})
	if err != nil {
		t.Fatalf("buildThreadResponseCreatePayload() error = %v", err)
	}

	include, ok := payload["include"].([]responses.ResponseIncludable)
	if !ok {
		t.Fatalf("include = %#v, want []responses.ResponseIncludable", payload["include"])
	}
	if len(include) != 1 || include[0] != responses.ResponseIncludableReasoningEncryptedContent {
		t.Fatalf("include = %v, want [%q]", include, responses.ResponseIncludableReasoningEncryptedContent)
	}

	tools, ok := payload["tools"].([]responses.ToolUnionParam)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %#v, want 1 tool", payload["tools"])
	}
	if toolParamName(tools[0]) != doccmd.ToolNameQueryDocument {
		t.Fatalf("tool name = %q, want %q", toolParamName(tools[0]), doccmd.ToolNameQueryDocument)
	}
}

func TestNormalizeInputItemsString(t *testing.T) {
	t.Parallel()

	raw, err := normalizeInputItems(json.RawMessage(`"review section 1"`))
	if err != nil {
		t.Fatalf("normalizeInputItems() error = %v", err)
	}

	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(items))
	}
	if items[0]["type"] != "message" {
		t.Fatalf("items[0].type = %v, want message", items[0]["type"])
	}
}

func TestLowerResponseCreatePayloadConvertsImageRef(t *testing.T) {
	t.Parallel()

	store, err := blobstore.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal() error = %v", err)
	}

	pngBytes, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aR1EAAAAASUVORK5CYII=")
	if err != nil {
		t.Fatalf("DecodeString() error = %v", err)
	}

	ref := store.Ref("images", "img_test", "source.png")
	if err := store.WriteRef(context.Background(), ref, pngBytes); err != nil {
		t.Fatalf("WriteRef() error = %v", err)
	}

	actor := &threadActor{
		ctx:  context.Background(),
		blob: store,
	}

	original := map[string]any{
		"model": "gpt-5.4",
		"input": []any{
			map[string]any{
				"type": "message",
				"role": "user",
				"content": []any{
					map[string]any{
						"type": "input_text",
						"text": "describe this image",
					},
					map[string]any{
						"type":         "image_ref",
						"image_ref":    ref,
						"content_type": "image/png",
					},
				},
			},
		},
	}

	lowered, stats, err := actor.lowerResponseCreatePayload(original)
	if err != nil {
		t.Fatalf("lowerResponseCreatePayload() error = %v", err)
	}
	if stats.InputItemsCount != 1 {
		t.Fatalf("InputItemsCount = %d, want 1", stats.InputItemsCount)
	}
	if stats.LoweredImageInputs != 1 {
		t.Fatalf("LoweredImageInputs = %d, want 1", stats.LoweredImageInputs)
	}
	if stats.LoweredBlobRefs != 1 {
		t.Fatalf("LoweredBlobRefs = %d, want 1", stats.LoweredBlobRefs)
	}

	items, ok := lowered["input"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("input = %#v, want one item", lowered["input"])
	}

	message, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("item = %#v, want object", items[0])
	}

	content, ok := message["content"].([]any)
	if !ok || len(content) != 2 {
		t.Fatalf("content = %#v, want two parts", message["content"])
	}

	imagePart, ok := content[1].(map[string]any)
	if !ok {
		t.Fatalf("image part = %#v, want object", content[1])
	}
	if imagePart["type"] != "input_image" {
		t.Fatalf("type = %v, want input_image", imagePart["type"])
	}

	imageURL, _ := imagePart["image_url"].(string)
	if !strings.HasPrefix(imageURL, "data:image/png;base64,") {
		t.Fatalf("image_url = %q, want data:image/png;base64,...", imageURL)
	}

	originalItems, ok := original["input"].([]any)
	if !ok {
		t.Fatalf("original input = %#v, want []any", original["input"])
	}
	originalMessage, ok := originalItems[0].(map[string]any)
	if !ok {
		t.Fatalf("original item = %#v, want object", originalItems[0])
	}
	originalContent, ok := originalMessage["content"].([]any)
	if !ok {
		t.Fatalf("original content = %#v, want []any", originalMessage["content"])
	}
	originalImagePart, ok := originalContent[1].(map[string]any)
	if !ok {
		t.Fatalf("original image part = %#v, want object", originalContent[1])
	}
	if originalImagePart["type"] != "image_ref" {
		t.Fatalf("original type = %v, want image_ref", originalImagePart["type"])
	}
}

func TestFilterChildThreadToolsRemovesSpawnTool(t *testing.T) {
	t.Parallel()

	filtered, err := filterChildThreadTools(`[{"type":"function","name":"spawn_threads"},{"type":"function","name":"lookup"}]`)
	if err != nil {
		t.Fatalf("filterChildThreadTools() error = %v", err)
	}

	var tools []map[string]any
	if err := json.Unmarshal([]byte(filtered), &tools); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	if len(tools) != 1 || tools[0]["name"] != "lookup" {
		t.Fatalf("tools = %v, want only lookup", tools)
	}
}

func TestResolveBranchPreviousResponseID(t *testing.T) {
	t.Parallel()

	parent := threadstore.ThreadMeta{
		ID:             tid("thread_parent"),
		LastResponseID: "resp_parent_latest",
	}

	got, err := resolveBranchPreviousResponseID(spawnRequest{
		SpawnMode: spawnModeWarmBranch,
	}, parent)
	if err != nil {
		t.Fatalf("resolveBranchPreviousResponseID() error = %v", err)
	}
	if got != "resp_parent_latest" {
		t.Fatalf("resolveBranchPreviousResponseID() = %q, want %q", got, "resp_parent_latest")
	}

	got, err = resolveBranchPreviousResponseID(spawnRequest{
		SpawnMode:              spawnModeWarmBranch,
		BranchPreviousResponse: "resp_explicit",
	}, parent)
	if err != nil {
		t.Fatalf("resolveBranchPreviousResponseID() explicit error = %v", err)
	}
	if got != "resp_explicit" {
		t.Fatalf("resolveBranchPreviousResponseID() explicit = %q, want %q", got, "resp_explicit")
	}
}

func TestResolveBranchPreviousResponseIDMissingParentResponse(t *testing.T) {
	t.Parallel()

	_, err := resolveBranchPreviousResponseID(spawnRequest{
		SpawnMode: spawnModeWarmBranch,
	}, threadstore.ThreadMeta{})
	if err == nil {
		t.Fatal("expected error for missing branch parent response id")
	}
}

func TestMergeMetadataJSONAddsWarmBranchFields(t *testing.T) {
	t.Parallel()

	merged, err := mergeMetadataJSON(`{"tenant":"acme"}`, map[string]string{
		"spawn_mode":                spawnModeWarmBranch,
		"branch_parent_thread_id":   formatThreadIDLocal(tid("thread_parent")),
		"branch_parent_response_id": "resp_parent",
		"branch_index":              "2",
	}, true)
	if err != nil {
		t.Fatalf("mergeMetadataJSON() error = %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal([]byte(merged), &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	if decoded["tenant"] != "acme" {
		t.Fatalf("tenant = %v, want acme", decoded["tenant"])
	}
	if decoded["spawn_mode"] != spawnModeWarmBranch {
		t.Fatalf("spawn_mode = %v, want %s", decoded["spawn_mode"], spawnModeWarmBranch)
	}
	if decoded["branch_parent_thread_id"] != formatThreadIDLocal(tid("thread_parent")) {
		t.Fatalf("branch_parent_thread_id = %v, want %s", decoded["branch_parent_thread_id"], formatThreadIDLocal(tid("thread_parent")))
	}
	if decoded["branch_parent_response_id"] != "resp_parent" {
		t.Fatalf("branch_parent_response_id = %v, want resp_parent", decoded["branch_parent_response_id"])
	}
	if decoded["branch_index"] != "2" {
		t.Fatalf("branch_index = %v, want 2", decoded["branch_index"])
	}
}

func TestFilterChildThreadToolChoicePreservesMode(t *testing.T) {
	t.Parallel()

	got, err := filterChildThreadToolChoice(`"auto"`)
	if err != nil {
		t.Fatalf("filterChildThreadToolChoice() error = %v", err)
	}
	if got != `"auto"` {
		t.Fatalf("filterChildThreadToolChoice() = %s, want %s", got, `"auto"`)
	}
}

func TestFilterChildThreadToolChoiceDropsInternalFunctionChoices(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
	}{
		{
			name: "spawn child threads",
			raw:  `{"type":"function","name":"spawn_threads"}`,
		},
		{
			name: "attached documents",
			raw:  `{"type":"function","name":"query_document"}`,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := filterChildThreadToolChoice(tt.raw)
			if err != nil {
				t.Fatalf("filterChildThreadToolChoice() error = %v", err)
			}
			if got != "" {
				t.Fatalf("filterChildThreadToolChoice() = %s, want empty", got)
			}
		})
	}
}

func TestFilterChildThreadToolChoiceFiltersAllowedTools(t *testing.T) {
	t.Parallel()

	got, err := filterChildThreadToolChoice(`{
		"type":"allowed_tools",
		"mode":"required",
		"tools":[
			{"type":"function","name":"spawn_threads"},
			{"type":"function","name":"query_document"},
			{"type":"function","name":"keep_tool"},
			{"type":"web_search_preview"}
		]
	}`)
	if err != nil {
		t.Fatalf("filterChildThreadToolChoice() error = %v", err)
	}
	if got == "" {
		t.Fatal("filterChildThreadToolChoice() returned empty result")
	}

	var decoded map[string]any
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	if decoded["mode"] != "required" {
		t.Fatalf("mode = %v, want required", decoded["mode"])
	}

	tools, ok := decoded["tools"].([]any)
	if !ok {
		t.Fatalf("tools = %#v, want []any", decoded["tools"])
	}
	if len(tools) != 2 {
		t.Fatalf("len(tools) = %d, want 2", len(tools))
	}

	first, ok := tools[0].(map[string]any)
	if !ok {
		t.Fatalf("tools[0] = %#v, want map[string]any", tools[0])
	}
	if first["name"] != "keep_tool" {
		t.Fatalf("tools[0].name = %v, want keep_tool", first["name"])
	}

	second, ok := tools[1].(map[string]any)
	if !ok {
		t.Fatalf("tools[1] = %#v, want map[string]any", tools[1])
	}
	if second["type"] != "web_search_preview" {
		t.Fatalf("tools[1].type = %v, want web_search_preview", second["type"])
	}
}

func TestFilterChildThreadToolChoiceDropsEmptyAllowedTools(t *testing.T) {
	t.Parallel()

	got, err := filterChildThreadToolChoice(`{
		"type":"allowed_tools",
		"mode":"required",
		"tools":[
			{"type":"function","name":"spawn_threads"},
			{"type":"function","name":"query_document"}
		]
	}`)
	if err != nil {
		t.Fatalf("filterChildThreadToolChoice() error = %v", err)
	}
	if got != "" {
		t.Fatalf("filterChildThreadToolChoice() = %s, want empty", got)
	}
}

func TestEnsureSessionReusesConnectedSessionWithoutPing(t *testing.T) {
	t.Parallel()

	cfg := testOpenAIConfig()
	conn := &actorTestConn{}
	session := openaiws.NewSession(cfg, &actorTestDialer{conn: conn})
	if err := session.Connect(context.Background()); err != nil {
		t.Fatalf("Connect() error = %v", err)
	}

	actor := &threadActor{
		threadID:       tid("thread_test"),
		logger:         testActorLogger(),
		cfg:            cfg,
		ctx:            context.Background(),
		session:        session,
		sessionFactory: func() *openaiws.Session { t.Fatal("sessionFactory should not be called"); return nil },
	}

	if err := actor.ensureSession(); err != nil {
		t.Fatalf("ensureSession() error = %v", err)
	}

	if conn.pings != 0 {
		t.Fatalf("pings = %d, want 0", conn.pings)
	}
}

func TestSendResponseCreateReconnectsAfterStaleSocket(t *testing.T) {
	t.Parallel()

	cfg := testOpenAIConfig()
	staleConn := &actorTestConn{writeErr: errors.New("broken pipe")}
	freshConn := &actorTestConn{}
	staleSession := openaiws.NewSession(cfg, &actorTestDialer{conn: staleConn})
	if err := staleSession.Connect(context.Background()); err != nil {
		t.Fatalf("staleSession.Connect() error = %v", err)
	}

	factoryCalls := 0
	actor := &threadActor{
		threadID: tid("thread_test"),
		logger:   testActorLogger(),
		cfg:      cfg,
		ctx:      context.Background(),
		session:  staleSession,
		sessionFactory: func() *openaiws.Session {
			factoryCalls++
			return openaiws.NewSession(cfg, &actorTestDialer{conn: freshConn})
		},
	}

	event, err := openaiws.NewResponseCreateEvent("evt_retry", json.RawMessage(`{"model":"gpt-5.4"}`))
	if err != nil {
		t.Fatalf("NewResponseCreateEvent() error = %v", err)
	}

	if err := actor.sendResponseCreate(event); err != nil {
		t.Fatalf("sendResponseCreate() error = %v", err)
	}

	if factoryCalls != 1 {
		t.Fatalf("sessionFactory calls = %d, want 1", factoryCalls)
	}
	if len(staleConn.writes) != 0 {
		t.Fatalf("stale writes = %d, want 0", len(staleConn.writes))
	}
	if !staleConn.closed {
		t.Fatal("expected stale connection to be closed")
	}
	if len(freshConn.writes) != 1 {
		t.Fatalf("fresh writes = %d, want 1", len(freshConn.writes))
	}
}

func TestSendResponseCreateReconnectsSocketRegistry(t *testing.T) {
	t.Parallel()

	store := newFakeActorStore(t)
	store.threads[tid("thread_test")] = threadstore.ThreadMeta{
		ID:               tid("thread_test"),
		RootThreadID:     tid("thread_test"),
		Status:           threadstore.ThreadStatusReady,
		OwnerWorkerID:    tid("worker-local-1"),
		SocketGeneration: 4,
	}

	cfg := testOpenAIConfig()
	staleConn := &actorTestConn{writeErr: errors.New("broken pipe")}
	freshConn := &actorTestConn{}
	staleSession := openaiws.NewSession(cfg, &actorTestDialer{conn: staleConn})
	if err := staleSession.Connect(context.Background()); err != nil {
		t.Fatalf("staleSession.Connect() error = %v", err)
	}

	actor := &threadActor{
		threadID:       tid("thread_test"),
		workerID:       tid("worker-local-1"),
		logger:         testActorLogger(),
		cfg:            cfg,
		ctx:            context.Background(),
		store:          store,
		session:        staleSession,
		openAISocketID: "socket_old",
		meta:           store.threads[tid("thread_test")],
		sessionFactory: func() *openaiws.Session {
			return openaiws.NewSession(cfg, &actorTestDialer{conn: freshConn})
		},
	}

	event, err := openaiws.NewResponseCreateEvent("evt_retry", json.RawMessage(`{"model":"gpt-5.4"}`))
	if err != nil {
		t.Fatalf("NewResponseCreateEvent() error = %v", err)
	}

	if err := actor.sendResponseCreate(event); err != nil {
		t.Fatalf("sendResponseCreate() error = %v", err)
	}

	if len(store.createdSockets) != 1 {
		t.Fatalf("createdSockets = %d, want 1", len(store.createdSockets))
	}
	if store.createdSockets[0].ThreadID != tid("thread_test") {
		t.Fatalf("created socket thread_id = %d, want %d", store.createdSockets[0].ThreadID, tid("thread_test"))
	}
	if len(store.disconnectedSockets) != 1 || store.disconnectedSockets[0] != "socket_old" {
		t.Fatalf("disconnectedSockets = %#v, want [socket_old]", store.disconnectedSockets)
	}
}

func TestHandleStartIncludesAvailableDocumentsInInstructions(t *testing.T) {
	t.Parallel()

	store := newFakeActorStore(t)
	conn := &actorTestConn{
		reads: [][]byte{
			[]byte(`{"type":"response.created","response":{"id":"resp_start"}}`),
			[]byte(`{"type":"response.completed","response":{"id":"resp_start"}}`),
		},
	}
	actor := newActorRecoveryHarness(t, store, conn)
	actor.threadDocs = &fakeThreadDocumentStore{
		documentsByThread: map[int64][]docstore.Document{
			tid("thread_parent"): {
				{ID: 1, Filename: "report.pdf"},
			},
		},
	}
	actor.docRuntime = fakeDocRuntimeContextClient(func(_ context.Context, req doccmd.RuntimeContextRequest) (doccmd.RuntimeContextResponse, error) {
		return doccmd.RuntimeContextResponse{
			RequestID:    req.RequestID,
			Status:       doccmd.PrepareStatusOK,
			Instructions: req.Instructions + "\n\n<available_documents>\n<document id=\"1\" name=\"report.pdf\" />\n</available_documents>",
		}, nil
	})

	body, err := json.Marshal(threadcmd.StartBody{
		Model:        "gpt-5.4",
		Instructions: "Base instructions.",
		InitialInput: json.RawMessage(`[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]`),
	})
	if err != nil {
		t.Fatalf("json.Marshal(StartBody) error = %v", err)
	}

	cmd := threadcmd.Command{
		CmdID:        101,
		Kind:         threadcmd.KindThreadStart,
		ThreadID:     tid("thread_parent"),
		RootThreadID: tid("thread_parent"),
		Body:         body,
	}

	if err := actor.handleStart(cmd); err != nil {
		t.Fatalf("handleStart() error = %v", err)
	}

	if len(conn.writes) != 1 {
		t.Fatalf("writes = %d, want 1", len(conn.writes))
	}

	var payload map[string]any
	if err := json.Unmarshal(conn.writes[0], &payload); err != nil {
		t.Fatalf("json.Unmarshal(sent) error = %v", err)
	}

	instructions, _ := payload["instructions"].(string)
	if !strings.Contains(instructions, `<document id="1" name="report.pdf" />`) {
		t.Fatalf("instructions = %q, want available_documents block", instructions)
	}
}

func TestHandleStartCanonicalizesStoredResponseFields(t *testing.T) {
	t.Parallel()

	store := newFakeActorStore(t)
	conn := &actorTestConn{
		reads: [][]byte{
			[]byte(`{"type":"response.created","response":{"id":"resp_start"}}`),
			[]byte(`{"type":"response.completed","response":{"id":"resp_start"}}`),
		},
	}
	actor := newActorRecoveryHarness(t, store, conn)

	body, err := json.Marshal(threadcmd.StartBody{
		Model:        "gpt-5.4",
		InitialInput: json.RawMessage(`[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]`),
		Metadata:     json.RawMessage(`{"branch_index":2}`),
		Tools:        json.RawMessage(`[{"type":"function","name":"lookup","parameters":{"type":"object"},"strict":true}]`),
		ToolChoice:   json.RawMessage(`"required"`),
		Reasoning:    json.RawMessage(`{"effort":"high","summary":"detailed","generate_summary":"concise"}`),
	})
	if err != nil {
		t.Fatalf("json.Marshal(StartBody) error = %v", err)
	}

	cmd := threadcmd.Command{
		CmdID:        102,
		Kind:         threadcmd.KindThreadStart,
		ThreadID:     tid("thread_parent"),
		RootThreadID: tid("thread_parent"),
		Body:         body,
	}

	if err := actor.handleStart(cmd); err != nil {
		t.Fatalf("handleStart() error = %v", err)
	}

	meta := store.threads[tid("thread_parent")]

	var metadata map[string]string
	if err := json.Unmarshal([]byte(meta.MetadataJSON), &metadata); err != nil {
		t.Fatalf("json.Unmarshal(meta.MetadataJSON) error = %v", err)
	}
	if metadata["branch_index"] != "2" {
		t.Fatalf("branch_index = %q, want 2", metadata["branch_index"])
	}
	if meta.ToolChoiceJSON != `"required"` {
		t.Fatalf("ToolChoiceJSON = %s, want %s", meta.ToolChoiceJSON, `"required"`)
	}
	var tools []map[string]any
	if err := json.Unmarshal([]byte(meta.ToolsJSON), &tools); err != nil {
		t.Fatalf("json.Unmarshal(meta.ToolsJSON) error = %v", err)
	}
	if len(tools) != 1 || tools[0]["name"] != "lookup" {
		t.Fatalf("tools = %#v, want one lookup tool", tools)
	}

	var reasoning map[string]any
	if err := json.Unmarshal([]byte(meta.ReasoningJSON), &reasoning); err != nil {
		t.Fatalf("json.Unmarshal(meta.ReasoningJSON) error = %v", err)
	}
	if reasoning["effort"] != "high" {
		t.Fatalf("effort = %v, want high", reasoning["effort"])
	}
	if _, exists := reasoning["summary"]; exists {
		t.Fatalf("summary should be omitted, got %#v", reasoning["summary"])
	}
	if _, exists := reasoning["generate_summary"]; exists {
		t.Fatalf("generate_summary should be omitted, got %#v", reasoning["generate_summary"])
	}
}

func TestContinueWithInputItemsIncludesAvailableDocumentsInInstructions(t *testing.T) {
	t.Parallel()

	store := newFakeActorStore(t)
	conn := &actorTestConn{
		reads: [][]byte{
			[]byte(`{"type":"response.created","response":{"id":"resp_resume"}}`),
			[]byte(`{"type":"response.completed","response":{"id":"resp_resume"}}`),
		},
	}
	actor := newActorRecoveryHarness(t, store, conn)
	actor.threadDocs = &fakeThreadDocumentStore{
		documentsByThread: map[int64][]docstore.Document{
			tid("thread_parent"): {
				{ID: 2, Filename: "followup.pdf"},
			},
		},
	}
	actor.docRuntime = fakeDocRuntimeContextClient(func(_ context.Context, req doccmd.RuntimeContextRequest) (doccmd.RuntimeContextResponse, error) {
		return doccmd.RuntimeContextResponse{
			RequestID:    req.RequestID,
			Status:       doccmd.PrepareStatusOK,
			Instructions: req.Instructions + "\n\n<available_documents>\n<document id=\"2\" name=\"followup.pdf\" />\n</available_documents>",
		}, nil
	})

	meta := threadstore.ThreadMeta{
		ID:               tid("thread_parent"),
		Model:            "gpt-5.4",
		Instructions:     "Base instructions.",
		LastResponseID:   "resp_prev",
		SocketGeneration: 1,
	}

	if err := actor.continueWithInputItems(meta, 103, json.RawMessage(`[{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}]`), "user_input"); err != nil {
		t.Fatalf("continueWithInputItems() error = %v", err)
	}

	if len(conn.writes) != 1 {
		t.Fatalf("writes = %d, want 1", len(conn.writes))
	}

	var payload map[string]any
	if err := json.Unmarshal(conn.writes[0], &payload); err != nil {
		t.Fatalf("json.Unmarshal(sent) error = %v", err)
	}

	instructions, _ := payload["instructions"].(string)
	if !strings.Contains(instructions, `<document id="2" name="followup.pdf" />`) {
		t.Fatalf("instructions = %q, want available_documents block", instructions)
	}
}

func TestHandleStartUsesPreparedInputRefForSend(t *testing.T) {
	t.Parallel()

	store := newFakeActorStore(t)
	conn := &actorTestConn{
		reads: [][]byte{
			[]byte(`{"type":"response.created","response":{"id":"resp_start"}}`),
			[]byte(`{"type":"response.completed","response":{"id":"resp_start"}}`),
		},
	}
	actor := newActorRecoveryHarness(t, store, conn)

	blob, err := blobstore.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal() error = %v", err)
	}
	actor.blob = blob

	preparedStore, err := preparedinput.NewStore(blob)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	ref, err := preparedStore.Write(context.Background(), "pi_start", preparedinput.Artifact{
		Version: preparedinput.VersionV1,
		Input:   []byte(`[{"type":"message","role":"user","content":[{"type":"input_text","text":"prepared hello"}]}]`),
	})
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	body, err := json.Marshal(threadcmd.StartBody{
		Model:            "gpt-5.4",
		PreparedInputRef: ref,
	})
	if err != nil {
		t.Fatalf("json.Marshal(StartBody) error = %v", err)
	}

	cmd := threadcmd.Command{
		CmdID:        104,
		Kind:         threadcmd.KindThreadStart,
		ThreadID:     tid("thread_parent"),
		RootThreadID: tid("thread_parent"),
		Body:         body,
	}

	if err := actor.handleStart(cmd); err != nil {
		t.Fatalf("handleStart() error = %v", err)
	}

	if len(store.appendedItems) != 0 {
		t.Fatalf("appendedItems = %d, want 0", len(store.appendedItems))
	}
	if len(conn.writes) != 1 {
		t.Fatalf("writes = %d, want 1", len(conn.writes))
	}

	var sent map[string]any
	if err := json.Unmarshal(conn.writes[0], &sent); err != nil {
		t.Fatalf("json.Unmarshal(sent) error = %v", err)
	}

	if _, exists := sent["prepared_input_ref"]; exists {
		t.Fatalf("prepared_input_ref should not be sent to OpenAI, got %#v", sent["prepared_input_ref"])
	}

	items, ok := sent["input"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("input = %#v, want one prepared item", sent["input"])
	}

	message, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("message = %#v, want object", items[0])
	}
	content, ok := message["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("content = %#v, want one text part", message["content"])
	}
	part, ok := content[0].(map[string]any)
	if !ok || part["text"] != "prepared hello" {
		t.Fatalf("content[0] = %#v, want prepared hello", content[0])
	}

	foundCheckpoint := false
	for _, entry := range store.historyEvents {
		if entry.EventType != "client.response.create" {
			continue
		}
		foundCheckpoint = true
		if !strings.Contains(entry.PayloadJSON, ref) {
			t.Fatalf("checkpoint payload = %s, want prepared_input_ref %q", entry.PayloadJSON, ref)
		}
		if strings.Contains(entry.PayloadJSON, "prepared hello") {
			t.Fatalf("checkpoint payload should stay compact, got %s", entry.PayloadJSON)
		}
	}
	if !foundCheckpoint {
		t.Fatal("expected client.response.create checkpoint to be appended")
	}
	if raw := string(store.latestClientCreateByID[tid("thread_parent")]); !strings.Contains(raw, ref) {
		t.Fatalf("saved checkpoint = %s, want prepared_input_ref %q", raw, ref)
	}
}

func TestHandleResumeUsesPreparedInputRefForSend(t *testing.T) {
	t.Parallel()

	store := newFakeActorStore(t)
	store.threads[tid("thread_parent")] = threadstore.ThreadMeta{
		ID:             tid("thread_parent"),
		Status:         threadstore.ThreadStatusReady,
		Model:          "gpt-5.4",
		LastResponseID: "resp_prev",
	}
	conn := &actorTestConn{
		reads: [][]byte{
			[]byte(`{"type":"response.created","response":{"id":"resp_resume"}}`),
			[]byte(`{"type":"response.completed","response":{"id":"resp_resume"}}`),
		},
	}
	actor := newActorRecoveryHarness(t, store, conn)

	blob, err := blobstore.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal() error = %v", err)
	}
	actor.blob = blob

	preparedStore, err := preparedinput.NewStore(blob)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	ref, err := preparedStore.Write(context.Background(), "pi_resume", preparedinput.Artifact{
		Version: preparedinput.VersionV1,
		Input:   []byte(`[{"type":"message","role":"user","content":[{"type":"input_text","text":"prepared continue"}]}]`),
	})
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	body, err := json.Marshal(threadcmd.ResumeBody{
		PreparedInputRef: ref,
	})
	if err != nil {
		t.Fatalf("json.Marshal(ResumeBody) error = %v", err)
	}

	cmd := threadcmd.Command{
		CmdID:        105,
		Kind:         threadcmd.KindThreadResume,
		ThreadID:     tid("thread_parent"),
		RootThreadID: tid("thread_parent"),
		Body:         body,
	}

	if err := actor.handleResume(cmd); err != nil {
		t.Fatalf("handleResume() error = %v", err)
	}

	if len(store.appendedItems) != 0 {
		t.Fatalf("appendedItems = %d, want 0", len(store.appendedItems))
	}
	if len(conn.writes) != 1 {
		t.Fatalf("writes = %d, want 1", len(conn.writes))
	}

	var sent map[string]any
	if err := json.Unmarshal(conn.writes[0], &sent); err != nil {
		t.Fatalf("json.Unmarshal(sent) error = %v", err)
	}

	if sent["previous_response_id"] != "resp_prev" {
		t.Fatalf("previous_response_id = %v, want resp_prev", sent["previous_response_id"])
	}
	if _, exists := sent["prepared_input_ref"]; exists {
		t.Fatalf("prepared_input_ref should not be sent to OpenAI, got %#v", sent["prepared_input_ref"])
	}

	items, ok := sent["input"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("input = %#v, want one prepared item", sent["input"])
	}

	message, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("message = %#v, want object", items[0])
	}
	content, ok := message["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("content = %#v, want one text part", message["content"])
	}
	part, ok := content[0].(map[string]any)
	if !ok || part["text"] != "prepared continue" {
		t.Fatalf("content[0] = %#v, want prepared continue", content[0])
	}
	if raw := string(store.latestClientCreateByID[tid("thread_parent")]); !strings.Contains(raw, ref) {
		t.Fatalf("saved checkpoint = %s, want prepared_input_ref %q", raw, ref)
	}
}

func TestTransientCommandRetryDelay(t *testing.T) {
	t.Parallel()

	tests := []struct {
		deliveries uint64
		want       time.Duration
	}{
		{deliveries: 0, want: 2 * time.Second},
		{deliveries: 1, want: 2 * time.Second},
		{deliveries: 2, want: 4 * time.Second},
		{deliveries: 3, want: 8 * time.Second},
		{deliveries: 4, want: 16 * time.Second},
		{deliveries: 5, want: 30 * time.Second},
		{deliveries: 9, want: 30 * time.Second},
	}

	for _, tt := range tests {
		if got := transientCommandRetryDelay(tt.deliveries); got != tt.want {
			t.Fatalf("transientCommandRetryDelay(%d) = %s, want %s", tt.deliveries, got, tt.want)
		}
	}
}

func TestIsTransientCommandError(t *testing.T) {
	t.Parallel()

	if !isTransientCommandError(&net.DNSError{Err: "no such host", Name: "api.openai.com"}) {
		t.Fatal("expected dns error to be transient")
	}

	if !isTransientCommandError(context.DeadlineExceeded) {
		t.Fatal("expected deadline exceeded to be transient")
	}

	if !isTransientCommandError(errors.New("dial responses websocket: dial tcp: lookup api.openai.com: no such host")) {
		t.Fatal("expected dial tcp dns failure to be transient")
	}

	if isTransientCommandError(context.Canceled) {
		t.Fatal("did not expect context cancellation to be transient")
	}

	if isTransientCommandError(errors.New("openai returned a permanent error")) {
		t.Fatal("did not expect arbitrary permanent error to be transient")
	}
}

func TestShouldDropMissingThreadCommand(t *testing.T) {
	t.Parallel()

	if !shouldDropMissingThreadCommand(threadstore.ErrThreadNotFound) {
		t.Fatal("expected bare thread-not-found to be droppable")
	}

	wrapped := errors.New("wrapped: " + threadstore.ErrThreadNotFound.Error())
	if shouldDropMissingThreadCommand(wrapped) {
		t.Fatal("did not expect plain string match to be droppable")
	}

	if !shouldDropMissingThreadCommand(fmt.Errorf("load thread: %w", threadstore.ErrThreadNotFound)) {
		t.Fatal("expected wrapped thread-not-found to be droppable")
	}
}

func TestFailThreadAfterRetryExhaustionMarksThreadFailed(t *testing.T) {
	t.Parallel()

	store := newFakeActorStore(t)
	store.threads[tid("thread_retry")] = threadstore.ThreadMeta{
		ID:               tid("thread_retry"),
		Status:           threadstore.ThreadStatusRunning,
		OwnerWorkerID:    tid("worker-local-1"),
		SocketGeneration: 3,
		SocketExpiresAt:  time.Now().UTC().Add(time.Minute),
		ActiveResponseID: "resp_active",
	}

	conn := &actorTestConn{}
	actor := newActorRecoveryHarness(t, store, conn)
	actor.threadID = tid("thread_retry")
	actor.setMeta(store.threads[tid("thread_retry")])

	if err := actor.failThreadAfterRetryExhaustion(tid("thread_retry")); err != nil {
		t.Fatalf("failThreadAfterRetryExhaustion() error = %v", err)
	}

	meta := store.threads[tid("thread_retry")]
	if meta.Status != threadstore.ThreadStatusFailed {
		t.Fatalf("status = %s, want failed", meta.Status)
	}
	if meta.ActiveResponseID != "" {
		t.Fatalf("active response id = %q, want empty", meta.ActiveResponseID)
	}
	if meta.OwnerWorkerID != 0 {
		t.Fatalf("owner worker id = %d, want empty", meta.OwnerWorkerID)
	}
	if !meta.SocketExpiresAt.IsZero() {
		t.Fatalf("socket expires at = %s, want zero", meta.SocketExpiresAt)
	}
	if len(store.releasedThreads) != 1 || store.releasedThreads[0] != tid("thread_retry") {
		t.Fatalf("releasedThreads = %#v, want [%d]", store.releasedThreads, tid("thread_retry"))
	}
	if !conn.closed {
		t.Fatal("expected session to be closed")
	}
}

func TestHandleDisconnectSocketClosesSessionAndReleasesOwnership(t *testing.T) {
	t.Parallel()

	store := newFakeActorStore(t)
	store.threads[tid("thread_idle")] = threadstore.ThreadMeta{
		ID:               tid("thread_idle"),
		Status:           threadstore.ThreadStatusReady,
		OwnerWorkerID:    tid("worker-local-1"),
		SocketGeneration: 5,
		SocketExpiresAt:  time.Now().UTC().Add(time.Minute),
	}

	conn := &actorTestConn{}
	actor := newActorRecoveryHarness(t, store, conn)
	actor.threadID = tid("thread_idle")
	actor.setMeta(store.threads[tid("thread_idle")])
	actor.openAISocketID = "socket_live_1"

	cmd := threadcmd.Command{
		CmdID:    201,
		Kind:     threadcmd.KindThreadDisconnectSocket,
		ThreadID: tid("thread_idle"),
	}

	if err := actor.handleDisconnectSocket(cmd); err != nil {
		t.Fatalf("handleDisconnectSocket() error = %v", err)
	}

	if !conn.closed {
		t.Fatal("expected session to be closed")
	}
	if len(store.releasedThreads) != 1 || store.releasedThreads[0] != tid("thread_idle") {
		t.Fatalf("releasedThreads = %#v, want [%d]", store.releasedThreads, tid("thread_idle"))
	}
	if len(store.disconnectedSockets) != 1 || store.disconnectedSockets[0] != "socket_live_1" {
		t.Fatalf("disconnectedSockets = %#v, want [socket_live_1]", store.disconnectedSockets)
	}

	meta := store.threads[tid("thread_idle")]
	if meta.Status != threadstore.ThreadStatusReady {
		t.Fatalf("status = %s, want ready (unchanged)", meta.Status)
	}
}

func TestHandleDisconnectSocketRejectsNonIdleThread(t *testing.T) {
	t.Parallel()

	store := newFakeActorStore(t)
	store.threads[tid("thread_running")] = threadstore.ThreadMeta{
		ID:               tid("thread_running"),
		Status:           threadstore.ThreadStatusRunning,
		OwnerWorkerID:    tid("worker-local-1"),
		SocketGeneration: 3,
	}

	actor := newActorRecoveryHarness(t, store, nil)
	actor.threadID = tid("thread_running")
	actor.setMeta(store.threads[tid("thread_running")])

	cmd := threadcmd.Command{
		CmdID:    202,
		Kind:     threadcmd.KindThreadDisconnectSocket,
		ThreadID: tid("thread_running"),
	}

	err := actor.handleDisconnectSocket(cmd)
	if !errors.Is(err, errCommandPrecond) {
		t.Fatalf("expected errCommandPrecond, got %v", err)
	}
	if len(store.releasedThreads) != 0 {
		t.Fatalf("releasedThreads = %#v, want empty", store.releasedThreads)
	}
}

func TestHandleDisconnectSocketNoopsWithoutSession(t *testing.T) {
	t.Parallel()

	store := newFakeActorStore(t)
	store.threads[tid("thread_nosess")] = threadstore.ThreadMeta{
		ID:               tid("thread_nosess"),
		Status:           threadstore.ThreadStatusReady,
		OwnerWorkerID:    tid("worker-local-1"),
		SocketGeneration: 2,
	}

	actor := newActorRecoveryHarness(t, store, nil)
	actor.threadID = tid("thread_nosess")
	actor.setMeta(store.threads[tid("thread_nosess")])

	cmd := threadcmd.Command{
		CmdID:    203,
		Kind:     threadcmd.KindThreadDisconnectSocket,
		ThreadID: tid("thread_nosess"),
	}

	if err := actor.handleDisconnectSocket(cmd); err != nil {
		t.Fatalf("handleDisconnectSocket() error = %v", err)
	}

	if len(store.releasedThreads) != 1 || store.releasedThreads[0] != tid("thread_nosess") {
		t.Fatalf("releasedThreads = %#v, want [%d]", store.releasedThreads, tid("thread_nosess"))
	}
}

func TestShouldReleaseTerminalChildResources(t *testing.T) {
	t.Parallel()

	if !shouldReleaseTerminalChildResources(threadstore.ThreadMeta{
		ParentThreadID: tid("thread_parent"),
		Status:         threadstore.ThreadStatusCompleted,
	}) {
		t.Fatal("expected completed child thread to release resources")
	}

	if shouldReleaseTerminalChildResources(threadstore.ThreadMeta{
		Status: threadstore.ThreadStatusReady,
	}) {
		t.Fatal("expected parent ready thread to keep reusable resources")
	}

	if shouldReleaseTerminalChildResources(threadstore.ThreadMeta{
		ParentThreadID: tid("thread_parent"),
		Status:         threadstore.ThreadStatusWaitingChildren,
	}) {
		t.Fatal("expected non-terminal child thread to keep active resources")
	}
}

func TestExtractAssistantTextFromItemPayload(t *testing.T) {
	t.Parallel()

	text, err := extractAssistantTextFromItemPayload(json.RawMessage(`{
		"type":"message",
		"content":[
			{"type":"output_text","text":"First paragraph."},
			{"type":"output_text","text":"Second paragraph."}
		]
	}`))
	if err != nil {
		t.Fatalf("extractAssistantTextFromItemPayload() error = %v", err)
	}

	if text != "First paragraph.\n\nSecond paragraph." {
		t.Fatalf("text = %q, want joined output text", text)
	}
}

func TestPublishChildTerminalIncludesAssistantText(t *testing.T) {
	t.Parallel()

	store := newFakeActorStore(t)
	store.threads[tid("thread_parent")] = threadstore.ThreadMeta{
		ID:            tid("thread_parent"),
		RootThreadID:  tid("thread_root"),
		OwnerWorkerID: tid("worker-parent"),
	}
	store.appendedItems = append(store.appendedItems, threadstore.ItemLogEntry{
		ThreadID:   tid("thread_child"),
		ResponseID: "resp_child_1",
		ItemType:   "message",
		Direction:  "output",
		PayloadJSON: `{
			"type":"message",
			"content":[
				{"type":"output_text","text":"Child summary line one."},
				{"type":"output_text","text":"Child summary line two."}
			]
		}`,
		CreatedAt: time.Now().UTC(),
	})

	var (
		publishedSubject string
		publishedCmd     threadcmd.Command
	)

	actor := newActorRecoveryHarness(t, store, nil)
	actor.publish = func(_ context.Context, subject string, cmd threadcmd.Command) error {
		publishedSubject = subject
		publishedCmd = cmd
		return nil
	}

	err := actor.publishChildInvocationResult(threadstore.ThreadMeta{
		ID:                 tid("thread_child"),
		ParentThreadID:     tid("thread_parent"),
		ActiveSpawnGroupID: tid("sg_123"),
		LastResponseID:     "resp_child_1",
		Status:             threadstore.ThreadStatusCompleted,
	}, "completed")
	if err != nil {
		t.Fatalf("publishChildInvocationResult() error = %v", err)
	}

	if publishedSubject != threadcmd.WorkerCommandSubject(tid("worker-parent"), threadcmd.KindThreadChildCompleted) {
		t.Fatalf("subject = %q, want worker child_completed subject", publishedSubject)
	}

	body, err := publishedCmd.ChildResultBody()
	if err != nil {
		t.Fatalf("ChildResultBody() error = %v", err)
	}

	if body.AssistantText != "Child summary line one.\n\nChild summary line two." {
		t.Fatalf("AssistantText = %q, want joined summary", body.AssistantText)
	}
}

func TestPublishChildInvocationResultNormalizesIncompleteToFailed(t *testing.T) {
	t.Parallel()

	store := newFakeActorStore(t)
	store.threads[tid("thread_parent")] = threadstore.ThreadMeta{
		ID:            tid("thread_parent"),
		RootThreadID:  tid("thread_parent"),
		OwnerWorkerID: tid("worker-parent"),
	}

	var (
		publishedSubject string
		publishedCmd     threadcmd.Command
	)

	actor := newActorRecoveryHarness(t, store, nil)
	actor.publish = func(_ context.Context, subject string, cmd threadcmd.Command) error {
		publishedSubject = subject
		publishedCmd = cmd
		return nil
	}

	resultStatus, ok := childInvocationResultStatus(threadstore.ThreadStatusIncomplete, "")
	if !ok {
		t.Fatal("expected childInvocationResultStatus to classify incomplete result")
	}

	err := actor.publishChildInvocationResult(threadstore.ThreadMeta{
		ID:                 tid("thread_child"),
		ParentThreadID:     tid("thread_parent"),
		ActiveSpawnGroupID: tid("sg_123"),
		LastResponseID:     "resp_child_1",
		Status:             threadstore.ThreadStatusIncomplete,
	}, resultStatus)
	if err != nil {
		t.Fatalf("publishChildInvocationResult() error = %v", err)
	}

	if publishedSubject != threadcmd.WorkerCommandSubject(tid("worker-parent"), threadcmd.KindThreadChildFailed) {
		t.Fatalf("subject = %q, want worker child_failed subject", publishedSubject)
	}

	body, err := publishedCmd.ChildResultBody()
	if err != nil {
		t.Fatalf("ChildResultBody() error = %v", err)
	}
	if body.Status != "failed" {
		t.Fatalf("body.Status = %q, want failed", body.Status)
	}
}

func TestHandleChildResultUsesAssistantTextFromCommand(t *testing.T) {
	t.Parallel()

	store := newFakeActorStore(t)
	store.threads[tid("thread_parent")] = threadstore.ThreadMeta{
		ID:                 tid("thread_parent"),
		Status:             threadstore.ThreadStatusWaitingChildren,
		ActiveSpawnGroupID: tid("sg_waiting"),
	}
	store.spawnGroups[tid("sg_waiting")] = threadstore.SpawnGroupMeta{
		ID:             tid("sg_waiting"),
		ParentThreadID: tid("thread_parent"),
		ParentCallID:   "call_parent",
		Expected:       2,
		Status:         threadstore.SpawnGroupStatusWaiting,
	}

	actor := newActorRecoveryHarness(t, store, nil)

	cmd := threadcmd.Command{
		CmdID:    301,
		Kind:     threadcmd.KindThreadChildCompleted,
		ThreadID: tid("thread_parent"),
		Body: json.RawMessage(`{
			"spawn_group_id":` + strconv.FormatInt(tid("sg_waiting"), 10) + `,
			"child_thread_id":1,
			"child_response_id":"resp_child_1",
			"assistant_text":"Direct child summary",
			"status":"completed"
		}`),
	}

	if err := actor.handleChildResult(cmd, "completed"); err != nil {
		t.Fatalf("handleChildResult() error = %v", err)
	}

	results := store.spawnResults[tid("sg_waiting")]
	if len(results) != 1 {
		t.Fatalf("spawnResults = %d, want 1", len(results))
	}
	if results[0].AssistantText != "Direct child summary" {
		t.Fatalf("AssistantText = %q, want %q", results[0].AssistantText, "Direct child summary")
	}
}

func TestHandleChildResultClearsActiveSpawnGroupAfterParentTurnCompletes(t *testing.T) {
	t.Parallel()

	store := newFakeActorStore(t)
	store.threads[tid("thread_parent")] = threadstore.ThreadMeta{
		ID:                 tid("thread_parent"),
		RootThreadID:       tid("thread_parent"),
		Status:             threadstore.ThreadStatusWaitingChildren,
		Model:              "gpt-5.4",
		LastResponseID:     "resp_parent_prev",
		ActiveSpawnGroupID: tid("sg_waiting"),
	}
	store.spawnGroups[tid("sg_waiting")] = threadstore.SpawnGroupMeta{
		ID:             tid("sg_waiting"),
		ParentThreadID: tid("thread_parent"),
		ParentCallID:   "call_parent",
		Expected:       1,
		Status:         threadstore.SpawnGroupStatusWaiting,
	}

	conn := &actorTestConn{
		reads: [][]byte{
			[]byte(`{"type":"response.created","response":{"id":"resp_parent_resume"}}`),
			[]byte(`{"type":"response.completed","response":{"id":"resp_parent_resume"}}`),
		},
	}
	actor := newActorRecoveryHarness(t, store, conn)

	cmd := threadcmd.Command{
		CmdID:    302,
		Kind:     threadcmd.KindThreadChildCompleted,
		ThreadID: tid("thread_parent"),
		Body: json.RawMessage(`{
			"spawn_group_id":` + strconv.FormatInt(tid("sg_waiting"), 10) + `,
			"child_thread_id":1,
			"child_response_id":"resp_child_1",
			"assistant_text":"Direct child summary",
			"status":"completed"
		}`),
	}

	if err := actor.handleChildResult(cmd, "completed"); err != nil {
		t.Fatalf("handleChildResult() error = %v", err)
	}

	final := store.threads[tid("thread_parent")]
	if final.Status != threadstore.ThreadStatusReady {
		t.Fatalf("final.Status = %q, want ready", final.Status)
	}
	if final.ActiveSpawnGroupID != 0 {
		t.Fatalf("final.ActiveSpawnGroupID = %d, want empty", final.ActiveSpawnGroupID)
	}
	if final.LastResponseID != "resp_parent_resume" {
		t.Fatalf("final.LastResponseID = %q, want resp_parent_resume", final.LastResponseID)
	}
}

func testOpenAIConfig() openaiws.Config {
	return openaiws.Config{
		APIKey:               "test-key",
		BaseURL:              "https://api.openai.com/v1",
		ResponsesSocketURL:   "wss://api.openai.com/v1/responses",
		DialTimeout:          50 * time.Millisecond,
		ReadTimeout:          50 * time.Millisecond,
		WriteTimeout:         50 * time.Millisecond,
		PingInterval:         50 * time.Millisecond,
		MaxMessageBytes:      1024,
		MaxConcurrentSockets: 16,
	}
}

func testActorLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type actorTestDialer struct {
	conn openaiws.Conn
}

type fakeThreadDocumentStore struct {
	documentsByThread map[int64][]docstore.Document
	err               error
}

func (s *fakeThreadDocumentStore) ListDocuments(_ context.Context, threadID int64, _ int64) ([]docstore.Document, error) {
	if s.err != nil {
		return nil, s.err
	}

	documents := s.documentsByThread[threadID]
	cloned := make([]docstore.Document, len(documents))
	copy(cloned, documents)
	return cloned, nil
}

func (s *fakeThreadDocumentStore) FilterAttached(_ context.Context, threadID int64, documentIDs []int64) ([]int64, error) {
	if s.err != nil {
		return nil, s.err
	}

	docs := s.documentsByThread[threadID]
	idSet := make(map[int64]bool, len(docs))
	for _, d := range docs {
		idSet[d.ID] = true
	}

	attached := make([]int64, 0, len(documentIDs))
	for _, id := range documentIDs {
		if idSet[id] {
			attached = append(attached, id)
		}
	}
	return attached, nil
}

type fakeDocRuntimeContextClient func(context.Context, doccmd.RuntimeContextRequest) (doccmd.RuntimeContextResponse, error)

func (f fakeDocRuntimeContextClient) RuntimeContext(ctx context.Context, req doccmd.RuntimeContextRequest) (doccmd.RuntimeContextResponse, error) {
	return f(ctx, req)
}

type fakeDocActorDocStore struct {
	docs        map[int64]docstore.Document
	baseUpdates []docBaseLineageUpdate
}

type docBaseLineageUpdate struct {
	DocumentID int64
	ResponseID string
	Model      string
	Reasoning  string
}

func (s *fakeDocActorDocStore) Get(_ context.Context, id int64) (docstore.Document, error) {
	doc, ok := s.docs[id]
	if !ok {
		return docstore.Document{}, docstore.ErrDocumentNotFound
	}
	return doc, nil
}

func (s *fakeDocActorDocStore) UpdateBaseLineage(_ context.Context, id int64, baseResponseID, baseModel, baseReasoning string) error {
	s.baseUpdates = append(s.baseUpdates, docBaseLineageUpdate{
		DocumentID: id,
		ResponseID: baseResponseID,
		Model:      baseModel,
		Reasoning:  baseReasoning,
	})

	if doc, ok := s.docs[id]; ok {
		doc.BaseResponseID = baseResponseID
		doc.BaseModel = baseModel
		doc.BaseReasoning = baseReasoning
		s.docs[id] = doc
	}

	return nil
}

type fakeDocActorPreparedInputClient struct {
	ref      string
	err      error
	requests []doccmd.PrepareInputRequest
}

func (c *fakeDocActorPreparedInputClient) PrepareInput(_ context.Context, req doccmd.PrepareInputRequest) (doccmd.PrepareInputResponse, error) {
	c.requests = append(c.requests, req)
	if c.err != nil {
		return doccmd.PrepareInputResponse{}, c.err
	}
	return doccmd.PrepareInputResponse{
		RequestID:        "test-req",
		Status:           doccmd.PrepareStatusOK,
		PreparedInputRef: c.ref,
	}, nil
}

func (d *actorTestDialer) Dial(_ context.Context, _ openaiws.DialRequest) (openaiws.Conn, error) {
	return d.conn, nil
}

type actorTestConn struct {
	mu          sync.Mutex
	reads       [][]byte
	writes      [][]byte
	writeErr    error
	pings       int
	closed      bool
	closeCode   openaiws.CloseCode
	closeReason string
	closedCh    chan struct{}
}

func (c *actorTestConn) Read(_ context.Context) ([]byte, error) {
	c.mu.Lock()
	if c.closedCh == nil {
		c.closedCh = make(chan struct{})
	}
	if len(c.reads) > 0 {
		payload := append([]byte(nil), c.reads[0]...)
		c.reads = c.reads[1:]
		c.mu.Unlock()
		return payload, nil
	}
	closedCh := c.closedCh
	c.mu.Unlock()

	<-closedCh
	return nil, context.Canceled
}

func (c *actorTestConn) Write(_ context.Context, payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.writeErr != nil {
		return c.writeErr
	}
	c.writes = append(c.writes, append([]byte(nil), payload...))
	return nil
}

func (c *actorTestConn) Ping(_ context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pings++
	return nil
}

func (c *actorTestConn) Close(code openaiws.CloseCode, reason string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.closed = true
	c.closeCode = code
	c.closeReason = reason
	if c.closedCh == nil {
		c.closedCh = make(chan struct{})
	}
	select {
	case <-c.closedCh:
	default:
		close(c.closedCh)
	}
	return nil
}

func TestApplyDocumentRuntimeContextDeletesEmptyFields(t *testing.T) {
	t.Parallel()

	actor := &threadActor{
		ctx: context.Background(),
		threadDocs: &fakeThreadDocumentStore{
			documentsByThread: map[int64][]docstore.Document{
				tid("thread_123"): {
					{ID: 1, Filename: "report.pdf"},
				},
			},
		},
		docRuntime: fakeDocRuntimeContextClient(func(_ context.Context, req doccmd.RuntimeContextRequest) (doccmd.RuntimeContextResponse, error) {
			return doccmd.RuntimeContextResponse{
				RequestID: req.RequestID,
				Status:    doccmd.PrepareStatusOK,
			}, nil
		}),
	}

	payload := map[string]any{
		"instructions": "Be concise.",
		"tools":        []any{map[string]any{"type": "function", "name": "lookup"}},
	}

	if err := actor.applyDocumentRuntimeContext(threadstore.ThreadMeta{ID: tid("thread_123")}, payload); err != nil {
		t.Fatalf("applyDocumentRuntimeContext() error = %v", err)
	}

	if _, exists := payload["instructions"]; exists {
		t.Fatalf("instructions should be deleted, got %#v", payload["instructions"])
	}
	if _, exists := payload["tools"]; exists {
		t.Fatalf("tools should be deleted, got %#v", payload["tools"])
	}
}

func TestDecodeDocQueryRequest(t *testing.T) {
	t.Parallel()

	req, err := decodeDocQueryRequest(`{"document_id":7,"task":"summarize findings"}`)
	if err != nil {
		t.Fatalf("decodeDocQueryRequest() error = %v", err)
	}
	if req.DocumentID != 7 {
		t.Fatalf("document_id = %d, want 7", req.DocumentID)
	}
	if req.Task != "summarize findings" {
		t.Fatalf("task = %q, want %q", req.Task, "summarize findings")
	}
}

func TestDecodeDocQueryRequestRejectsNonPositiveDocumentID(t *testing.T) {
	t.Parallel()

	for _, args := range []string{
		`{"document_id":0,"task":"summarize"}`,
		`{"document_id":-1,"task":"summarize"}`,
		`{"task":"summarize"}`,
	} {
		if _, err := decodeDocQueryRequest(args); err == nil {
			t.Fatalf("expected error for args %q", args)
		}
	}
}

func TestDecodeDocQueryRequestRejectsEmptyTask(t *testing.T) {
	t.Parallel()

	_, err := decodeDocQueryRequest(`{"document_id":1,"task":""}`)
	if err == nil {
		t.Fatal("expected error for empty task")
	}
}

func TestStartDocumentQueryGroupRejectsUnattachedDocs(t *testing.T) {
	t.Parallel()

	store := newFakeActorStore(t)
	actor := &threadActor{
		ctx: context.Background(),
		threadDocs: &fakeThreadDocumentStore{
			documentsByThread: map[int64][]docstore.Document{
				tid("thread_123"): {{ID: 1}},
			},
		},
		store: store,
	}

	meta := threadstore.ThreadMeta{ID: tid("thread_123"), RootThreadID: tid("thread_123")}
	_, err := actor.startDocumentQueryGroup(meta, []docQueryCall{
		{
			CallID:  "call_1",
			Request: docQueryRequest{DocumentID: 1, Task: "summarize"},
		},
		{
			CallID:  "call_2",
			Request: docQueryRequest{DocumentID: 999, Task: "summarize other"},
		},
	})
	if err == nil {
		t.Fatal("expected error for unattached document")
	}
	if !strings.Contains(err.Error(), "999") {
		t.Fatalf("error = %v, want mention of missing document id", err)
	}
	if !errors.Is(err, errCommandPrecond) {
		t.Fatalf("expected errCommandPrecond, got %v", err)
	}
}

func TestFilterChildThreadToolsRemovesDocumentQueryTool(t *testing.T) {
	t.Parallel()

	filtered, err := filterChildThreadTools(`[{"type":"function","name":"query_document"},{"type":"function","name":"lookup"}]`)
	if err != nil {
		t.Fatalf("filterChildThreadTools() error = %v", err)
	}

	var tools []map[string]any
	if err := json.Unmarshal([]byte(filtered), &tools); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	if len(tools) != 1 || tools[0]["name"] != "lookup" {
		t.Fatalf("tools = %v, want only lookup", tools)
	}
}

func TestFilterChildThreadToolsRemovesReadDocumentPageTool(t *testing.T) {
	t.Parallel()

	filtered, err := filterChildThreadTools(`[{"type":"function","name":"read_document_page"},{"type":"function","name":"query_document"},{"type":"function","name":"lookup"}]`)
	if err != nil {
		t.Fatalf("filterChildThreadTools() error = %v", err)
	}

	var tools []map[string]any
	if err := json.Unmarshal([]byte(filtered), &tools); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	if len(tools) != 1 || tools[0]["name"] != "lookup" {
		t.Fatalf("tools = %v, want only lookup (read_document_page + query_document should both be stripped from child)", tools)
	}
}

func TestStreamUntilTerminalDoesNotReturnPendingPageReadsOnResponseFailed(t *testing.T) {
	t.Parallel()

	store := newFakeActorStore(t)
	store.threads[tid("thread_root")] = threadstore.ThreadMeta{
		ID:               tid("thread_root"),
		Status:           threadstore.ThreadStatusRunning,
		OwnerWorkerID:    tid("worker-local-1"),
		SocketGeneration: 1,
	}

	conn := &actorTestConn{
		reads: [][]byte{
			[]byte(`{"type":"response.output_item.done","response":{"id":"resp_1"},"item":{"id":"fc_1","type":"function_call","call_id":"call_pr_1","name":"read_document_page","arguments":"{\"document_id\":15,\"page_number\":4}"}}`),
			[]byte(`{"type":"response.failed","response":{"id":"resp_1","status":"failed"}}`),
		},
	}

	actor := newActorRecoveryHarness(t, store, conn)
	actor.threadID = tid("thread_root")

	pending, err := actor.streamUntilTerminal(store.threads[tid("thread_root")])
	if err != nil {
		t.Fatalf("streamUntilTerminal() error = %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending page reads = %d, want 0 (must not return pending on response.failed — thread is already Failed and cannot accept a follow-up response.create)", len(pending))
	}

	final := store.threads[tid("thread_root")]
	if final.Status != threadstore.ThreadStatusFailed {
		t.Fatalf("final.Status = %q, want failed", final.Status)
	}
}

func TestSubmitResponseCreateEventIDIsUniquePerTurn(t *testing.T) {
	t.Parallel()

	first := submitResponseCreateEventID(42, 0)
	second := submitResponseCreateEventID(42, 1)
	third := submitResponseCreateEventID(42, 2)

	if first == second || first == third || second == third {
		t.Fatalf("event IDs not unique per turn: turn0=%q turn1=%q turn2=%q", first, second, third)
	}
	if first != "42" {
		t.Errorf("turn 0 eventID = %q, want %q (backwards-compat with pre-loop behavior)", first, "42")
	}
	if second != "42-turn-1" {
		t.Errorf("turn 1 eventID = %q, want %q", second, "42-turn-1")
	}
}

func TestStartDocumentQueryGroupUsesLatestCompletedDocumentChildLineage(t *testing.T) {
	t.Parallel()

	store := newFakeActorStore(t)
	store.threads[tid("thread_parent")] = threadstore.ThreadMeta{
		ID:           tid("thread_parent"),
		RootThreadID: tid("thread_parent"),
	}
	store.threads[tid("thread_doc_old")] = threadstore.ThreadMeta{
		ID:             tid("thread_doc_old"),
		RootThreadID:   tid("thread_parent"),
		ParentThreadID: tid("thread_parent"),
		Status:         threadstore.ThreadStatusCompleted,
		Model:          "gpt-5.4-mini",
		LastResponseID: "resp_doc_old",
		ChildKind:      "document",
		DocumentID:     1,
		DocumentPhase:  "query",
		CreatedAt:      time.Date(2026, 4, 13, 10, 0, 0, 0, time.UTC),
		UpdatedAt:      time.Date(2026, 4, 13, 10, 1, 0, 0, time.UTC),
	}

	var publishedCommands []threadcmd.Command
	actor := &threadActor{
		ctx:    context.Background(),
		logger: testActorLogger(),
		store:  store,
		threadDocs: &fakeThreadDocumentStore{
			documentsByThread: map[int64][]docstore.Document{
				tid("thread_parent"): {{ID: 1, Filename: "report.pdf"}},
			},
		},
		docStore: &fakeDocActorDocStore{
			docs: map[int64]docstore.Document{
				1: {
					ID:         1,
					Filename:   "report.pdf",
					QueryModel: "gpt-5.4-nano",
				},
			},
		},
		preparedInputs: &fakeDocActorPreparedInputClient{ref: "blob://prepared/unused"},
		publish: func(_ context.Context, _ string, cmd threadcmd.Command) error {
			publishedCommands = append(publishedCommands, cmd)
			return nil
		},
	}

	meta := store.threads[tid("thread_parent")]
	if _, err := actor.startDocumentQueryGroup(meta, []docQueryCall{{
		CallID: "call_1",
		Request: docQueryRequest{
			DocumentID: 1,
			Task:       "summarize",
		},
	}}); err != nil {
		t.Fatalf("startDocumentQueryGroup() error = %v", err)
	}

	if len(publishedCommands) != 1 {
		t.Fatalf("publishedCommands = %d, want 1", len(publishedCommands))
	}

	var body map[string]any
	if err := json.Unmarshal(publishedCommands[0].Body, &body); err != nil {
		t.Fatalf("json.Unmarshal(body) error = %v", err)
	}

	if body["model"] != "gpt-5.4-mini" {
		t.Fatalf("model = %v, want gpt-5.4-mini", body["model"])
	}
	if body["previous_response_id"] != "resp_doc_old" {
		t.Fatalf("previous_response_id = %v, want resp_doc_old", body["previous_response_id"])
	}
	if _, exists := body["prepared_input_ref"]; exists {
		t.Fatalf("prepared_input_ref should not be set, got %#v", body["prepared_input_ref"])
	}
}

func TestStartDocumentQueryGroupUsesLatestReadyDocumentChildLineage(t *testing.T) {
	t.Parallel()

	store := newFakeActorStore(t)
	store.threads[tid("thread_parent")] = threadstore.ThreadMeta{
		ID:           tid("thread_parent"),
		RootThreadID: tid("thread_parent"),
	}
	store.threads[tid("thread_doc_ready")] = threadstore.ThreadMeta{
		ID:             tid("thread_doc_ready"),
		RootThreadID:   tid("thread_parent"),
		ParentThreadID: tid("thread_parent"),
		Status:         threadstore.ThreadStatusReady,
		Model:          "gpt-5.4-mini",
		LastResponseID: "resp_doc_ready",
		ChildKind:      "document",
		DocumentID:     1,
		DocumentPhase:  "query",
		CreatedAt:      time.Date(2026, 4, 13, 11, 0, 0, 0, time.UTC),
		UpdatedAt:      time.Date(2026, 4, 13, 11, 1, 0, 0, time.UTC),
	}

	var publishedCommands []threadcmd.Command
	actor := &threadActor{
		ctx:    context.Background(),
		logger: testActorLogger(),
		store:  store,
		threadDocs: &fakeThreadDocumentStore{
			documentsByThread: map[int64][]docstore.Document{
				tid("thread_parent"): {{ID: 1, Filename: "report.pdf"}},
			},
		},
		docStore: &fakeDocActorDocStore{
			docs: map[int64]docstore.Document{
				1: {
					ID:         1,
					Filename:   "report.pdf",
					QueryModel: "gpt-5.4-nano",
				},
			},
		},
		preparedInputs: &fakeDocActorPreparedInputClient{ref: "blob://prepared/unused"},
		publish: func(_ context.Context, _ string, cmd threadcmd.Command) error {
			publishedCommands = append(publishedCommands, cmd)
			return nil
		},
	}

	meta := store.threads[tid("thread_parent")]
	if _, err := actor.startDocumentQueryGroup(meta, []docQueryCall{{
		CallID: "call_1",
		Request: docQueryRequest{
			DocumentID: 1,
			Task:       "summarize",
		},
	}}); err != nil {
		t.Fatalf("startDocumentQueryGroup() error = %v", err)
	}

	if len(publishedCommands) != 1 {
		t.Fatalf("publishedCommands = %d, want 1", len(publishedCommands))
	}

	var body map[string]any
	if err := json.Unmarshal(publishedCommands[0].Body, &body); err != nil {
		t.Fatalf("json.Unmarshal(body) error = %v", err)
	}

	if body["model"] != "gpt-5.4-mini" {
		t.Fatalf("model = %v, want gpt-5.4-mini", body["model"])
	}
	if body["previous_response_id"] != "resp_doc_ready" {
		t.Fatalf("previous_response_id = %v, want resp_doc_ready", body["previous_response_id"])
	}
	if _, exists := body["prepared_input_ref"]; exists {
		t.Fatalf("prepared_input_ref should not be set, got %#v", body["prepared_input_ref"])
	}
}

func TestStartDocumentQueryGroupUsesBaseAnchorWhenNoChildLineage(t *testing.T) {
	t.Parallel()

	store := newFakeActorStore(t)
	store.threads[tid("thread_parent")] = threadstore.ThreadMeta{
		ID:           tid("thread_parent"),
		RootThreadID: tid("thread_parent"),
		Model:        "gpt-5.4",
	}

	preparedInputs := &fakeDocActorPreparedInputClient{ref: "blob://prepared/unused"}
	var publishedCommands []threadcmd.Command
	actor := &threadActor{
		ctx:    context.Background(),
		logger: testActorLogger(),
		store:  store,
		threadDocs: &fakeThreadDocumentStore{
			documentsByThread: map[int64][]docstore.Document{
				tid("thread_parent"): {{ID: 1, Filename: "report.pdf"}},
			},
		},
		docStore: &fakeDocActorDocStore{
			docs: map[int64]docstore.Document{
				1: {
					ID:             1,
					Filename:       "report.pdf",
					QueryModel:     "gpt-5.4-mini",
					BaseResponseID: "resp_doc_base",
					BaseModel:      "gpt-5.4-mini",
				},
			},
		},
		preparedInputs: preparedInputs,
		publish: func(_ context.Context, _ string, cmd threadcmd.Command) error {
			publishedCommands = append(publishedCommands, cmd)
			return nil
		},
	}

	meta := store.threads[tid("thread_parent")]
	if _, err := actor.startDocumentQueryGroup(meta, []docQueryCall{{
		CallID: "call_1",
		Request: docQueryRequest{
			DocumentID: 1,
			Task:       "summarize",
		},
	}}); err != nil {
		t.Fatalf("startDocumentQueryGroup() error = %v", err)
	}

	if len(preparedInputs.requests) != 0 {
		t.Fatalf("preparedInputs.requests = %d, want 0", len(preparedInputs.requests))
	}
	if len(publishedCommands) != 1 {
		t.Fatalf("publishedCommands = %d, want 1", len(publishedCommands))
	}

	var body map[string]any
	if err := json.Unmarshal(publishedCommands[0].Body, &body); err != nil {
		t.Fatalf("json.Unmarshal(body) error = %v", err)
	}

	if body["model"] != "gpt-5.4-mini" {
		t.Fatalf("model = %v, want gpt-5.4-mini", body["model"])
	}
	if body["previous_response_id"] != "resp_doc_base" {
		t.Fatalf("previous_response_id = %v, want resp_doc_base", body["previous_response_id"])
	}
	if _, exists := body["prepared_input_ref"]; exists {
		t.Fatalf("prepared_input_ref should not be set, got %#v", body["prepared_input_ref"])
	}

	metadata, ok := body["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("metadata = %#v, want object", body["metadata"])
	}
	if metadata["spawn_mode"] != "document_query" {
		t.Fatalf("spawn_mode = %v, want document_query", metadata["spawn_mode"])
	}
}

func TestStartDocumentQueryGroupUsesWarmupChildWhenNoLineageOrBaseAnchor(t *testing.T) {
	t.Parallel()

	store := newFakeActorStore(t)
	store.threads[tid("thread_parent")] = threadstore.ThreadMeta{
		ID:           tid("thread_parent"),
		RootThreadID: tid("thread_parent"),
		Model:        "gpt-5.4",
	}

	preparedInputs := &fakeDocActorPreparedInputClient{ref: "blob://prepared/warmup"}
	var publishedCommands []threadcmd.Command
	actor := &threadActor{
		ctx:    context.Background(),
		logger: testActorLogger(),
		store:  store,
		threadDocs: &fakeThreadDocumentStore{
			documentsByThread: map[int64][]docstore.Document{
				tid("thread_parent"): {{ID: 1, Filename: "report.pdf"}},
			},
		},
		docStore: &fakeDocActorDocStore{
			docs: map[int64]docstore.Document{
				1: {
					ID:         1,
					Filename:   "report.pdf",
					QueryModel: "gpt-5.4-mini",
				},
			},
		},
		preparedInputs: preparedInputs,
		publish: func(_ context.Context, _ string, cmd threadcmd.Command) error {
			publishedCommands = append(publishedCommands, cmd)
			return nil
		},
	}

	meta := store.threads[tid("thread_parent")]
	if _, err := actor.startDocumentQueryGroup(meta, []docQueryCall{{
		CallID: "call_1",
		Request: docQueryRequest{
			DocumentID: 1,
			Task:       "summarize",
		},
	}}); err != nil {
		t.Fatalf("startDocumentQueryGroup() error = %v", err)
	}

	if len(preparedInputs.requests) != 1 {
		t.Fatalf("preparedInputs.requests = %d, want 1", len(preparedInputs.requests))
	}
	if preparedInputs.requests[0].Kind != doccmd.PrepareKindWarmup {
		t.Fatalf("preparedInputs.requests[0].Kind = %q, want %q", preparedInputs.requests[0].Kind, doccmd.PrepareKindWarmup)
	}
	if len(publishedCommands) != 1 {
		t.Fatalf("publishedCommands = %d, want 1", len(publishedCommands))
	}
	if publishedCommands[0].ThreadID <= 0 {
		t.Fatalf("publishedCommands[0].ThreadID = %d, want positive warmup thread id", publishedCommands[0].ThreadID)
	}

	var body map[string]any
	if err := json.Unmarshal(publishedCommands[0].Body, &body); err != nil {
		t.Fatalf("json.Unmarshal(body) error = %v", err)
	}

	if body["prepared_input_ref"] != "blob://prepared/warmup" {
		t.Fatalf("prepared_input_ref = %v, want blob://prepared/warmup", body["prepared_input_ref"])
	}
	if _, exists := body["previous_response_id"]; exists {
		t.Fatalf("previous_response_id should not be set, got %#v", body["previous_response_id"])
	}

	metadata, ok := body["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("metadata = %#v, want object", body["metadata"])
	}
	if metadata["spawn_mode"] != "document_warmup" {
		t.Fatalf("spawn_mode = %v, want document_warmup", metadata["spawn_mode"])
	}
	if metadata["document_task"] != "summarize" {
		t.Fatalf("document_task = %v, want summarize", metadata["document_task"])
	}
}

func TestHandleChildResultDocumentWarmupCompletionStoresBaseLineageAndSpawnsQueryChild(t *testing.T) {
	t.Parallel()

	store := newFakeActorStore(t)
	spawnGroupID := stableDocumentSpawnGroupID(tid("thread_parent"), "call_1")
	store.threads[tid("thread_parent")] = threadstore.ThreadMeta{
		ID:                 tid("thread_parent"),
		RootThreadID:       tid("thread_parent"),
		Model:              "gpt-5.4",
		Status:             threadstore.ThreadStatusWaitingChildren,
		ActiveSpawnGroupID: spawnGroupID,
	}

	warmupThreadID := tid("thread_warmup")
	store.threads[warmupThreadID] = threadstore.ThreadMeta{
		ID:                 warmupThreadID,
		RootThreadID:       tid("thread_parent"),
		ParentThreadID:     tid("thread_parent"),
		ParentCallID:       "call_1",
		Depth:              1,
		Status:             threadstore.ThreadStatusCompleted,
		Model:              "gpt-5.4-mini",
		LastResponseID:     "resp_warmup",
		ActiveSpawnGroupID: spawnGroupID,
		MetadataJSON:       `{"document_id":"1","document_name":"report.pdf","document_task":"summarize","spawn_mode":"document_warmup"}`,
		ChildKind:          "document",
		DocumentID:         1,
		DocumentPhase:      "warmup",
	}
	store.spawnGroups[spawnGroupID] = threadstore.SpawnGroupMeta{
		ID:             spawnGroupID,
		ParentThreadID: tid("thread_parent"),
		ParentCallID:   "call_1",
		Expected:       1,
		Status:         threadstore.SpawnGroupStatusWaiting,
	}

	docStore := &fakeDocActorDocStore{
		docs: map[int64]docstore.Document{
			1: {
				ID:       1,
				Filename: "report.pdf",
			},
		},
	}

	var publishedSubject string
	var publishedCommands []threadcmd.Command
	actor := newActorRecoveryHarness(t, store, nil)
	var logBuf bytes.Buffer
	actor.logger = slog.New(slog.NewTextHandler(&logBuf, nil))
	actor.docStore = docStore
	actor.publish = func(_ context.Context, subject string, cmd threadcmd.Command) error {
		publishedSubject = subject
		publishedCommands = append(publishedCommands, cmd)
		return nil
	}

	cmd := threadcmd.Command{
		CmdID:    303,
		Kind:     threadcmd.KindThreadChildCompleted,
		ThreadID: tid("thread_parent"),
		Body: json.RawMessage(`{
			"spawn_group_id":` + strconv.FormatInt(spawnGroupID, 10) + `,
			"child_thread_id":` + strconv.FormatInt(warmupThreadID, 10) + `,
			"child_response_id":"resp_warmup",
			"status":"completed"
		}`),
	}

	if err := actor.handleChildResult(cmd, "completed"); err != nil {
		t.Fatalf("handleChildResult() error = %v", err)
	}

	if len(docStore.baseUpdates) != 1 {
		t.Fatalf("baseUpdates = %d, want 1", len(docStore.baseUpdates))
	}
	if docStore.baseUpdates[0].DocumentID != 1 {
		t.Fatalf("DocumentID = %d, want 1", docStore.baseUpdates[0].DocumentID)
	}
	if docStore.baseUpdates[0].ResponseID != "resp_warmup" {
		t.Fatalf("ResponseID = %q, want resp_warmup", docStore.baseUpdates[0].ResponseID)
	}
	if docStore.baseUpdates[0].Model != "gpt-5.4-mini" {
		t.Fatalf("Model = %q, want gpt-5.4-mini", docStore.baseUpdates[0].Model)
	}

	if publishedSubject != threadcmd.DispatchStartSubject {
		t.Fatalf("publishedSubject = %q, want %q", publishedSubject, threadcmd.DispatchStartSubject)
	}
	if len(publishedCommands) != 1 {
		t.Fatalf("publishedCommands = %d, want 1", len(publishedCommands))
	}

	queryThreadID := warmupThreadID
	if publishedCommands[0].ThreadID != queryThreadID {
		t.Fatalf("publishedCommands[0].ThreadID = %d, want %d", publishedCommands[0].ThreadID, queryThreadID)
	}
	if publishedCommands[0].CausationID != "resp_warmup" {
		t.Fatalf("publishedCommands[0].CausationID = %q, want resp_warmup", publishedCommands[0].CausationID)
	}

	var body map[string]any
	if err := json.Unmarshal(publishedCommands[0].Body, &body); err != nil {
		t.Fatalf("json.Unmarshal(body) error = %v", err)
	}

	if body["model"] != "gpt-5.4-mini" {
		t.Fatalf("model = %v, want gpt-5.4-mini", body["model"])
	}
	if body["previous_response_id"] != "resp_warmup" {
		t.Fatalf("previous_response_id = %v, want resp_warmup", body["previous_response_id"])
	}
	if _, exists := body["prepared_input_ref"]; exists {
		t.Fatalf("prepared_input_ref should not be set, got %#v", body["prepared_input_ref"])
	}

	metadata, ok := body["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("metadata = %#v, want object", body["metadata"])
	}
	if metadata["spawn_mode"] != "document_query" {
		t.Fatalf("spawn_mode = %v, want document_query", metadata["spawn_mode"])
	}
	if metadata["bootstrap_child_thread_id"] != strconv.FormatInt(warmupThreadID, 10) {
		t.Fatalf("bootstrap_child_thread_id = %v, want %s", metadata["bootstrap_child_thread_id"], strconv.FormatInt(warmupThreadID, 10))
	}

	queryMeta, ok := store.threads[queryThreadID]
	if !ok {
		t.Fatalf("query thread %d was not created", queryThreadID)
	}
	if queryMeta.ActiveSpawnGroupID != spawnGroupID {
		t.Fatalf("queryMeta.ActiveSpawnGroupID = %d, want %d", queryMeta.ActiveSpawnGroupID, spawnGroupID)
	}
	if len(store.spawnResults[spawnGroupID]) != 0 {
		t.Fatalf("spawnResults = %#v, want no stored result after warmup completion", store.spawnResults[spawnGroupID])
	}

	logs := logBuf.String()
	for _, needle := range []string{
		`msg="spawning child thread"`,
		`bootstrap_child_thread_id=` + strconv.FormatInt(warmupThreadID, 10),
		`document_name=report.pdf`,
		`phase=query`,
		`has_previous_response_id=true`,
	} {
		if !strings.Contains(logs, needle) {
			t.Fatalf("logs = %q, want %s", logs, needle)
		}
	}
}

func TestStreamUntilTerminalSpawnsDocumentQueryChildren(t *testing.T) {
	t.Parallel()

	store := newFakeActorStore(t)
	store.threads[tid("thread_parent")] = threadstore.ThreadMeta{
		ID:               tid("thread_parent"),
		RootThreadID:     tid("thread_parent"),
		Status:           threadstore.ThreadStatusRunning,
		Model:            "gpt-5.4",
		OwnerWorkerID:    tid("worker-local-1"),
		SocketGeneration: 1,
	}

	funcCallItem := `{
		"type":"function_call",
		"call_id":"call_doc_1",
		"name":"query_document",
		"arguments":"{\"document_id\":1,\"task\":\"summarize\"}"
	}`
	conn := &actorTestConn{
		reads: [][]byte{
			[]byte(`{"type":"response.created","response":{"id":"resp_1"}}`),
			[]byte(`{"type":"response.output_item.done","response":{"id":"resp_1"},"item":` + funcCallItem + `}`),
			[]byte(`{"type":"response.completed","response":{"id":"resp_1"}}`),
		},
	}
	var publishedCommands []threadcmd.Command
	actor := newActorRecoveryHarness(t, store, conn)
	actor.threadDocs = &fakeThreadDocumentStore{
		documentsByThread: map[int64][]docstore.Document{
			tid("thread_parent"): {{ID: 1, Filename: "report.pdf"}},
		},
	}
	actor.docStore = &fakeDocActorDocStore{
		docs: map[int64]docstore.Document{
			1: {ID: 1, Filename: "report.pdf", QueryModel: "gpt-5.4", ManifestRef: "blob://manifest"},
		},
	}
	actor.preparedInputs = &fakeDocActorPreparedInputClient{ref: "blob://prepared/test"}
	actor.publish = func(_ context.Context, _ string, cmd threadcmd.Command) error {
		publishedCommands = append(publishedCommands, cmd)
		return nil
	}

	meta := store.threads[tid("thread_parent")]
	if _, err := actor.streamUntilTerminal(meta); err != nil {
		t.Fatalf("streamUntilTerminal() error = %v", err)
	}

	final := store.threads[tid("thread_parent")]
	if final.Status != threadstore.ThreadStatusWaitingChildren {
		t.Fatalf("final status = %q, want waiting_children", final.Status)
	}
	if final.ActiveSpawnGroupID == 0 {
		t.Fatal("expected active spawn group ID to be set")
	}

	if len(publishedCommands) != 1 {
		t.Fatalf("publishedCommands = %d, want 1", len(publishedCommands))
	}
	cmd := publishedCommands[0]
	if cmd.Kind != threadcmd.KindThreadStart {
		t.Fatalf("command kind = %q, want %q", cmd.Kind, threadcmd.KindThreadStart)
	}
}

func TestStreamUntilTerminalSpawnsDocumentQueryChildrenForMultipleFunctionCalls(t *testing.T) {
	t.Parallel()

	store := newFakeActorStore(t)
	store.threads[tid("thread_parent")] = threadstore.ThreadMeta{
		ID:               tid("thread_parent"),
		RootThreadID:     tid("thread_parent"),
		Status:           threadstore.ThreadStatusRunning,
		Model:            "gpt-5.4",
		OwnerWorkerID:    tid("worker-local-1"),
		SocketGeneration: 1,
	}

	funcCallItem1 := `{
		"type":"function_call",
		"call_id":"call_doc_1",
		"name":"query_document",
		"arguments":"{\"document_id\":1,\"task\":\"summarize doc 1\"}"
	}`
	funcCallItem2 := `{
		"type":"function_call",
		"call_id":"call_doc_2",
		"name":"query_document",
		"arguments":"{\"document_id\":2,\"task\":\"summarize doc 2\"}"
	}`
	conn := &actorTestConn{
		reads: [][]byte{
			[]byte(`{"type":"response.created","response":{"id":"resp_1"}}`),
			[]byte(`{"type":"response.output_item.done","response":{"id":"resp_1"},"item":` + funcCallItem1 + `}`),
			[]byte(`{"type":"response.output_item.done","response":{"id":"resp_1"},"item":` + funcCallItem2 + `}`),
			[]byte(`{"type":"response.completed","response":{"id":"resp_1"}}`),
		},
	}
	var publishedCommands []threadcmd.Command
	actor := newActorRecoveryHarness(t, store, conn)
	actor.threadDocs = &fakeThreadDocumentStore{
		documentsByThread: map[int64][]docstore.Document{
			tid("thread_parent"): {
				{ID: 1, Filename: "report-1.pdf"},
				{ID: 2, Filename: "report-2.pdf"},
			},
		},
	}
	actor.docStore = &fakeDocActorDocStore{
		docs: map[int64]docstore.Document{
			1: {ID: 1, Filename: "report-1.pdf", QueryModel: "gpt-5.4", ManifestRef: "blob://manifest-1"},
			2: {ID: 2, Filename: "report-2.pdf", QueryModel: "gpt-5.4", ManifestRef: "blob://manifest-2"},
		},
	}
	actor.preparedInputs = &fakeDocActorPreparedInputClient{ref: "blob://prepared/test"}
	actor.publish = func(_ context.Context, _ string, cmd threadcmd.Command) error {
		publishedCommands = append(publishedCommands, cmd)
		return nil
	}

	meta := store.threads[tid("thread_parent")]
	if _, err := actor.streamUntilTerminal(meta); err != nil {
		t.Fatalf("streamUntilTerminal() error = %v", err)
	}

	final := store.threads[tid("thread_parent")]
	if final.Status != threadstore.ThreadStatusWaitingChildren {
		t.Fatalf("final status = %q, want waiting_children", final.Status)
	}
	if final.ActiveSpawnGroupID == 0 {
		t.Fatal("expected active spawn group ID to be set")
	}
	if len(publishedCommands) != 2 {
		t.Fatalf("publishedCommands = %d, want 2", len(publishedCommands))
	}

	spawn := store.spawnGroups[final.ActiveSpawnGroupID]
	roundCalls, err := decodeDocQueryRoundCalls(spawn.ParentCallID)
	if err != nil {
		t.Fatalf("decodeDocQueryRoundCalls() error = %v", err)
	}
	if len(roundCalls) != 2 {
		t.Fatalf("len(roundCalls) = %d, want 2", len(roundCalls))
	}
	if roundCalls[0].CallID != "call_doc_1" || roundCalls[1].CallID != "call_doc_2" {
		t.Fatalf("roundCalls = %#v, want call_doc_1 and call_doc_2", roundCalls)
	}
}

func TestStreamUntilTerminalDocumentWarmupPublishesAfterCleanup(t *testing.T) {
	t.Parallel()

	store := newFakeActorStore(t)
	spawnGroupID := stableDocumentSpawnGroupID(tid("thread_parent"), "call_1")
	store.threads[tid("thread_parent")] = threadstore.ThreadMeta{
		ID:                 tid("thread_parent"),
		RootThreadID:       tid("thread_parent"),
		Model:              "gpt-5.4",
		Status:             threadstore.ThreadStatusWaitingChildren,
		ActiveSpawnGroupID: spawnGroupID,
	}

	warmupThreadID := tid("thread_warmup")
	store.threads[warmupThreadID] = threadstore.ThreadMeta{
		ID:                 warmupThreadID,
		RootThreadID:       tid("thread_parent"),
		ParentThreadID:     tid("thread_parent"),
		ParentCallID:       "call_1",
		Depth:              1,
		Status:             threadstore.ThreadStatusRunning,
		Model:              "gpt-5.4-mini",
		OwnerWorkerID:      tid("worker-child"),
		SocketGeneration:   1,
		ActiveSpawnGroupID: spawnGroupID,
		MetadataJSON:       `{"document_id":"1","document_name":"report.pdf","document_task":"summarize","spawn_mode":"document_warmup"}`,
		ChildKind:          "document",
		DocumentID:         1,
		DocumentPhase:      "warmup",
	}
	store.spawnGroups[spawnGroupID] = threadstore.SpawnGroupMeta{
		ID:             spawnGroupID,
		ParentThreadID: tid("thread_parent"),
		ParentCallID:   "call_1",
		Expected:       1,
		Status:         threadstore.SpawnGroupStatusWaiting,
	}

	parentActor := newActorRecoveryHarness(t, store, nil)
	parentActor.docStore = &fakeDocActorDocStore{
		docs: map[int64]docstore.Document{
			1: {ID: 1, Filename: "report.pdf"},
		},
	}
	var queryStarts []threadcmd.Command
	parentActor.publish = func(_ context.Context, _ string, cmd threadcmd.Command) error {
		queryStarts = append(queryStarts, cmd)
		return nil
	}

	childConn := &actorTestConn{
		reads: [][]byte{
			[]byte(`{"type":"response.created","response":{"id":"resp_warmup"}}`),
			[]byte(`{"type":"response.completed","response":{"id":"resp_warmup"}}`),
		},
	}
	childActor := newActorRecoveryHarness(t, store, childConn)
	childActor.threadID = warmupThreadID
	childActor.workerID = tid("worker-child")
	childActor.publish = func(_ context.Context, _ string, cmd threadcmd.Command) error {
		return parentActor.handleChildResult(cmd, "completed")
	}

	if _, err := childActor.streamUntilTerminal(store.threads[warmupThreadID]); err != nil {
		t.Fatalf("streamUntilTerminal() error = %v", err)
	}

	if len(queryStarts) != 1 {
		t.Fatalf("queryStarts = %d, want 1", len(queryStarts))
	}

	final := store.threads[warmupThreadID]
	if final.ParentCallID != "call_1" {
		t.Fatalf("final.ParentCallID = %q, want call_1", final.ParentCallID)
	}
	if final.ActiveSpawnGroupID != spawnGroupID {
		t.Fatalf("final.ActiveSpawnGroupID = %d, want %d", final.ActiveSpawnGroupID, spawnGroupID)
	}
}

func TestStreamUntilTerminalClearsActiveSpawnGroupForTerminalChild(t *testing.T) {
	t.Parallel()

	store := newFakeActorStore(t)
	store.threads[tid("thread_parent")] = threadstore.ThreadMeta{
		ID:           tid("thread_parent"),
		RootThreadID: tid("thread_parent"),
		Status:       threadstore.ThreadStatusWaitingChildren,
		Model:        "gpt-5.4",
	}
	store.threads[tid("thread_child")] = threadstore.ThreadMeta{
		ID:                 tid("thread_child"),
		RootThreadID:       tid("thread_parent"),
		ParentThreadID:     tid("thread_parent"),
		ParentCallID:       "call_parent",
		Status:             threadstore.ThreadStatusRunning,
		Model:              "gpt-5.4-mini",
		OwnerWorkerID:      tid("worker-local-1"),
		SocketGeneration:   1,
		ActiveSpawnGroupID: tid("sg_waiting"),
	}

	conn := &actorTestConn{
		reads: [][]byte{
			[]byte(`{"type":"response.created","response":{"id":"resp_child_done"}}`),
			[]byte(`{"type":"response.completed","response":{"id":"resp_child_done"}}`),
		},
	}
	actor := newActorRecoveryHarness(t, store, conn)
	actor.threadID = tid("thread_child")

	if _, err := actor.streamUntilTerminal(store.threads[tid("thread_child")]); err != nil {
		t.Fatalf("streamUntilTerminal() error = %v", err)
	}

	final := store.threads[tid("thread_child")]
	if final.Status != threadstore.ThreadStatusCompleted {
		t.Fatalf("final.Status = %q, want completed", final.Status)
	}
	if final.ParentCallID != "" {
		t.Fatalf("final.ParentCallID = %q, want empty", final.ParentCallID)
	}
	if final.ActiveSpawnGroupID != 0 {
		t.Fatalf("final.ActiveSpawnGroupID = %d, want empty", final.ActiveSpawnGroupID)
	}
	if len(store.releasedThreads) != 1 || store.releasedThreads[0] != tid("thread_child") {
		t.Fatalf("releasedThreads = %#v, want [%d]", store.releasedThreads, tid("thread_child"))
	}
}

func TestStreamUntilTerminalLeavesSuccessfulDocumentQueryChildReady(t *testing.T) {
	t.Parallel()

	store := newFakeActorStore(t)
	store.threads[tid("thread_parent")] = threadstore.ThreadMeta{
		ID:            tid("thread_parent"),
		RootThreadID:  tid("thread_parent"),
		Status:        threadstore.ThreadStatusWaitingChildren,
		Model:         "gpt-5.4",
		OwnerWorkerID: tid("worker-parent"),
	}
	store.threads[tid("thread_doc_query")] = threadstore.ThreadMeta{
		ID:                 tid("thread_doc_query"),
		RootThreadID:       tid("thread_parent"),
		ParentThreadID:     tid("thread_parent"),
		ParentCallID:       "call_parent",
		Status:             threadstore.ThreadStatusRunning,
		Model:              "gpt-5.4-mini",
		OwnerWorkerID:      tid("worker-local-1"),
		SocketGeneration:   1,
		ActiveSpawnGroupID: tid("sg_waiting"),
		MetadataJSON:       `{"document_id":"1","spawn_mode":"document_query"}`,
		ChildKind:          "document",
		DocumentID:         1,
		DocumentPhase:      "query",
	}

	conn := &actorTestConn{
		reads: [][]byte{
			[]byte(`{"type":"response.created","response":{"id":"resp_doc_done"}}`),
			[]byte(`{"type":"response.completed","response":{"id":"resp_doc_done"}}`),
		},
	}
	actor := newActorRecoveryHarness(t, store, conn)
	actor.threadID = tid("thread_doc_query")

	if _, err := actor.streamUntilTerminal(store.threads[tid("thread_doc_query")]); err != nil {
		t.Fatalf("streamUntilTerminal() error = %v", err)
	}

	final := store.threads[tid("thread_doc_query")]
	if final.Status != threadstore.ThreadStatusReady {
		t.Fatalf("final.Status = %q, want ready", final.Status)
	}
	if final.ParentCallID != "" {
		t.Fatalf("final.ParentCallID = %q, want empty", final.ParentCallID)
	}
	if final.ActiveSpawnGroupID != 0 {
		t.Fatalf("final.ActiveSpawnGroupID = %d, want empty", final.ActiveSpawnGroupID)
	}
	if final.LastResponseID != "resp_doc_done" {
		t.Fatalf("final.LastResponseID = %q, want resp_doc_done", final.LastResponseID)
	}
	if len(store.releasedThreads) != 0 {
		t.Fatalf("releasedThreads = %#v, want empty", store.releasedThreads)
	}
}

func TestStreamUntilTerminalPublishesRootDeltaEventsButKeepsThemOutOfHistory(t *testing.T) {
	t.Parallel()

	store := newFakeActorStore(t)
	store.threads[tid("thread_root")] = threadstore.ThreadMeta{
		ID:               tid("thread_root"),
		Status:           threadstore.ThreadStatusRunning,
		OwnerWorkerID:    tid("worker-local-1"),
		SocketGeneration: 1,
	}

	conn := &actorTestConn{
		reads: [][]byte{
			[]byte(`{"type":"response.output_text.delta","response":{"id":"resp_1"},"delta":"hello"}`),
			[]byte(`{"type":"response.completed","response":{"id":"resp_1","status":"completed"}}`),
		},
	}

	var publishedEventTypes []string

	actor := newActorRecoveryHarness(t, store, conn)
	actor.publishEvent = func(_ context.Context, _ int64, _ uint64, _ string, eventType string, _ json.RawMessage) error {
		publishedEventTypes = append(publishedEventTypes, eventType)
		return nil
	}

	if _, err := actor.streamUntilTerminal(store.threads[tid("thread_root")]); err != nil {
		t.Fatalf("streamUntilTerminal() error = %v", err)
	}

	hasDelta := false
	hasCompleted := false
	for _, eventType := range publishedEventTypes {
		if eventType == "response.output_text.delta" {
			hasDelta = true
		}
		if eventType == "response.completed" {
			hasCompleted = true
		}
	}
	if !hasDelta || !hasCompleted {
		t.Fatalf("publishedEventTypes = %#v, want response.output_text.delta and response.completed", publishedEventTypes)
	}

	for _, entry := range store.historyEvents {
		if entry.EventType == "response.output_text.delta" {
			t.Fatalf("delta event leaked into history: %#v", entry)
		}
	}

	foundCompleted := false
	for _, entry := range store.historyEvents {
		if entry.EventType == "response.completed" {
			foundCompleted = true
			break
		}
	}
	if !foundCompleted {
		t.Fatalf("historyEvents = %#v, want response.completed entry", store.historyEvents)
	}
}

func TestStreamUntilTerminalSuppressesChildDeltaLiveEvents(t *testing.T) {
	t.Parallel()

	store := newFakeActorStore(t)
	store.threads[tid("thread_child")] = threadstore.ThreadMeta{
		ID:               tid("thread_child"),
		ParentThreadID:   tid("thread_parent"),
		Status:           threadstore.ThreadStatusRunning,
		OwnerWorkerID:    tid("worker-local-1"),
		SocketGeneration: 1,
	}

	conn := &actorTestConn{
		reads: [][]byte{
			[]byte(`{"type":"response.output_text.delta","response":{"id":"resp_1"},"delta":"hello"}`),
			[]byte(`{"type":"response.completed","response":{"id":"resp_1","status":"completed"}}`),
		},
	}

	var publishedEventTypes []string

	actor := newActorRecoveryHarness(t, store, conn)
	actor.publishEvent = func(_ context.Context, _ int64, _ uint64, _ string, eventType string, _ json.RawMessage) error {
		publishedEventTypes = append(publishedEventTypes, eventType)
		return nil
	}

	if _, err := actor.streamUntilTerminal(store.threads[tid("thread_child")]); err != nil {
		t.Fatalf("streamUntilTerminal() error = %v", err)
	}

	for _, eventType := range publishedEventTypes {
		if eventType == "response.output_text.delta" {
			t.Fatalf("child delta event should not publish live: %#v", publishedEventTypes)
		}
	}

	foundCompleted := false
	for _, eventType := range publishedEventTypes {
		if eventType == "response.completed" {
			foundCompleted = true
			break
		}
	}
	if !foundCompleted {
		t.Fatalf("publishedEventTypes = %#v, want response.completed entry", publishedEventTypes)
	}

	for _, entry := range store.historyEvents {
		if entry.EventType == "response.output_text.delta" {
			t.Fatalf("delta event leaked into history: %#v", entry)
		}
	}
}

func TestFlushDeltaLogForDoneEventLogsOnlySuppressedLastDelta(t *testing.T) {
	t.Parallel()

	var logBuf bytes.Buffer
	actor := &threadActor{
		logger: slog.New(slog.NewTextHandler(&logBuf, nil)),
	}

	deltaLogs := map[openaiws.EventType]*deltaLogState{
		openaiws.EventTypeResponseOutputTextDelta: {
			firstRaw:      `{"type":"response.output_text.delta","delta":"hel"}`,
			lastRaw:       `{"type":"response.output_text.delta","delta":"lo"}`,
			firstLogged:   true,
			suppressedAny: true,
		},
	}

	actor.flushDeltaLogForDoneEvent(deltaLogs, threadstore.ThreadMeta{
		RootThreadID: tid("thread_root"),
		Depth:        0,
	}, "resp_123", openaiws.EventTypeResponseOutputTextDone)

	if len(deltaLogs) != 0 {
		t.Fatalf("deltaLogs still has %d entries, want 0", len(deltaLogs))
	}
	logs := logBuf.String()
	if !strings.Contains(logs, `event_type=response.output_text.delta`) {
		t.Fatalf("logs = %q, want flushed delta event type", logs)
	}
	if !strings.Contains(logs, `root_thread_id=`+strconv.FormatInt(tid("thread_root"), 10)) {
		t.Fatalf("logs = %q, want root_thread_id", logs)
	}
	if !strings.Contains(logs, `response_id=resp_123`) {
		t.Fatalf("logs = %q, want response_id", logs)
	}
	if strings.Contains(logs, `raw=`) {
		t.Fatalf("logs = %q, want no raw delta payload", logs)
	}
}

func TestFlushDeltaLogForDoneEventSkipsWhenOnlyFirstDeltaSeen(t *testing.T) {
	t.Parallel()

	var logBuf bytes.Buffer
	actor := &threadActor{
		logger: slog.New(slog.NewTextHandler(&logBuf, nil)),
	}

	deltaLogs := map[openaiws.EventType]*deltaLogState{
		openaiws.EventTypeResponseOutputTextDelta: {
			firstRaw:    `{"type":"response.output_text.delta","delta":"hello"}`,
			lastRaw:     `{"type":"response.output_text.delta","delta":"hello"}`,
			firstLogged: true,
		},
	}

	actor.flushDeltaLogForDoneEvent(deltaLogs, threadstore.ThreadMeta{}, "", openaiws.EventTypeResponseOutputTextDone)

	if len(deltaLogs) != 0 {
		t.Fatalf("deltaLogs still has %d entries, want 0", len(deltaLogs))
	}
	if got := strings.TrimSpace(logBuf.String()); got != "" {
		t.Fatalf("logs = %q, want empty", got)
	}
}

func TestStreamUntilTerminalDropsReasoningDeltas(t *testing.T) {
	t.Parallel()

	store := newFakeActorStore(t)
	store.threads[tid("thread_root")] = threadstore.ThreadMeta{
		ID:               tid("thread_root"),
		Status:           threadstore.ThreadStatusRunning,
		OwnerWorkerID:    tid("worker-local-1"),
		SocketGeneration: 1,
	}

	conn := &actorTestConn{
		reads: [][]byte{
			[]byte(`{"type":"response.reasoning_text.delta","delta":"thinking..."}`),
			[]byte(`{"type":"response.reasoning_summary_text.delta","delta":"summary..."}`),
			[]byte(`{"type":"response.output_text.delta","response":{"id":"resp_1"},"delta":"hello"}`),
			[]byte(`{"type":"response.completed","response":{"id":"resp_1","status":"completed"}}`),
		},
	}

	var publishedEventTypes []string

	actor := newActorRecoveryHarness(t, store, conn)
	actor.publishEvent = func(_ context.Context, _ int64, _ uint64, _ string, eventType string, _ json.RawMessage) error {
		publishedEventTypes = append(publishedEventTypes, eventType)
		return nil
	}

	if _, err := actor.streamUntilTerminal(store.threads[tid("thread_root")]); err != nil {
		t.Fatalf("streamUntilTerminal() error = %v", err)
	}

	for _, eventType := range publishedEventTypes {
		if eventType == "response.reasoning_text.delta" || eventType == "response.reasoning_summary_text.delta" {
			t.Fatalf("reasoning delta event should not publish: %#v", publishedEventTypes)
		}
	}

	for _, entry := range store.historyEvents {
		if entry.EventType == "response.reasoning_text.delta" || entry.EventType == "response.reasoning_summary_text.delta" {
			t.Fatalf("reasoning delta event should not be in history: %#v", entry)
		}
	}

	found := false
	for _, eventType := range publishedEventTypes {
		if eventType == "response.output_text.delta" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("publishedEventTypes = %#v, want response.output_text.delta still published", publishedEventTypes)
	}
}

func TestStreamUntilTerminalDropsToolCallDeltas(t *testing.T) {
	t.Parallel()

	store := newFakeActorStore(t)
	store.threads[tid("thread_root")] = threadstore.ThreadMeta{
		ID:               tid("thread_root"),
		Status:           threadstore.ThreadStatusRunning,
		OwnerWorkerID:    tid("worker-local-1"),
		SocketGeneration: 1,
	}

	conn := &actorTestConn{
		reads: [][]byte{
			[]byte(`{"type":"response.output_item.added","response":{"id":"resp_1"},"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"some_user_tool","arguments":""}}`),
			[]byte(`{"type":"response.function_call_arguments.delta","response":{"id":"resp_1"},"item_id":"fc_1","delta":"{\"k\":"}`),
			[]byte(`{"type":"response.function_call_arguments.delta","response":{"id":"resp_1"},"item_id":"fc_1","delta":"\"v\"}"}`),
			[]byte(`{"type":"response.function_call_arguments.done","response":{"id":"resp_1"},"item_id":"fc_1","arguments":"{\"k\":\"v\"}"}`),
			[]byte(`{"type":"response.output_item.done","response":{"id":"resp_1"},"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"some_user_tool","arguments":"{\"k\":\"v\"}"}}`),
			[]byte(`{"type":"response.completed","response":{"id":"resp_1","status":"completed"}}`),
		},
	}

	var publishedEventTypes []string

	actor := newActorRecoveryHarness(t, store, conn)
	actor.publishEvent = func(_ context.Context, _ int64, _ uint64, _ string, eventType string, _ json.RawMessage) error {
		publishedEventTypes = append(publishedEventTypes, eventType)
		return nil
	}

	if _, err := actor.streamUntilTerminal(store.threads[tid("thread_root")]); err != nil {
		t.Fatalf("streamUntilTerminal() error = %v", err)
	}

	for _, eventType := range publishedEventTypes {
		if eventType == "response.function_call_arguments.delta" {
			t.Fatalf("tool-call delta event should not publish: %#v", publishedEventTypes)
		}
	}

	for _, entry := range store.historyEvents {
		if entry.EventType == "response.function_call_arguments.delta" {
			t.Fatalf("tool-call delta event should not be in history: %#v", entry)
		}
	}

	// The added / arguments.done / output_item.done envelope around the call
	// must still survive — the FE needs these to render the tool invocation.
	var hasAdded, hasArgsDone, hasItemDone bool
	for _, eventType := range publishedEventTypes {
		switch eventType {
		case "response.output_item.added":
			hasAdded = true
		case "response.function_call_arguments.done":
			hasArgsDone = true
		case "response.output_item.done":
			hasItemDone = true
		}
	}
	if !hasAdded || !hasArgsDone || !hasItemDone {
		t.Fatalf("publishedEventTypes = %#v, want output_item.added + function_call_arguments.done + output_item.done", publishedEventTypes)
	}
}

func TestStreamUntilTerminalPublishesChildNonDeltaEvents(t *testing.T) {
	t.Parallel()

	store := newFakeActorStore(t)
	store.threads[tid("thread_child")] = threadstore.ThreadMeta{
		ID:               tid("thread_child"),
		RootThreadID:     tid("thread_root"),
		ParentThreadID:   tid("thread_parent"),
		Status:           threadstore.ThreadStatusRunning,
		OwnerWorkerID:    tid("worker-local-1"),
		SocketGeneration: 1,
	}

	conn := &actorTestConn{
		reads: [][]byte{
			[]byte(`{"type":"response.output_text.delta","response":{"id":"resp_1"},"delta":"hello"}`),
			[]byte(`{"type":"response.output_item.done","response":{"id":"resp_1"},"item":{"id":"item_1","type":"message","role":"assistant","content":[]}}`),
			[]byte(`{"type":"response.completed","response":{"id":"resp_1","status":"completed"}}`),
		},
	}

	var publishedEventTypes []string

	actor := newActorRecoveryHarness(t, store, conn)
	actor.publishEvent = func(_ context.Context, _ int64, _ uint64, _ string, eventType string, _ json.RawMessage) error {
		publishedEventTypes = append(publishedEventTypes, eventType)
		return nil
	}

	if _, err := actor.streamUntilTerminal(store.threads[tid("thread_child")]); err != nil {
		t.Fatalf("streamUntilTerminal() error = %v", err)
	}

	for _, eventType := range publishedEventTypes {
		if eventType == "response.output_text.delta" {
			t.Fatalf("child delta event should not publish: %#v", publishedEventTypes)
		}
	}

	hasItemDone := false
	hasCompleted := false
	for _, eventType := range publishedEventTypes {
		if eventType == "response.output_item.done" {
			hasItemDone = true
		}
		if eventType == "response.completed" {
			hasCompleted = true
		}
	}
	if !hasItemDone || !hasCompleted {
		t.Fatalf("publishedEventTypes = %#v, want response.output_item.done and response.completed for child", publishedEventTypes)
	}
}

func TestInjectThreadFields(t *testing.T) {
	t.Parallel()

	meta := threadstore.ThreadMeta{
		ID:             7,
		RootThreadID:   3,
		ParentThreadID: 3,
	}

	tests := []struct {
		name string
		raw  json.RawMessage
		want string
	}{
		{
			name: "normal event",
			raw:  json.RawMessage(`{"type":"response.completed"}`),
			want: `{"thread_id":7,"root_thread_id":3,"parent_thread_id":3,"type":"response.completed"}`,
		},
		{
			name: "empty object produces valid JSON",
			raw:  json.RawMessage(`{}`),
			want: `{"thread_id":7,"root_thread_id":3,"parent_thread_id":3}`,
		},
		{
			name: "non-object passthrough",
			raw:  json.RawMessage(`[]`),
			want: `[]`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(injectThreadFields(meta, tt.raw))
			if got != tt.want {
				t.Fatalf("injectThreadFields() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestOutputItemLogEventTypeIncludesItemTypeForAdded(t *testing.T) {
	t.Parallel()

	event := openaiws.ServerEvent{
		Type: openaiws.EventTypeResponseOutputItemAdded,
		Raw:  json.RawMessage(`{"type":"response.output_item.added","item":{"id":"msg_123","type":"message"}}`),
	}

	got := outputItemLogEventType(event)
	if got != "response.output_item.added.message" {
		t.Fatalf("outputItemLogEventType() = %q, want %q", got, "response.output_item.added.message")
	}
}

func TestOutputItemLogEventTypeIncludesItemTypeForDone(t *testing.T) {
	t.Parallel()

	event := openaiws.ServerEvent{
		Type: openaiws.EventTypeResponseOutputItemDone,
		Raw:  json.RawMessage(`{"type":"response.output_item.done","item":{"id":"rs_123","type":"reasoning"}}`),
	}

	got := outputItemLogEventType(event)
	if got != "response.output_item.done.reasoning" {
		t.Fatalf("outputItemLogEventType() = %q, want %q", got, "response.output_item.done.reasoning")
	}
}

func TestOutputItemLogEventTypeFallsBackWithoutItemType(t *testing.T) {
	t.Parallel()

	event := openaiws.ServerEvent{
		Type: openaiws.EventTypeResponseOutputItemAdded,
		Raw:  json.RawMessage(`{"type":"response.output_item.added","item":{"id":"msg_123"}}`),
	}

	got := outputItemLogEventType(event)
	if got != "response.output_item.added" {
		t.Fatalf("outputItemLogEventType() = %q, want %q", got, "response.output_item.added")
	}
}

func TestOutputItemEventSequenceNumber(t *testing.T) {
	t.Parallel()

	event := openaiws.ServerEvent{
		Type: openaiws.EventTypeResponseOutputItemDone,
		Raw:  json.RawMessage(`{"type":"response.output_item.done","sequence_number":3}`),
	}

	got, ok := outputItemEventSequenceNumber(event)
	if !ok {
		t.Fatalf("outputItemEventSequenceNumber() ok = false, want true")
	}
	if got != 3 {
		t.Fatalf("outputItemEventSequenceNumber() = %d, want 3", got)
	}
}

func TestOutputItemSemanticAttrsForDocumentQuery(t *testing.T) {
	t.Parallel()

	attrs := attrsMap(outputItemSemanticAttrs(json.RawMessage(`{
		"type":"function_call",
		"call_id":"call_doc_123",
		"name":"query_document",
		"arguments":"{\"document_id\":42,\"task\":\"Summarize page 1\"}"
	}`)))

	if attrs["call_id"] != "call_doc_123" {
		t.Fatalf("call_id = %v, want call_doc_123", attrs["call_id"])
	}
	if attrs["call_name"] != "query_document" {
		t.Fatalf("call_name = %v, want query_document", attrs["call_name"])
	}
	if attrs["call_kind"] != "document_query" {
		t.Fatalf("call_kind = %v, want document_query", attrs["call_kind"])
	}
	if attrs["document_id"] != int64(42) {
		t.Fatalf("document_id = %v, want 42", attrs["document_id"])
	}
}

func TestOutputItemSemanticAttrsForSpawnThreads(t *testing.T) {
	t.Parallel()

	attrs := attrsMap(outputItemSemanticAttrs(json.RawMessage(`{
		"type":"function_call",
		"call_id":"call_spawn_123",
		"name":"spawn_threads",
		"arguments":"{\"spawn_mode\":\"warm_branch\",\"children\":[{\"prompt\":\"one\"},{\"prompt\":\"two\"}]}"
	}`)))

	if attrs["call_kind"] != "spawn_threads" {
		t.Fatalf("call_kind = %v, want spawn_threads", attrs["call_kind"])
	}
	if attrs["child_count"] != 2 {
		t.Fatalf("child_count = %v, want 2", attrs["child_count"])
	}
	if attrs["spawn_mode"] != "warm_branch" {
		t.Fatalf("spawn_mode = %v, want warm_branch", attrs["spawn_mode"])
	}
}

func TestLogOutputItemEventIncludesThreadGraphAndCallAttrs(t *testing.T) {
	t.Parallel()

	var logBuf bytes.Buffer
	actor := &threadActor{
		logger: slog.New(slog.NewTextHandler(&logBuf, nil)),
	}

	actor.logOutputItemEvent(threadstore.ThreadMeta{
		RootThreadID:   tid("thread_root"),
		ParentThreadID: tid("thread_parent"),
		ParentCallID:   "call_parent",
		Depth:          1,
	}, "resp_123", openaiws.ServerEvent{
		Type: openaiws.EventTypeResponseOutputItemDone,
		Raw: json.RawMessage(`{
			"type":"response.output_item.done",
			"sequence_number":7,
			"item":{
				"type":"function_call",
				"call_id":"call_doc_123",
				"name":"query_document",
				"arguments":"{\"document_id\":7,\"task\":\"Summarize page 1\"}"
			}
		}`),
	})

	logs := logBuf.String()
	for _, needle := range []string{
		`event_type=response.output_item.done.function_call`,
		`response_id=resp_123`,
		`root_thread_id=` + strconv.FormatInt(tid("thread_root"), 10),
		`parent_thread_id=` + strconv.FormatInt(tid("thread_parent"), 10),
		`parent_call_id=call_parent`,
		`depth=1`,
		`call_id=call_doc_123`,
		`call_name=query_document`,
		`call_kind=document_query`,
		`document_id=7`,
	} {
		if !strings.Contains(logs, needle) {
			t.Fatalf("logs = %q, want %s", logs, needle)
		}
	}
}

func attrsMap(attrs []any) map[string]any {
	mapped := make(map[string]any, len(attrs)/2)
	for i := 0; i+1 < len(attrs); i += 2 {
		key, ok := attrs[i].(string)
		if !ok {
			continue
		}
		mapped[key] = attrs[i+1]
	}
	return mapped
}

func TestResponseCreateReasoningEffort(t *testing.T) {
	t.Parallel()

	payload := map[string]any{
		"reasoning": map[string]any{
			"effort": "medium",
		},
	}

	got := responseCreateReasoningEffort(payload)
	if got != "medium" {
		t.Fatalf("responseCreateReasoningEffort() = %q, want %q", got, "medium")
	}
}

func TestResponseCreateReasoningEffortMissing(t *testing.T) {
	t.Parallel()

	got := responseCreateReasoningEffort(map[string]any{})
	if got != "" {
		t.Fatalf("responseCreateReasoningEffort() = %q, want empty", got)
	}
}

func TestResponseCreateReasoningEffortTypedParam(t *testing.T) {
	t.Parallel()

	payload := map[string]any{
		"reasoning": shared.ReasoningParam{
			Effort: shared.ReasoningEffortMedium,
		},
	}

	got := responseCreateReasoningEffort(payload)
	if got != "medium" {
		t.Fatalf("responseCreateReasoningEffort() = %q, want %q", got, "medium")
	}
}
