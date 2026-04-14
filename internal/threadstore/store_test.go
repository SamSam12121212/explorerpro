package threadstore

import (
	"encoding/json"
	"testing"
	"time"
)

func TestSpawnChildResultJSONIncludesAssistantText(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 4, 13, 12, 0, 0, 0, time.UTC)
	result := SpawnChildResult{
		ChildThreadID:   101,
		Status:          "completed",
		ChildResponseID: "resp_child_1",
		AssistantText:   "Child summary",
		ResultRef:       "blob://child-results/1.json",
		SummaryRef:      "blob://child-results/1-summary.json",
		ErrorRef:        "blob://child-results/1-error.json",
		UpdatedAt:       now,
	}

	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	if decoded["child_thread_id"] != float64(101) {
		t.Fatalf("child_thread_id = %v, want 101", decoded["child_thread_id"])
	}
	if decoded["assistant_text"] != "Child summary" {
		t.Fatalf("assistant_text = %v, want Child summary", decoded["assistant_text"])
	}
	if decoded["summary_ref"] != "blob://child-results/1-summary.json" {
		t.Fatalf("summary_ref = %v, want blob://child-results/1-summary.json", decoded["summary_ref"])
	}
}

func TestSpawnChildResultJSONOmitsEmptyOptionalFields(t *testing.T) {
	t.Parallel()

	result := SpawnChildResult{
		ChildThreadID: 102,
		Status:        "failed",
	}

	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	if _, exists := decoded["assistant_text"]; exists {
		t.Fatalf("assistant_text should be omitted, got %v", decoded["assistant_text"])
	}
	if _, exists := decoded["result_ref"]; exists {
		t.Fatalf("result_ref should be omitted, got %v", decoded["result_ref"])
	}
	if _, exists := decoded["error_ref"]; exists {
		t.Fatalf("error_ref should be omitted, got %v", decoded["error_ref"])
	}
}
