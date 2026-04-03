package postgresstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"explorer/internal/threadstore"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const linkKindOwned = "owned"

type Store struct {
	pool *pgxpool.Pool
}

type ThreadListEntry struct {
	Meta               threadstore.ThreadMeta
	FirstMessageText   string
	LatestMessageText  string
	LatestMessageAt    time.Time
	LatestMessageIsOut bool
}

var _ threadstore.DurableSink = (*Store)(nil)

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) CreateThreadIfAbsent(ctx context.Context, meta threadstore.ThreadMeta) error {
	_, err := s.pool.Exec(ctx, `
INSERT INTO threads (
    id,
    root_thread_id,
    parent_thread_id,
    parent_call_id,
    depth,
    status,
    model,
    instructions,
    metadata_json,
    include_json,
    tools_json,
    tool_choice_json,
    reasoning_json,
    owner_worker_id,
    socket_generation,
    socket_expires_at,
    last_response_id,
    active_response_id,
    active_spawn_group_id,
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
    $9::jsonb,
    $10::jsonb,
    $11::jsonb,
    $12::jsonb,
    $13::jsonb,
    $14,
    $15,
    $16,
    $17,
    $18,
    $19,
    $20,
    $21
)
ON CONFLICT (id) DO NOTHING
`,
		meta.ID,
		meta.RootThreadID,
		nullIfBlank(meta.ParentThreadID),
		nullIfBlank(meta.ParentCallID),
		meta.Depth,
		string(meta.Status),
		meta.Model,
		meta.Instructions,
		requiredJSON(meta.MetadataJSON, "{}"),
		optionalJSON(meta.IncludeJSON),
		optionalJSON(meta.ToolsJSON),
		optionalJSON(meta.ToolChoiceJSON),
		optionalJSON(meta.ReasoningJSON),
		nullIfBlank(meta.OwnerWorkerID),
		int64(meta.SocketGeneration),
		nullIfZeroTime(meta.SocketExpiresAt),
		nullIfBlank(meta.LastResponseID),
		nullIfBlank(meta.ActiveResponseID),
		nullIfBlank(meta.ActiveSpawnGroupID),
		nonZeroTime(meta.CreatedAt),
		nonZeroTime(meta.UpdatedAt),
	)
	if err != nil {
		return fmt.Errorf("persist thread create %s: %w", meta.ID, err)
	}

	return nil
}

func (s *Store) SaveThread(ctx context.Context, meta threadstore.ThreadMeta) error {
	_, err := s.pool.Exec(ctx, `
INSERT INTO threads (
    id,
    root_thread_id,
    parent_thread_id,
    parent_call_id,
    depth,
    status,
    model,
    instructions,
    metadata_json,
    include_json,
    tools_json,
    tool_choice_json,
    reasoning_json,
    owner_worker_id,
    socket_generation,
    socket_expires_at,
    last_response_id,
    active_response_id,
    active_spawn_group_id,
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
    $9::jsonb,
    $10::jsonb,
    $11::jsonb,
    $12::jsonb,
    $13::jsonb,
    $14,
    $15,
    $16,
    $17,
    $18,
    $19,
    $20,
    $21
)
ON CONFLICT (id) DO UPDATE SET
    root_thread_id = EXCLUDED.root_thread_id,
    parent_thread_id = EXCLUDED.parent_thread_id,
    parent_call_id = EXCLUDED.parent_call_id,
    depth = EXCLUDED.depth,
    status = EXCLUDED.status,
    model = EXCLUDED.model,
    instructions = EXCLUDED.instructions,
    metadata_json = EXCLUDED.metadata_json,
    include_json = EXCLUDED.include_json,
    tools_json = EXCLUDED.tools_json,
    tool_choice_json = EXCLUDED.tool_choice_json,
    reasoning_json = EXCLUDED.reasoning_json,
    owner_worker_id = EXCLUDED.owner_worker_id,
    socket_generation = EXCLUDED.socket_generation,
    socket_expires_at = EXCLUDED.socket_expires_at,
    last_response_id = EXCLUDED.last_response_id,
    active_response_id = EXCLUDED.active_response_id,
    active_spawn_group_id = EXCLUDED.active_spawn_group_id,
    created_at = EXCLUDED.created_at,
    updated_at = EXCLUDED.updated_at
`,
		meta.ID,
		meta.RootThreadID,
		nullIfBlank(meta.ParentThreadID),
		nullIfBlank(meta.ParentCallID),
		meta.Depth,
		string(meta.Status),
		meta.Model,
		meta.Instructions,
		requiredJSON(meta.MetadataJSON, "{}"),
		optionalJSON(meta.IncludeJSON),
		optionalJSON(meta.ToolsJSON),
		optionalJSON(meta.ToolChoiceJSON),
		optionalJSON(meta.ReasoningJSON),
		nullIfBlank(meta.OwnerWorkerID),
		int64(meta.SocketGeneration),
		nullIfZeroTime(meta.SocketExpiresAt),
		nullIfBlank(meta.LastResponseID),
		nullIfBlank(meta.ActiveResponseID),
		nullIfBlank(meta.ActiveSpawnGroupID),
		nonZeroTime(meta.CreatedAt),
		nonZeroTime(meta.UpdatedAt),
	)
	if err != nil {
		return fmt.Errorf("persist thread snapshot %s: %w", meta.ID, err)
	}

	return nil
}

func (s *Store) AppendItem(ctx context.Context, entry threadstore.ItemLogEntry, seq int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin thread item tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	_, err = tx.Exec(ctx, `
INSERT INTO thread_items (
    thread_id,
    seq,
    response_id,
    item_type,
    direction,
    payload_json,
    created_at
) VALUES (
    $1,
    $2,
    $3,
    $4,
    $5,
    $6::jsonb,
    $7
)
ON CONFLICT (thread_id, seq) DO NOTHING
`,
		entry.ThreadID,
		seq,
		nullIfBlank(entry.ResponseID),
		entry.ItemType,
		entry.Direction,
		requiredJSON(entry.PayloadJSON, "{}"),
		nonZeroTime(entry.CreatedAt),
	)
	if err != nil {
		return fmt.Errorf("insert thread item: %w", err)
	}

	if strings.TrimSpace(entry.ResponseID) != "" && entry.Direction == "output" {
		if err := upsertThreadResponseLink(ctx, tx, entry.ThreadID, entry.ResponseID, linkKindOwned); err != nil {
			return err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit thread item tx: %w", err)
	}

	return nil
}

func (s *Store) AppendEvent(ctx context.Context, entry threadstore.EventLogEntry, eventSeq int64) error {
	_, err := s.pool.Exec(ctx, `
INSERT INTO thread_events (
    thread_id,
    event_seq,
    socket_generation,
    event_type,
    response_id,
    payload_json,
    created_at
) VALUES (
    $1,
    $2,
    $3,
    $4,
    $5,
    $6::jsonb,
    $7
)
ON CONFLICT (thread_id, event_seq) DO NOTHING
`,
		entry.ThreadID,
		eventSeq,
		int64(entry.SocketGeneration),
		entry.EventType,
		nullIfBlank(entry.ResponseID),
		requiredJSON(entry.PayloadJSON, "{}"),
		nonZeroTime(entry.CreatedAt),
	)
	if err != nil {
		return fmt.Errorf("insert thread event: %w", err)
	}

	return nil
}

func (s *Store) SaveResponseRaw(ctx context.Context, threadID, responseID string, payload json.RawMessage) error {
	if strings.TrimSpace(responseID) == "" || len(payload) == 0 {
		return nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin response tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	_, err = tx.Exec(ctx, `
INSERT INTO responses (
    id,
    source_thread_id,
    status,
    response_json,
    recorded_at
) VALUES (
    $1,
    $2,
    $3,
    $4::jsonb,
    now()
)
ON CONFLICT (id) DO UPDATE SET
    source_thread_id = EXCLUDED.source_thread_id,
    status = EXCLUDED.status,
    response_json = EXCLUDED.response_json,
    recorded_at = now()
`,
		responseID,
		threadID,
		nullIfBlank(parseResponseStatus(payload)),
		string(payload),
	)
	if err != nil {
		return fmt.Errorf("upsert response %s: %w", responseID, err)
	}

	if err := upsertThreadResponseLink(ctx, tx, threadID, responseID, linkKindOwned); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit response tx: %w", err)
	}

	return nil
}

func (s *Store) CreateSpawnGroup(ctx context.Context, meta threadstore.SpawnGroupMeta, childThreadIDs []string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin spawn group tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	_, err = tx.Exec(ctx, `
INSERT INTO spawn_groups (
    id,
    parent_thread_id,
    parent_call_id,
    expected,
    completed,
    failed,
    cancelled,
    status,
    aggregate_submitted_at,
    aggregate_cmd_id,
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
    $12
)
ON CONFLICT (id) DO NOTHING
`,
		meta.ID,
		meta.ParentThreadID,
		meta.ParentCallID,
		meta.Expected,
		meta.Completed,
		meta.Failed,
		meta.Cancelled,
		string(meta.Status),
		nullIfZeroTime(meta.AggregateSubmittedAt),
		nullIfBlank(meta.AggregateCmdID),
		nonZeroTime(meta.CreatedAt),
		nonZeroTime(meta.UpdatedAt),
	)
	if err != nil {
		return fmt.Errorf("insert spawn group %s: %w", meta.ID, err)
	}

	for index, childThreadID := range childThreadIDs {
		_, err = tx.Exec(ctx, `
INSERT INTO spawn_group_children (
    spawn_group_id,
    child_thread_id,
    child_index,
    status,
    created_at,
    updated_at
) VALUES (
    $1,
    $2,
    $3,
    'pending',
    $4,
    $5
)
ON CONFLICT (spawn_group_id, child_thread_id) DO UPDATE SET
    child_index = EXCLUDED.child_index
`,
			meta.ID,
			childThreadID,
			index,
			nonZeroTime(meta.CreatedAt),
			nonZeroTime(meta.UpdatedAt),
		)
		if err != nil {
			return fmt.Errorf("insert spawn child %s: %w", childThreadID, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit spawn group tx: %w", err)
	}

	return nil
}

func (s *Store) SaveSpawnGroup(ctx context.Context, meta threadstore.SpawnGroupMeta) error {
	_, err := s.pool.Exec(ctx, `
INSERT INTO spawn_groups (
    id,
    parent_thread_id,
    parent_call_id,
    expected,
    completed,
    failed,
    cancelled,
    status,
    aggregate_submitted_at,
    aggregate_cmd_id,
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
    $12
)
ON CONFLICT (id) DO UPDATE SET
    parent_thread_id = EXCLUDED.parent_thread_id,
    parent_call_id = EXCLUDED.parent_call_id,
    expected = EXCLUDED.expected,
    completed = EXCLUDED.completed,
    failed = EXCLUDED.failed,
    cancelled = EXCLUDED.cancelled,
    status = EXCLUDED.status,
    aggregate_submitted_at = EXCLUDED.aggregate_submitted_at,
    aggregate_cmd_id = EXCLUDED.aggregate_cmd_id,
    created_at = EXCLUDED.created_at,
    updated_at = EXCLUDED.updated_at
`,
		meta.ID,
		meta.ParentThreadID,
		meta.ParentCallID,
		meta.Expected,
		meta.Completed,
		meta.Failed,
		meta.Cancelled,
		string(meta.Status),
		nullIfZeroTime(meta.AggregateSubmittedAt),
		nullIfBlank(meta.AggregateCmdID),
		nonZeroTime(meta.CreatedAt),
		nonZeroTime(meta.UpdatedAt),
	)
	if err != nil {
		return fmt.Errorf("persist spawn group %s: %w", meta.ID, err)
	}

	return nil
}

func (s *Store) UpsertSpawnResult(ctx context.Context, spawnGroupID string, result threadstore.SpawnChildResult) error {
	_, err := s.pool.Exec(ctx, `
INSERT INTO spawn_group_children (
    spawn_group_id,
    child_thread_id,
    status,
    child_response_id,
    assistant_text,
    result_ref,
    summary_ref,
    error_ref,
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
    $10
)
ON CONFLICT (spawn_group_id, child_thread_id) DO UPDATE SET
    status = EXCLUDED.status,
    child_response_id = EXCLUDED.child_response_id,
    assistant_text = EXCLUDED.assistant_text,
    result_ref = EXCLUDED.result_ref,
    summary_ref = EXCLUDED.summary_ref,
    error_ref = EXCLUDED.error_ref,
    updated_at = EXCLUDED.updated_at
`,
		spawnGroupID,
		result.ChildThreadID,
		result.Status,
		nullIfBlank(result.ChildResponseID),
		nullIfBlank(result.AssistantText),
		nullIfBlank(result.ResultRef),
		nullIfBlank(result.SummaryRef),
		nullIfBlank(result.ErrorRef),
		nonZeroTime(result.UpdatedAt),
		nonZeroTime(result.UpdatedAt),
	)
	if err != nil {
		return fmt.Errorf("upsert spawn result %s/%s: %w", spawnGroupID, result.ChildThreadID, err)
	}

	return nil
}

func (s *Store) LoadThread(ctx context.Context, threadID string) (threadstore.ThreadMeta, error) {
	row := s.pool.QueryRow(ctx, `
SELECT
    id,
    root_thread_id,
    COALESCE(parent_thread_id, ''),
    COALESCE(parent_call_id, ''),
    depth,
    status,
    model,
    instructions,
    COALESCE(metadata_json::text, ''),
    COALESCE(include_json::text, ''),
    COALESCE(tools_json::text, ''),
    COALESCE(tool_choice_json::text, ''),
    COALESCE(reasoning_json::text, ''),
    COALESCE(owner_worker_id, ''),
    socket_generation,
    socket_expires_at,
    COALESCE(last_response_id, ''),
    COALESCE(active_response_id, ''),
    COALESCE(active_spawn_group_id, ''),
    created_at,
    updated_at
FROM threads
WHERE id = $1
`, threadID)

	var meta threadstore.ThreadMeta
	var status string
	var socketGeneration int64
	var socketExpiresAt *time.Time
	if err := row.Scan(
		&meta.ID,
		&meta.RootThreadID,
		&meta.ParentThreadID,
		&meta.ParentCallID,
		&meta.Depth,
		&status,
		&meta.Model,
		&meta.Instructions,
		&meta.MetadataJSON,
		&meta.IncludeJSON,
		&meta.ToolsJSON,
		&meta.ToolChoiceJSON,
		&meta.ReasoningJSON,
		&meta.OwnerWorkerID,
		&socketGeneration,
		&socketExpiresAt,
		&meta.LastResponseID,
		&meta.ActiveResponseID,
		&meta.ActiveSpawnGroupID,
		&meta.CreatedAt,
		&meta.UpdatedAt,
	); err != nil {
		if isNoRows(err) {
			return threadstore.ThreadMeta{}, threadstore.ErrThreadNotFound
		}
		return threadstore.ThreadMeta{}, fmt.Errorf("load thread %s: %w", threadID, err)
	}

	meta.Status = threadstore.ThreadStatus(status)
	meta.SocketGeneration = uint64(maxInt64(socketGeneration))
	if socketExpiresAt != nil {
		meta.SocketExpiresAt = socketExpiresAt.UTC()
	}
	return meta, nil
}

func (s *Store) ListRootThreads(ctx context.Context, limit int64) ([]ThreadListEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 200 {
		limit = 200
	}

	rows, err := s.pool.Query(ctx, `
SELECT
    t.id,
    t.root_thread_id,
    COALESCE(t.parent_thread_id, ''),
    COALESCE(t.parent_call_id, ''),
    t.depth,
    t.status,
    t.model,
    t.instructions,
    COALESCE(t.metadata_json::text, ''),
    COALESCE(t.include_json::text, ''),
    COALESCE(t.tools_json::text, ''),
    COALESCE(t.tool_choice_json::text, ''),
    COALESCE(t.reasoning_json::text, ''),
    COALESCE(t.owner_worker_id, ''),
    t.socket_generation,
    t.socket_expires_at,
    COALESCE(t.last_response_id, ''),
    COALESCE(t.active_response_id, ''),
    COALESCE(t.active_spawn_group_id, ''),
    t.created_at,
    t.updated_at,
    COALESCE(first_item.payload_json, '{}'::jsonb)::text,
    COALESCE(latest_item.payload_json, '{}'::jsonb)::text,
    COALESCE(latest_item.direction, ''),
    latest_item.created_at
FROM threads AS t
LEFT JOIN LATERAL (
    SELECT payload_json
    FROM thread_items
    WHERE thread_id = t.id
      AND item_type = 'message'
      AND direction = 'input'
    ORDER BY seq ASC
    LIMIT 1
) AS first_item ON true
LEFT JOIN LATERAL (
    SELECT payload_json, direction, created_at
    FROM thread_items
    WHERE thread_id = t.id
      AND item_type = 'message'
    ORDER BY seq DESC
    LIMIT 1
) AS latest_item ON true
WHERE t.parent_thread_id IS NULL
ORDER BY t.updated_at DESC, t.id DESC
LIMIT $1
`, limit)
	if err != nil {
		return nil, fmt.Errorf("list root threads: %w", err)
	}
	defer rows.Close()

	entries := make([]ThreadListEntry, 0)
	for rows.Next() {
		var (
			meta              threadstore.ThreadMeta
			status            string
			socketGeneration  int64
			socketExpiresAt   *time.Time
			firstPayloadText  string
			latestPayloadText string
			latestDirection   string
			latestMessageAt   *time.Time
		)
		if err := rows.Scan(
			&meta.ID,
			&meta.RootThreadID,
			&meta.ParentThreadID,
			&meta.ParentCallID,
			&meta.Depth,
			&status,
			&meta.Model,
			&meta.Instructions,
			&meta.MetadataJSON,
			&meta.IncludeJSON,
			&meta.ToolsJSON,
			&meta.ToolChoiceJSON,
			&meta.ReasoningJSON,
			&meta.OwnerWorkerID,
			&socketGeneration,
			&socketExpiresAt,
			&meta.LastResponseID,
			&meta.ActiveResponseID,
			&meta.ActiveSpawnGroupID,
			&meta.CreatedAt,
			&meta.UpdatedAt,
			&firstPayloadText,
			&latestPayloadText,
			&latestDirection,
			&latestMessageAt,
		); err != nil {
			return nil, fmt.Errorf("scan root thread: %w", err)
		}

		meta.Status = threadstore.ThreadStatus(status)
		meta.SocketGeneration = uint64(maxInt64(socketGeneration))
		if socketExpiresAt != nil {
			meta.SocketExpiresAt = socketExpiresAt.UTC()
		}

		entry := ThreadListEntry{
			Meta:               meta,
			FirstMessageText:   extractMessageText(json.RawMessage(firstPayloadText)),
			LatestMessageText:  extractMessageText(json.RawMessage(latestPayloadText)),
			LatestMessageIsOut: latestDirection == "output",
		}
		if latestMessageAt != nil {
			entry.LatestMessageAt = latestMessageAt.UTC()
		}
		entries = append(entries, entry)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate root threads: %w", err)
	}

	return entries, nil
}

func (s *Store) ListItems(ctx context.Context, threadID string, options threadstore.ListOptions) ([]threadstore.ItemRecord, error) {
	limit := normalizeLimit(options.Limit)
	if options.After != "" {
		afterSeq, err := parseSequenceCursor(options.After)
		if err != nil {
			return nil, err
		}
		rows, err := s.pool.Query(ctx, `
SELECT seq, COALESCE(response_id, ''), item_type, direction, payload_json, created_at
FROM thread_items
WHERE thread_id = $1 AND seq > $2
ORDER BY seq ASC
LIMIT $3
`, threadID, afterSeq, limit)
		if err != nil {
			return nil, fmt.Errorf("list thread items after cursor: %w", err)
		}
		defer rows.Close()
		return scanItemRows(rows)
	}

	if options.Before != "" {
		beforeSeq, err := parseSequenceCursor(options.Before)
		if err != nil {
			return nil, err
		}
		rows, err := s.pool.Query(ctx, `
SELECT seq, COALESCE(response_id, ''), item_type, direction, payload_json, created_at
FROM (
    SELECT seq, response_id, item_type, direction, payload_json, created_at
    FROM thread_items
    WHERE thread_id = $1 AND seq < $2
    ORDER BY seq DESC
    LIMIT $3
) AS page
ORDER BY seq ASC
`, threadID, beforeSeq, limit)
		if err != nil {
			return nil, fmt.Errorf("list thread items before cursor: %w", err)
		}
		defer rows.Close()
		return scanItemRows(rows)
	}

	rows, err := s.pool.Query(ctx, `
SELECT seq, COALESCE(response_id, ''), item_type, direction, payload_json, created_at
FROM (
    SELECT seq, response_id, item_type, direction, payload_json, created_at
    FROM thread_items
    WHERE thread_id = $1
    ORDER BY seq DESC
    LIMIT $2
) AS page
ORDER BY seq ASC
`, threadID, limit)
	if err != nil {
		return nil, fmt.Errorf("list thread items: %w", err)
	}
	defer rows.Close()
	return scanItemRows(rows)
}

func extractMessageText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	var payload struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text,omitempty"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ""
	}

	parts := make([]string, 0, len(payload.Content))
	for _, content := range payload.Content {
		switch content.Type {
		case "input_text", "output_text":
			if trimmed := strings.TrimSpace(content.Text); trimmed != "" {
				parts = append(parts, trimmed)
			}
		}
	}

	return strings.Join(parts, "\n\n")
}

func (s *Store) ListEvents(ctx context.Context, threadID string, options threadstore.ListOptions) ([]threadstore.EventRecord, error) {
	limit := normalizeLimit(options.Limit)
	if options.After != "" {
		afterSeq, err := parseSequenceCursor(options.After)
		if err != nil {
			return nil, err
		}
		rows, err := s.pool.Query(ctx, `
SELECT event_seq, socket_generation, event_type, COALESCE(response_id, ''), payload_json, created_at
FROM thread_events
WHERE thread_id = $1 AND event_seq > $2
ORDER BY event_seq ASC
LIMIT $3
`, threadID, afterSeq, limit)
		if err != nil {
			return nil, fmt.Errorf("list thread events after cursor: %w", err)
		}
		defer rows.Close()
		return scanEventRows(rows)
	}

	if options.Before != "" {
		beforeSeq, err := parseSequenceCursor(options.Before)
		if err != nil {
			return nil, err
		}
		rows, err := s.pool.Query(ctx, `
SELECT event_seq, socket_generation, event_type, COALESCE(response_id, ''), payload_json, created_at
FROM (
    SELECT event_seq, socket_generation, event_type, response_id, payload_json, created_at
    FROM thread_events
    WHERE thread_id = $1 AND event_seq < $2
    ORDER BY event_seq DESC
    LIMIT $3
) AS page
ORDER BY event_seq ASC
`, threadID, beforeSeq, limit)
		if err != nil {
			return nil, fmt.Errorf("list thread events before cursor: %w", err)
		}
		defer rows.Close()
		return scanEventRows(rows)
	}

	rows, err := s.pool.Query(ctx, `
SELECT event_seq, socket_generation, event_type, COALESCE(response_id, ''), payload_json, created_at
FROM (
    SELECT event_seq, socket_generation, event_type, response_id, payload_json, created_at
    FROM thread_events
    WHERE thread_id = $1
    ORDER BY event_seq DESC
    LIMIT $2
) AS page
ORDER BY event_seq ASC
`, threadID, limit)
	if err != nil {
		return nil, fmt.Errorf("list thread events: %w", err)
	}
	defer rows.Close()
	return scanEventRows(rows)
}

func (s *Store) ThreadHasResponse(ctx context.Context, threadID, responseID string) (bool, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM thread_response_links
    WHERE thread_id = $1 AND response_id = $2
)
`, threadID, responseID).Scan(&exists); err != nil {
		return false, fmt.Errorf("check thread response link %s/%s: %w", threadID, responseID, err)
	}

	return exists, nil
}

func (s *Store) LoadResponseRaw(ctx context.Context, responseID string) (json.RawMessage, error) {
	var raw []byte
	if err := s.pool.QueryRow(ctx, `
SELECT response_json
FROM responses
WHERE id = $1
`, responseID).Scan(&raw); err != nil {
		if isNoRows(err) {
			return nil, threadstore.ErrThreadNotFound
		}
		return nil, fmt.Errorf("load response %s: %w", responseID, err)
	}

	return json.RawMessage(raw), nil
}

func (s *Store) LoadSpawnGroup(ctx context.Context, spawnGroupID string) (threadstore.SpawnGroupMeta, error) {
	row := s.pool.QueryRow(ctx, `
SELECT
    id,
    parent_thread_id,
    parent_call_id,
    expected,
    completed,
    failed,
    cancelled,
    status,
    aggregate_submitted_at,
    COALESCE(aggregate_cmd_id, ''),
    created_at,
    updated_at
FROM spawn_groups
WHERE id = $1
`, spawnGroupID)

	var meta threadstore.SpawnGroupMeta
	var status string
	var aggregateSubmittedAt *time.Time
	if err := row.Scan(
		&meta.ID,
		&meta.ParentThreadID,
		&meta.ParentCallID,
		&meta.Expected,
		&meta.Completed,
		&meta.Failed,
		&meta.Cancelled,
		&status,
		&aggregateSubmittedAt,
		&meta.AggregateCmdID,
		&meta.CreatedAt,
		&meta.UpdatedAt,
	); err != nil {
		if isNoRows(err) {
			return threadstore.SpawnGroupMeta{}, threadstore.ErrThreadNotFound
		}
		return threadstore.SpawnGroupMeta{}, fmt.Errorf("load spawn group %s: %w", spawnGroupID, err)
	}

	meta.Status = threadstore.SpawnGroupStatus(status)
	if aggregateSubmittedAt != nil {
		meta.AggregateSubmittedAt = aggregateSubmittedAt.UTC()
	}
	return meta, nil
}

func (s *Store) ListSpawnGroupsByParent(ctx context.Context, parentThreadID string) ([]threadstore.SpawnGroupMeta, error) {
	rows, err := s.pool.Query(ctx, `
SELECT
    id,
    parent_thread_id,
    parent_call_id,
    expected,
    completed,
    failed,
    cancelled,
    status,
    aggregate_submitted_at,
    COALESCE(aggregate_cmd_id, ''),
    created_at,
    updated_at
FROM spawn_groups
WHERE parent_thread_id = $1
ORDER BY created_at ASC, id ASC
`, parentThreadID)
	if err != nil {
		return nil, fmt.Errorf("list spawn groups by parent %s: %w", parentThreadID, err)
	}
	defer rows.Close()

	var groups []threadstore.SpawnGroupMeta
	for rows.Next() {
		var meta threadstore.SpawnGroupMeta
		var status string
		var aggregateSubmittedAt *time.Time
		if err := rows.Scan(
			&meta.ID,
			&meta.ParentThreadID,
			&meta.ParentCallID,
			&meta.Expected,
			&meta.Completed,
			&meta.Failed,
			&meta.Cancelled,
			&status,
			&aggregateSubmittedAt,
			&meta.AggregateCmdID,
			&meta.CreatedAt,
			&meta.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan spawn group row: %w", err)
		}
		meta.Status = threadstore.SpawnGroupStatus(status)
		if aggregateSubmittedAt != nil {
			meta.AggregateSubmittedAt = aggregateSubmittedAt.UTC()
		}
		groups = append(groups, meta)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate spawn groups: %w", err)
	}

	return groups, nil
}

func (s *Store) LoadSpawnGroupChildThreadIDs(ctx context.Context, spawnGroupID string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
SELECT child_thread_id
FROM spawn_group_children
WHERE spawn_group_id = $1
ORDER BY child_index ASC NULLS LAST, child_thread_id ASC
`, spawnGroupID)
	if err != nil {
		return nil, fmt.Errorf("list spawn group children %s: %w", spawnGroupID, err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan spawn child id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate spawn child ids: %w", err)
	}

	return ids, nil
}

func (s *Store) ListSpawnResults(ctx context.Context, spawnGroupID string) ([]threadstore.SpawnChildResult, error) {
	rows, err := s.pool.Query(ctx, `
SELECT
    child_thread_id,
    status,
    COALESCE(child_response_id, ''),
    COALESCE(assistant_text, ''),
    COALESCE(result_ref, ''),
    COALESCE(summary_ref, ''),
    COALESCE(error_ref, ''),
    updated_at
FROM spawn_group_children
WHERE spawn_group_id = $1 AND status <> 'pending'
ORDER BY updated_at ASC, child_thread_id ASC
`, spawnGroupID)
	if err != nil {
		return nil, fmt.Errorf("list spawn results %s: %w", spawnGroupID, err)
	}
	defer rows.Close()

	var results []threadstore.SpawnChildResult
	for rows.Next() {
		var result threadstore.SpawnChildResult
		if err := rows.Scan(
			&result.ChildThreadID,
			&result.Status,
			&result.ChildResponseID,
			&result.AssistantText,
			&result.ResultRef,
			&result.SummaryRef,
			&result.ErrorRef,
			&result.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan spawn result row: %w", err)
		}
		results = append(results, result)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate spawn results: %w", err)
	}

	return results, nil
}

type pgxExec interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

func upsertThreadResponseLink(ctx context.Context, db pgxExec, threadID, responseID, linkKind string) error {
	_, err := db.Exec(ctx, `
INSERT INTO thread_response_links (
    thread_id,
    response_id,
    link_kind,
    created_at
) VALUES (
    $1,
    $2,
    $3,
    now()
)
ON CONFLICT (thread_id, response_id) DO UPDATE SET
    link_kind = EXCLUDED.link_kind
`,
		threadID,
		responseID,
		linkKind,
	)
	if err != nil {
		return fmt.Errorf("upsert thread response link %s/%s: %w", threadID, responseID, err)
	}

	return nil
}

func parseResponseStatus(payload json.RawMessage) string {
	var response struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(payload, &response); err != nil {
		return ""
	}
	return strings.TrimSpace(response.Status)
}

func optionalJSON(raw string) any {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	return raw
}

func requiredJSON(raw, fallback string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	return raw
}

func nullIfBlank(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func nullIfZeroTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value.UTC()
}

func nonZeroTime(value time.Time) time.Time {
	if value.IsZero() {
		return time.Now().UTC()
	}
	return value.UTC()
}

func isNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}

func parseSequenceCursor(raw string) (int64, error) {
	value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("cursor must be a numeric sequence")
	}
	if value <= 0 {
		return 0, fmt.Errorf("cursor must be greater than zero")
	}
	return value, nil
}

func normalizeLimit(limit int64) int64 {
	if limit <= 0 {
		return 100
	}
	return limit
}

func scanItemRows(rows pgxRows) ([]threadstore.ItemRecord, error) {
	var items []threadstore.ItemRecord
	for rows.Next() {
		var record threadstore.ItemRecord
		var payload []byte
		if err := rows.Scan(&record.Seq, &record.ResponseID, &record.ItemType, &record.Direction, &payload, &record.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan thread item row: %w", err)
		}
		record.Payload = json.RawMessage(payload)
		items = append(items, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate thread item rows: %w", err)
	}
	return items, nil
}

func scanEventRows(rows pgxRows) ([]threadstore.EventRecord, error) {
	var events []threadstore.EventRecord
	for rows.Next() {
		var record threadstore.EventRecord
		var payload []byte
		var socketGeneration int64
		if err := rows.Scan(&record.EventSeq, &socketGeneration, &record.EventType, &record.ResponseID, &payload, &record.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan thread event row: %w", err)
		}
		record.SocketGeneration = uint64(maxInt64(socketGeneration))
		record.Payload = json.RawMessage(payload)
		events = append(events, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate thread event rows: %w", err)
	}
	return events, nil
}

type pgxRows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close()
}

func maxInt64(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}
