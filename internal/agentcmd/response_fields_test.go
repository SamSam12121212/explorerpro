package agentcmd

import (
	"encoding/json"
	"testing"

	"github.com/openai/openai-go/v3/shared"
)

func TestDecodeReasoningRewritesUnsupportedEffort(t *testing.T) {
	t.Parallel()

	reasoning, err := DecodeReasoning(json.RawMessage(`{
		"effort":"legacy",
		"summary":"concise"
	}`))
	if err != nil {
		t.Fatalf("DecodeReasoning() error = %v", err)
	}

	if reasoning.Effort != shared.ReasoningEffortNone {
		t.Fatalf("Effort = %q, want %q", reasoning.Effort, shared.ReasoningEffortNone)
	}
	if reasoning.Summary != "" {
		t.Fatalf("Summary = %q, want empty", reasoning.Summary)
	}
}

func TestNormalizeReasoningParamRewritesUnsupportedEffort(t *testing.T) {
	t.Parallel()

	reasoning := NormalizeReasoningParam(shared.ReasoningParam{
		Effort:  shared.ReasoningEffort("legacy"),
		Summary: shared.ReasoningSummaryConcise,
	})

	if reasoning.Effort != shared.ReasoningEffortNone {
		t.Fatalf("Effort = %q, want %q", reasoning.Effort, shared.ReasoningEffortNone)
	}
	if reasoning.Summary != "" {
		t.Fatalf("Summary = %q, want empty", reasoning.Summary)
	}
}
