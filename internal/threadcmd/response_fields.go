package threadcmd

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
)

func NormalizeMetadata(raw json.RawMessage) (json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}

	metadata, err := DecodeMetadata(raw)
	if err != nil {
		return nil, err
	}

	normalized, err := json.Marshal(metadata)
	if err != nil {
		return nil, fmt.Errorf("marshal metadata payload: %w", err)
	}
	return normalized, nil
}

func DecodeMetadata(raw json.RawMessage) (shared.Metadata, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()

	var metadata map[string]any
	if err := decoder.Decode(&metadata); err != nil {
		return nil, fmt.Errorf("decode metadata payload: %w", err)
	}

	return normalizeMetadataMap(metadata), nil
}

func NormalizeReasoning(raw json.RawMessage) (json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}

	reasoning, err := DecodeReasoning(raw)
	if err != nil {
		return nil, err
	}

	normalized, err := json.Marshal(reasoning)
	if err != nil {
		return nil, fmt.Errorf("marshal reasoning payload: %w", err)
	}
	return normalized, nil
}

func DecodeReasoning(raw json.RawMessage) (shared.ReasoningParam, error) {
	var reasoning shared.ReasoningParam
	if err := json.Unmarshal(raw, &reasoning); err != nil {
		return shared.ReasoningParam{}, fmt.Errorf("decode reasoning payload: %w", err)
	}

	return normalizeReasoningParam(reasoning), nil
}

func NormalizeToolChoice(raw json.RawMessage) (json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}

	toolChoice, err := DecodeToolChoice(raw)
	if err != nil {
		return nil, err
	}

	normalized, err := json.Marshal(toolChoice)
	if err != nil {
		return nil, fmt.Errorf("marshal tool_choice payload: %w", err)
	}
	return normalized, nil
}

func NormalizeTools(raw json.RawMessage) (json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}

	tools, err := DecodeTools(raw)
	if err != nil {
		return nil, err
	}

	normalized, err := json.Marshal(tools)
	if err != nil {
		return nil, fmt.Errorf("marshal tools payload: %w", err)
	}
	return normalized, nil
}

// DecodeToolChoice parses a raw tool_choice payload into the typed union.
//
// The SDK's ResponseNewParamsToolChoiceUnion has a broken JSON unmarshaler:
// none of the Of* variant pointers get populated, and re-marshalling drops
// every field except `type`. That silently corrupted anything object-shaped
// (function/mcp/custom/allowed_tools/hosted) — a payload like
// `{"type":"function","name":"emit_bboxes"}` round-tripped to
// `{"type":"function"}`, which OpenAI then rejected on the wire as an
// invalid hosted-tool type. Bypass the union's unmarshaler and dispatch on
// the `type` discriminator ourselves, loading each concrete Param struct
// directly (which unmarshals correctly) and setting the matching Of* field.
func DecodeToolChoice(raw json.RawMessage) (responses.ResponseNewParamsToolChoiceUnion, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return responses.ResponseNewParamsToolChoiceUnion{}, nil
	}

	// Mode form: "none" | "auto" | "required" as a bare JSON string.
	if trimmed[0] == '"' {
		var mode string
		if err := json.Unmarshal(trimmed, &mode); err != nil {
			return responses.ResponseNewParamsToolChoiceUnion{}, fmt.Errorf("decode tool_choice mode: %w", err)
		}
		switch responses.ToolChoiceOptions(mode) {
		case responses.ToolChoiceOptionsNone, responses.ToolChoiceOptionsAuto, responses.ToolChoiceOptionsRequired:
			return responses.ResponseNewParamsToolChoiceUnion{
				OfToolChoiceMode: param.NewOpt(responses.ToolChoiceOptions(mode)),
			}, nil
		default:
			return responses.ResponseNewParamsToolChoiceUnion{}, fmt.Errorf("unsupported tool_choice mode %q", mode)
		}
	}

	var discriminator struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(trimmed, &discriminator); err != nil {
		return responses.ResponseNewParamsToolChoiceUnion{}, fmt.Errorf("decode tool_choice type discriminator: %w", err)
	}

	switch discriminator.Type {
	case "function":
		var choice responses.ToolChoiceFunctionParam
		if err := json.Unmarshal(trimmed, &choice); err != nil {
			return responses.ResponseNewParamsToolChoiceUnion{}, fmt.Errorf("decode tool_choice function: %w", err)
		}
		return responses.ResponseNewParamsToolChoiceUnion{OfFunctionTool: &choice}, nil
	case "allowed_tools":
		var choice responses.ToolChoiceAllowedParam
		if err := json.Unmarshal(trimmed, &choice); err != nil {
			return responses.ResponseNewParamsToolChoiceUnion{}, fmt.Errorf("decode tool_choice allowed_tools: %w", err)
		}
		return responses.ResponseNewParamsToolChoiceUnion{OfAllowedTools: &choice}, nil
	case "mcp":
		var choice responses.ToolChoiceMcpParam
		if err := json.Unmarshal(trimmed, &choice); err != nil {
			return responses.ResponseNewParamsToolChoiceUnion{}, fmt.Errorf("decode tool_choice mcp: %w", err)
		}
		return responses.ResponseNewParamsToolChoiceUnion{OfMcpTool: &choice}, nil
	case "custom":
		var choice responses.ToolChoiceCustomParam
		if err := json.Unmarshal(trimmed, &choice); err != nil {
			return responses.ResponseNewParamsToolChoiceUnion{}, fmt.Errorf("decode tool_choice custom: %w", err)
		}
		return responses.ResponseNewParamsToolChoiceUnion{OfCustomTool: &choice}, nil
	case "apply_patch":
		return responses.ResponseNewParamsToolChoiceUnion{
			OfSpecificApplyPatchToolChoice: &responses.ToolChoiceApplyPatchParam{},
		}, nil
	case "shell":
		shellParam := responses.NewToolChoiceShellParam()
		return responses.ResponseNewParamsToolChoiceUnion{OfSpecificShellToolChoice: &shellParam}, nil
	case
		string(responses.ToolChoiceTypesTypeFileSearch),
		string(responses.ToolChoiceTypesTypeWebSearchPreview),
		string(responses.ToolChoiceTypesTypeComputer),
		string(responses.ToolChoiceTypesTypeComputerUsePreview),
		string(responses.ToolChoiceTypesTypeComputerUse),
		string(responses.ToolChoiceTypesTypeWebSearchPreview2025_03_11),
		string(responses.ToolChoiceTypesTypeImageGeneration),
		string(responses.ToolChoiceTypesTypeCodeInterpreter):
		var choice responses.ToolChoiceTypesParam
		if err := json.Unmarshal(trimmed, &choice); err != nil {
			return responses.ResponseNewParamsToolChoiceUnion{}, fmt.Errorf("decode tool_choice hosted tool: %w", err)
		}
		return responses.ResponseNewParamsToolChoiceUnion{OfHostedTool: &choice}, nil
	default:
		return responses.ResponseNewParamsToolChoiceUnion{}, fmt.Errorf("unsupported tool_choice type %q", discriminator.Type)
	}
}

func DecodeTools(raw json.RawMessage) ([]responses.ToolUnionParam, error) {
	var tools []responses.ToolUnionParam
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, fmt.Errorf("decode tools payload: %w", err)
	}

	for idx, tool := range tools {
		if !isRecognizedToolParam(tool) {
			return nil, fmt.Errorf("decode tools payload: unsupported tool at index %d", idx)
		}
	}

	return tools, nil
}

func normalizeMetadataMap(raw map[string]any) shared.Metadata {
	if len(raw) == 0 {
		return shared.Metadata{}
	}

	metadata := make(shared.Metadata, len(raw))
	for key, value := range raw {
		metadata[key] = stringifyMetadataValue(value)
	}
	return metadata
}

func normalizeReasoningParam(reasoning shared.ReasoningParam) shared.ReasoningParam {
	return NormalizeReasoningParam(reasoning)
}

func NormalizeReasoningParam(reasoning shared.ReasoningParam) shared.ReasoningParam {
	// GPT-5.4 rejects legacy/unsupported efforts like `minimal`, so we coerce
	// anything outside the supported set to `none` before sending it upstream.
	switch reasoning.Effort {
	case "":
	case shared.ReasoningEffortNone, shared.ReasoningEffortLow, shared.ReasoningEffortMedium, shared.ReasoningEffortHigh, shared.ReasoningEffortXhigh:
	default:
		reasoning.Effort = shared.ReasoningEffortNone
	}

	// We don't consume reasoning summary events yet, so avoid requesting them.
	reasoning.Summary = ""
	reasoning.GenerateSummary = ""
	return reasoning
}

func normalizeToolsParam(tools []responses.ToolUnionParam) []responses.ToolUnionParam {
	cloned := make([]responses.ToolUnionParam, len(tools))
	copy(cloned, tools)
	return cloned
}

func NormalizeToolsParam(tools []responses.ToolUnionParam) []responses.ToolUnionParam {
	return normalizeToolsParam(tools)
}

func isRecognizedToolParam(tool responses.ToolUnionParam) bool {
	return tool.OfFunction != nil ||
		tool.OfFileSearch != nil ||
		tool.OfComputer != nil ||
		tool.OfComputerUsePreview != nil ||
		tool.OfWebSearch != nil ||
		tool.OfMcp != nil ||
		tool.OfCodeInterpreter != nil ||
		tool.OfImageGeneration != nil ||
		tool.OfLocalShell != nil ||
		tool.OfShell != nil ||
		tool.OfCustom != nil ||
		tool.OfNamespace != nil ||
		tool.OfToolSearch != nil ||
		tool.OfWebSearchPreview != nil ||
		tool.OfApplyPatch != nil
}

func stringifyMetadataValue(value any) string {
	switch typed := value.(type) {
	case nil:
		return "null"
	case string:
		return typed
	case json.Number:
		return typed.String()
	case bool:
		if typed {
			return "true"
		}
		return "false"
	default:
		raw, err := json.Marshal(typed)
		if err != nil {
			return fmt.Sprintf("%v", typed)
		}
		return string(raw)
	}
}
