package threadstore

import (
	"testing"
	"time"
)

func TestThreadMetaRoundTrip(t *testing.T) {
	now := time.Date(2026, 3, 13, 18, 30, 0, 0, time.UTC)

	meta := ThreadMeta{
		ID:               "thread_123",
		RootThreadID:     "thread_123",
		Status:           ThreadStatusRunning,
		Model:            "gpt-5.4",
		Instructions:     "be sharp",
		MetadataJSON:     `{"tenant":"acme"}`,
		ToolsJSON:        `[{"type":"function","name":"spawn_subagents"}]`,
		ToolChoiceJSON:   `{"type":"function","name":"spawn_subagents"}`,
		OwnerWorkerID:    "worker_a",
		SocketGeneration: 2,
		SocketExpiresAt:  now.Add(time.Hour),
		LastResponseID:   "resp_prev",
		ActiveResponseID: "resp_live",
		CreatedAt:        now,
		UpdatedAt:        now.Add(time.Minute),
	}

	fields := metaToHash(meta)
	hash := map[string]string{}
	for key, value := range fields {
		hash[key] = value.(string)
	}

	decoded, err := threadMetaFromHash(hash)
	if err != nil {
		t.Fatalf("threadMetaFromHash() error = %v", err)
	}

	if decoded.ID != meta.ID {
		t.Fatalf("ID = %q, want %q", decoded.ID, meta.ID)
	}

	if decoded.SocketGeneration != meta.SocketGeneration {
		t.Fatalf("SocketGeneration = %d, want %d", decoded.SocketGeneration, meta.SocketGeneration)
	}

	if decoded.ToolsJSON != meta.ToolsJSON {
		t.Fatalf("ToolsJSON = %q, want %q", decoded.ToolsJSON, meta.ToolsJSON)
	}

	if decoded.ToolChoiceJSON != meta.ToolChoiceJSON {
		t.Fatalf("ToolChoiceJSON = %q, want %q", decoded.ToolChoiceJSON, meta.ToolChoiceJSON)
	}

	if !decoded.SocketExpiresAt.Equal(meta.SocketExpiresAt) {
		t.Fatalf("SocketExpiresAt = %s, want %s", decoded.SocketExpiresAt, meta.SocketExpiresAt)
	}
}
