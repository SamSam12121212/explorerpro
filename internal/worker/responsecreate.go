package worker

import (
	"bytes"
	"encoding/json"
	"fmt"

	"explorer/internal/agentcmd"
	"explorer/internal/threadstore"

	openai "github.com/openai/openai-go/v3"
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

func (a *threadActor) finalizeThreadResponseCreatePayload(threadID string, payload map[string]any) error {
	if err := a.injectDocumentTools(threadID, payload); err != nil {
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
	case responses.ResponseNewParamsToolChoiceUnion:
		return normalizeToolChoiceParam(typed), true, nil
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
	case "tool_choice":
		return decodeToolChoiceParam(raw)
	default:
		return rawJSONToAny(raw)
	}
}

func normalizeMetadataJSON(raw json.RawMessage) (json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}

	metadata, err := decodeMetadataParam(raw)
	if err != nil {
		return nil, err
	}

	normalized, err := json.Marshal(metadata)
	if err != nil {
		return nil, fmt.Errorf("marshal metadata payload: %w", err)
	}
	return normalized, nil
}

func decodeMetadataParam(raw json.RawMessage) (shared.Metadata, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()

	var metadata map[string]any
	if err := decoder.Decode(&metadata); err != nil {
		return nil, fmt.Errorf("decode metadata payload: %w", err)
	}

	return normalizeMetadataMap(metadata), nil
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

func decodeReasoningParam(raw json.RawMessage) (shared.ReasoningParam, error) {
	var reasoning shared.ReasoningParam
	if err := json.Unmarshal(raw, &reasoning); err != nil {
		return shared.ReasoningParam{}, fmt.Errorf("decode reasoning payload: %w", err)
	}

	return normalizeReasoningParam(reasoning), nil
}

func normalizeReasoningParam(reasoning shared.ReasoningParam) shared.ReasoningParam {
	// We don't consume reasoning summary events yet, so avoid requesting them.
	reasoning.Summary = ""
	reasoning.GenerateSummary = ""
	return reasoning
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

func decodeToolChoiceParam(raw json.RawMessage) (responses.ResponseNewParamsToolChoiceUnion, error) {
	var toolChoice responses.ResponseNewParamsToolChoiceUnion
	if err := json.Unmarshal(raw, &toolChoice); err != nil {
		return responses.ResponseNewParamsToolChoiceUnion{}, fmt.Errorf("decode tool_choice payload: %w", err)
	}

	return normalizeToolChoiceParam(toolChoice), nil
}

func normalizeToolChoiceParam(toolChoice responses.ResponseNewParamsToolChoiceUnion) responses.ResponseNewParamsToolChoiceUnion {
	return toolChoice
}

func filterSubagentToolChoiceParam(toolChoice responses.ResponseNewParamsToolChoiceUnion) (responses.ResponseNewParamsToolChoiceUnion, bool) {
	if !param.IsOmitted(toolChoice.OfToolChoiceMode) {
		return toolChoice, true
	}

	if choice := toolChoice.OfAllowedTools; choice != nil {
		filtered := filterSubagentAllowedTools(choice.Tools)
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

func filterSubagentAllowedTools(tools []map[string]any) []map[string]any {
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
	case "spawn_subagents", toolNameQueryAttachedDocuments:
		return true
	default:
		return false
	}
}

func ensureRequiredResponseInclude(payload map[string]any) error {
	include, err := agentcmd.EnsureIncludeValue(payload["include"])
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

func queryAttachedDocumentsToolDef() (map[string]any, error) {
	return marshalJSONObject(responses.ToolUnionParam{
		OfFunction: &responses.FunctionToolParam{
			Name:        toolNameQueryAttachedDocuments,
			Description: openai.String("Query one or more attached documents. Each document has all of its pages already loaded into a separate analysis session. Describe what you need in the task field; mention specific page numbers there if needed."),
			Strict:      openai.Bool(true),
			Parameters: map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]any{
					"document_ids": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "IDs of the attached documents to query.",
					},
					"task": map[string]any{
						"type":        "string",
						"description": "What to look for or ask about in the documents.",
					},
				},
				"required": []string{"document_ids", "task"},
			},
		},
	})
}

func marshalJSONObject(value any) (map[string]any, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("marshal json object: %w", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, fmt.Errorf("decode json object: %w", err)
	}
	if decoded == nil {
		return nil, fmt.Errorf("value did not encode as a json object")
	}

	return decoded, nil
}
