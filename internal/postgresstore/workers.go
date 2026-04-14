package postgresstore

import (
	"context"
	"fmt"
	"strings"
	"time"

	"explorer/internal/threadstore"
)

func (s *Store) RegisterWorker(ctx context.Context, record threadstore.WorkerRecord) (threadstore.WorkerRecord, error) {
	if strings.TrimSpace(record.ServiceName) == "" {
		return threadstore.WorkerRecord{}, fmt.Errorf("worker service_name is required")
	}

	startedAt := record.StartedAt.UTC()
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}
	lastHeartbeatAt := record.LastHeartbeatAt.UTC()
	if lastHeartbeatAt.IsZero() {
		lastHeartbeatAt = startedAt
	}
	leaseUntil := record.LeaseUntil.UTC()
	if leaseUntil.IsZero() {
		leaseUntil = lastHeartbeatAt
	}
	createdAt := record.CreatedAt.UTC()
	if createdAt.IsZero() {
		createdAt = startedAt
	}
	updatedAt := record.UpdatedAt.UTC()
	if updatedAt.IsZero() {
		updatedAt = startedAt
	}

	row := s.pool.QueryRow(ctx, `
INSERT INTO workers (
    service_name,
    nats_client_name,
    responses_ws_url,
    lease_until,
    started_at,
    last_heartbeat_at,
    stopped_at,
    stop_reason,
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
RETURNING
    id,
    service_name,
    COALESCE(nats_client_name, ''),
    COALESCE(responses_ws_url, ''),
    lease_until,
    started_at,
    last_heartbeat_at,
    stopped_at,
    COALESCE(stop_reason, ''),
    created_at,
    updated_at
`,
		record.ServiceName,
		nullIfBlank(record.NATSClientName),
		nullIfBlank(record.ResponsesWSURL),
		nonZeroTime(leaseUntil),
		nonZeroTime(startedAt),
		nonZeroTime(lastHeartbeatAt),
		nullIfZeroTime(record.StoppedAt),
		nullIfBlank(record.StopReason),
		nonZeroTime(createdAt),
		nonZeroTime(updatedAt),
	)

	var registered threadstore.WorkerRecord
	var stoppedAt *time.Time
	if err := row.Scan(
		&registered.ID,
		&registered.ServiceName,
		&registered.NATSClientName,
		&registered.ResponsesWSURL,
		&registered.LeaseUntil,
		&registered.StartedAt,
		&registered.LastHeartbeatAt,
		&stoppedAt,
		&registered.StopReason,
		&registered.CreatedAt,
		&registered.UpdatedAt,
	); err != nil {
		return threadstore.WorkerRecord{}, fmt.Errorf("insert worker registration: %w", err)
	}
	if stoppedAt != nil {
		registered.StoppedAt = stoppedAt.UTC()
	}
	registered.LeaseUntil = registered.LeaseUntil.UTC()
	registered.StartedAt = registered.StartedAt.UTC()
	registered.LastHeartbeatAt = registered.LastHeartbeatAt.UTC()
	registered.CreatedAt = registered.CreatedAt.UTC()
	registered.UpdatedAt = registered.UpdatedAt.UTC()
	return registered, nil
}

func (s *Store) RenewWorker(ctx context.Context, workerID int64, leaseUntil time.Time) error {
	now := time.Now().UTC()
	tag, err := s.pool.Exec(ctx, `
UPDATE workers
SET lease_until = $2,
    last_heartbeat_at = $3,
    updated_at = $3
WHERE id = $1
  AND stopped_at IS NULL
`, workerID, nonZeroTime(leaseUntil.UTC()), now)
	if err != nil {
		return fmt.Errorf("renew worker %d: %w", workerID, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("renew worker %d: no active registration", workerID)
	}
	return nil
}

func (s *Store) ReleaseWorker(ctx context.Context, workerID int64, reason string, stoppedAt time.Time) error {
	if workerID <= 0 {
		return nil
	}
	now := stoppedAt.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	_, err := s.pool.Exec(ctx, `
UPDATE workers
SET lease_until = $2,
    last_heartbeat_at = $2,
    stopped_at = $2,
    stop_reason = $3,
    updated_at = $2
WHERE id = $1
`, workerID, now, nullIfBlank(reason))
	if err != nil {
		return fmt.Errorf("release worker %d: %w", workerID, err)
	}
	return nil
}
