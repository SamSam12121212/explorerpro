package threadcmd

import (
	"encoding/json"
	"testing"

	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
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

func TestDecodeToolChoiceDispatchesByType(t *testing.T) {
	t.Parallel()

	// Each case asserts (1) the union lands on the correct Of* variant and
	// (2) the round-trip through NormalizeToolChoice preserves every field.
	// Prior to dispatch-by-type, the SDK union's UnmarshalJSON dropped every
	// non-type field (a regression specifically caught when the citation
	// locator began forcing tool_choice:{type:function,name:emit_bboxes} —
	// OpenAI saw {"type":"function"} on the wire and rejected it).
	cases := []struct {
		name     string
		raw      string
		wantJSON string
		check    func(t *testing.T, u responses.ResponseNewParamsToolChoiceUnion)
	}{
		{
			name:     "function",
			raw:      `{"type":"function","name":"emit_bboxes"}`,
			wantJSON: `{"name":"emit_bboxes","type":"function"}`,
			check: func(t *testing.T, u responses.ResponseNewParamsToolChoiceUnion) {
				if u.OfFunctionTool == nil || u.OfFunctionTool.Name != "emit_bboxes" {
					t.Fatalf("OfFunctionTool = %+v, want name=emit_bboxes", u.OfFunctionTool)
				}
			},
		},
		{
			name:     "mode-required",
			raw:      `"required"`,
			wantJSON: `"required"`,
			check: func(t *testing.T, u responses.ResponseNewParamsToolChoiceUnion) {
				if param.IsOmitted(u.OfToolChoiceMode) || u.OfToolChoiceMode.Value != responses.ToolChoiceOptionsRequired {
					t.Fatalf("OfToolChoiceMode = %+v, want required", u.OfToolChoiceMode)
				}
			},
		},
		{
			name:     "hosted-file-search",
			raw:      `{"type":"file_search"}`,
			wantJSON: `{"type":"file_search"}`,
			check: func(t *testing.T, u responses.ResponseNewParamsToolChoiceUnion) {
				if u.OfHostedTool == nil || u.OfHostedTool.Type != responses.ToolChoiceTypesTypeFileSearch {
					t.Fatalf("OfHostedTool = %+v, want file_search", u.OfHostedTool)
				}
			},
		},
		{
			name:     "mcp",
			raw:      `{"type":"mcp","server_label":"docs","name":"lookup"}`,
			wantJSON: `{"server_label":"docs","name":"lookup","type":"mcp"}`,
			check: func(t *testing.T, u responses.ResponseNewParamsToolChoiceUnion) {
				if u.OfMcpTool == nil || u.OfMcpTool.ServerLabel != "docs" || !u.OfMcpTool.Name.Valid() || u.OfMcpTool.Name.Value != "lookup" {
					t.Fatalf("OfMcpTool = %+v, want server_label=docs name=lookup", u.OfMcpTool)
				}
			},
		},
		{
			name:     "custom",
			raw:      `{"type":"custom","name":"my_tool"}`,
			wantJSON: `{"name":"my_tool","type":"custom"}`,
			check: func(t *testing.T, u responses.ResponseNewParamsToolChoiceUnion) {
				if u.OfCustomTool == nil || u.OfCustomTool.Name != "my_tool" {
					t.Fatalf("OfCustomTool = %+v, want name=my_tool", u.OfCustomTool)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			u, err := DecodeToolChoice(json.RawMessage(tc.raw))
			if err != nil {
				t.Fatalf("DecodeToolChoice(%s) error = %v", tc.raw, err)
			}
			tc.check(t, u)

			normalized, err := NormalizeToolChoice(json.RawMessage(tc.raw))
			if err != nil {
				t.Fatalf("NormalizeToolChoice(%s) error = %v", tc.raw, err)
			}
			if string(normalized) != tc.wantJSON {
				t.Fatalf("NormalizeToolChoice(%s) = %s, want %s", tc.raw, string(normalized), tc.wantJSON)
			}
		})
	}
}

func TestDecodeToolChoiceRejectsUnknownType(t *testing.T) {
	t.Parallel()

	if _, err := DecodeToolChoice(json.RawMessage(`{"type":"bogus"}`)); err == nil {
		t.Fatalf("DecodeToolChoice(bogus) error = nil, want error")
	}
	if _, err := DecodeToolChoice(json.RawMessage(`"banana"`)); err == nil {
		t.Fatalf("DecodeToolChoice(banana) error = nil, want error")
	}
}
