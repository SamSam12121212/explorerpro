package threadstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const processedCommandTTL = 7 * 24 * time.Hour

var ErrThreadNotFound = errors.New("thread not found")

type Store struct {
	raw     *redis.Client
	durable DurableSink
}

type DurableSink interface {
	CreateThreadIfAbsent(ctx context.Context, meta ThreadMeta) error
	SaveThread(ctx context.Context, meta ThreadMeta) error
	AppendItem(ctx context.Context, entry ItemLogEntry, seq int64) error
	AppendEvent(ctx context.Context, entry EventLogEntry, eventSeq int64) error
	SaveResponseRaw(ctx context.Context, threadID, responseID string, payload json.RawMessage) error
	CreateSpawnGroup(ctx context.Context, meta SpawnGroupMeta, childThreadIDs []string) error
	SaveSpawnGroup(ctx context.Context, meta SpawnGroupMeta) error
	UpsertSpawnResult(ctx context.Context, spawnGroupID string, result SpawnChildResult) error
}

type ThreadStatus string

const (
	ThreadStatusNew             ThreadStatus = "new"
	ThreadStatusReady           ThreadStatus = "ready"
	ThreadStatusRunning         ThreadStatus = "running"
	ThreadStatusReconciling     ThreadStatus = "reconciling"
	ThreadStatusWaitingTool     ThreadStatus = "waiting_tool"
	ThreadStatusWaitingChildren ThreadStatus = "waiting_children"
	ThreadStatusCompleted       ThreadStatus = "completed"
	ThreadStatusFailed          ThreadStatus = "failed"
	ThreadStatusIncomplete      ThreadStatus = "incomplete"
	ThreadStatusCancelled       ThreadStatus = "cancelled"
	ThreadStatusOrphaned        ThreadStatus = "orphaned"
)

type ThreadMeta struct {
	ID                 string
	RootThreadID       string
	ParentThreadID     string
	ParentCallID       string
	Depth              int
	Status             ThreadStatus
	Model              string
	Instructions       string
	MetadataJSON       string
	IncludeJSON        string
	ToolsJSON          string
	ToolChoiceJSON     string
	ReasoningJSON      string
	OwnerWorkerID      string
	SocketGeneration   uint64
	SocketExpiresAt    time.Time
	LastResponseID     string
	ActiveResponseID   string
	ActiveSpawnGroupID string
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

type ItemLogEntry struct {
	ThreadID    string
	ResponseID  string
	ItemType    string
	Direction   string
	PayloadJSON string
	CreatedAt   time.Time
}

type EventLogEntry struct {
	ThreadID         string
	SocketGeneration uint64
	EventType        string
	ResponseID       string
	PayloadJSON      string
	CreatedAt        time.Time
}

type ListOptions struct {
	Limit  int64
	After  string
	Before string
}

type ItemRecord struct {
	StreamID   string
	Seq        int64
	ResponseID string
	ItemType   string
	Direction  string
	Payload    json.RawMessage
	CreatedAt  time.Time
}

type EventRecord struct {
	StreamID         string
	EventSeq         int64
	SocketGeneration uint64
	EventType        string
	ResponseID       string
	Payload          json.RawMessage
	CreatedAt        time.Time
}

type ClaimResult struct {
	Claimed          bool
	SocketGeneration uint64
	PreviousWorkerID string
}

type SpawnGroupStatus string

const (
	SpawnGroupStatusWaiting   SpawnGroupStatus = "waiting"
	SpawnGroupStatusClosed    SpawnGroupStatus = "closed"
	SpawnGroupStatusCancelled SpawnGroupStatus = "cancelled"
)

type SpawnGroupMeta struct {
	ID                   string
	ParentThreadID       string
	ParentCallID         string
	Expected             int
	Completed            int
	Failed               int
	Cancelled            int
	Status               SpawnGroupStatus
	AggregateSubmittedAt time.Time
	AggregateCmdID       string
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

type SpawnChildResult struct {
	ChildThreadID   string    `json:"child_thread_id"`
	Status          string    `json:"status"`
	ChildResponseID string    `json:"child_response_id,omitempty"`
	AssistantText   string    `json:"assistant_text,omitempty"`
	ResultRef       string    `json:"result_ref,omitempty"`
	SummaryRef      string    `json:"summary_ref,omitempty"`
	ErrorRef        string    `json:"error_ref,omitempty"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type OwnerRecord struct {
	WorkerID         string
	SocketGeneration uint64
	LeaseUntil       time.Time
	ClaimedAt        time.Time
	UpdatedAt        time.Time
}

func New(raw *redis.Client, durable DurableSink) *Store {
	return &Store{raw: raw, durable: durable}
}

func (s *Store) CreateThreadIfAbsent(ctx context.Context, meta ThreadMeta) error {
	now := utcNow()
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = now
	}
	if meta.UpdatedAt.IsZero() {
		meta.UpdatedAt = now
	}
	if meta.RootThreadID == "" {
		meta.RootThreadID = meta.ID
	}
	if meta.Status == "" {
		meta.Status = ThreadStatusNew
	}

	metaKey := threadMetaKey(meta.ID)
	err := s.raw.Watch(ctx, func(tx *redis.Tx) error {
		exists, err := tx.Exists(ctx, metaKey).Result()
		if err != nil {
			return fmt.Errorf("check thread existence: %w", err)
		}
		if exists > 0 {
			return nil
		}

		_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			pipe.HSet(ctx, metaKey, metaToHash(meta))
			pipe.SAdd(ctx, threadStatusKey(meta.Status), meta.ID)
			pipe.SAdd(ctx, threadRootKey(meta.RootThreadID), meta.ID)
			if strings.TrimSpace(meta.ParentThreadID) != "" {
				pipe.SAdd(ctx, threadParentKey(meta.ParentThreadID), meta.ID)
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("create thread: %w", err)
		}

		return nil
	}, metaKey)
	if err != nil {
		return err
	}

	if s.durable != nil {
		if err := s.durable.CreateThreadIfAbsent(ctx, meta); err != nil {
			return fmt.Errorf("persist thread create: %w", err)
		}
	}

	return nil
}

func (s *Store) LoadThread(ctx context.Context, threadID string) (ThreadMeta, error) {
	fields, err := s.raw.HGetAll(ctx, threadMetaKey(threadID)).Result()
	if err != nil {
		return ThreadMeta{}, fmt.Errorf("load thread hash: %w", err)
	}
	if len(fields) == 0 {
		return ThreadMeta{}, ErrThreadNotFound
	}

	meta, err := threadMetaFromHash(fields)
	if err != nil {
		return ThreadMeta{}, err
	}

	return meta, nil
}

func (s *Store) SaveThread(ctx context.Context, meta ThreadMeta) error {
	current, err := s.LoadThread(ctx, meta.ID)
	if err != nil {
		return err
	}

	meta.CreatedAt = current.CreatedAt
	if meta.RootThreadID == "" {
		meta.RootThreadID = current.RootThreadID
	}
	meta.UpdatedAt = utcNow()

	statusChanged := current.Status != meta.Status

	_, err = s.raw.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.HSet(ctx, threadMetaKey(meta.ID), metaToHash(meta))
		if statusChanged {
			pipe.SRem(ctx, threadStatusKey(current.Status), meta.ID)
			pipe.SAdd(ctx, threadStatusKey(meta.Status), meta.ID)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("save thread meta: %w", err)
	}

	if s.durable != nil {
		if err := s.durable.SaveThread(ctx, meta); err != nil {
			return fmt.Errorf("persist thread meta: %w", err)
		}
	}

	return nil
}

func (s *Store) MarkCommandProcessed(ctx context.Context, threadID, cmdID string) (bool, error) {
	if strings.TrimSpace(cmdID) == "" {
		return false, fmt.Errorf("cmdID is required")
	}

	key := processedCommandsKey(threadID)
	added, err := s.raw.SAdd(ctx, key, cmdID).Result()
	if err != nil {
		return false, fmt.Errorf("dedupe command: %w", err)
	}

	if err := s.raw.Expire(ctx, key, processedCommandTTL).Err(); err != nil {
		return false, fmt.Errorf("expire dedupe command set: %w", err)
	}

	return added == 1, nil
}

func (s *Store) CommandProcessed(ctx context.Context, threadID, cmdID string) (bool, error) {
	exists, err := s.raw.SIsMember(ctx, processedCommandsKey(threadID), cmdID).Result()
	if err != nil {
		return false, fmt.Errorf("check processed command: %w", err)
	}

	return exists, nil
}

func (s *Store) ClaimOwnership(ctx context.Context, threadID, workerID string, leaseUntil time.Time) (ClaimResult, error) {
	metaKey := threadMetaKey(threadID)
	ownerKey := threadOwnerKey(threadID)
	now := utcNow()

	result := ClaimResult{}

	err := s.raw.Watch(ctx, func(tx *redis.Tx) error {
		metaFields, err := tx.HGetAll(ctx, metaKey).Result()
		if err != nil {
			return fmt.Errorf("load thread metadata: %w", err)
		}
		if len(metaFields) == 0 {
			return ErrThreadNotFound
		}

		ownerFields, err := tx.HGetAll(ctx, ownerKey).Result()
		if err != nil {
			return fmt.Errorf("load thread owner: %w", err)
		}

		meta, err := threadMetaFromHash(metaFields)
		if err != nil {
			return err
		}

		currentGeneration := meta.SocketGeneration
		currentWorkerID := strings.TrimSpace(ownerFields["worker_id"])
		if currentWorkerID == "" {
			currentWorkerID = strings.TrimSpace(meta.OwnerWorkerID)
		}

		leaseValid := false
		if leaseRaw := strings.TrimSpace(ownerFields["lease_until"]); leaseRaw != "" {
			leaseUntilParsed, err := time.Parse(time.RFC3339, leaseRaw)
			if err == nil && leaseUntilParsed.After(now) {
				leaseValid = true
			}
		}

		if currentWorkerID != "" && currentWorkerID != workerID && leaseValid {
			result.Claimed = false
			result.SocketGeneration = currentGeneration
			result.PreviousWorkerID = currentWorkerID
			return nil
		}

		newGeneration := currentGeneration
		if currentWorkerID != workerID || newGeneration == 0 {
			newGeneration++
			if newGeneration == 0 {
				newGeneration = 1
			}
		}

		result.Claimed = true
		result.SocketGeneration = newGeneration
		result.PreviousWorkerID = currentWorkerID

		_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			pipe.HSet(ctx, ownerKey, map[string]any{
				"worker_id":         workerID,
				"lease_until":       leaseUntil.UTC().Format(time.RFC3339),
				"socket_generation": strconv.FormatUint(newGeneration, 10),
				"claimed_at":        now.Format(time.RFC3339),
				"updated_at":        now.Format(time.RFC3339),
			})

			pipe.HSet(ctx, metaKey, map[string]any{
				"owner_worker_id":   workerID,
				"socket_generation": strconv.FormatUint(newGeneration, 10),
				"updated_at":        now.Format(time.RFC3339),
			})

			pipe.SAdd(ctx, workerThreadsKey(workerID), threadID)
			if currentWorkerID != "" && currentWorkerID != workerID {
				pipe.SRem(ctx, workerThreadsKey(currentWorkerID), threadID)
			}

			return nil
		})
		if err != nil {
			return fmt.Errorf("claim thread ownership: %w", err)
		}

		return nil
	}, ownerKey, metaKey)
	if err != nil {
		return ClaimResult{}, err
	}

	return result, nil
}

func (s *Store) RenewOwnership(ctx context.Context, threadID, workerID string, socketGeneration uint64, leaseUntil time.Time) (bool, error) {
	ownerKey := threadOwnerKey(threadID)
	now := utcNow()

	var renewed bool

	err := s.raw.Watch(ctx, func(tx *redis.Tx) error {
		ownerFields, err := tx.HGetAll(ctx, ownerKey).Result()
		if err != nil {
			return fmt.Errorf("load thread owner: %w", err)
		}
		if len(ownerFields) == 0 {
			renewed = false
			return nil
		}

		currentWorkerID := strings.TrimSpace(ownerFields["worker_id"])
		currentGeneration, _ := strconv.ParseUint(ownerFields["socket_generation"], 10, 64)
		if currentWorkerID != workerID || currentGeneration != socketGeneration {
			renewed = false
			return nil
		}

		renewed = true
		_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			pipe.HSet(ctx, ownerKey, map[string]any{
				"lease_until": leaseUntil.UTC().Format(time.RFC3339),
				"updated_at":  now.Format(time.RFC3339),
			})
			return nil
		})
		if err != nil {
			return fmt.Errorf("renew thread ownership: %w", err)
		}

		return nil
	}, ownerKey)
	if err != nil {
		return false, err
	}

	return renewed, nil
}

func (s *Store) LoadOwner(ctx context.Context, threadID string) (OwnerRecord, error) {
	fields, err := s.raw.HGetAll(ctx, threadOwnerKey(threadID)).Result()
	if err != nil {
		return OwnerRecord{}, fmt.Errorf("load thread owner: %w", err)
	}
	if len(fields) == 0 {
		return OwnerRecord{}, ErrThreadNotFound
	}

	record := OwnerRecord{
		WorkerID: fields["worker_id"],
	}

	if raw := fields["socket_generation"]; raw != "" {
		record.SocketGeneration, err = strconv.ParseUint(raw, 10, 64)
		if err != nil {
			return OwnerRecord{}, fmt.Errorf("parse owner socket_generation %q: %w", raw, err)
		}
	}

	record.LeaseUntil, err = parseTimeField(fields["lease_until"])
	if err != nil {
		return OwnerRecord{}, err
	}
	record.ClaimedAt, err = parseTimeField(fields["claimed_at"])
	if err != nil {
		return OwnerRecord{}, err
	}
	record.UpdatedAt, err = parseTimeField(fields["updated_at"])
	if err != nil {
		return OwnerRecord{}, err
	}

	return record, nil
}

func (s *Store) ListThreadIDsByStatus(ctx context.Context, status ThreadStatus) ([]string, error) {
	ids, err := s.raw.SMembers(ctx, threadStatusKey(status)).Result()
	if err != nil {
		return nil, fmt.Errorf("load thread ids by status %s: %w", status, err)
	}
	sort.Strings(ids)
	return ids, nil
}

func (s *Store) RotateOwnership(ctx context.Context, threadID, workerID string, currentGeneration uint64, leaseUntil, socketExpiresAt time.Time) (uint64, bool, error) {
	ownerKey := threadOwnerKey(threadID)
	metaKey := threadMetaKey(threadID)
	now := utcNow()

	var newGeneration uint64
	var rotated bool

	err := s.raw.Watch(ctx, func(tx *redis.Tx) error {
		ownerFields, err := tx.HGetAll(ctx, ownerKey).Result()
		if err != nil {
			return fmt.Errorf("load thread owner: %w", err)
		}
		if len(ownerFields) == 0 {
			rotated = false
			return nil
		}

		metaFields, err := tx.HGetAll(ctx, metaKey).Result()
		if err != nil {
			return fmt.Errorf("load thread meta: %w", err)
		}
		if len(metaFields) == 0 {
			return ErrThreadNotFound
		}

		meta, err := threadMetaFromHash(metaFields)
		if err != nil {
			return err
		}

		currentWorkerID := strings.TrimSpace(ownerFields["worker_id"])
		ownerGeneration, _ := strconv.ParseUint(ownerFields["socket_generation"], 10, 64)
		if ownerGeneration == 0 {
			ownerGeneration = meta.SocketGeneration
		}

		if currentWorkerID != workerID || ownerGeneration != currentGeneration || meta.SocketGeneration != currentGeneration {
			rotated = false
			return nil
		}

		newGeneration = currentGeneration + 1
		if newGeneration == 0 {
			newGeneration = 1
		}

		rotated = true
		_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			pipe.HSet(ctx, ownerKey, map[string]any{
				"worker_id":         workerID,
				"lease_until":       leaseUntil.UTC().Format(time.RFC3339),
				"socket_generation": strconv.FormatUint(newGeneration, 10),
				"updated_at":        now.Format(time.RFC3339),
			})
			pipe.HSet(ctx, metaKey, map[string]any{
				"owner_worker_id":   workerID,
				"socket_generation": strconv.FormatUint(newGeneration, 10),
				"socket_expires_at": socketExpiresAt.UTC().Format(time.RFC3339),
				"updated_at":        now.Format(time.RFC3339),
			})
			return nil
		})
		if err != nil {
			return fmt.Errorf("rotate thread ownership: %w", err)
		}

		return nil
	}, ownerKey, metaKey)
	if err != nil {
		return 0, false, err
	}

	if rotated && s.durable != nil {
		meta, err := s.LoadThread(ctx, threadID)
		if err != nil {
			return 0, false, err
		}
		if err := s.durable.SaveThread(ctx, meta); err != nil {
			return 0, false, fmt.Errorf("persist rotated thread meta: %w", err)
		}
	}

	return newGeneration, rotated, nil
}

func (s *Store) ReleaseOwnership(ctx context.Context, threadID, workerID string, socketGeneration uint64) error {
	ownerKey := threadOwnerKey(threadID)
	metaKey := threadMetaKey(threadID)

	err := s.raw.Watch(ctx, func(tx *redis.Tx) error {
		ownerFields, err := tx.HGetAll(ctx, ownerKey).Result()
		if err != nil {
			return fmt.Errorf("load thread owner: %w", err)
		}
		if len(ownerFields) == 0 {
			return nil
		}

		currentWorkerID := strings.TrimSpace(ownerFields["worker_id"])
		if currentWorkerID != workerID {
			return nil
		}

		currentGeneration, _ := strconv.ParseUint(ownerFields["socket_generation"], 10, 64)
		if currentGeneration != socketGeneration {
			return nil
		}

		_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			pipe.Del(ctx, ownerKey)
			pipe.HSet(ctx, metaKey, map[string]any{
				"owner_worker_id":    "",
				"active_response_id": "",
				"updated_at":         utcNow().Format(time.RFC3339),
			})
			pipe.SRem(ctx, workerThreadsKey(workerID), threadID)
			return nil
		})
		if err != nil {
			return fmt.Errorf("release thread ownership: %w", err)
		}

		return nil
	}, ownerKey)
	if err != nil {
		return err
	}

	if s.durable != nil {
		meta, err := s.LoadThread(ctx, threadID)
		if err != nil && !errors.Is(err, ErrThreadNotFound) {
			return err
		}
		if err == nil {
			if err := s.durable.SaveThread(ctx, meta); err != nil {
				return fmt.Errorf("persist released thread meta: %w", err)
			}
		}
	}

	return nil
}

func (s *Store) AppendItem(ctx context.Context, entry ItemLogEntry) (ItemRecord, error) {
	seq, err := s.raw.Incr(ctx, itemSeqKey(entry.ThreadID)).Result()
	if err != nil {
		return ItemRecord{}, fmt.Errorf("increment item sequence: %w", err)
	}

	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = utcNow()
	}

	if _, err := s.raw.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.XAdd(ctx, &redis.XAddArgs{
			Stream: threadItemsKey(entry.ThreadID),
			Values: map[string]any{
				"seq":          strconv.FormatInt(seq, 10),
				"response_id":  entry.ResponseID,
				"item_type":    entry.ItemType,
				"direction":    entry.Direction,
				"payload_json": entry.PayloadJSON,
				"created_at":   entry.CreatedAt.UTC().Format(time.RFC3339),
			},
		})
		if strings.TrimSpace(entry.ResponseID) != "" {
			pipe.SAdd(ctx, threadResponsesKey(entry.ThreadID), entry.ResponseID)
		}
		return nil
	}); err != nil {
		return ItemRecord{}, fmt.Errorf("append thread item: %w", err)
	}

	if s.durable != nil {
		if err := s.durable.AppendItem(ctx, entry, seq); err != nil {
			return ItemRecord{}, fmt.Errorf("persist thread item: %w", err)
		}
	}

	return ItemRecord{
		Seq:        seq,
		ResponseID: entry.ResponseID,
		ItemType:   entry.ItemType,
		Direction:  entry.Direction,
		Payload:    json.RawMessage(entry.PayloadJSON),
		CreatedAt:  entry.CreatedAt,
	}, nil
}

func (s *Store) AppendEvent(ctx context.Context, entry EventLogEntry) error {
	seq, err := s.raw.Incr(ctx, eventSeqKey(entry.ThreadID)).Result()
	if err != nil {
		return fmt.Errorf("increment event sequence: %w", err)
	}

	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = utcNow()
	}

	if _, err := s.raw.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.XAdd(ctx, &redis.XAddArgs{
			Stream: threadEventsKey(entry.ThreadID),
			Values: map[string]any{
				"event_seq":         strconv.FormatInt(seq, 10),
				"socket_generation": strconv.FormatUint(entry.SocketGeneration, 10),
				"event_type":        entry.EventType,
				"response_id":       entry.ResponseID,
				"payload_json":      entry.PayloadJSON,
				"created_at":        entry.CreatedAt.UTC().Format(time.RFC3339),
			},
		})
		if strings.TrimSpace(entry.ResponseID) != "" {
			pipe.SAdd(ctx, threadResponsesKey(entry.ThreadID), entry.ResponseID)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("append thread event: %w", err)
	}

	if s.durable != nil {
		if err := s.durable.AppendEvent(ctx, entry, seq); err != nil {
			return fmt.Errorf("persist thread event: %w", err)
		}
	}

	return nil
}

func (s *Store) SaveResponseRaw(ctx context.Context, threadID, responseID string, payload json.RawMessage) error {
	if strings.TrimSpace(responseID) == "" || len(payload) == 0 {
		return nil
	}

	if err := s.raw.Set(ctx, responseRawKey(responseID), string(payload), 0).Err(); err != nil {
		return fmt.Errorf("save raw response: %w", err)
	}

	if s.durable != nil {
		if err := s.durable.SaveResponseRaw(ctx, threadID, responseID, payload); err != nil {
			return fmt.Errorf("persist raw response: %w", err)
		}
	}

	return nil
}

func (s *Store) LoadResponseRaw(ctx context.Context, responseID string) (json.RawMessage, error) {
	value, err := s.raw.Get(ctx, responseRawKey(responseID)).Result()
	if err != nil {
		if err == redis.Nil {
			return nil, ErrThreadNotFound
		}
		return nil, fmt.Errorf("load raw response: %w", err)
	}

	return json.RawMessage(value), nil
}

func (s *Store) ThreadHasResponse(ctx context.Context, threadID, responseID string) (bool, error) {
	if strings.TrimSpace(threadID) == "" || strings.TrimSpace(responseID) == "" {
		return false, nil
	}

	exists, err := s.raw.SIsMember(ctx, threadResponsesKey(threadID), responseID).Result()
	if err != nil {
		return false, fmt.Errorf("check thread response index: %w", err)
	}

	return exists, nil
}

func (s *Store) LoadLatestClientResponseCreatePayload(ctx context.Context, threadID string) (json.RawMessage, error) {
	messages, err := s.raw.XRevRangeN(ctx, threadEventsKey(threadID), "+", "-", 256).Result()
	if err != nil {
		return nil, fmt.Errorf("load latest client response.create event: %w", err)
	}

	for _, message := range messages {
		if valueToString(message.Values["event_type"]) != "client.response.create" {
			continue
		}

		payload := strings.TrimSpace(valueToString(message.Values["payload_json"]))
		if payload == "" {
			continue
		}

		return json.RawMessage(payload), nil
	}

	return nil, ErrThreadNotFound
}

func (s *Store) ListItems(ctx context.Context, threadID string, options ListOptions) ([]ItemRecord, error) {
	messages, err := s.listStream(ctx, threadItemsKey(threadID), options, "seq")
	if err != nil {
		return nil, fmt.Errorf("list thread items: %w", err)
	}

	records := make([]ItemRecord, 0, len(messages))
	for _, message := range messages {
		record, err := itemRecordFromMessage(message)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}

	return records, nil
}

func (s *Store) ListEvents(ctx context.Context, threadID string, options ListOptions) ([]EventRecord, error) {
	messages, err := s.listStream(ctx, threadEventsKey(threadID), options, "event_seq")
	if err != nil {
		return nil, fmt.Errorf("list thread events: %w", err)
	}

	records := make([]EventRecord, 0, len(messages))
	for _, message := range messages {
		record, err := eventRecordFromMessage(message)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}

	return records, nil
}

func (s *Store) CreateSpawnGroup(ctx context.Context, meta SpawnGroupMeta, childThreadIDs []string) error {
	now := utcNow()
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = now
	}
	if meta.UpdatedAt.IsZero() {
		meta.UpdatedAt = now
	}
	if meta.Status == "" {
		meta.Status = SpawnGroupStatusWaiting
	}

	metaKey := spawnGroupMetaKey(meta.ID)
	childrenKey := spawnGroupChildrenKey(meta.ID)

	err := s.raw.Watch(ctx, func(tx *redis.Tx) error {
		exists, err := tx.Exists(ctx, metaKey).Result()
		if err != nil {
			return fmt.Errorf("check spawn group existence: %w", err)
		}
		if exists > 0 {
			return nil
		}

		_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			pipe.HSet(ctx, metaKey, spawnGroupToHash(meta))
			if len(childThreadIDs) > 0 {
				values := make([]any, 0, len(childThreadIDs))
				for _, childID := range childThreadIDs {
					values = append(values, childID)
				}
				pipe.SAdd(ctx, childrenKey, values...)
			}
			if meta.ParentThreadID != "" {
				pipe.SAdd(ctx, spawnGroupParentKey(meta.ParentThreadID), meta.ID)
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("create spawn group: %w", err)
		}

		return nil
	}, metaKey)
	if err != nil {
		return err
	}

	if s.durable != nil {
		if err := s.durable.CreateSpawnGroup(ctx, meta, childThreadIDs); err != nil {
			return fmt.Errorf("persist spawn group create: %w", err)
		}
	}

	return nil
}

func (s *Store) LoadSpawnGroup(ctx context.Context, spawnGroupID string) (SpawnGroupMeta, error) {
	fields, err := s.raw.HGetAll(ctx, spawnGroupMetaKey(spawnGroupID)).Result()
	if err != nil {
		return SpawnGroupMeta{}, fmt.Errorf("load spawn group hash: %w", err)
	}
	if len(fields) == 0 {
		return SpawnGroupMeta{}, ErrThreadNotFound
	}

	meta, err := spawnGroupFromHash(fields)
	if err != nil {
		return SpawnGroupMeta{}, err
	}

	return meta, nil
}

func (s *Store) SaveSpawnGroup(ctx context.Context, meta SpawnGroupMeta) error {
	current, err := s.LoadSpawnGroup(ctx, meta.ID)
	if err != nil {
		return err
	}

	meta.CreatedAt = current.CreatedAt
	meta.UpdatedAt = utcNow()

	if _, err := s.raw.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.HSet(ctx, spawnGroupMetaKey(meta.ID), spawnGroupToHash(meta))
		return nil
	}); err != nil {
		return fmt.Errorf("save spawn group meta: %w", err)
	}

	if s.durable != nil {
		if err := s.durable.SaveSpawnGroup(ctx, meta); err != nil {
			return fmt.Errorf("persist spawn group meta: %w", err)
		}
	}

	return nil
}

func (s *Store) ListSpawnResults(ctx context.Context, spawnGroupID string) ([]SpawnChildResult, error) {
	values, err := s.raw.HGetAll(ctx, spawnGroupResultsKey(spawnGroupID)).Result()
	if err != nil {
		return nil, fmt.Errorf("load spawn results: %w", err)
	}

	results := make([]SpawnChildResult, 0, len(values))
	for _, raw := range values {
		var result SpawnChildResult
		if err := json.Unmarshal([]byte(raw), &result); err != nil {
			return nil, fmt.Errorf("decode spawn child result: %w", err)
		}
		results = append(results, result)
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].UpdatedAt.Equal(results[j].UpdatedAt) {
			return results[i].ChildThreadID < results[j].ChildThreadID
		}
		return results[i].UpdatedAt.Before(results[j].UpdatedAt)
	})

	return results, nil
}

func (s *Store) ListSpawnGroupsByParent(ctx context.Context, parentThreadID string) ([]SpawnGroupMeta, error) {
	ids, err := s.raw.SMembers(ctx, spawnGroupParentKey(parentThreadID)).Result()
	if err != nil {
		return nil, fmt.Errorf("load parent spawn group ids: %w", err)
	}

	groups := make([]SpawnGroupMeta, 0, len(ids))
	for _, id := range ids {
		meta, err := s.LoadSpawnGroup(ctx, id)
		if err != nil {
			if errors.Is(err, ErrThreadNotFound) {
				continue
			}
			return nil, err
		}
		groups = append(groups, meta)
	}

	sort.Slice(groups, func(i, j int) bool {
		if groups[i].CreatedAt.Equal(groups[j].CreatedAt) {
			return groups[i].ID < groups[j].ID
		}
		return groups[i].CreatedAt.Before(groups[j].CreatedAt)
	})

	return groups, nil
}

func (s *Store) LoadSpawnGroupChildThreadIDs(ctx context.Context, spawnGroupID string) ([]string, error) {
	ids, err := s.raw.SMembers(ctx, spawnGroupChildrenKey(spawnGroupID)).Result()
	if err != nil {
		return nil, fmt.Errorf("load spawn group child thread ids: %w", err)
	}
	sort.Strings(ids)
	return ids, nil
}

func (s *Store) UpsertSpawnResult(ctx context.Context, spawnGroupID string, result SpawnChildResult) (bool, []SpawnChildResult, error) {
	metaKey := spawnGroupMetaKey(spawnGroupID)
	resultsKey := spawnGroupResultsKey(spawnGroupID)

	var stored bool
	var allResults []SpawnChildResult

	if result.UpdatedAt.IsZero() {
		result.UpdatedAt = utcNow()
	}

	err := s.raw.Watch(ctx, func(tx *redis.Tx) error {
		metaFields, err := tx.HGetAll(ctx, metaKey).Result()
		if err != nil {
			return fmt.Errorf("load spawn group meta: %w", err)
		}
		if len(metaFields) == 0 {
			return ErrThreadNotFound
		}

		existingRaw, err := tx.HGet(ctx, resultsKey, result.ChildThreadID).Result()
		if err != nil && err != redis.Nil {
			return fmt.Errorf("load existing spawn result: %w", err)
		}
		if existingRaw != "" {
			var existing SpawnChildResult
			if err := json.Unmarshal([]byte(existingRaw), &existing); err != nil {
				return fmt.Errorf("decode existing spawn result: %w", err)
			}
			if existing.Status != "" {
				stored = false
				allResults, err = s.ListSpawnResults(ctx, spawnGroupID)
				return err
			}
		}

		payload, err := json.Marshal(result)
		if err != nil {
			return fmt.Errorf("marshal spawn result: %w", err)
		}

		if _, err := tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			pipe.HSet(ctx, resultsKey, result.ChildThreadID, string(payload))
			return nil
		}); err != nil {
			return fmt.Errorf("save spawn result: %w", err)
		}

		stored = true
		allResults, err = s.ListSpawnResults(ctx, spawnGroupID)
		return err
	}, metaKey, resultsKey)
	if err != nil {
		return false, nil, err
	}

	if s.durable != nil {
		if err := s.durable.UpsertSpawnResult(ctx, spawnGroupID, result); err != nil {
			return false, nil, fmt.Errorf("persist spawn result: %w", err)
		}
	}

	return stored, allResults, nil
}

func threadMetaFromHash(fields map[string]string) (ThreadMeta, error) {
	meta := ThreadMeta{
		ID:                 fields["id"],
		RootThreadID:       fields["root_thread_id"],
		ParentThreadID:     fields["parent_thread_id"],
		ParentCallID:       fields["parent_call_id"],
		Status:             ThreadStatus(fields["status"]),
		Model:              fields["model"],
		Instructions:       fields["instructions_json"],
		MetadataJSON:       fields["metadata_json"],
		IncludeJSON:        fields["include_json"],
		ToolsJSON:          fields["tools_json"],
		ToolChoiceJSON:     fields["tool_choice_json"],
		ReasoningJSON:      fields["reasoning_json"],
		OwnerWorkerID:      fields["owner_worker_id"],
		LastResponseID:     fields["last_response_id"],
		ActiveResponseID:   fields["active_response_id"],
		ActiveSpawnGroupID: fields["active_spawn_group_id"],
	}

	if raw := fields["depth"]; raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil {
			return ThreadMeta{}, fmt.Errorf("parse thread depth %q: %w", raw, err)
		}
		meta.Depth = value
	}

	if raw := fields["socket_generation"]; raw != "" {
		value, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			return ThreadMeta{}, fmt.Errorf("parse socket generation %q: %w", raw, err)
		}
		meta.SocketGeneration = value
	}

	var err error
	meta.CreatedAt, err = parseTimeField(fields["created_at"])
	if err != nil {
		return ThreadMeta{}, err
	}
	meta.UpdatedAt, err = parseTimeField(fields["updated_at"])
	if err != nil {
		return ThreadMeta{}, err
	}
	meta.SocketExpiresAt, err = parseTimeField(fields["socket_expires_at"])
	if err != nil {
		return ThreadMeta{}, err
	}

	return meta, nil
}

func spawnGroupFromHash(fields map[string]string) (SpawnGroupMeta, error) {
	meta := SpawnGroupMeta{
		ID:             fields["id"],
		ParentThreadID: fields["parent_thread_id"],
		ParentCallID:   fields["parent_call_id"],
		Status:         SpawnGroupStatus(fields["status"]),
		AggregateCmdID: fields["aggregate_cmd_id"],
	}

	var err error
	if raw := fields["expected"]; raw != "" {
		meta.Expected, err = strconv.Atoi(raw)
		if err != nil {
			return SpawnGroupMeta{}, fmt.Errorf("parse spawn expected %q: %w", raw, err)
		}
	}
	if raw := fields["completed"]; raw != "" {
		meta.Completed, err = strconv.Atoi(raw)
		if err != nil {
			return SpawnGroupMeta{}, fmt.Errorf("parse spawn completed %q: %w", raw, err)
		}
	}
	if raw := fields["failed"]; raw != "" {
		meta.Failed, err = strconv.Atoi(raw)
		if err != nil {
			return SpawnGroupMeta{}, fmt.Errorf("parse spawn failed %q: %w", raw, err)
		}
	}
	if raw := fields["cancelled"]; raw != "" {
		meta.Cancelled, err = strconv.Atoi(raw)
		if err != nil {
			return SpawnGroupMeta{}, fmt.Errorf("parse spawn cancelled %q: %w", raw, err)
		}
	}

	meta.CreatedAt, err = parseTimeField(fields["created_at"])
	if err != nil {
		return SpawnGroupMeta{}, err
	}
	meta.UpdatedAt, err = parseTimeField(fields["updated_at"])
	if err != nil {
		return SpawnGroupMeta{}, err
	}
	meta.AggregateSubmittedAt, err = parseTimeField(fields["aggregate_submitted_at"])
	if err != nil {
		return SpawnGroupMeta{}, err
	}

	return meta, nil
}

func metaToHash(meta ThreadMeta) map[string]any {
	fields := map[string]any{
		"id":                    meta.ID,
		"root_thread_id":        meta.RootThreadID,
		"parent_thread_id":      meta.ParentThreadID,
		"parent_call_id":        meta.ParentCallID,
		"depth":                 strconv.Itoa(meta.Depth),
		"status":                string(meta.Status),
		"model":                 meta.Model,
		"instructions_json":     meta.Instructions,
		"metadata_json":         meta.MetadataJSON,
		"include_json":          meta.IncludeJSON,
		"tools_json":            meta.ToolsJSON,
		"tool_choice_json":      meta.ToolChoiceJSON,
		"reasoning_json":        meta.ReasoningJSON,
		"owner_worker_id":       meta.OwnerWorkerID,
		"socket_generation":     strconv.FormatUint(meta.SocketGeneration, 10),
		"last_response_id":      meta.LastResponseID,
		"active_response_id":    meta.ActiveResponseID,
		"active_spawn_group_id": meta.ActiveSpawnGroupID,
		"created_at":            zeroTimeToString(meta.CreatedAt),
		"updated_at":            zeroTimeToString(meta.UpdatedAt),
		"socket_expires_at":     zeroTimeToString(meta.SocketExpiresAt),
	}

	return fields
}

func spawnGroupToHash(meta SpawnGroupMeta) map[string]any {
	return map[string]any{
		"id":                     meta.ID,
		"parent_thread_id":       meta.ParentThreadID,
		"parent_call_id":         meta.ParentCallID,
		"expected":               strconv.Itoa(meta.Expected),
		"completed":              strconv.Itoa(meta.Completed),
		"failed":                 strconv.Itoa(meta.Failed),
		"cancelled":              strconv.Itoa(meta.Cancelled),
		"status":                 string(meta.Status),
		"aggregate_submitted_at": zeroTimeToString(meta.AggregateSubmittedAt),
		"aggregate_cmd_id":       meta.AggregateCmdID,
		"created_at":             zeroTimeToString(meta.CreatedAt),
		"updated_at":             zeroTimeToString(meta.UpdatedAt),
	}
}

func parseTimeField(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, nil
	}

	value, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse time %q: %w", raw, err)
	}

	return value, nil
}

func zeroTimeToString(value time.Time) string {
	if value.IsZero() {
		return ""
	}

	return value.UTC().Format(time.RFC3339)
}

func utcNow() time.Time {
	return time.Now().UTC()
}

func (s *Store) listStream(ctx context.Context, key string, options ListOptions, sequenceField string) ([]redis.XMessage, error) {
	limit := options.Limit
	if limit <= 0 {
		limit = 100
	}

	if strings.TrimSpace(options.After) != "" && strings.TrimSpace(options.Before) != "" {
		return nil, fmt.Errorf("after and before cannot both be set")
	}

	if after := strings.TrimSpace(options.After); after != "" {
		if sequence, ok := parseSequenceCursor(after); ok {
			messages, err := s.listStreamAfterSequence(ctx, key, sequenceField, sequence, limit)
			if err != nil {
				return nil, fmt.Errorf("range stream after sequence %d: %w", sequence, err)
			}
			return messages, nil
		}

		messages, err := s.raw.XRangeN(ctx, key, exclusiveStreamID(after), "+", limit).Result()
		if err != nil {
			return nil, fmt.Errorf("range stream after %s: %w", after, err)
		}
		return messages, nil
	}

	if before := strings.TrimSpace(options.Before); before != "" {
		if sequence, ok := parseSequenceCursor(before); ok {
			messages, err := s.listStreamBeforeSequence(ctx, key, sequenceField, sequence, limit)
			if err != nil {
				return nil, fmt.Errorf("range stream before sequence %d: %w", sequence, err)
			}
			return messages, nil
		}

		messages, err := s.raw.XRevRangeN(ctx, key, exclusiveStreamID(before), "-", limit).Result()
		if err != nil {
			return nil, fmt.Errorf("range stream before %s: %w", before, err)
		}
		reverseMessages(messages)
		return messages, nil
	}

	messages, err := s.raw.XRevRangeN(ctx, key, "+", "-", limit).Result()
	if err != nil {
		return nil, fmt.Errorf("range latest stream entries: %w", err)
	}
	reverseMessages(messages)
	return messages, nil
}

func (s *Store) listStreamAfterSequence(ctx context.Context, key, sequenceField string, afterSequence, limit int64) ([]redis.XMessage, error) {
	cursor := "-"
	batchSize := maxInt64(limit, 100)
	messages := make([]redis.XMessage, 0, limit)

	for {
		batch, err := s.raw.XRangeN(ctx, key, cursor, "+", batchSize).Result()
		if err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			return messages, nil
		}

		for _, message := range batch {
			sequence, err := streamMessageSequence(message, sequenceField)
			if err != nil {
				return nil, err
			}
			if sequence <= afterSequence {
				continue
			}

			messages = append(messages, message)
			if int64(len(messages)) >= limit {
				return messages, nil
			}
		}

		if int64(len(batch)) < batchSize {
			return messages, nil
		}

		cursor = exclusiveStreamID(batch[len(batch)-1].ID)
	}
}

func (s *Store) listStreamBeforeSequence(ctx context.Context, key, sequenceField string, beforeSequence, limit int64) ([]redis.XMessage, error) {
	cursor := "+"
	batchSize := maxInt64(limit, 100)
	messages := make([]redis.XMessage, 0, limit)

	for {
		batch, err := s.raw.XRevRangeN(ctx, key, cursor, "-", batchSize).Result()
		if err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			reverseMessages(messages)
			return messages, nil
		}

		for _, message := range batch {
			sequence, err := streamMessageSequence(message, sequenceField)
			if err != nil {
				return nil, err
			}
			if sequence >= beforeSequence {
				continue
			}

			messages = append(messages, message)
			if int64(len(messages)) >= limit {
				reverseMessages(messages)
				return messages, nil
			}
		}

		if int64(len(batch)) < batchSize {
			reverseMessages(messages)
			return messages, nil
		}

		cursor = exclusiveStreamID(batch[len(batch)-1].ID)
	}
}

func itemRecordFromMessage(message redis.XMessage) (ItemRecord, error) {
	record := ItemRecord{
		StreamID:   message.ID,
		ResponseID: valueToString(message.Values["response_id"]),
		ItemType:   valueToString(message.Values["item_type"]),
		Direction:  valueToString(message.Values["direction"]),
		Payload:    json.RawMessage(valueToString(message.Values["payload_json"])),
	}

	var err error
	record.Seq, err = parseInt64Value(message.Values["seq"])
	if err != nil {
		return ItemRecord{}, fmt.Errorf("parse item seq for %s: %w", message.ID, err)
	}
	record.CreatedAt, err = parseTimeField(valueToString(message.Values["created_at"]))
	if err != nil {
		return ItemRecord{}, err
	}

	return record, nil
}

func eventRecordFromMessage(message redis.XMessage) (EventRecord, error) {
	record := EventRecord{
		StreamID:   message.ID,
		EventType:  valueToString(message.Values["event_type"]),
		ResponseID: valueToString(message.Values["response_id"]),
		Payload:    json.RawMessage(valueToString(message.Values["payload_json"])),
	}

	var err error
	record.EventSeq, err = parseInt64Value(message.Values["event_seq"])
	if err != nil {
		return EventRecord{}, fmt.Errorf("parse event_seq for %s: %w", message.ID, err)
	}
	record.SocketGeneration, err = parseUint64Value(message.Values["socket_generation"])
	if err != nil {
		return EventRecord{}, fmt.Errorf("parse socket_generation for %s: %w", message.ID, err)
	}
	record.CreatedAt, err = parseTimeField(valueToString(message.Values["created_at"]))
	if err != nil {
		return EventRecord{}, err
	}

	return record, nil
}

func parseInt64Value(value any) (int64, error) {
	raw := strings.TrimSpace(valueToString(value))
	if raw == "" {
		return 0, nil
	}
	parsed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, err
	}
	return parsed, nil
}

func parseUint64Value(value any) (uint64, error) {
	raw := strings.TrimSpace(valueToString(value))
	if raw == "" {
		return 0, nil
	}
	parsed, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, err
	}
	return parsed, nil
}

func valueToString(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case []byte:
		return string(typed)
	default:
		return fmt.Sprint(typed)
	}
}

func parseSequenceCursor(raw string) (int64, bool) {
	sequence, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || sequence <= 0 {
		return 0, false
	}
	return sequence, true
}

func streamMessageSequence(message redis.XMessage, sequenceField string) (int64, error) {
	sequence, err := parseInt64Value(message.Values[sequenceField])
	if err != nil {
		return 0, fmt.Errorf("parse %s for %s: %w", sequenceField, message.ID, err)
	}
	return sequence, nil
}

func exclusiveStreamID(streamID string) string {
	streamID = strings.TrimSpace(streamID)
	if streamID == "" || strings.HasPrefix(streamID, "(") {
		return streamID
	}
	return "(" + streamID
}

func reverseMessages(messages []redis.XMessage) {
	for left, right := 0, len(messages)-1; left < right; left, right = left+1, right-1 {
		messages[left], messages[right] = messages[right], messages[left]
	}
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}

func threadMetaKey(threadID string) string {
	return fmt.Sprintf("thread:%s:meta", threadID)
}

func threadItemsKey(threadID string) string {
	return fmt.Sprintf("thread:%s:items", threadID)
}

func threadEventsKey(threadID string) string {
	return fmt.Sprintf("thread:%s:events", threadID)
}

func threadResponsesKey(threadID string) string {
	return fmt.Sprintf("thread:%s:responses", threadID)
}

func itemSeqKey(threadID string) string {
	return fmt.Sprintf("thread:%s:item_seq", threadID)
}

func eventSeqKey(threadID string) string {
	return fmt.Sprintf("thread:%s:event_seq", threadID)
}

func processedCommandsKey(threadID string) string {
	return fmt.Sprintf("thread:%s:processed_cmds", threadID)
}

func threadOwnerKey(threadID string) string {
	return fmt.Sprintf("thread_owner:%s", threadID)
}

func workerThreadsKey(workerID string) string {
	return fmt.Sprintf("worker:%s:threads", workerID)
}

func threadStatusKey(status ThreadStatus) string {
	return fmt.Sprintf("thread_status:%s", status)
}

func threadRootKey(rootThreadID string) string {
	return fmt.Sprintf("thread_root:%s", rootThreadID)
}

func threadParentKey(parentThreadID string) string {
	return fmt.Sprintf("thread_parent:%s", parentThreadID)
}

func responseRawKey(responseID string) string {
	return fmt.Sprintf("response:%s:raw", responseID)
}

func spawnGroupMetaKey(spawnGroupID string) string {
	return fmt.Sprintf("spawn_group:%s:meta", spawnGroupID)
}

func spawnGroupResultsKey(spawnGroupID string) string {
	return fmt.Sprintf("spawn_group:%s:results", spawnGroupID)
}

func spawnGroupChildrenKey(spawnGroupID string) string {
	return fmt.Sprintf("spawn_group:%s:children", spawnGroupID)
}

func spawnGroupParentKey(parentThreadID string) string {
	return fmt.Sprintf("spawn_group_parent:%s", parentThreadID)
}
