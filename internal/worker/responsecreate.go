package worker

import (
	"encoding/json"
	"fmt"

	"explorer/internal/doccmd"
	"explorer/internal/threadcmd"
	"explorer/internal/threadstore"

	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
)

func buildResponseCreatePayload(meta threadstore.ThreadMeta, fields map[string]any) (json.RawMessage, error) {
	payload, err := buildResponseCreatePayloadObject(meta, fields)
	if err != nil {
		return nil, err
	}

	return marshalResponseCreatePayload(payload)
}

func buildResponseCreatePayloadObject(meta threadstore.ThreadMeta, fields map[string]any) (map[string]any, error) {
	payload := map[string]any{}

	for key, value := range fields {
		normalized, ok, err := normalizeResponseCreateField(key, value)
		if err != nil {
			return nil, err
		}
		if ok {
			payload[key] = normalized
		}
	}

	if meta.MetadataJSON != "" {
		if err := mergeStoredJSONField(payload, "metadata", meta.MetadataJSON); err != nil {
			return nil, err
		}
	}
	if meta.IncludeJSON != "" {
		if err := mergeStoredJSONField(payload, "include", meta.IncludeJSON); err != nil {
			return nil, err
		}
	}
	if meta.ToolsJSON != "" {
		if err := mergeStoredJSONField(payload, "tools", meta.ToolsJSON); err != nil {
			return nil, err
		}
	}
	if meta.ToolChoiceJSON != "" {
		if err := mergeStoredJSONField(payload, "tool_choice", meta.ToolChoiceJSON); err != nil {
			return nil, err
		}
	}
	if meta.ReasoningJSON != "" {
		if err := mergeStoredJSONField(payload, "reasoning", meta.ReasoningJSON); err != nil {
			return nil, err
		}
	}

	return payload, nil
}

func (a *threadActor) buildThreadResponseCreatePayload(meta threadstore.ThreadMeta, fields map[string]any) (map[string]any, error) {
	payload, err := buildResponseCreatePayloadObject(meta, fields)
	if err != nil {
		return nil, err
	}

	if err := a.finalizeThreadResponseCreatePayload(meta.ID, payload); err != nil {
		return nil, err
	}

	return payload, nil
}

func (a *threadActor) finalizeThreadResponseCreatePayload(threadID int64, payload map[string]any) error {
	if err := a.applyDocumentRuntimeContext(threadID, payload); err != nil {
		return err
	}

	return ensureRequiredResponseInclude(payload)
}

func decodeResponseCreatePayloadObject(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("response.create payload is empty")
	}

	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("decode response.create payload: %w", err)
	}
	if payload == nil {
		return nil, fmt.Errorf("response.create payload must be an object")
	}

	return payload, nil
}

func marshalResponseCreatePayload(payload map[string]any) (json.RawMessage, error) {
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal response.create payload: %w", err)
	}

	return payloadJSON, nil
}

func mergeStoredJSONField(payload map[string]any, key, raw string) error {
	if _, exists := payload[key]; exists {
		return nil
	}

	decoded, err := decodeResponseCreateField(key, json.RawMessage(raw))
	if err != nil {
		return err
	}
	payload[key] = decoded
	return nil
}

func normalizeResponseCreateField(key string, value any) (any, bool, error) {
	switch typed := value.(type) {
	case json.RawMessage:
		if len(typed) == 0 {
			return nil, false, nil
		}
		decoded, err := decodeResponseCreateField(key, typed)
		if err != nil {
			return nil, false, err
		}
		return decoded, true, nil
	case string:
		if typed == "" {
			return nil, false, nil
		}
		if key == "tool_choice" || key == "metadata" {
			raw, err := json.Marshal(typed)
			if err != nil {
				return nil, false, fmt.Errorf("marshal %s payload: %w", key, err)
			}
			decoded, err := decodeResponseCreateField(key, raw)
			if err != nil {
				return nil, false, err
			}
			return decoded, true, nil
		}
		return typed, true, nil
	case shared.Metadata:
		return normalizeSharedMetadataParam(typed), true, nil
	case shared.ReasoningParam:
		return normalizeReasoningParam(typed), true, nil
	case []responses.ToolUnionParam:
		return normalizeToolsParam(typed), true, nil
	case responses.ResponseNewParamsToolChoiceUnion:
		return normalizeToolChoiceParam(typed), true, nil
	case []any:
		if key != "tools" {
			return typed, true, nil
		}
		raw, err := json.Marshal(typed)
		if err != nil {
			return nil, false, fmt.Errorf("marshal tools payload: %w", err)
		}
		decoded, err := decodeResponseCreateField(key, raw)
		if err != nil {
			return nil, false, err
		}
		return decoded, true, nil
	case map[string]any:
		if key != "metadata" && key != "reasoning" && key != "tool_choice" {
			return typed, true, nil
		}
		raw, err := json.Marshal(typed)
		if err != nil {
			return nil, false, fmt.Errorf("marshal %s payload: %w", key, err)
		}
		decoded, err := decodeResponseCreateField(key, raw)
		if err != nil {
			return nil, false, err
		}
		return decoded, true, nil
	default:
		if value == nil {
			return nil, false, nil
		}
		if key == "metadata" {
			raw, err := json.Marshal(value)
			if err != nil {
				return nil, false, fmt.Errorf("marshal metadata payload: %w", err)
			}
			decoded, err := decodeResponseCreateField(key, raw)
			if err != nil {
				return nil, false, err
			}
			return decoded, true, nil
		}
		return value, true, nil
	}
}

func decodeResponseCreateField(key string, raw json.RawMessage) (any, error) {
	switch key {
	case "metadata":
		return decodeMetadataParam(raw)
	case "reasoning":
		return decodeReasoningParam(raw)
	case "tools":
		return decodeToolsParam(raw)
	case "tool_choice":
		return decodeToolChoiceParam(raw)
	default:
		return rawJSONToAny(raw)
	}
}

func normalizeMetadataJSON(raw json.RawMessage) (json.RawMessage, error) {
	return threadcmd.NormalizeMetadata(raw)
}

func decodeMetadataParam(raw json.RawMessage) (shared.Metadata, error) {
	return threadcmd.DecodeMetadata(raw)
}

func normalizeSharedMetadataParam(raw shared.Metadata) shared.Metadata {
	if len(raw) == 0 {
		return shared.Metadata{}
	}

	metadata := make(shared.Metadata, len(raw))
	for key, value := range raw {
		metadata[key] = value
	}
	return metadata
}

func decodeToolsParam(raw json.RawMessage) ([]responses.ToolUnionParam, error) {
	return threadcmd.DecodeTools(raw)
}

func normalizeToolsParam(tools []responses.ToolUnionParam) []responses.ToolUnionParam {
	return threadcmd.NormalizeToolsParam(tools)
}

func decodePayloadTools(value any) ([]responses.ToolUnionParam, error) {
	switch typed := value.(type) {
	case nil:
		return nil, nil
	case []responses.ToolUnionParam:
		return normalizeToolsParam(typed), nil
	case []any:
		raw, err := json.Marshal(typed)
		if err != nil {
			return nil, fmt.Errorf("marshal tools payload: %w", err)
		}
		return decodeToolsParam(raw)
	default:
		return nil, fmt.Errorf("tools payload has unsupported type %T", value)
	}
}

func toolParamName(tool responses.ToolUnionParam) string {
	if name := tool.GetName(); name != nil {
		return *name
	}
	return ""
}

func decodeReasoningParam(raw json.RawMessage) (shared.ReasoningParam, error) {
	return threadcmd.DecodeReasoning(raw)
}

func normalizeReasoningParam(reasoning shared.ReasoningParam) shared.ReasoningParam {
	return threadcmd.NormalizeReasoningParam(reasoning)
}

func decodeToolChoiceParam(raw json.RawMessage) (responses.ResponseNewParamsToolChoiceUnion, error) {
	return threadcmd.DecodeToolChoice(raw)
}

func normalizeToolChoiceParam(toolChoice responses.ResponseNewParamsToolChoiceUnion) responses.ResponseNewParamsToolChoiceUnion {
	return toolChoice
}

func filterChildThreadToolChoiceParam(toolChoice responses.ResponseNewParamsToolChoiceUnion) (responses.ResponseNewParamsToolChoiceUnion, bool) {
	if !param.IsOmitted(toolChoice.OfToolChoiceMode) {
		return toolChoice, true
	}

	if choice := toolChoice.OfAllowedTools; choice != nil {
		filtered := filterChildThreadAllowedTools(choice.Tools)
		if len(filtered) == 0 {
			return responses.ResponseNewParamsToolChoiceUnion{}, false
		}

		clone := *choice
		clone.Tools = filtered
		toolChoice.OfAllowedTools = &clone
		return toolChoice, true
	}

	if choice := toolChoice.OfFunctionTool; choice != nil && isInternalRuntimeToolName(choice.Name) {
		return responses.ResponseNewParamsToolChoiceUnion{}, false
	}

	return toolChoice, true
}

func filterChildThreadAllowedTools(tools []map[string]any) []map[string]any {
	filtered := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		if isInternalRuntimeToolName(toolDefinitionName(tool)) {
			continue
		}
		filtered = append(filtered, tool)
	}
	return filtered
}

func toolDefinitionName(tool map[string]any) string {
	name, _ := tool["name"].(string)
	return name
}

func isInternalRuntimeToolName(name string) bool {
	switch name {
	case "spawn_threads", doccmd.ToolNameQueryDocument:
		return true
	default:
		return false
	}
}

func ensureRequiredResponseInclude(payload map[string]any) error {
	include, err := threadcmd.EnsureIncludeValue(payload["include"])
	if err != nil {
		return err
	}

	normalized := make([]responses.ResponseIncludable, 0, len(include))
	for _, value := range include {
		normalized = append(normalized, responses.ResponseIncludable(value))
	}
	payload["include"] = normalized
	return nil
}
