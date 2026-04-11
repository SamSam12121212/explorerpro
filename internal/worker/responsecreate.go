package worker

import (
	"encoding/json"
	"fmt"

	"explorer/internal/agentcmd"
	"explorer/internal/threadstore"

	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
)

func buildResponseCreatePayload(meta threadstore.ThreadMeta, fields map[string]any) (json.RawMessage, error) {
	payload, err := buildResponseCreatePayloadObject(meta, fields)
	if err != nil {
		return nil, err
	}

	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal response.create payload: %w", err)
	}

	return payloadJSON, nil
}

func buildResponseCreatePayloadObject(meta threadstore.ThreadMeta, fields map[string]any) (map[string]any, error) {
	payload := map[string]any{}

	for key, value := range fields {
		switch typed := value.(type) {
		case json.RawMessage:
			if len(typed) == 0 {
				continue
			}
			decoded, err := rawJSONToAny(typed)
			if err != nil {
				return nil, err
			}
			payload[key] = decoded
		case string:
			if typed != "" {
				payload[key] = typed
			}
		default:
			if value != nil {
				payload[key] = value
			}
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

	// Strip reasoning summary fields because we do not consume those events yet.
	if reasoning, ok := payload["reasoning"].(map[string]any); ok {
		delete(reasoning, "summary")
		delete(reasoning, "generate_summary")
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

func mergeStoredJSONField(payload map[string]any, key, raw string) error {
	if _, exists := payload[key]; exists {
		return nil
	}

	decoded, err := rawJSONToAny(json.RawMessage(raw))
	if err != nil {
		return err
	}
	payload[key] = decoded
	return nil
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
