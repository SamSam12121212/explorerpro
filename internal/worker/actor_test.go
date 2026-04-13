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
	"strings"
	"sync"
	"testing"
	"time"

	"explorer/internal/agentcmd"
	"explorer/internal/blobstore"
	"explorer/internal/doccmd"
	"explorer/internal/docstore"
	"explorer/internal/openaiws"
	"explorer/internal/preparedinput"
	"explorer/internal/threadstore"

	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
)

func TestWrapRawItemAsArray(t *testing.T) {
	raw, err := wrapRawItemAsArray([]byte(`{"type":"function_call_output","call_id":"call_123"}`))
	if err != nil {
		t.Fatalf("wrapRawItemAsArray() error = %v", err)
	}

	if got := string(raw); got != `[{"type":"function_call_output","call_id":"call_123"}]` {
		t.Fatalf("wrapRawItemAsArray() = %s", got)
	}
}

func TestAggregateSpawnOutputItem(t *testing.T) {
	raw, err := aggregateSpawnOutputItem(threadstore.SpawnGroupMeta{
		ID:           "sg_123",
		ParentCallID: "call_parent",
	}, []threadstore.SpawnChildResult{
		{
			ChildThreadID:   "thread_child_1",
			Status:          "completed",
			ChildResponseID: "resp_1",
			AssistantText:   "Auth paths look healthy.",
			ResultRef:       "blob://child-results/1.json",
		},
		{
			ChildThreadID: "thread_child_2",
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

	if parsed["spawn_group_id"] != "sg_123" {
		t.Fatalf("spawn_group_id = %v, want sg_123", parsed["spawn_group_id"])
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

func TestValidateCommandPreconditions(t *testing.T) {
	t.Parallel()

	meta := threadstore.ThreadMeta{
		ID:               "thread_123",
		Status:           threadstore.ThreadStatusWaitingTool,
		LastResponseID:   "resp_123",
		SocketGeneration: 7,
	}

	if err := validateCommandPreconditions(agentcmd.Command{
		ExpectedStatus:           "waiting_tool",
		ExpectedLastResponseID:   "resp_123",
		ExpectedSocketGeneration: 7,
	}, meta); err != nil {
		t.Fatalf("validateCommandPreconditions() unexpected error: %v", err)
	}

	err := validateCommandPreconditions(agentcmd.Command{
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
		ToolsJSON:      `[{"type":"function","name":"spawn_subagents"}]`,
		ToolChoiceJSON: `{"type":"function","name":"spawn_subagents"}`,
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
			documentsByThread: map[string][]docstore.Document{
				"thread_123": {
					{ID: "doc_1", Filename: "report.pdf"},
				},
			},
		},
		docRuntime: fakeDocRuntimeContextClient(func(_ context.Context, req doccmd.RuntimeContextRequest) (doccmd.RuntimeContextResponse, error) {
			if req.ThreadID != "thread_123" {
				t.Fatalf("ThreadID = %q, want %q", req.ThreadID, "thread_123")
			}
			if req.Instructions != "Be concise." {
				t.Fatalf("Instructions = %q, want %q", req.Instructions, "Be concise.")
			}
			return doccmd.RuntimeContextResponse{
				RequestID:    req.RequestID,
				Status:       doccmd.PrepareStatusOK,
				Instructions: "Be concise.\n\n<available_documents>\n<document id=\"doc_1\" name=\"report.pdf\" />\n</available_documents>",
				Tools:        json.RawMessage(`[{"type":"function","name":"lookup"},{"type":"function","name":"query_attached_documents"}]`),
			}, nil
		}),
	}

	payload := map[string]any{
		"instructions": "Be concise.",
		"tools":        []any{map[string]any{"type": "function", "name": "lookup"}},
	}

	if err := actor.applyDocumentRuntimeContext("thread_123", payload); err != nil {
		t.Fatalf("applyDocumentRuntimeContext() error = %v", err)
	}

	if got := payload["instructions"]; got != "Be concise.\n\n<available_documents>\n<document id=\"doc_1\" name=\"report.pdf\" />\n</available_documents>" {
		t.Fatalf("instructions = %v, want runtime-augmented instructions", got)
	}

	tools, ok := payload["tools"].([]responses.ToolUnionParam)
	if !ok || len(tools) != 2 {
		t.Fatalf("tools = %#v, want 2 tools", payload["tools"])
	}
	if toolParamName(tools[1]) != doccmd.ToolNameQueryAttachedDocuments {
		t.Fatalf("tool name = %q, want %q", toolParamName(tools[1]), doccmd.ToolNameQueryAttachedDocuments)
	}
}

func TestApplyDocumentRuntimeContextSkipsWhenNoDocumentsAttached(t *testing.T) {
	t.Parallel()

	actor := &threadActor{
		ctx: context.Background(),
		threadDocs: &fakeThreadDocumentStore{
			documentsByThread: map[string][]docstore.Document{},
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

	if err := actor.applyDocumentRuntimeContext("thread_123", payload); err != nil {
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
			documentsByThread: map[string][]docstore.Document{
				"thread_123": {
					{ID: "doc_1", Filename: `Quarterly "Report" <Draft>.pdf`},
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

	if err := actor.applyDocumentRuntimeContext("thread_123", payload); err != nil {
		t.Fatalf("applyDocumentRuntimeContext() error = %v", err)
	}

	instructions, _ := payload["instructions"].(string)
	if !strings.Contains(instructions, `<document id="doc_1" name="Quarterly &quot;Report&quot; &lt;Draft&gt;.pdf" />`) {
		t.Fatalf("instructions = %q, want local available_documents block", instructions)
	}

	tools, ok := payload["tools"].([]responses.ToolUnionParam)
	if !ok || len(tools) != 2 {
		t.Fatalf("tools = %#v, want lookup plus document query tool", payload["tools"])
	}
	if toolParamName(tools[1]) != doccmd.ToolNameQueryAttachedDocuments {
		t.Fatalf("tool name = %q, want %q", toolParamName(tools[1]), doccmd.ToolNameQueryAttachedDocuments)
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
			documentsByThread: map[string][]docstore.Document{
				"thread_123": {
					{ID: "doc_1", Filename: "report.pdf"},
				},
			},
		},
		docRuntime: fakeDocRuntimeContextClient(func(_ context.Context, req doccmd.RuntimeContextRequest) (doccmd.RuntimeContextResponse, error) {
			return doccmd.RuntimeContextResponse{
				RequestID:    req.RequestID,
				Status:       doccmd.PrepareStatusOK,
				Instructions: req.Instructions,
				Tools:        json.RawMessage(`[{"type":"function","name":"query_attached_documents"}]`),
			}, nil
		}),
	}

	payload, err := actor.buildThreadResponseCreatePayload(threadstore.ThreadMeta{
		ID: "thread_123",
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
	if toolParamName(tools[0]) != doccmd.ToolNameQueryAttachedDocuments {
		t.Fatalf("tool name = %q, want %q", toolParamName(tools[0]), doccmd.ToolNameQueryAttachedDocuments)
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

func TestFilterSubagentToolsRemovesSpawnTool(t *testing.T) {
	t.Parallel()

	filtered, err := filterSubagentTools(`[{"type":"function","name":"spawn_subagents"},{"type":"function","name":"lookup"}]`)
	if err != nil {
		t.Fatalf("filterSubagentTools() error = %v", err)
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
		ID:             "thread_parent",
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
		"branch_parent_thread_id":   "thread_parent",
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
	if decoded["branch_parent_thread_id"] != "thread_parent" {
		t.Fatalf("branch_parent_thread_id = %v, want thread_parent", decoded["branch_parent_thread_id"])
	}
	if decoded["branch_parent_response_id"] != "resp_parent" {
		t.Fatalf("branch_parent_response_id = %v, want resp_parent", decoded["branch_parent_response_id"])
	}
	if decoded["branch_index"] != "2" {
		t.Fatalf("branch_index = %v, want 2", decoded["branch_index"])
	}
}

func TestFilterSubagentToolChoicePreservesMode(t *testing.T) {
	t.Parallel()

	got, err := filterSubagentToolChoice(`"auto"`)
	if err != nil {
		t.Fatalf("filterSubagentToolChoice() error = %v", err)
	}
	if got != `"auto"` {
		t.Fatalf("filterSubagentToolChoice() = %s, want %s", got, `"auto"`)
	}
}

func TestFilterSubagentToolChoiceDropsInternalFunctionChoices(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
	}{
		{
			name: "spawn subagents",
			raw:  `{"type":"function","name":"spawn_subagents"}`,
		},
		{
			name: "attached documents",
			raw:  `{"type":"function","name":"query_attached_documents"}`,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := filterSubagentToolChoice(tt.raw)
			if err != nil {
				t.Fatalf("filterSubagentToolChoice() error = %v", err)
			}
			if got != "" {
				t.Fatalf("filterSubagentToolChoice() = %s, want empty", got)
			}
		})
	}
}

func TestFilterSubagentToolChoiceFiltersAllowedTools(t *testing.T) {
	t.Parallel()

	got, err := filterSubagentToolChoice(`{
		"type":"allowed_tools",
		"mode":"required",
		"tools":[
			{"type":"function","name":"spawn_subagents"},
			{"type":"function","name":"query_attached_documents"},
			{"type":"function","name":"keep_tool"},
			{"type":"web_search_preview"}
		]
	}`)
	if err != nil {
		t.Fatalf("filterSubagentToolChoice() error = %v", err)
	}
	if got == "" {
		t.Fatal("filterSubagentToolChoice() returned empty result")
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

func TestFilterSubagentToolChoiceDropsEmptyAllowedTools(t *testing.T) {
	t.Parallel()

	got, err := filterSubagentToolChoice(`{
		"type":"allowed_tools",
		"mode":"required",
		"tools":[
			{"type":"function","name":"spawn_subagents"},
			{"type":"function","name":"query_attached_documents"}
		]
	}`)
	if err != nil {
		t.Fatalf("filterSubagentToolChoice() error = %v", err)
	}
	if got != "" {
		t.Fatalf("filterSubagentToolChoice() = %s, want empty", got)
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
		threadID:       "thread_test",
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
		threadID: "thread_test",
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
		documentsByThread: map[string][]docstore.Document{
			"thread_parent": {
				{ID: "doc_1", Filename: "report.pdf"},
			},
		},
	}
	actor.docRuntime = fakeDocRuntimeContextClient(func(_ context.Context, req doccmd.RuntimeContextRequest) (doccmd.RuntimeContextResponse, error) {
		return doccmd.RuntimeContextResponse{
			RequestID:    req.RequestID,
			Status:       doccmd.PrepareStatusOK,
			Instructions: req.Instructions + "\n\n<available_documents>\n<document id=\"doc_1\" name=\"report.pdf\" />\n</available_documents>",
		}, nil
	})

	body, err := json.Marshal(agentcmd.StartBody{
		Model:        "gpt-5.4",
		Instructions: "Base instructions.",
		InitialInput: json.RawMessage(`[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]`),
	})
	if err != nil {
		t.Fatalf("json.Marshal(StartBody) error = %v", err)
	}

	cmd := agentcmd.Command{
		CmdID:        "cmd_start",
		Kind:         agentcmd.KindThreadStart,
		ThreadID:     "thread_parent",
		RootThreadID: "thread_parent",
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
	if !strings.Contains(instructions, `<document id="doc_1" name="report.pdf" />`) {
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

	body, err := json.Marshal(agentcmd.StartBody{
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

	cmd := agentcmd.Command{
		CmdID:        "cmd_start",
		Kind:         agentcmd.KindThreadStart,
		ThreadID:     "thread_parent",
		RootThreadID: "thread_parent",
		Body:         body,
	}

	if err := actor.handleStart(cmd); err != nil {
		t.Fatalf("handleStart() error = %v", err)
	}

	meta := store.threads["thread_parent"]

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
		documentsByThread: map[string][]docstore.Document{
			"thread_parent": {
				{ID: "doc_2", Filename: "followup.pdf"},
			},
		},
	}
	actor.docRuntime = fakeDocRuntimeContextClient(func(_ context.Context, req doccmd.RuntimeContextRequest) (doccmd.RuntimeContextResponse, error) {
		return doccmd.RuntimeContextResponse{
			RequestID:    req.RequestID,
			Status:       doccmd.PrepareStatusOK,
			Instructions: req.Instructions + "\n\n<available_documents>\n<document id=\"doc_2\" name=\"followup.pdf\" />\n</available_documents>",
		}, nil
	})

	meta := threadstore.ThreadMeta{
		ID:               "thread_parent",
		Model:            "gpt-5.4",
		Instructions:     "Base instructions.",
		LastResponseID:   "resp_prev",
		SocketGeneration: 1,
	}

	if err := actor.continueWithInputItems(meta, "cmd_resume", json.RawMessage(`[{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}]`)); err != nil {
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
	if !strings.Contains(instructions, `<document id="doc_2" name="followup.pdf" />`) {
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

	body, err := json.Marshal(agentcmd.StartBody{
		Model:            "gpt-5.4",
		PreparedInputRef: ref,
	})
	if err != nil {
		t.Fatalf("json.Marshal(StartBody) error = %v", err)
	}

	cmd := agentcmd.Command{
		CmdID:        "cmd_start_prepared",
		Kind:         agentcmd.KindThreadStart,
		ThreadID:     "thread_parent",
		RootThreadID: "thread_parent",
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
	if raw := string(store.latestClientCreateByID["thread_parent"]); !strings.Contains(raw, ref) {
		t.Fatalf("saved checkpoint = %s, want prepared_input_ref %q", raw, ref)
	}
}

func TestHandleResumeUsesPreparedInputRefForSend(t *testing.T) {
	t.Parallel()

	store := newFakeActorStore(t)
	store.threads["thread_parent"] = threadstore.ThreadMeta{
		ID:             "thread_parent",
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

	body, err := json.Marshal(agentcmd.ResumeBody{
		PreparedInputRef: ref,
	})
	if err != nil {
		t.Fatalf("json.Marshal(ResumeBody) error = %v", err)
	}

	cmd := agentcmd.Command{
		CmdID:        "cmd_resume_prepared",
		Kind:         agentcmd.KindThreadResume,
		ThreadID:     "thread_parent",
		RootThreadID: "thread_parent",
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
	if raw := string(store.latestClientCreateByID["thread_parent"]); !strings.Contains(raw, ref) {
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
	store.threads["thread_retry"] = threadstore.ThreadMeta{
		ID:               "thread_retry",
		Status:           threadstore.ThreadStatusRunning,
		OwnerWorkerID:    "worker-local-1",
		SocketGeneration: 3,
		SocketExpiresAt:  time.Now().UTC().Add(time.Minute),
		ActiveResponseID: "resp_active",
	}

	conn := &actorTestConn{}
	actor := newActorRecoveryHarness(t, store, conn)
	actor.threadID = "thread_retry"
	actor.setMeta(store.threads["thread_retry"])

	if err := actor.failThreadAfterRetryExhaustion("thread_retry"); err != nil {
		t.Fatalf("failThreadAfterRetryExhaustion() error = %v", err)
	}

	meta := store.threads["thread_retry"]
	if meta.Status != threadstore.ThreadStatusFailed {
		t.Fatalf("status = %s, want failed", meta.Status)
	}
	if meta.ActiveResponseID != "" {
		t.Fatalf("active response id = %q, want empty", meta.ActiveResponseID)
	}
	if meta.OwnerWorkerID != "" {
		t.Fatalf("owner worker id = %q, want empty", meta.OwnerWorkerID)
	}
	if !meta.SocketExpiresAt.IsZero() {
		t.Fatalf("socket expires at = %s, want zero", meta.SocketExpiresAt)
	}
	if len(store.releasedThreads) != 1 || store.releasedThreads[0] != "thread_retry" {
		t.Fatalf("releasedThreads = %#v, want [thread_retry]", store.releasedThreads)
	}
	if !conn.closed {
		t.Fatal("expected session to be closed")
	}
}

func TestHandleDisconnectSocketClosesSessionAndReleasesOwnership(t *testing.T) {
	t.Parallel()

	store := newFakeActorStore(t)
	store.threads["thread_idle"] = threadstore.ThreadMeta{
		ID:               "thread_idle",
		Status:           threadstore.ThreadStatusReady,
		OwnerWorkerID:    "worker-local-1",
		SocketGeneration: 5,
		SocketExpiresAt:  time.Now().UTC().Add(time.Minute),
	}

	conn := &actorTestConn{}
	actor := newActorRecoveryHarness(t, store, conn)
	actor.threadID = "thread_idle"
	actor.setMeta(store.threads["thread_idle"])

	cmd := agentcmd.Command{
		CmdID:    "cmd_disconnect_1",
		Kind:     agentcmd.KindThreadDisconnectSocket,
		ThreadID: "thread_idle",
	}

	if err := actor.handleDisconnectSocket(cmd); err != nil {
		t.Fatalf("handleDisconnectSocket() error = %v", err)
	}

	if !conn.closed {
		t.Fatal("expected session to be closed")
	}
	if len(store.releasedThreads) != 1 || store.releasedThreads[0] != "thread_idle" {
		t.Fatalf("releasedThreads = %#v, want [thread_idle]", store.releasedThreads)
	}

	meta := store.threads["thread_idle"]
	if meta.Status != threadstore.ThreadStatusReady {
		t.Fatalf("status = %s, want ready (unchanged)", meta.Status)
	}
}

func TestHandleDisconnectSocketRejectsNonIdleThread(t *testing.T) {
	t.Parallel()

	store := newFakeActorStore(t)
	store.threads["thread_running"] = threadstore.ThreadMeta{
		ID:               "thread_running",
		Status:           threadstore.ThreadStatusRunning,
		OwnerWorkerID:    "worker-local-1",
		SocketGeneration: 3,
	}

	actor := newActorRecoveryHarness(t, store, nil)
	actor.threadID = "thread_running"
	actor.setMeta(store.threads["thread_running"])

	cmd := agentcmd.Command{
		CmdID:    "cmd_disconnect_2",
		Kind:     agentcmd.KindThreadDisconnectSocket,
		ThreadID: "thread_running",
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
	store.threads["thread_nosess"] = threadstore.ThreadMeta{
		ID:               "thread_nosess",
		Status:           threadstore.ThreadStatusReady,
		OwnerWorkerID:    "worker-local-1",
		SocketGeneration: 2,
	}

	actor := newActorRecoveryHarness(t, store, nil)
	actor.threadID = "thread_nosess"
	actor.setMeta(store.threads["thread_nosess"])

	cmd := agentcmd.Command{
		CmdID:    "cmd_disconnect_3",
		Kind:     agentcmd.KindThreadDisconnectSocket,
		ThreadID: "thread_nosess",
	}

	if err := actor.handleDisconnectSocket(cmd); err != nil {
		t.Fatalf("handleDisconnectSocket() error = %v", err)
	}

	if len(store.releasedThreads) != 1 || store.releasedThreads[0] != "thread_nosess" {
		t.Fatalf("releasedThreads = %#v, want [thread_nosess]", store.releasedThreads)
	}
}

func TestShouldReleaseTerminalChildResources(t *testing.T) {
	t.Parallel()

	if !shouldReleaseTerminalChildResources(threadstore.ThreadMeta{
		ParentThreadID: "thread_parent",
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
		ParentThreadID: "thread_parent",
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
	store.threads["thread_parent"] = threadstore.ThreadMeta{
		ID:            "thread_parent",
		RootThreadID:  "thread_root",
		OwnerWorkerID: "worker-parent",
	}
	store.appendedItems = append(store.appendedItems, threadstore.ItemLogEntry{
		ThreadID:   "thread_child",
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
		publishedCmd     agentcmd.Command
	)

	actor := newActorRecoveryHarness(t, store, nil)
	actor.publish = func(_ context.Context, subject string, cmd agentcmd.Command) error {
		publishedSubject = subject
		publishedCmd = cmd
		return nil
	}

	err := actor.publishChildTerminal(threadstore.ThreadMeta{
		ID:                 "thread_child",
		ParentThreadID:     "thread_parent",
		ActiveSpawnGroupID: "sg_123",
		LastResponseID:     "resp_child_1",
		Status:             threadstore.ThreadStatusCompleted,
	})
	if err != nil {
		t.Fatalf("publishChildTerminal() error = %v", err)
	}

	if publishedSubject != agentcmd.WorkerCommandSubject("worker-parent", agentcmd.KindThreadChildCompleted) {
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

func TestHandleChildResultUsesAssistantTextFromCommand(t *testing.T) {
	t.Parallel()

	store := newFakeActorStore(t)
	store.threads["thread_parent"] = threadstore.ThreadMeta{
		ID:                 "thread_parent",
		Status:             threadstore.ThreadStatusWaitingChildren,
		ActiveSpawnGroupID: "sg_waiting",
	}
	store.spawnGroups["sg_waiting"] = threadstore.SpawnGroupMeta{
		ID:             "sg_waiting",
		ParentThreadID: "thread_parent",
		ParentCallID:   "call_parent",
		Expected:       2,
		Status:         threadstore.SpawnGroupStatusWaiting,
	}

	actor := newActorRecoveryHarness(t, store, nil)

	cmd := agentcmd.Command{
		CmdID:    "cmd_child_completed",
		Kind:     agentcmd.KindThreadChildCompleted,
		ThreadID: "thread_parent",
		Body: json.RawMessage(`{
			"spawn_group_id":"sg_waiting",
			"child_thread_id":"thread_child_1",
			"child_response_id":"resp_child_1",
			"assistant_text":"Direct child summary",
			"status":"completed"
		}`),
	}

	if err := actor.handleChildResult(cmd, "completed"); err != nil {
		t.Fatalf("handleChildResult() error = %v", err)
	}

	results := store.spawnResults["sg_waiting"]
	if len(results) != 1 {
		t.Fatalf("spawnResults = %d, want 1", len(results))
	}
	if results[0].AssistantText != "Direct child summary" {
		t.Fatalf("AssistantText = %q, want %q", results[0].AssistantText, "Direct child summary")
	}
}

func testOpenAIConfig() openaiws.Config {
	return openaiws.Config{
		APIKey:             "test-key",
		BaseURL:            "https://api.openai.com/v1",
		ResponsesSocketURL: "wss://api.openai.com/v1/responses",
		DialTimeout:        50 * time.Millisecond,
		ReadTimeout:        50 * time.Millisecond,
		WriteTimeout:       50 * time.Millisecond,
		PingInterval:       50 * time.Millisecond,
		MaxMessageBytes:    1024,
	}
}

func testActorLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type actorTestDialer struct {
	conn openaiws.Conn
}

type fakeThreadDocumentStore struct {
	documentsByThread map[string][]docstore.Document
	err               error
}

func (s *fakeThreadDocumentStore) ListDocuments(_ context.Context, threadID string, _ int64) ([]docstore.Document, error) {
	if s.err != nil {
		return nil, s.err
	}

	documents := s.documentsByThread[threadID]
	cloned := make([]docstore.Document, len(documents))
	copy(cloned, documents)
	return cloned, nil
}

func (s *fakeThreadDocumentStore) FilterAttached(_ context.Context, threadID string, documentIDs []string) ([]string, error) {
	if s.err != nil {
		return nil, s.err
	}

	docs := s.documentsByThread[threadID]
	idSet := make(map[string]bool, len(docs))
	for _, d := range docs {
		idSet[d.ID] = true
	}

	attached := make([]string, 0, len(documentIDs))
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
	docs map[string]docstore.Document
}

func (s *fakeDocActorDocStore) Get(_ context.Context, id string) (docstore.Document, error) {
	doc, ok := s.docs[id]
	if !ok {
		return docstore.Document{}, docstore.ErrDocumentNotFound
	}
	return doc, nil
}

type fakeDocActorPreparedInputClient struct {
	ref string
	err error
}

func (c *fakeDocActorPreparedInputClient) PrepareInput(_ context.Context, _ doccmd.PrepareInputRequest) (doccmd.PrepareInputResponse, error) {
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
			documentsByThread: map[string][]docstore.Document{
				"thread_123": {
					{ID: "doc_1", Filename: "report.pdf"},
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

	if err := actor.applyDocumentRuntimeContext("thread_123", payload); err != nil {
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

	req, err := decodeDocQueryRequest(`{"document_ids":["doc_1","doc_2"],"task":"summarize findings"}`)
	if err != nil {
		t.Fatalf("decodeDocQueryRequest() error = %v", err)
	}
	if len(req.DocumentIDs) != 2 {
		t.Fatalf("document_ids count = %d, want 2", len(req.DocumentIDs))
	}
	if req.Task != "summarize findings" {
		t.Fatalf("task = %q, want %q", req.Task, "summarize findings")
	}
}

func TestDecodeDocQueryRequestRejectsEmptyDocumentIDs(t *testing.T) {
	t.Parallel()

	_, err := decodeDocQueryRequest(`{"document_ids":[],"task":"summarize"}`)
	if err == nil {
		t.Fatal("expected error for empty document_ids")
	}
}

func TestDecodeDocQueryRequestRejectsEmptyTask(t *testing.T) {
	t.Parallel()

	_, err := decodeDocQueryRequest(`{"document_ids":["doc_1"],"task":""}`)
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
			documentsByThread: map[string][]docstore.Document{
				"thread_123": {{ID: "doc_1"}},
			},
		},
		store: store,
	}

	meta := threadstore.ThreadMeta{ID: "thread_123", RootThreadID: "thread_123"}
	_, err := actor.startDocumentQueryGroup(meta, "call_1", docQueryRequest{
		DocumentIDs: []string{"doc_1", "doc_missing"},
		Task:        "summarize",
	})
	if err == nil {
		t.Fatal("expected error for unattached document")
	}
	if !strings.Contains(err.Error(), "doc_missing") {
		t.Fatalf("error = %v, want mention of doc_missing", err)
	}
}

func TestFilterSubagentToolsRemovesDocumentQueryTool(t *testing.T) {
	t.Parallel()

	filtered, err := filterSubagentTools(`[{"type":"function","name":"query_attached_documents"},{"type":"function","name":"lookup"}]`)
	if err != nil {
		t.Fatalf("filterSubagentTools() error = %v", err)
	}

	var tools []map[string]any
	if err := json.Unmarshal([]byte(filtered), &tools); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	if len(tools) != 1 || tools[0]["name"] != "lookup" {
		t.Fatalf("tools = %v, want only lookup", tools)
	}
}

func TestStartDocumentQueryGroupUsesLatestCompletedDocumentChildLineage(t *testing.T) {
	t.Parallel()

	store := newFakeActorStore(t)
	store.threads["thread_parent"] = threadstore.ThreadMeta{
		ID:           "thread_parent",
		RootThreadID: "thread_parent",
	}
	store.threads["thread_doc_old"] = threadstore.ThreadMeta{
		ID:             "thread_doc_old",
		RootThreadID:   "thread_parent",
		ParentThreadID: "thread_parent",
		Status:         threadstore.ThreadStatusCompleted,
		Model:          "gpt-5.4-mini",
		LastResponseID: "resp_doc_old",
		MetadataJSON:   `{"document_id":"doc_1","spawn_mode":"document_query"}`,
		CreatedAt:      time.Date(2026, 4, 13, 10, 0, 0, 0, time.UTC),
		UpdatedAt:      time.Date(2026, 4, 13, 10, 1, 0, 0, time.UTC),
	}

	var publishedCommands []agentcmd.Command
	actor := &threadActor{
		ctx:    context.Background(),
		logger: testActorLogger(),
		store:  store,
		threadDocs: &fakeThreadDocumentStore{
			documentsByThread: map[string][]docstore.Document{
				"thread_parent": {{ID: "doc_1", Filename: "report.pdf"}},
			},
		},
		docStore: &fakeDocActorDocStore{
			docs: map[string]docstore.Document{
				"doc_1": {
					ID:         "doc_1",
					Filename:   "report.pdf",
					QueryModel: "gpt-5.4-nano",
				},
			},
		},
		preparedInputs: &fakeDocActorPreparedInputClient{ref: "blob://prepared/unused"},
		publish: func(_ context.Context, _ string, cmd agentcmd.Command) error {
			publishedCommands = append(publishedCommands, cmd)
			return nil
		},
	}

	meta := store.threads["thread_parent"]
	if _, err := actor.startDocumentQueryGroup(meta, "call_1", docQueryRequest{
		DocumentIDs: []string{"doc_1"},
		Task:        "summarize",
	}); err != nil {
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

func TestStreamUntilTerminalSpawnsDocumentQueryChildren(t *testing.T) {
	t.Parallel()

	store := newFakeActorStore(t)
	store.threads["thread_parent"] = threadstore.ThreadMeta{
		ID:               "thread_parent",
		RootThreadID:     "thread_parent",
		Status:           threadstore.ThreadStatusRunning,
		Model:            "gpt-5.4",
		OwnerWorkerID:    "worker-local-1",
		SocketGeneration: 1,
	}

	funcCallItem := `{
		"type":"function_call",
		"call_id":"call_doc_1",
		"name":"query_attached_documents",
		"arguments":"{\"document_ids\":[\"doc_1\"],\"task\":\"summarize\"}"
	}`
	conn := &actorTestConn{
		reads: [][]byte{
			[]byte(`{"type":"response.created","response":{"id":"resp_1"}}`),
			[]byte(`{"type":"response.output_item.done","response":{"id":"resp_1"},"item":` + funcCallItem + `}`),
			[]byte(`{"type":"response.completed","response":{"id":"resp_1"}}`),
		},
	}
	var publishedCommands []agentcmd.Command
	actor := newActorRecoveryHarness(t, store, conn)
	actor.threadDocs = &fakeThreadDocumentStore{
		documentsByThread: map[string][]docstore.Document{
			"thread_parent": {{ID: "doc_1", Filename: "report.pdf"}},
		},
	}
	actor.docStore = &fakeDocActorDocStore{
		docs: map[string]docstore.Document{
			"doc_1": {ID: "doc_1", Filename: "report.pdf", QueryModel: "gpt-5.4", ManifestRef: "blob://manifest"},
		},
	}
	actor.preparedInputs = &fakeDocActorPreparedInputClient{ref: "blob://prepared/test"}
	actor.publish = func(_ context.Context, _ string, cmd agentcmd.Command) error {
		publishedCommands = append(publishedCommands, cmd)
		return nil
	}

	meta := store.threads["thread_parent"]
	if err := actor.streamUntilTerminal(meta); err != nil {
		t.Fatalf("streamUntilTerminal() error = %v", err)
	}

	final := store.threads["thread_parent"]
	if final.Status != threadstore.ThreadStatusWaitingChildren {
		t.Fatalf("final status = %q, want waiting_children", final.Status)
	}
	if final.ActiveSpawnGroupID == "" {
		t.Fatal("expected active spawn group ID to be set")
	}

	if len(publishedCommands) != 1 {
		t.Fatalf("publishedCommands = %d, want 1", len(publishedCommands))
	}
	cmd := publishedCommands[0]
	if cmd.Kind != agentcmd.KindThreadStart {
		t.Fatalf("command kind = %q, want %q", cmd.Kind, agentcmd.KindThreadStart)
	}
}

func TestStreamUntilTerminalKeepsDeltaEventsOutOfHistory(t *testing.T) {
	t.Parallel()

	store := newFakeActorStore(t)
	store.threads["thread_parent"] = threadstore.ThreadMeta{
		ID:               "thread_parent",
		Status:           threadstore.ThreadStatusRunning,
		OwnerWorkerID:    "worker-local-1",
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
	actor.publishEvent = func(_ context.Context, _ string, _ uint64, _ string, eventType string, _ json.RawMessage) error {
		publishedEventTypes = append(publishedEventTypes, eventType)
		return nil
	}

	if err := actor.streamUntilTerminal(store.threads["thread_parent"]); err != nil {
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

func TestFlushDeltaLogForDoneEventLogsOnlySuppressedLastDelta(t *testing.T) {
	t.Parallel()

	var logBuf bytes.Buffer
	actor := &threadActor{
		logger: slog.New(slog.NewTextHandler(&logBuf, nil)),
	}

	deltaLogs := map[openaiws.EventType]*deltaLogState{
		openaiws.EventType("response.function_call_arguments.delta"): {
			firstRaw:      `{"type":"response.function_call_arguments.delta","delta":"{"}`,
			lastRaw:       `{"type":"response.function_call_arguments.delta","delta":"}"}`,
			firstLogged:   true,
			suppressedAny: true,
		},
	}

	actor.flushDeltaLogForDoneEvent(deltaLogs, openaiws.EventTypeResponseFunctionArgsDone)

	if len(deltaLogs) != 0 {
		t.Fatalf("deltaLogs still has %d entries, want 0", len(deltaLogs))
	}
	logs := logBuf.String()
	if !strings.Contains(logs, `event_type=response.function_call_arguments.delta`) {
		t.Fatalf("logs = %q, want flushed delta event type", logs)
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

	actor.flushDeltaLogForDoneEvent(deltaLogs, openaiws.EventTypeResponseOutputTextDone)

	if len(deltaLogs) != 0 {
		t.Fatalf("deltaLogs still has %d entries, want 0", len(deltaLogs))
	}
	if got := strings.TrimSpace(logBuf.String()); got != "" {
		t.Fatalf("logs = %q, want empty", got)
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
