package threadcmd

import (
	"encoding/json"
	"testing"
)

func TestDecodeDefaultsRootThreadID(t *testing.T) {
	cmd, err := Decode([]byte(`{
		"cmd_id":"cmd_123",
		"kind":"thread.start",
		"thread_id":123,
		"body":{"model":"gpt-5.4","initial_input":[{"type":"message"}]}
	}`))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}

	if cmd.RootThreadID != cmd.ThreadID {
		t.Fatalf("RootThreadID = %d, want %d", cmd.RootThreadID, cmd.ThreadID)
	}
}

func TestStartBody(t *testing.T) {
	cmd, err := Decode([]byte(`{
		"cmd_id":"cmd_123",
		"kind":"thread.start",
		"thread_id":123,
		"body":{
			"model":"gpt-5.4",
			"instructions":"be sharp",
			"initial_input":[{"type":"message","role":"user"}],
			"tools":[{"type":"function","name":"spawn_threads"}],
			"tool_choice":{"type":"function","name":"spawn_threads"}
		}
	}`))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}

	body, err := cmd.StartBody()
	if err != nil {
		t.Fatalf("StartBody() error = %v", err)
	}

	if body.Model != "gpt-5.4" {
		t.Fatalf("Model = %q, want %q", body.Model, "gpt-5.4")
	}

	var tools []map[string]any
	if err := json.Unmarshal(body.Tools, &tools); err != nil {
		t.Fatalf("json.Unmarshal(body.Tools) error = %v", err)
	}
	if len(tools) != 1 || tools[0]["name"] != "spawn_threads" {
		t.Fatalf("Tools = %v, want spawn_threads", tools)
	}

	cmd, err = Decode([]byte(`{
		"cmd_id":"cmd_branch",
		"kind":"thread.start",
		"thread_id":456,
		"body":{
			"model":"gpt-5.4",
			"initial_input":[{"type":"message","role":"user"}],
			"store":true,
			"previous_response_id":"resp_parent_123"
		}
	}`))
	if err != nil {
		t.Fatalf("Decode() branch error = %v", err)
	}

	body, err = cmd.StartBody()
	if err != nil {
		t.Fatalf("StartBody() branch error = %v", err)
	}
	if body.PreviousResponseID != "resp_parent_123" {
		t.Fatalf("PreviousResponseID = %q, want %q", body.PreviousResponseID, "resp_parent_123")
	}
}

func TestStartBodyAcceptsPreparedInputRef(t *testing.T) {
	cmd, err := Decode([]byte(`{
		"cmd_id":"cmd_prepared",
		"kind":"thread.start",
		"thread_id":789,
		"body":{
			"model":"gpt-5.4",
			"prepared_input_ref":"blob://prepared-inputs/pi_123.json"
		}
	}`))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}

	body, err := cmd.StartBody()
	if err != nil {
		t.Fatalf("StartBody() error = %v", err)
	}

	if body.PreparedInputRef != "blob://prepared-inputs/pi_123.json" {
		t.Fatalf("PreparedInputRef = %q, want %q", body.PreparedInputRef, "blob://prepared-inputs/pi_123.json")
	}
}

func TestResumeBodyAcceptsPreparedInputRef(t *testing.T) {
	cmd, err := Decode([]byte(`{
		"cmd_id":"cmd_resume_prepared",
		"kind":"thread.resume",
		"thread_id":123,
		"body":{
			"prepared_input_ref":"blob://prepared-inputs/pi_456.json"
		}
	}`))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}

	body, err := cmd.ResumeBody()
	if err != nil {
		t.Fatalf("ResumeBody() error = %v", err)
	}

	if body.PreparedInputRef != "blob://prepared-inputs/pi_456.json" {
		t.Fatalf("PreparedInputRef = %q, want %q", body.PreparedInputRef, "blob://prepared-inputs/pi_456.json")
	}
}

func TestWorkerCommandWildcard(t *testing.T) {
	if got := WorkerCommandWildcard("worker-a"); got != "thread.worker.worker-a.cmd.>" {
		t.Fatalf("WorkerCommandWildcard() = %q", got)
	}
}

func TestSubmitToolOutputBody(t *testing.T) {
	cmd, err := Decode([]byte(`{
		"cmd_id":"cmd_456",
		"kind":"thread.submit_tool_output",
		"thread_id":123,
		"body":{
			"call_id":"call_123",
			"output_item":{
				"type":"function_call_output",
				"call_id":"call_123",
				"output":{"status":"ok"}
			}
		}
	}`))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}

	body, err := cmd.SubmitToolOutputBody()
	if err != nil {
		t.Fatalf("SubmitToolOutputBody() error = %v", err)
	}

	if body.CallID != "call_123" {
		t.Fatalf("CallID = %q, want %q", body.CallID, "call_123")
	}
}

func TestRotateSocketBody(t *testing.T) {
	cmd, err := Decode([]byte(`{
		"cmd_id":"cmd_rotate",
		"kind":"thread.rotate_socket",
		"thread_id":123,
		"body":{
			"reason":"pre_expiry_rotation",
			"scheduled_at":"2026-03-13T19:20:00Z"
		}
	}`))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}

	body, err := cmd.RotateSocketBody()
	if err != nil {
		t.Fatalf("RotateSocketBody() error = %v", err)
	}

	if body.Reason != "pre_expiry_rotation" {
		t.Fatalf("Reason = %q, want %q", body.Reason, "pre_expiry_rotation")
	}
}

func TestChildResultBody(t *testing.T) {
	cmd, err := Decode([]byte(`{
		"cmd_id":"cmd_789",
		"kind":"thread.child_completed",
		"thread_id":100,
		"body":{
			"spawn_group_id":123,
			"child_thread_id":101,
			"child_response_id":"resp_child_1",
			"assistant_text":"Direct child summary",
			"result_ref":"blob://child-results/1.json"
		}
	}`))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}

	body, err := cmd.ChildResultBody()
	if err != nil {
		t.Fatalf("ChildResultBody() error = %v", err)
	}

	if body.SpawnGroupID != 123 {
		t.Fatalf("SpawnGroupID = %d, want %d", body.SpawnGroupID, 123)
	}
	if body.AssistantText != "Direct child summary" {
		t.Fatalf("AssistantText = %q, want %q", body.AssistantText, "Direct child summary")
	}
}

func TestInputKind(t *testing.T) {
	tests := []struct {
		name             string
		inputItems       json.RawMessage
		preparedInputRef string
		want             string
	}{
		{
			name:             "prepared input",
			preparedInputRef: "blob://prepared-inputs/pi_123.json",
			want:             "prepared_input",
		},
		{
			name:       "user message",
			inputItems: json.RawMessage(`[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]`),
			want:       "user_message",
		},
		{
			name:       "function call output",
			inputItems: json.RawMessage(`[{"type":"function_call_output","call_id":"call_123","output":{"ok":true}}]`),
			want:       "function_call_output",
		},
		{
			name: "none",
			want: "none",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := InputKind(tt.inputItems, tt.preparedInputRef); got != tt.want {
				t.Fatalf("InputKind() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLogAttrs(t *testing.T) {
	t.Run("start command", func(t *testing.T) {
		cmd, err := Decode([]byte(`{
			"cmd_id":"cmd_start",
			"kind":"thread.start",
			"thread_id":123,
			"root_thread_id":999,
			"body":{
				"model":"gpt-5.4",
				"initial_input":[{"type":"message","role":"user"}],
				"previous_response_id":"resp_prev"
			}
		}`))
		if err != nil {
			t.Fatalf("Decode() error = %v", err)
		}

		attrs := attrsToMap(LogAttrs(cmd))
		if attrs["cmd_id"] != "cmd_start" {
			t.Fatalf("cmd_id = %v, want cmd_start", attrs["cmd_id"])
		}
		if attrs["root_thread_id"] != int64(999) {
			t.Fatalf("root_thread_id = %v, want 999", attrs["root_thread_id"])
		}
		if attrs["input_kind"] != "user_message" {
			t.Fatalf("input_kind = %v, want user_message", attrs["input_kind"])
		}
		if attrs["has_previous_response_id"] != true {
			t.Fatalf("has_previous_response_id = %v, want true", attrs["has_previous_response_id"])
		}
	})

	t.Run("child result command", func(t *testing.T) {
		cmd, err := Decode([]byte(`{
			"cmd_id":"cmd_child",
			"kind":"thread.child_completed",
			"thread_id":100,
			"root_thread_id":999,
			"causation_id":"resp_child",
			"body":{
				"spawn_group_id":123,
				"child_thread_id":101,
				"child_response_id":"resp_child",
				"status":"completed"
			}
		}`))
		if err != nil {
			t.Fatalf("Decode() error = %v", err)
		}

		attrs := attrsToMap(LogAttrs(cmd))
		if attrs["spawn_group_id"] != int64(123) {
			t.Fatalf("spawn_group_id = %v, want 123", attrs["spawn_group_id"])
		}
		if attrs["child_thread_id"] != int64(101) {
			t.Fatalf("child_thread_id = %v, want 101", attrs["child_thread_id"])
		}
		if attrs["child_response_id"] != "resp_child" {
			t.Fatalf("child_response_id = %v, want resp_child", attrs["child_response_id"])
		}
		if attrs["child_status"] != "completed" {
			t.Fatalf("child_status = %v, want completed", attrs["child_status"])
		}
	})
}

func attrsToMap(attrs []any) map[string]any {
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

func TestReconcileBody(t *testing.T) {
	cmd, err := Decode([]byte(`{
		"cmd_id":"cmd_recover",
		"kind":"thread.reconcile",
		"thread_id":123,
		"body":{
			"previous_worker_id":"worker_old",
			"required_generation":7
		}
	}`))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}

	body, err := cmd.ReconcileBody()
	if err != nil {
		t.Fatalf("ReconcileBody() error = %v", err)
	}

	if body.RequiredGeneration != 7 {
		t.Fatalf("RequiredGeneration = %d, want 7", body.RequiredGeneration)
	}
}

func TestNormalizeIncludeAddsEncryptedContent(t *testing.T) {
	t.Parallel()

	raw, err := NormalizeInclude(nil)
	if err != nil {
		t.Fatalf("NormalizeInclude(nil) error = %v", err)
	}

	var include []string
	if err := json.Unmarshal(raw, &include); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if len(include) != 1 || include[0] != RequiredIncludeReasoningEncryptedContent {
		t.Fatalf("include = %v, want [%q]", include, RequiredIncludeReasoningEncryptedContent)
	}

	raw, err = NormalizeInclude(json.RawMessage(`["message.output_text.logprobs"]`))
	if err != nil {
		t.Fatalf("NormalizeInclude(custom) error = %v", err)
	}
	if err := json.Unmarshal(raw, &include); err != nil {
		t.Fatalf("json.Unmarshal(custom) error = %v", err)
	}
	if len(include) != 2 {
		t.Fatalf("include len = %d, want 2", len(include))
	}
	if include[0] != "message.output_text.logprobs" || include[1] != RequiredIncludeReasoningEncryptedContent {
		t.Fatalf("include = %v, want custom include plus required encrypted content", include)
	}
}

func TestNormalizeMetadataCanonicalizesValuesToStrings(t *testing.T) {
	t.Parallel()

	raw, err := NormalizeMetadata(json.RawMessage(`{
		"tenant":"acme",
		"branch_index":2,
		"enabled":true,
		"filters":{"region":"eu"}
	}`))
	if err != nil {
		t.Fatalf("NormalizeMetadata() error = %v", err)
	}

	var metadata map[string]string
	if err := json.Unmarshal(raw, &metadata); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	if metadata["tenant"] != "acme" {
		t.Fatalf("tenant = %q, want acme", metadata["tenant"])
	}
	if metadata["branch_index"] != "2" {
		t.Fatalf("branch_index = %q, want 2", metadata["branch_index"])
	}
	if metadata["enabled"] != "true" {
		t.Fatalf("enabled = %q, want true", metadata["enabled"])
	}
	if metadata["filters"] != `{"region":"eu"}` {
		t.Fatalf("filters = %q, want %q", metadata["filters"], `{"region":"eu"}`)
	}
}

func TestNormalizeReasoningStripsSummaryFields(t *testing.T) {
	t.Parallel()

	raw, err := NormalizeReasoning(json.RawMessage(`{"effort":"high","summary":"detailed","generate_summary":"concise"}`))
	if err != nil {
		t.Fatalf("NormalizeReasoning() error = %v", err)
	}

	var reasoning map[string]any
	if err := json.Unmarshal(raw, &reasoning); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
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

func TestNormalizeToolChoicePreservesStringMode(t *testing.T) {
	t.Parallel()

	raw, err := NormalizeToolChoice(json.RawMessage(`"required"`))
	if err != nil {
		t.Fatalf("NormalizeToolChoice() error = %v", err)
	}

	if string(raw) != `"required"` {
		t.Fatalf("NormalizeToolChoice() = %s, want %s", raw, `"required"`)
	}
}

func TestNormalizeToolsPreservesSupportedTools(t *testing.T) {
	t.Parallel()

	raw, err := NormalizeTools(json.RawMessage(`[
		{"type":"function","name":"lookup","parameters":{"type":"object"},"strict":true},
		{"type":"web_search_preview"}
	]`))
	if err != nil {
		t.Fatalf("NormalizeTools() error = %v", err)
	}

	var tools []map[string]any
	if err := json.Unmarshal(raw, &tools); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	if len(tools) != 2 {
		t.Fatalf("len(tools) = %d, want 2", len(tools))
	}
	if tools[0]["name"] != "lookup" {
		t.Fatalf("tools[0].name = %v, want lookup", tools[0]["name"])
	}
	if tools[1]["type"] != "web_search_preview" {
		t.Fatalf("tools[1].type = %v, want web_search_preview", tools[1]["type"])
	}
}

func TestNormalizeToolsRejectsUnknownTool(t *testing.T) {
	t.Parallel()

	if _, err := NormalizeTools(json.RawMessage(`[{"type":"definitely_not_a_tool"}]`)); err == nil {
		t.Fatal("expected NormalizeTools() to reject unknown tool")
	}
}
