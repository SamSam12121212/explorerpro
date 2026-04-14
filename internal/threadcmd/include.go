package threadcmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

const RequiredIncludeReasoningEncryptedContent = "reasoning.encrypted_content"

func NormalizeInclude(raw json.RawMessage) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return json.Marshal([]string{RequiredIncludeReasoningEncryptedContent})
	}

	var decoded any
	if err := json.Unmarshal(trimmed, &decoded); err != nil {
		return nil, fmt.Errorf("decode include: %w", err)
	}

	values, err := EnsureIncludeValue(decoded)
	if err != nil {
		return nil, err
	}

	normalized, err := json.Marshal(values)
	if err != nil {
		return nil, fmt.Errorf("marshal include: %w", err)
	}

	return normalized, nil
}

func EnsureIncludeValue(value any) ([]string, error) {
	switch typed := value.(type) {
	case nil:
		return []string{RequiredIncludeReasoningEncryptedContent}, nil
	case []string:
		return EnsureRequiredInclude(typed), nil
	case []any:
		values := make([]string, 0, len(typed))
		for index, entry := range typed {
			text, ok := entry.(string)
			if !ok {
				return nil, fmt.Errorf("include[%d] must be a string", index)
			}
			values = append(values, text)
		}
		return EnsureRequiredInclude(values), nil
	default:
		return nil, fmt.Errorf("include must be an array of strings")
	}
}

func EnsureRequiredInclude(values []string) []string {
	normalized := make([]string, 0, len(values)+1)
	seen := map[string]struct{}{}

	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		if _, exists := seen[trimmed]; exists {
			continue
		}
		seen[trimmed] = struct{}{}
		normalized = append(normalized, trimmed)
	}

	if _, exists := seen[RequiredIncludeReasoningEncryptedContent]; !exists {
		normalized = append(normalized, RequiredIncludeReasoningEncryptedContent)
	}

	return normalized
}
