package postgresstore

import (
	"context"
	"fmt"
	"strings"
	"time"

	"explorer/internal/threadstore"
)

func (s *Store) CreateOpenAISocketSession(ctx context.Context, session threadstore.OpenAISocketSession) error {
	if strings.TrimSpace(session.ID) == "" {
		return fmt.Errorf("openai socket session id is required")
	}
	if session.ThreadID <= 0 {
		return fmt.Errorf("openai socket session thread_id is required")
	}
	if session.RootThreadID <= 0 {
		return fmt.Errorf("openai socket session root_thread_id is required")
	}
	if session.WorkerID <= 0 {
		return fmt.Errorf("openai socket session worker_id is required")
	}
	if session.ThreadSocketGeneration == 0 {
		return fmt.Errorf("openai socket session thread_socket_generation must be positive")
	}
	if strings.TrimSpace(string(session.State)) == "" {
		session.State = threadstore.OpenAISocketStateConnected
	}
	if session.ConnectedAt.IsZero() {
		session.ConnectedAt = time.Now().UTC()
	}
	if session.LastHeartbeatAt.IsZero() {
		session.LastHeartbeatAt = session.ConnectedAt
	}
	if session.CreatedAt.IsZero() {
		session.CreatedAt = session.ConnectedAt
	}
	if session.UpdatedAt.IsZero() {
		session.UpdatedAt = session.ConnectedAt
	}

	_, err := s.pool.Exec(ctx, `
INSERT INTO openai_socket_sessions (
    id,
    thread_id,
    root_thread_id,
    parent_thread_id,
    worker_id,
    thread_socket_generation,
    state,
    connected_at,
    last_read_at,
    last_write_at,
    last_heartbeat_at,
    heartbeat_expires_at,
    disconnected_at,
    disconnect_reason,
    expires_at,
    created_at,
    updated_at
) VALUES (
    $1,
    $2,
    $3,
    $4,
    $5,
    $6,
    $7,
    $8,
    $9,
    $10,
    $11,
    $12,
    $13,
    $14,
    $15,
    $16,
    $17
)
`,
		session.ID,
		session.ThreadID,
		session.RootThreadID,
		nullIfZeroInt64(session.ParentThreadID),
		session.WorkerID,
		int64(session.ThreadSocketGeneration),
		string(session.State),
		nonZeroTime(session.ConnectedAt),
		nullIfZeroTime(session.LastReadAt),
		nullIfZeroTime(session.LastWriteAt),
		nonZeroTime(session.LastHeartbeatAt),
		nullIfZeroTime(session.HeartbeatExpiresAt),
		nullIfZeroTime(session.DisconnectedAt),
		nullIfBlank(session.DisconnectReason),
		nullIfZeroTime(session.ExpiresAt),
		nonZeroTime(session.CreatedAt),
		nonZeroTime(session.UpdatedAt),
	)
	if err != nil {
		return fmt.Errorf("insert openai socket session %s: %w", session.ID, err)
	}

	return nil
}

func (s *Store) TouchOpenAISocketSession(ctx context.Context, touch threadstore.OpenAISocketTouch) error {
	if strings.TrimSpace(touch.ID) == "" {
		return nil
	}
	if touch.LastReadAt.IsZero() && touch.LastWriteAt.IsZero() && touch.LastHeartbeatAt.IsZero() && touch.HeartbeatExpiresAt.IsZero() {
		return nil
	}

	_, err := s.pool.Exec(ctx, `
UPDATE openai_socket_sessions
SET last_read_at = CASE
        WHEN $2::timestamptz IS NOT NULL AND (last_read_at IS NULL OR last_read_at < $2::timestamptz)
            THEN $2::timestamptz
        ELSE last_read_at
    END,
    last_write_at = CASE
        WHEN $3::timestamptz IS NOT NULL AND (last_write_at IS NULL OR last_write_at < $3::timestamptz)
            THEN $3::timestamptz
        ELSE last_write_at
    END,
    last_heartbeat_at = CASE
        WHEN $4::timestamptz IS NOT NULL AND last_heartbeat_at < $4::timestamptz
            THEN $4::timestamptz
        ELSE last_heartbeat_at
    END,
    heartbeat_expires_at = CASE
        WHEN $5::timestamptz IS NOT NULL
            THEN $5::timestamptz
        ELSE heartbeat_expires_at
    END,
    updated_at = now()
WHERE id = $1
  AND state = 'connected'
`,
		touch.ID,
		nullIfZeroTime(touch.LastReadAt),
		nullIfZeroTime(touch.LastWriteAt),
		nullIfZeroTime(touch.LastHeartbeatAt),
		nullIfZeroTime(touch.HeartbeatExpiresAt),
	)
	if err != nil {
		return fmt.Errorf("touch openai socket session %s: %w", touch.ID, err)
	}

	return nil
}

func (s *Store) DisconnectOpenAISocketSession(ctx context.Context, socketID, reason string, disconnectedAt, expiresAt time.Time) error {
	if strings.TrimSpace(socketID) == "" {
		return nil
	}
	if strings.TrimSpace(reason) == "" {
		reason = "disconnected"
	}
	if disconnectedAt.IsZero() {
		disconnectedAt = time.Now().UTC()
	}

	_, err := s.pool.Exec(ctx, `
UPDATE openai_socket_sessions
SET state = 'disconnected',
    disconnected_at = COALESCE(disconnected_at, $2),
    disconnect_reason = CASE
        WHEN btrim(COALESCE(disconnect_reason, '')) = ''
            THEN $3
        ELSE disconnect_reason
    END,
    heartbeat_expires_at = NULL,
    expires_at = COALESCE(expires_at, $4),
    updated_at = $2
WHERE id = $1
`,
		socketID,
		nonZeroTime(disconnectedAt),
		reason,
		nullIfZeroTime(expiresAt),
	)
	if err != nil {
		return fmt.Errorf("disconnect openai socket session %s: %w", socketID, err)
	}

	return nil
}

func (s *Store) DisconnectExpiredOpenAISocketSessions(ctx context.Context, disconnectedAt, expiresAt time.Time, limit int) (int64, error) {
	if limit <= 0 {
		limit = 100
	}

	tag, err := s.pool.Exec(ctx, `
WITH stale AS (
    SELECT id
    FROM openai_socket_sessions
    WHERE state = 'connected'
      AND heartbeat_expires_at IS NOT NULL
      AND heartbeat_expires_at <= $1
    ORDER BY heartbeat_expires_at ASC, id ASC
    LIMIT $3
)
UPDATE openai_socket_sessions AS sockets
SET state = 'disconnected',
    disconnected_at = $1,
    disconnect_reason = 'heartbeat_expired',
    heartbeat_expires_at = NULL,
    expires_at = $2,
    updated_at = $1
FROM stale
WHERE sockets.id = stale.id
`,
		nonZeroTime(disconnectedAt),
		nonZeroTime(expiresAt),
		limit,
	)
	if err != nil {
		return 0, fmt.Errorf("disconnect expired openai socket sessions: %w", err)
	}

	return tag.RowsAffected(), nil
}

func (s *Store) PruneExpiredOpenAISocketSessions(ctx context.Context, now time.Time, limit int) (int64, error) {
	if limit <= 0 {
		limit = 100
	}

	tag, err := s.pool.Exec(ctx, `
WITH doomed AS (
    SELECT id
    FROM openai_socket_sessions
    WHERE state = 'disconnected'
      AND expires_at IS NOT NULL
      AND expires_at <= $1
    ORDER BY expires_at ASC, id ASC
    LIMIT $2
)
DELETE FROM openai_socket_sessions AS sockets
USING doomed
WHERE sockets.id = doomed.id
`,
		nonZeroTime(now),
		limit,
	)
	if err != nil {
		return 0, fmt.Errorf("prune expired openai socket sessions: %w", err)
	}

	return tag.RowsAffected(), nil
}
