package agentcmd

import (
	"encoding/json"
	"testing"
)

func TestDecodeDefaultsRootThreadID(t *testing.T) {
	cmd, err := Decode([]byte(`{
		"cmd_id":"cmd_123",
		"kind":"thread.start",
		"thread_id":"thread_123",
		"body":{"model":"gpt-5.4","initial_input":[{"type":"message"}]}
	}`))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}

	if cmd.RootThreadID != cmd.ThreadID {
		t.Fatalf("RootThreadID = %q, want %q", cmd.RootThreadID, cmd.ThreadID)
	}
}

func TestStartBody(t *testing.T) {
	cmd, err := Decode([]byte(`{
		"cmd_id":"cmd_123",
		"kind":"thread.start",
		"thread_id":"thread_123",
		"body":{
			"model":"gpt-5.4",
			"instructions":"be sharp",
			"initial_input":[{"type":"message","role":"user"}],
			"tools":[{"type":"function","name":"spawn_subagents"}],
			"tool_choice":{"type":"function","name":"spawn_subagents"}
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
	if len(tools) != 1 || tools[0]["name"] != "spawn_subagents" {
		t.Fatalf("Tools = %v, want spawn_subagents", tools)
	}

	cmd, err = Decode([]byte(`{
		"cmd_id":"cmd_branch",
		"kind":"thread.start",
		"thread_id":"thread_branch",
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

func TestWorkerCommandWildcard(t *testing.T) {
	if got := WorkerCommandWildcard("worker-a"); got != "agent.worker.worker-a.cmd.>" {
		t.Fatalf("WorkerCommandWildcard() = %q", got)
	}
}

func TestSubmitToolOutputBody(t *testing.T) {
	cmd, err := Decode([]byte(`{
		"cmd_id":"cmd_456",
		"kind":"thread.submit_tool_output",
		"thread_id":"thread_123",
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
		"thread_id":"thread_123",
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
		"thread_id":"thread_parent",
		"body":{
			"spawn_group_id":"sg_123",
			"child_thread_id":"thread_child_1",
			"child_response_id":"resp_child_1",
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

	if body.SpawnGroupID != "sg_123" {
		t.Fatalf("SpawnGroupID = %q, want %q", body.SpawnGroupID, "sg_123")
	}
}

func TestReconcileBody(t *testing.T) {
	cmd, err := Decode([]byte(`{
		"cmd_id":"cmd_recover",
		"kind":"thread.reconcile",
		"thread_id":"thread_123",
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
