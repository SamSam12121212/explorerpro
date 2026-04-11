package agentcmd

import (
	"bytes"
	"encoding/json"
	"fmt"

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

func DecodeToolChoice(raw json.RawMessage) (responses.ResponseNewParamsToolChoiceUnion, error) {
	var toolChoice responses.ResponseNewParamsToolChoiceUnion
	if err := json.Unmarshal(raw, &toolChoice); err != nil {
		return responses.ResponseNewParamsToolChoiceUnion{}, fmt.Errorf("decode tool_choice payload: %w", err)
	}

	return normalizeToolChoiceParam(toolChoice), nil
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
	// We don't consume reasoning summary events yet, so avoid requesting them.
	reasoning.Summary = ""
	reasoning.GenerateSummary = ""
	return reasoning
}

func normalizeToolChoiceParam(toolChoice responses.ResponseNewParamsToolChoiceUnion) responses.ResponseNewParamsToolChoiceUnion {
	return toolChoice
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
