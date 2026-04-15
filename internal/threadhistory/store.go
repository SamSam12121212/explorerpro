package threadhistory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"explorer/internal/threadstore"
	"github.com/nats-io/nats.go"
)

const (
	StreamName              = "THREAD_HISTORY"
	SubjectPrefix           = "thread.history."
	CheckpointSubjectPrefix = SubjectPrefix + "checkpoint."
	EventSubjectPrefix      = SubjectPrefix + "events."
)

var ErrCheckpointNotFound = errors.New("response.create checkpoint not found")

type Store struct {
	js nats.JetStreamContext
}

func New(js nats.JetStreamContext) *Store {
	return &Store{js: js}
}

func CheckpointSubject(threadID int64) string {
	return CheckpointSubjectPrefix + strconv.FormatInt(threadID, 10)
}

func EventSubject(threadID int64) string {
	return EventSubjectPrefix + strconv.FormatInt(threadID, 10)
}

func CheckpointMsgID(threadID int64, checkpointID string) string {
	checkpointID = strings.TrimSpace(checkpointID)
	if checkpointID == "" {
		return "checkpoint-" + strconv.FormatInt(threadID, 10)
	}
	return fmt.Sprintf("checkpoint-%d-%s", threadID, checkpointID)
}

func EventMsgID(threadID int64, eventID string) string {
	eventID = strings.TrimSpace(eventID)
	return fmt.Sprintf("event-%d-%s", threadID, eventID)
}

func (s *Store) SaveResponseCreateCheckpoint(ctx context.Context, threadID int64, checkpointID string, payload json.RawMessage) error {
	if s == nil || s.js == nil {
		return fmt.Errorf("thread history store is not configured")
	}
	if threadID <= 0 {
		return fmt.Errorf("response.create checkpoint missing thread id")
	}
	if len(bytes.TrimSpace(payload)) == 0 {
		return fmt.Errorf("response.create checkpoint missing payload")
	}

	msg := &nats.Msg{
		Subject: CheckpointSubject(threadID),
		Header:  nats.Header{},
		Data:    payload,
	}
	msg.Header.Set("Nats-Msg-Id", CheckpointMsgID(threadID, checkpointID))

	if _, err := s.js.PublishMsg(msg, nats.Context(ctx)); err != nil {
		return fmt.Errorf("publish response.create checkpoint for thread %d: %w", threadID, err)
	}

	return nil
}

func (s *Store) LoadLatestResponseCreateCheckpoint(ctx context.Context, threadID int64) (json.RawMessage, error) {
	if s == nil || s.js == nil {
		return nil, fmt.Errorf("thread history store is not configured")
	}
	if threadID <= 0 {
		return nil, fmt.Errorf("response.create checkpoint missing thread id")
	}

	msg, err := s.js.GetLastMsg(StreamName, CheckpointSubject(threadID), nats.Context(ctx))
	if err != nil {
		if errors.Is(err, nats.ErrMsgNotFound) || errors.Is(err, nats.ErrNoResponders) {
			return nil, ErrCheckpointNotFound
		}
		return nil, fmt.Errorf("load latest response.create checkpoint for thread %d: %w", threadID, err)
	}
	if len(bytes.TrimSpace(msg.Data)) == 0 {
		return nil, ErrCheckpointNotFound
	}

	return append(json.RawMessage(nil), msg.Data...), nil
}

type storedEvent struct {
	SocketGeneration uint64          `json:"socket_generation"`
	EventType        string          `json:"event_type"`
	ResponseID       string          `json:"response_id,omitempty"`
	Payload          json.RawMessage `json:"payload"`
	CreatedAt        string          `json:"created_at"`
}

func (s *Store) AppendEvent(ctx context.Context, entry threadstore.EventLogEntry, eventID string) error {
	if s == nil || s.js == nil {
		return fmt.Errorf("thread history store is not configured")
	}
	if entry.ThreadID <= 0 {
		return fmt.Errorf("thread history event missing thread id")
	}
	if strings.TrimSpace(entry.EventType) == "" {
		return fmt.Errorf("thread history event missing event type")
	}
	if len(bytes.TrimSpace([]byte(entry.PayloadJSON))) == 0 {
		return fmt.Errorf("thread history event missing payload")
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now().UTC()
	}

	data, err := json.Marshal(storedEvent{
		SocketGeneration: entry.SocketGeneration,
		EventType:        entry.EventType,
		ResponseID:       entry.ResponseID,
		Payload:          json.RawMessage(entry.PayloadJSON),
		CreatedAt:        entry.CreatedAt.UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return fmt.Errorf("marshal thread history event for thread %d: %w", entry.ThreadID, err)
	}

	msg := &nats.Msg{
		Subject: EventSubject(entry.ThreadID),
		Header:  nats.Header{},
		Data:    data,
	}
	if eventID = strings.TrimSpace(eventID); eventID != "" {
		msg.Header.Set("Nats-Msg-Id", EventMsgID(entry.ThreadID, eventID))
	}

	if _, err := s.js.PublishMsg(msg, nats.Context(ctx)); err != nil {
		return fmt.Errorf("publish thread history event for thread %d: %w", entry.ThreadID, err)
	}

	return nil
}

func (s *Store) ListEvents(ctx context.Context, threadID int64, options threadstore.ListOptions) ([]threadstore.EventRecord, error) {
	if s == nil || s.js == nil {
		return nil, fmt.Errorf("thread history store is not configured")
	}
	if threadID <= 0 {
		return nil, fmt.Errorf("thread history events missing thread id")
	}

	subject := EventSubject(threadID)
	limit := normalizeLimit(options.Limit)

	if options.After != "" {
		afterSeq, err := parseSequenceCursor(options.After)
		if err != nil {
			return nil, err
		}
		return s.scanForward(ctx, subject, uint64(afterSeq+1), limit)
	}

	last, err := s.js.GetLastMsg(StreamName, subject, nats.Context(ctx))
	if err != nil {
		if errors.Is(err, nats.ErrMsgNotFound) || errors.Is(err, nats.ErrNoResponders) {
			return []threadstore.EventRecord{}, nil
		}
		return nil, fmt.Errorf("load latest thread history event for thread %d: %w", threadID, err)
	}

	maxSeq := last.Sequence
	if options.Before != "" {
		beforeSeq, err := parseSequenceCursor(options.Before)
		if err != nil {
			return nil, err
		}
		if beforeSeq <= 1 {
			return []threadstore.EventRecord{}, nil
		}
		maxSeq = uint64(beforeSeq - 1)
	}

	return s.scanBackward(ctx, subject, maxSeq, limit)
}

func (s *Store) PurgeThread(ctx context.Context, threadID int64) error {
	if s == nil || s.js == nil {
		return fmt.Errorf("thread history store is not configured")
	}
	if threadID <= 0 {
		return fmt.Errorf("thread history purge missing thread id")
	}

	var errs []error
	for _, subject := range []string{
		CheckpointSubject(threadID),
		EventSubject(threadID),
	} {
		if err := s.js.PurgeStream(StreamName, &nats.StreamPurgeRequest{Subject: subject}, nats.Context(ctx)); err != nil {
			errs = append(errs, fmt.Errorf("purge thread history subject %s: %w", subject, err))
		}
	}

	return errors.Join(errs...)
}

func (s *Store) scanForward(ctx context.Context, subject string, startSeq uint64, limit int64) ([]threadstore.EventRecord, error) {
	if limit <= 0 {
		return []threadstore.EventRecord{}, nil
	}

	records := make([]threadstore.EventRecord, 0, limit)
	nextSeq := startSeq
	for int64(len(records)) < limit {
		msg, err := s.js.GetMsg(StreamName, nextSeq, nats.DirectGetNext(subject), nats.Context(ctx))
		if err != nil {
			if errors.Is(err, nats.ErrMsgNotFound) || errors.Is(err, nats.ErrNoResponders) {
				break
			}
			return nil, fmt.Errorf("load thread history event after sequence %d: %w", nextSeq, err)
		}

		record, err := eventRecordFromRaw(msg)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
		nextSeq = msg.Sequence + 1
	}

	return records, nil
}

func (s *Store) scanBackward(ctx context.Context, subject string, startSeq uint64, limit int64) ([]threadstore.EventRecord, error) {
	if limit <= 0 || startSeq == 0 {
		return []threadstore.EventRecord{}, nil
	}

	reversed := make([]threadstore.EventRecord, 0, limit)
	for seq := startSeq; seq > 0 && int64(len(reversed)) < limit; seq-- {
		msg, err := s.js.GetMsg(StreamName, seq, nats.DirectGet(), nats.Context(ctx))
		if err != nil {
			if errors.Is(err, nats.ErrMsgNotFound) || errors.Is(err, nats.ErrNoResponders) {
				continue
			}
			return nil, fmt.Errorf("load thread history event at sequence %d: %w", seq, err)
		}
		if msg.Subject != subject {
			continue
		}

		record, err := eventRecordFromRaw(msg)
		if err != nil {
			return nil, err
		}
		reversed = append(reversed, record)
	}

	records := make([]threadstore.EventRecord, len(reversed))
	for i := range reversed {
		records[len(reversed)-1-i] = reversed[i]
	}
	return records, nil
}

func eventRecordFromRaw(msg *nats.RawStreamMsg) (threadstore.EventRecord, error) {
	var stored storedEvent
	if err := json.Unmarshal(msg.Data, &stored); err != nil {
		return threadstore.EventRecord{}, fmt.Errorf("decode thread history event %d: %w", msg.Sequence, err)
	}
	if strings.TrimSpace(stored.EventType) == "" {
		return threadstore.EventRecord{}, fmt.Errorf("thread history event %d missing event_type", msg.Sequence)
	}

	createdAt, err := time.Parse(time.RFC3339Nano, stored.CreatedAt)
	if err != nil {
		return threadstore.EventRecord{}, fmt.Errorf("parse thread history event %d created_at: %w", msg.Sequence, err)
	}

	return threadstore.EventRecord{
		EventSeq:         int64(msg.Sequence),
		SocketGeneration: stored.SocketGeneration,
		EventType:        stored.EventType,
		ResponseID:       stored.ResponseID,
		Payload:          append(json.RawMessage(nil), stored.Payload...),
		CreatedAt:        createdAt.UTC(),
	}, nil
}

func parseSequenceCursor(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, fmt.Errorf("cursor must be a numeric sequence")
	}

	value, err := strconv.ParseInt(raw, 10, 64)
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
	if limit > 500 {
		return 500
	}
	return limit
}
