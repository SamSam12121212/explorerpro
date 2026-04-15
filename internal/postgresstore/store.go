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

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func shouldPersistThreadEvent(eventType string) bool {
	eventType = strings.TrimSpace(eventType)
	if eventType == "" {
		return false
	}
	return !strings.HasSuffix(eventType, ".delta")
}

func formatDocumentMetadataID(id int64) string {
	return strconv.FormatInt(id, 10)
}

func (s *Store) ReserveThreadID(ctx context.Context) (int64, error) {
	var threadID int64
	if err := s.pool.QueryRow(ctx, `
SELECT nextval(pg_get_serial_sequence('threads', 'id'))
`).Scan(&threadID); err != nil {
		return 0, fmt.Errorf("reserve thread id: %w", err)
	}
	return threadID, nil
}

func (s *Store) ReserveSpawnGroupID(ctx context.Context) (int64, error) {
	var spawnGroupID int64
	if err := s.pool.QueryRow(ctx, `
SELECT nextval(pg_get_serial_sequence('spawn_groups', 'id'))
`).Scan(&spawnGroupID); err != nil {
		return 0, fmt.Errorf("reserve spawn group id: %w", err)
	}
	return spawnGroupID, nil
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
    child_kind,
    document_id,
    document_phase,
    archived_at,
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
    $21,
    $22,
    $23,
    $24,
    $25
)
ON CONFLICT (id) DO NOTHING
`,
		meta.ID,
		meta.RootThreadID,
		nullIfZeroInt64(meta.ParentThreadID),
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
		nullIfZeroInt64(meta.OwnerWorkerID),
		int64(meta.SocketGeneration),
		nullIfZeroTime(meta.SocketExpiresAt),
		nullIfBlank(meta.LastResponseID),
		nullIfBlank(meta.ActiveResponseID),
		nullIfZeroInt64(meta.ActiveSpawnGroupID),
		nullIfBlank(meta.ChildKind),
		nullIfZeroInt64(meta.DocumentID),
		nullIfBlank(meta.DocumentPhase),
		nullIfZeroTime(meta.ArchivedAt),
		nonZeroTime(meta.CreatedAt),
		nonZeroTime(meta.UpdatedAt),
	)
	if err != nil {
		return fmt.Errorf("persist thread create %d: %w", meta.ID, err)
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
    child_kind,
    document_id,
    document_phase,
    archived_at,
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
    $21,
    $22,
    $23,
    $24,
    $25
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
    child_kind = EXCLUDED.child_kind,
    document_id = EXCLUDED.document_id,
    document_phase = EXCLUDED.document_phase,
    archived_at = COALESCE(EXCLUDED.archived_at, threads.archived_at),
    created_at = EXCLUDED.created_at,
    updated_at = EXCLUDED.updated_at
`,
		meta.ID,
		meta.RootThreadID,
		nullIfZeroInt64(meta.ParentThreadID),
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
		nullIfZeroInt64(meta.OwnerWorkerID),
		int64(meta.SocketGeneration),
		nullIfZeroTime(meta.SocketExpiresAt),
		nullIfBlank(meta.LastResponseID),
		nullIfBlank(meta.ActiveResponseID),
		nullIfZeroInt64(meta.ActiveSpawnGroupID),
		nullIfBlank(meta.ChildKind),
		nullIfZeroInt64(meta.DocumentID),
		nullIfBlank(meta.DocumentPhase),
		nullIfZeroTime(meta.ArchivedAt),
		nonZeroTime(meta.CreatedAt),
		nonZeroTime(meta.UpdatedAt),
	)
	if err != nil {
		return fmt.Errorf("persist thread snapshot %d: %w", meta.ID, err)
	}

	return nil
}

func (s *Store) CommandProcessed(ctx context.Context, threadID int64, cmdID int64) (bool, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM thread_processed_commands
    WHERE thread_id = $1 AND cmd_id = $2
)
`, threadID, cmdID).Scan(&exists); err != nil {
		return false, fmt.Errorf("check processed command %d/%d: %w", threadID, cmdID, err)
	}
	return exists, nil
}

func (s *Store) MarkCommandProcessed(ctx context.Context, threadID int64, cmdID int64) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
INSERT INTO thread_processed_commands (
    thread_id,
    cmd_id,
    processed_at
) VALUES (
    $1,
    $2,
    now()
)
ON CONFLICT (thread_id, cmd_id) DO NOTHING
`, threadID, cmdID)
	if err != nil {
		return false, fmt.Errorf("mark processed command %d/%d: %w", threadID, cmdID, err)
	}
	return tag.RowsAffected() == 1, nil
}

func (s *Store) ClaimOwnership(ctx context.Context, threadID int64, workerID int64, leaseUntil time.Time) (threadstore.ClaimResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return threadstore.ClaimResult{}, fmt.Errorf("begin claim ownership tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	meta, err := s.loadThreadForUpdate(ctx, tx, threadID)
	if err != nil {
		return threadstore.ClaimResult{}, err
	}

	owner, found, err := s.loadOwnerForUpdate(ctx, tx, threadID)
	if err != nil {
		return threadstore.ClaimResult{}, err
	}

	now := time.Now().UTC()
	currentWorkerID := meta.OwnerWorkerID
	currentGeneration := meta.SocketGeneration
	if found {
		if owner.WorkerID > 0 {
			currentWorkerID = owner.WorkerID
		}
		if owner.SocketGeneration > 0 {
			currentGeneration = owner.SocketGeneration
		}
	}

	if currentWorkerID > 0 && currentWorkerID != workerID && found && owner.LeaseUntil.After(now) {
		return threadstore.ClaimResult{
			Claimed:          false,
			SocketGeneration: currentGeneration,
			PreviousWorkerID: currentWorkerID,
		}, nil
	}

	newGeneration := currentGeneration
	if currentWorkerID != workerID || newGeneration == 0 {
		newGeneration++
		if newGeneration == 0 {
			newGeneration = 1
		}
	}

	claimedAt := now
	if found && owner.WorkerID == workerID && !owner.ClaimedAt.IsZero() {
		claimedAt = owner.ClaimedAt
	}

	if _, err := tx.Exec(ctx, `
INSERT INTO thread_owners (
    thread_id,
    worker_id,
    lease_until,
    socket_generation,
    claimed_at,
    updated_at
) VALUES (
    $1,
    $2,
    $3,
    $4,
    $5,
    $6
)
ON CONFLICT (thread_id) DO UPDATE SET
    worker_id = EXCLUDED.worker_id,
    lease_until = EXCLUDED.lease_until,
    socket_generation = EXCLUDED.socket_generation,
    claimed_at = EXCLUDED.claimed_at,
    updated_at = EXCLUDED.updated_at
`, threadID, workerID, nonZeroTime(leaseUntil), int64(newGeneration), claimedAt, now); err != nil {
		return threadstore.ClaimResult{}, fmt.Errorf("upsert thread owner %d: %w", threadID, err)
	}

	meta.OwnerWorkerID = workerID
	meta.SocketGeneration = newGeneration
	meta.UpdatedAt = now
	if err := s.saveThreadTx(ctx, tx, meta); err != nil {
		return threadstore.ClaimResult{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return threadstore.ClaimResult{}, fmt.Errorf("commit claim ownership tx: %w", err)
	}

	return threadstore.ClaimResult{
		Claimed:          true,
		SocketGeneration: newGeneration,
		PreviousWorkerID: currentWorkerID,
	}, nil
}

func (s *Store) RenewOwnership(ctx context.Context, threadID int64, workerID int64, socketGeneration uint64, leaseUntil time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
UPDATE thread_owners
SET lease_until = $4,
    updated_at = now()
WHERE thread_id = $1
  AND worker_id = $2
  AND socket_generation = $3
`, threadID, workerID, int64(socketGeneration), nonZeroTime(leaseUntil))
	if err != nil {
		return false, fmt.Errorf("renew thread owner %d: %w", threadID, err)
	}
	return tag.RowsAffected() == 1, nil
}

func (s *Store) LoadOwner(ctx context.Context, threadID int64) (threadstore.OwnerRecord, error) {
	row := s.pool.QueryRow(ctx, `
SELECT
    worker_id,
    socket_generation,
    lease_until,
    claimed_at,
    updated_at
FROM thread_owners
WHERE thread_id = $1
`, threadID)

	var owner threadstore.OwnerRecord
	var socketGeneration int64
	if err := row.Scan(
		&owner.WorkerID,
		&socketGeneration,
		&owner.LeaseUntil,
		&owner.ClaimedAt,
		&owner.UpdatedAt,
	); err != nil {
		if isNoRows(err) {
			return threadstore.OwnerRecord{}, threadstore.ErrThreadNotFound
		}
		return threadstore.OwnerRecord{}, fmt.Errorf("load thread owner %d: %w", threadID, err)
	}

	owner.SocketGeneration = uint64(maxInt64(socketGeneration))
	owner.LeaseUntil = owner.LeaseUntil.UTC()
	owner.ClaimedAt = owner.ClaimedAt.UTC()
	owner.UpdatedAt = owner.UpdatedAt.UTC()
	return owner, nil
}

func (s *Store) ListThreadIDsByStatus(ctx context.Context, status threadstore.ThreadStatus) ([]int64, error) {
	rows, err := s.pool.Query(ctx, `
SELECT id
FROM threads
WHERE status = $1
ORDER BY id ASC
`, string(status))
	if err != nil {
		return nil, fmt.Errorf("list thread ids by status %s: %w", status, err)
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan thread id by status: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate thread ids by status: %w", err)
	}
	return ids, nil
}

func (s *Store) RotateOwnership(ctx context.Context, threadID int64, workerID int64, currentGeneration uint64, leaseUntil, socketExpiresAt time.Time) (uint64, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("begin rotate ownership tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	meta, err := s.loadThreadForUpdate(ctx, tx, threadID)
	if err != nil {
		return 0, false, err
	}
	owner, found, err := s.loadOwnerForUpdate(ctx, tx, threadID)
	if err != nil {
		return 0, false, err
	}
	if !found || owner.WorkerID != workerID || owner.SocketGeneration != currentGeneration || meta.SocketGeneration != currentGeneration {
		return 0, false, nil
	}

	newGeneration := currentGeneration + 1
	if newGeneration == 0 {
		newGeneration = 1
	}
	now := time.Now().UTC()

	if _, err := tx.Exec(ctx, `
UPDATE thread_owners
SET worker_id = $2,
    lease_until = $3,
    socket_generation = $4,
    updated_at = $5
WHERE thread_id = $1
`, threadID, workerID, nonZeroTime(leaseUntil), int64(newGeneration), now); err != nil {
		return 0, false, fmt.Errorf("update rotated owner %d: %w", threadID, err)
	}

	meta.OwnerWorkerID = workerID
	meta.SocketGeneration = newGeneration
	meta.SocketExpiresAt = socketExpiresAt.UTC()
	meta.UpdatedAt = now
	if err := s.saveThreadTx(ctx, tx, meta); err != nil {
		return 0, false, err
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, false, fmt.Errorf("commit rotate ownership tx: %w", err)
	}

	return newGeneration, true, nil
}

func (s *Store) ReleaseOwnership(ctx context.Context, threadID int64, workerID int64, socketGeneration uint64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin release ownership tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	meta, err := s.loadThreadForUpdate(ctx, tx, threadID)
	if err != nil {
		if errors.Is(err, threadstore.ErrThreadNotFound) {
			return nil
		}
		return err
	}

	owner, found, err := s.loadOwnerForUpdate(ctx, tx, threadID)
	if err != nil {
		return err
	}
	if !found || owner.WorkerID != workerID || owner.SocketGeneration != socketGeneration {
		return nil
	}

	if _, err := tx.Exec(ctx, `
DELETE FROM thread_owners
WHERE thread_id = $1
`, threadID); err != nil {
		return fmt.Errorf("delete thread owner %d: %w", threadID, err)
	}

	meta.OwnerWorkerID = 0
	meta.ActiveResponseID = ""
	meta.SocketExpiresAt = time.Time{}
	meta.UpdatedAt = time.Now().UTC()
	if err := s.saveThreadTx(ctx, tx, meta); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit release ownership tx: %w", err)
	}
	return nil
}

func (s *Store) AppendItem(ctx context.Context, entry threadstore.ItemLogEntry) (threadstore.ItemRecord, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return threadstore.ItemRecord{}, fmt.Errorf("begin thread item tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	seq, err := s.nextThreadItemSeq(ctx, tx, entry.ThreadID)
	if err != nil {
		return threadstore.ItemRecord{}, err
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now().UTC()
	}

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
		return threadstore.ItemRecord{}, fmt.Errorf("insert thread item: %w", err)
	}

	if strings.TrimSpace(entry.ResponseID) != "" && entry.Direction == "output" {
		if err := upsertThreadResponseLink(ctx, tx, entry.ThreadID, entry.ResponseID, linkKindOwned); err != nil {
			return threadstore.ItemRecord{}, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return threadstore.ItemRecord{}, fmt.Errorf("commit thread item tx: %w", err)
	}

	return threadstore.ItemRecord{
		Seq:        seq,
		ResponseID: entry.ResponseID,
		ItemType:   entry.ItemType,
		Direction:  entry.Direction,
		Payload:    json.RawMessage(entry.PayloadJSON),
		CreatedAt:  entry.CreatedAt,
	}, nil
}

func (s *Store) AppendEvent(ctx context.Context, entry threadstore.EventLogEntry, eventSeq int64) error {
	// Delta events are live-only UI telemetry for now. Keep them in NATS and
	// avoid persisting them into Postgres until we need historical replay
	// outside the runtime store.
	if !shouldPersistThreadEvent(entry.EventType) {
		return nil
	}

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

func (s *Store) SaveResponseRaw(ctx context.Context, threadID int64, responseID string, payload json.RawMessage) error {
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

func (s *Store) CreateSpawnGroup(ctx context.Context, meta threadstore.SpawnGroupMeta, childThreadIDs []int64) error {
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
    group_kind,
    stable_key,
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
    $12,
    $13,
    $14
)
ON CONFLICT (id) DO NOTHING
`,
		meta.ID,
		meta.ParentThreadID,
		meta.ParentCallID,
		meta.GroupKind,
		nullIfBlank(meta.StableKey),
		meta.Expected,
		meta.Completed,
		meta.Failed,
		meta.Cancelled,
		string(meta.Status),
		nullIfZeroTime(meta.AggregateSubmittedAt),
		nullIfZeroInt64(meta.AggregateCmdID),
		nonZeroTime(meta.CreatedAt),
		nonZeroTime(meta.UpdatedAt),
	)
	if err != nil {
		return fmt.Errorf("insert spawn group %d: %w", meta.ID, err)
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
			return fmt.Errorf("insert spawn child %d: %w", childThreadID, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit spawn group tx: %w", err)
	}

	return nil
}

func (s *Store) LoadOrCreateDocumentQuerySpawnGroup(ctx context.Context, meta threadstore.SpawnGroupMeta) (threadstore.SpawnGroupMeta, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return threadstore.SpawnGroupMeta{}, fmt.Errorf("begin document query spawn group tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	row := tx.QueryRow(ctx, `
INSERT INTO spawn_groups (
    parent_thread_id,
    parent_call_id,
    group_kind,
    stable_key,
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
    'document_query',
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
ON CONFLICT (parent_thread_id, group_kind, stable_key) WHERE stable_key IS NOT NULL DO NOTHING
RETURNING
    id,
    parent_thread_id,
    parent_call_id,
    group_kind,
    COALESCE(stable_key, ''),
    expected,
    completed,
    failed,
    cancelled,
    status,
    aggregate_submitted_at,
    COALESCE(aggregate_cmd_id, 0),
    created_at,
    updated_at
`,
		meta.ParentThreadID,
		meta.ParentCallID,
		meta.StableKey,
		meta.Expected,
		meta.Completed,
		meta.Failed,
		meta.Cancelled,
		string(meta.Status),
		nullIfZeroTime(meta.AggregateSubmittedAt),
		nullIfZeroInt64(meta.AggregateCmdID),
		nonZeroTime(meta.CreatedAt),
		nonZeroTime(meta.UpdatedAt),
	)

	resolved, err := scanSpawnGroupScanner(row)
	if err != nil {
		if !isNoRows(err) {
			return threadstore.SpawnGroupMeta{}, fmt.Errorf("insert document query spawn group for thread %d: %w", meta.ParentThreadID, err)
		}

		resolved, err = loadDocumentQuerySpawnGroupTx(ctx, tx, meta.ParentThreadID, meta.StableKey)
		if err != nil {
			return threadstore.SpawnGroupMeta{}, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return threadstore.SpawnGroupMeta{}, fmt.Errorf("commit document query spawn group tx: %w", err)
	}

	return resolved, nil
}

func (s *Store) SaveSpawnGroup(ctx context.Context, meta threadstore.SpawnGroupMeta) error {
	_, err := s.pool.Exec(ctx, `
INSERT INTO spawn_groups (
    id,
    parent_thread_id,
    parent_call_id,
    group_kind,
    stable_key,
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
    $12,
    $13,
    $14
)
ON CONFLICT (id) DO UPDATE SET
    parent_thread_id = EXCLUDED.parent_thread_id,
    parent_call_id = EXCLUDED.parent_call_id,
    group_kind = EXCLUDED.group_kind,
    stable_key = EXCLUDED.stable_key,
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
		meta.GroupKind,
		nullIfBlank(meta.StableKey),
		meta.Expected,
		meta.Completed,
		meta.Failed,
		meta.Cancelled,
		string(meta.Status),
		nullIfZeroTime(meta.AggregateSubmittedAt),
		nullIfZeroInt64(meta.AggregateCmdID),
		nonZeroTime(meta.CreatedAt),
		nonZeroTime(meta.UpdatedAt),
	)
	if err != nil {
		return fmt.Errorf("persist spawn group %d: %w", meta.ID, err)
	}

	return nil
}

func (s *Store) UpsertSpawnResult(ctx context.Context, spawnGroupID int64, result threadstore.SpawnChildResult) (bool, []threadstore.SpawnChildResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, nil, fmt.Errorf("begin spawn result tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	var existingStatus string
	err = tx.QueryRow(ctx, `
SELECT COALESCE(status, '')
FROM spawn_group_children
WHERE spawn_group_id = $1 AND child_thread_id = $2
FOR UPDATE
`, spawnGroupID, result.ChildThreadID).Scan(&existingStatus)
	if err != nil && !isNoRows(err) {
		return false, nil, fmt.Errorf("load spawn result %d/%d: %w", spawnGroupID, result.ChildThreadID, err)
	}
	if existingStatus != "" && existingStatus != "pending" {
		results, listErr := listSpawnResultsTx(ctx, tx, spawnGroupID)
		if listErr != nil {
			return false, nil, listErr
		}
		if err := tx.Commit(ctx); err != nil {
			return false, nil, fmt.Errorf("commit existing spawn result tx: %w", err)
		}
		return false, results, nil
	}

	if result.UpdatedAt.IsZero() {
		result.UpdatedAt = time.Now().UTC()
	}

	_, err = tx.Exec(ctx, `
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
		return false, nil, fmt.Errorf("upsert spawn result %d/%d: %w", spawnGroupID, result.ChildThreadID, err)
	}

	results, err := listSpawnResultsTx(ctx, tx, spawnGroupID)
	if err != nil {
		return false, nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return false, nil, fmt.Errorf("commit spawn result tx: %w", err)
	}

	return true, results, nil
}

func (s *Store) LoadThread(ctx context.Context, threadID int64) (threadstore.ThreadMeta, error) {
	row := s.pool.QueryRow(ctx, `
SELECT
    id,
    root_thread_id,
    COALESCE(parent_thread_id, 0),
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
    COALESCE(owner_worker_id, 0),
    socket_generation,
    socket_expires_at,
    COALESCE(last_response_id, ''),
    COALESCE(active_response_id, ''),
    COALESCE(active_spawn_group_id, 0),
    COALESCE(child_kind, ''),
    COALESCE(document_id, 0),
    COALESCE(document_phase, ''),
    archived_at,
    created_at,
    updated_at
FROM threads
WHERE id = $1
`, threadID)

	var meta threadstore.ThreadMeta
	var status string
	var socketGeneration int64
	var socketExpiresAt *time.Time
	var archivedAt *time.Time
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
		&meta.ChildKind,
		&meta.DocumentID,
		&meta.DocumentPhase,
		&archivedAt,
		&meta.CreatedAt,
		&meta.UpdatedAt,
	); err != nil {
		if isNoRows(err) {
			return threadstore.ThreadMeta{}, threadstore.ErrThreadNotFound
		}
		return threadstore.ThreadMeta{}, fmt.Errorf("load thread %d: %w", threadID, err)
	}

	meta.Status = threadstore.ThreadStatus(status)
	meta.SocketGeneration = uint64(maxInt64(socketGeneration))
	if socketExpiresAt != nil {
		meta.SocketExpiresAt = socketExpiresAt.UTC()
	}
	if archivedAt != nil {
		meta.ArchivedAt = archivedAt.UTC()
	}
	return meta, nil
}

func (s *Store) LoadReusableDocumentChildThread(ctx context.Context, parentThreadID, documentID int64) (threadstore.ThreadMeta, error) {
	row := s.pool.QueryRow(ctx, `
SELECT
    id,
    root_thread_id,
    COALESCE(parent_thread_id, 0),
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
    COALESCE(owner_worker_id, 0),
    socket_generation,
    socket_expires_at,
    COALESCE(last_response_id, ''),
    COALESCE(active_response_id, ''),
    COALESCE(active_spawn_group_id, 0),
    COALESCE(child_kind, ''),
    COALESCE(document_id, 0),
    COALESCE(document_phase, ''),
    archived_at,
    created_at,
    updated_at
FROM threads
WHERE parent_thread_id = $1
  AND child_kind = 'document'
  AND document_id = $2
LIMIT 1
`, parentThreadID, documentID)

	var meta threadstore.ThreadMeta
	var status string
	var socketGeneration int64
	var socketExpiresAt *time.Time
	var archivedAt *time.Time
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
		&meta.ChildKind,
		&meta.DocumentID,
		&meta.DocumentPhase,
		&archivedAt,
		&meta.CreatedAt,
		&meta.UpdatedAt,
	); err != nil {
		if isNoRows(err) {
			return threadstore.ThreadMeta{}, threadstore.ErrThreadNotFound
		}
		return threadstore.ThreadMeta{}, fmt.Errorf("load reusable document child thread for %d/%d: %w", parentThreadID, documentID, err)
	}

	meta.Status = threadstore.ThreadStatus(status)
	meta.SocketGeneration = uint64(maxInt64(socketGeneration))
	if socketExpiresAt != nil {
		meta.SocketExpiresAt = socketExpiresAt.UTC()
	}
	if archivedAt != nil {
		meta.ArchivedAt = archivedAt.UTC()
	}
	return meta, nil
}

func (s *Store) LoadLatestCompletedDocumentQueryLineage(ctx context.Context, parentThreadID int64, documentID int64) (threadstore.DocumentQueryLineage, error) {
	row := s.pool.QueryRow(ctx, `
SELECT
    id,
    COALESCE(last_response_id, ''),
    model
FROM threads
WHERE parent_thread_id = $1
  AND child_kind = 'document'
  AND document_id = $2
  AND document_phase = 'query'
  AND status IN ('completed', 'ready')
  AND last_response_id IS NOT NULL
ORDER BY updated_at DESC, created_at DESC, id DESC
LIMIT 1
`, parentThreadID, documentID)

	var lineage threadstore.DocumentQueryLineage
	if err := row.Scan(&lineage.ChildThreadID, &lineage.ResponseID, &lineage.Model); err != nil {
		if isNoRows(err) {
			return threadstore.DocumentQueryLineage{}, threadstore.ErrThreadNotFound
		}
		return threadstore.DocumentQueryLineage{}, fmt.Errorf("load latest completed document query lineage for %d/%d: %w", parentThreadID, documentID, err)
	}

	return lineage, nil
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
    COALESCE(t.parent_thread_id, 0),
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
    COALESCE(t.owner_worker_id, 0),
    t.socket_generation,
    t.socket_expires_at,
    COALESCE(t.last_response_id, ''),
    COALESCE(t.active_response_id, ''),
    COALESCE(t.active_spawn_group_id, 0),
    COALESCE(t.child_kind, ''),
    COALESCE(t.document_id, 0),
    COALESCE(t.document_phase, ''),
    t.archived_at,
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
  AND t.archived_at IS NULL
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
			archivedAt        *time.Time
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
			&meta.ChildKind,
			&meta.DocumentID,
			&meta.DocumentPhase,
			&archivedAt,
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
		if archivedAt != nil {
			meta.ArchivedAt = archivedAt.UTC()
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

func (s *Store) ArchiveRootThread(ctx context.Context, threadID int64) (threadstore.ThreadMeta, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return threadstore.ThreadMeta{}, fmt.Errorf("begin archive thread tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	meta, err := s.loadThreadForUpdate(ctx, tx, threadID)
	if err != nil {
		return threadstore.ThreadMeta{}, err
	}
	if meta.ParentThreadID > 0 {
		return threadstore.ThreadMeta{}, threadstore.ErrThreadNotRoot
	}

	if meta.ArchivedAt.IsZero() {
		now := time.Now().UTC()
		if _, err := tx.Exec(ctx, `
UPDATE threads
SET archived_at = $2,
    updated_at = $2
WHERE id = $1
`, threadID, now); err != nil {
			return threadstore.ThreadMeta{}, fmt.Errorf("archive thread %d: %w", threadID, err)
		}
		meta.ArchivedAt = now
		meta.UpdatedAt = now
	}

	if err := tx.Commit(ctx); err != nil {
		return threadstore.ThreadMeta{}, fmt.Errorf("commit archive thread tx: %w", err)
	}

	return meta, nil
}

func (s *Store) DeleteRootThread(ctx context.Context, threadID int64) ([]int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin delete thread tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	meta, err := s.loadThreadForUpdate(ctx, tx, threadID)
	if err != nil {
		return nil, err
	}
	if meta.ParentThreadID > 0 {
		return nil, threadstore.ErrThreadNotRoot
	}

	busy, err := s.threadTreeBusyTx(ctx, tx, threadID)
	if err != nil {
		return nil, err
	}
	if busy {
		return nil, threadstore.ErrThreadBusy
	}

	threadIDs, err := s.threadTreeIDsTx(ctx, tx, threadID)
	if err != nil {
		return nil, err
	}
	if len(threadIDs) == 0 {
		return nil, threadstore.ErrThreadNotFound
	}

	responseIDs, err := s.threadResponseIDsTx(ctx, tx, threadIDs)
	if err != nil {
		return nil, err
	}

	if _, err := tx.Exec(ctx, `
DELETE FROM spawn_group_children
WHERE child_thread_id = ANY($1::bigint[])
   OR spawn_group_id IN (
        SELECT id
        FROM spawn_groups
        WHERE parent_thread_id = ANY($1::bigint[])
   )
`, threadIDs); err != nil {
		return nil, fmt.Errorf("delete spawn group children for thread %d: %w", threadID, err)
	}

	if _, err := tx.Exec(ctx, `
DELETE FROM spawn_groups
WHERE parent_thread_id = ANY($1::bigint[])
`, threadIDs); err != nil {
		return nil, fmt.Errorf("delete spawn groups for thread %d: %w", threadID, err)
	}

	if _, err := tx.Exec(ctx, `
DELETE FROM thread_response_links
WHERE thread_id = ANY($1::bigint[])
   OR response_id = ANY($2::text[])
`, threadIDs, responseIDs); err != nil {
		return nil, fmt.Errorf("delete thread response links for thread %d: %w", threadID, err)
	}

	if _, err := tx.Exec(ctx, `
DELETE FROM responses
WHERE source_thread_id = ANY($1::bigint[])
`, threadIDs); err != nil {
		return nil, fmt.Errorf("delete responses for thread %d: %w", threadID, err)
	}

	if _, err := tx.Exec(ctx, `
DELETE FROM threads
WHERE id = ANY($1::bigint[])
`, threadIDs); err != nil {
		return nil, fmt.Errorf("delete thread tree %d: %w", threadID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit delete thread tx: %w", err)
	}

	return threadIDs, nil
}

func (s *Store) ListItems(ctx context.Context, threadID int64, options threadstore.ListOptions) ([]threadstore.ItemRecord, error) {
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

func (s *Store) ListEvents(ctx context.Context, threadID int64, options threadstore.ListOptions) ([]threadstore.EventRecord, error) {
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

func (s *Store) ThreadHasResponse(ctx context.Context, threadID int64, responseID string) (bool, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM thread_response_links
    WHERE thread_id = $1 AND response_id = $2
)
`, threadID, responseID).Scan(&exists); err != nil {
		return false, fmt.Errorf("check thread response link %d/%s: %w", threadID, responseID, err)
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

func (s *Store) LoadSpawnGroup(ctx context.Context, spawnGroupID int64) (threadstore.SpawnGroupMeta, error) {
	row := s.pool.QueryRow(ctx, `
SELECT
    id,
    parent_thread_id,
    parent_call_id,
    group_kind,
    COALESCE(stable_key, ''),
    expected,
    completed,
    failed,
    cancelled,
    status,
    aggregate_submitted_at,
    COALESCE(aggregate_cmd_id, 0),
    created_at,
    updated_at
FROM spawn_groups
WHERE id = $1
`, spawnGroupID)

	meta, err := scanSpawnGroupScanner(row)
	if err != nil {
		if isNoRows(err) {
			return threadstore.SpawnGroupMeta{}, threadstore.ErrThreadNotFound
		}
		return threadstore.SpawnGroupMeta{}, fmt.Errorf("load spawn group %d: %w", spawnGroupID, err)
	}

	return meta, nil
}

func (s *Store) ListSpawnGroupsByParent(ctx context.Context, parentThreadID int64) ([]threadstore.SpawnGroupMeta, error) {
	rows, err := s.pool.Query(ctx, `
SELECT
    id,
    parent_thread_id,
    parent_call_id,
    group_kind,
    COALESCE(stable_key, ''),
    expected,
    completed,
    failed,
    cancelled,
    status,
    aggregate_submitted_at,
    COALESCE(aggregate_cmd_id, 0),
    created_at,
    updated_at
FROM spawn_groups
WHERE parent_thread_id = $1
ORDER BY created_at ASC, id ASC
`, parentThreadID)
	if err != nil {
		return nil, fmt.Errorf("list spawn groups by parent %d: %w", parentThreadID, err)
	}
	defer rows.Close()

	var groups []threadstore.SpawnGroupMeta
	for rows.Next() {
		meta, err := scanSpawnGroupScanner(rows)
		if err != nil {
			return nil, fmt.Errorf("scan spawn group row: %w", err)
		}
		groups = append(groups, meta)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate spawn groups: %w", err)
	}

	return groups, nil
}

func (s *Store) LoadSpawnGroupChildThreadIDs(ctx context.Context, spawnGroupID int64) ([]int64, error) {
	rows, err := s.pool.Query(ctx, `
SELECT child_thread_id
FROM spawn_group_children
WHERE spawn_group_id = $1
ORDER BY child_index ASC NULLS LAST, child_thread_id ASC
`, spawnGroupID)
	if err != nil {
		return nil, fmt.Errorf("list spawn group children %d: %w", spawnGroupID, err)
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
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

func (s *Store) ListSpawnResults(ctx context.Context, spawnGroupID int64) ([]threadstore.SpawnChildResult, error) {
	rows, err := s.pool.Query(ctx, `
SELECT
    sgc.child_thread_id,
    COALESCE(t.document_id, 0),
    sgc.status,
    COALESCE(sgc.child_response_id, ''),
    COALESCE(sgc.assistant_text, ''),
    COALESCE(sgc.result_ref, ''),
    COALESCE(sgc.summary_ref, ''),
    COALESCE(sgc.error_ref, ''),
    sgc.updated_at
FROM spawn_group_children AS sgc
LEFT JOIN threads AS t ON t.id = sgc.child_thread_id
WHERE sgc.spawn_group_id = $1 AND sgc.status <> 'pending'
ORDER BY sgc.updated_at ASC, sgc.child_thread_id ASC
`, spawnGroupID)
	if err != nil {
		return nil, fmt.Errorf("list spawn results %d: %w", spawnGroupID, err)
	}
	defer rows.Close()

	var results []threadstore.SpawnChildResult
	for rows.Next() {
		var result threadstore.SpawnChildResult
		if err := rows.Scan(
			&result.ChildThreadID,
			&result.DocumentID,
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

func loadDocumentQuerySpawnGroupTx(ctx context.Context, tx pgx.Tx, parentThreadID int64, stableKey string) (threadstore.SpawnGroupMeta, error) {
	row := tx.QueryRow(ctx, `
SELECT
    id,
    parent_thread_id,
    parent_call_id,
    group_kind,
    COALESCE(stable_key, ''),
    expected,
    completed,
    failed,
    cancelled,
    status,
    aggregate_submitted_at,
    COALESCE(aggregate_cmd_id, 0),
    created_at,
    updated_at
FROM spawn_groups
WHERE parent_thread_id = $1
  AND group_kind = 'document_query'
  AND stable_key = $2
`, parentThreadID, stableKey)

	meta, err := scanSpawnGroupScanner(row)
	if err != nil {
		if isNoRows(err) {
			return threadstore.SpawnGroupMeta{}, threadstore.ErrThreadNotFound
		}
		return threadstore.SpawnGroupMeta{}, fmt.Errorf("load document query spawn group for thread %d/%s: %w", parentThreadID, stableKey, err)
	}
	return meta, nil
}

type pgxScanner interface {
	Scan(dest ...any) error
}

func scanSpawnGroupScanner(scanner pgxScanner) (threadstore.SpawnGroupMeta, error) {
	var meta threadstore.SpawnGroupMeta
	var status string
	var aggregateSubmittedAt *time.Time
	if err := scanner.Scan(
		&meta.ID,
		&meta.ParentThreadID,
		&meta.ParentCallID,
		&meta.GroupKind,
		&meta.StableKey,
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
		return threadstore.SpawnGroupMeta{}, err
	}

	meta.Status = threadstore.SpawnGroupStatus(status)
	if aggregateSubmittedAt != nil {
		meta.AggregateSubmittedAt = aggregateSubmittedAt.UTC()
	}
	return meta, nil
}

type pgxExec interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

type pgxQueryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func upsertThreadResponseLink(ctx context.Context, db pgxExec, threadID int64, responseID, linkKind string) error {
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
		return fmt.Errorf("upsert thread response link %d/%s: %w", threadID, responseID, err)
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

func nullIfZeroInt64(value int64) any {
	if value <= 0 {
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

func (s *Store) threadTreeIDsTx(ctx context.Context, tx pgx.Tx, threadID int64) ([]int64, error) {
	rows, err := tx.Query(ctx, `
WITH RECURSIVE subtree AS (
    SELECT id
    FROM threads
    WHERE id = $1
    UNION ALL
    SELECT child.id
    FROM threads AS child
    JOIN subtree AS parent ON child.parent_thread_id = parent.id
)
SELECT id
FROM subtree
ORDER BY id ASC
`, threadID)
	if err != nil {
		return nil, fmt.Errorf("list thread tree ids for %d: %w", threadID, err)
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan thread tree id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate thread tree ids for %d: %w", threadID, err)
	}

	return ids, nil
}

func (s *Store) threadResponseIDsTx(ctx context.Context, tx pgx.Tx, threadIDs []int64) ([]string, error) {
	rows, err := tx.Query(ctx, `
SELECT id
FROM responses
WHERE source_thread_id = ANY($1::bigint[])
ORDER BY id ASC
`, threadIDs)
	if err != nil {
		return nil, fmt.Errorf("list response ids for thread tree: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan response id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate response ids for thread tree: %w", err)
	}

	return ids, nil
}

func (s *Store) threadTreeBusyTx(ctx context.Context, tx pgx.Tx, threadID int64) (bool, error) {
	var busy bool
	if err := tx.QueryRow(ctx, `
WITH RECURSIVE subtree AS (
    SELECT id
    FROM threads
    WHERE id = $1
    UNION ALL
    SELECT child.id
    FROM threads AS child
    JOIN subtree AS parent ON child.parent_thread_id = parent.id
)
SELECT EXISTS (
    SELECT 1
    FROM threads AS t
    LEFT JOIN thread_owners AS o ON o.thread_id = t.id
    WHERE t.id IN (SELECT id FROM subtree)
      AND (
          t.status IN ('new', 'running', 'reconciling', 'waiting_tool', 'waiting_children')
          OR (o.thread_id IS NOT NULL AND o.lease_until > now())
      )
)
`, threadID).Scan(&busy); err != nil {
		return false, fmt.Errorf("check busy thread tree %d: %w", threadID, err)
	}
	return busy, nil
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

func (s *Store) saveThreadTx(ctx context.Context, db pgxExec, meta threadstore.ThreadMeta) error {
	_, err := db.Exec(ctx, `
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
    child_kind,
    document_id,
    document_phase,
    archived_at,
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
    $21,
    $22,
    $23,
    $24,
    $25
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
    child_kind = EXCLUDED.child_kind,
    document_id = EXCLUDED.document_id,
    document_phase = EXCLUDED.document_phase,
    archived_at = COALESCE(EXCLUDED.archived_at, threads.archived_at),
    created_at = EXCLUDED.created_at,
    updated_at = EXCLUDED.updated_at
`,
		meta.ID,
		meta.RootThreadID,
		nullIfZeroInt64(meta.ParentThreadID),
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
		nullIfZeroInt64(meta.OwnerWorkerID),
		int64(meta.SocketGeneration),
		nullIfZeroTime(meta.SocketExpiresAt),
		nullIfBlank(meta.LastResponseID),
		nullIfBlank(meta.ActiveResponseID),
		nullIfZeroInt64(meta.ActiveSpawnGroupID),
		nullIfBlank(meta.ChildKind),
		nullIfZeroInt64(meta.DocumentID),
		nullIfBlank(meta.DocumentPhase),
		nullIfZeroTime(meta.ArchivedAt),
		nonZeroTime(meta.CreatedAt),
		nonZeroTime(meta.UpdatedAt),
	)
	if err != nil {
		return fmt.Errorf("persist thread snapshot %d: %w", meta.ID, err)
	}
	return nil
}

func (s *Store) loadThreadForUpdate(ctx context.Context, tx pgx.Tx, threadID int64) (threadstore.ThreadMeta, error) {
	row := tx.QueryRow(ctx, `
SELECT
    id,
    root_thread_id,
    COALESCE(parent_thread_id, 0),
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
    COALESCE(owner_worker_id, 0),
    socket_generation,
    socket_expires_at,
    COALESCE(last_response_id, ''),
    COALESCE(active_response_id, ''),
    COALESCE(active_spawn_group_id, 0),
    COALESCE(child_kind, ''),
    COALESCE(document_id, 0),
    COALESCE(document_phase, ''),
    archived_at,
    created_at,
    updated_at
FROM threads
WHERE id = $1
FOR UPDATE
`, threadID)

	var meta threadstore.ThreadMeta
	var status string
	var socketGeneration int64
	var socketExpiresAt *time.Time
	var archivedAt *time.Time
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
		&meta.ChildKind,
		&meta.DocumentID,
		&meta.DocumentPhase,
		&archivedAt,
		&meta.CreatedAt,
		&meta.UpdatedAt,
	); err != nil {
		if isNoRows(err) {
			return threadstore.ThreadMeta{}, threadstore.ErrThreadNotFound
		}
		return threadstore.ThreadMeta{}, fmt.Errorf("load thread %d for update: %w", threadID, err)
	}

	meta.Status = threadstore.ThreadStatus(status)
	meta.SocketGeneration = uint64(maxInt64(socketGeneration))
	if socketExpiresAt != nil {
		meta.SocketExpiresAt = socketExpiresAt.UTC()
	}
	if archivedAt != nil {
		meta.ArchivedAt = archivedAt.UTC()
	}
	return meta, nil
}

func (s *Store) loadOwnerForUpdate(ctx context.Context, tx pgx.Tx, threadID int64) (threadstore.OwnerRecord, bool, error) {
	row := tx.QueryRow(ctx, `
SELECT
    worker_id,
    socket_generation,
    lease_until,
    claimed_at,
    updated_at
FROM thread_owners
WHERE thread_id = $1
FOR UPDATE
`, threadID)

	var owner threadstore.OwnerRecord
	var socketGeneration int64
	if err := row.Scan(
		&owner.WorkerID,
		&socketGeneration,
		&owner.LeaseUntil,
		&owner.ClaimedAt,
		&owner.UpdatedAt,
	); err != nil {
		if isNoRows(err) {
			return threadstore.OwnerRecord{}, false, nil
		}
		return threadstore.OwnerRecord{}, false, fmt.Errorf("load thread owner %d for update: %w", threadID, err)
	}

	owner.SocketGeneration = uint64(maxInt64(socketGeneration))
	owner.LeaseUntil = owner.LeaseUntil.UTC()
	owner.ClaimedAt = owner.ClaimedAt.UTC()
	owner.UpdatedAt = owner.UpdatedAt.UTC()
	return owner, true, nil
}

func (s *Store) nextThreadItemSeq(ctx context.Context, tx pgx.Tx, threadID int64) (int64, error) {
	if _, err := s.loadThreadForUpdate(ctx, tx, threadID); err != nil {
		return 0, err
	}

	var current int64
	if err := tx.QueryRow(ctx, `
SELECT COALESCE(MAX(seq), 0)
FROM thread_items
WHERE thread_id = $1
`, threadID).Scan(&current); err != nil {
		return 0, fmt.Errorf("load next item seq for thread %d: %w", threadID, err)
	}

	return current + 1, nil
}

func listSpawnResultsTx(ctx context.Context, db pgxQueryer, spawnGroupID int64) ([]threadstore.SpawnChildResult, error) {
	rows, err := db.Query(ctx, `
SELECT
    sgc.child_thread_id,
    COALESCE(t.document_id, 0),
    sgc.status,
    COALESCE(sgc.child_response_id, ''),
    COALESCE(sgc.assistant_text, ''),
    COALESCE(sgc.result_ref, ''),
    COALESCE(sgc.summary_ref, ''),
    COALESCE(sgc.error_ref, ''),
    sgc.updated_at
FROM spawn_group_children AS sgc
LEFT JOIN threads AS t ON t.id = sgc.child_thread_id
WHERE sgc.spawn_group_id = $1 AND sgc.status <> 'pending'
ORDER BY sgc.updated_at ASC, sgc.child_thread_id ASC
`, spawnGroupID)
	if err != nil {
		return nil, fmt.Errorf("list spawn results %d: %w", spawnGroupID, err)
	}
	defer rows.Close()

	var results []threadstore.SpawnChildResult
	for rows.Next() {
		var result threadstore.SpawnChildResult
		if err := rows.Scan(
			&result.ChildThreadID,
			&result.DocumentID,
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
