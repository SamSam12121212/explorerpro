package worker

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"explorer/internal/agentcmd"
	"explorer/internal/blobstore"
	"explorer/internal/openaiws"
	"explorer/internal/preparedinput"
	"explorer/internal/threadhistory"
	"explorer/internal/threadstore"
)

type fakeActorStore struct {
	t *testing.T

	threads                map[string]threadstore.ThreadMeta
	latestClientCreateByID map[string]json.RawMessage
	spawnGroups            map[string]threadstore.SpawnGroupMeta
	spawnResults           map[string][]threadstore.SpawnChildResult

	savedThreads        []threadstore.ThreadMeta
	savedSpawnGroups    []threadstore.SpawnGroupMeta
	appendedItems       []threadstore.ItemLogEntry
	historyEvents       []threadstore.EventLogEntry
	savedResponses      map[string]json.RawMessage
	releasedThreads     []string
	createdSockets      []threadstore.OpenAISocketSession
	touchedSockets      []threadstore.OpenAISocketTouch
	disconnectedSockets []string
}

func newFakeActorStore(t *testing.T) *fakeActorStore {
	t.Helper()
	return &fakeActorStore{
		t:                      t,
		threads:                map[string]threadstore.ThreadMeta{},
		latestClientCreateByID: map[string]json.RawMessage{},
		spawnGroups:            map[string]threadstore.SpawnGroupMeta{},
		spawnResults:           map[string][]threadstore.SpawnChildResult{},
		savedResponses:         map[string]json.RawMessage{},
	}
}

func (s *fakeActorStore) CreateThreadIfAbsent(_ context.Context, meta threadstore.ThreadMeta) error {
	if _, exists := s.threads[meta.ID]; !exists {
		s.threads[meta.ID] = meta
	}
	return nil
}

func (s *fakeActorStore) LoadThread(_ context.Context, threadID string) (threadstore.ThreadMeta, error) {
	meta, ok := s.threads[threadID]
	if !ok {
		return threadstore.ThreadMeta{}, threadstore.ErrThreadNotFound
	}
	return meta, nil
}

func (s *fakeActorStore) LoadLatestCompletedDocumentQueryLineage(_ context.Context, parentThreadID string, documentID int64) (threadstore.DocumentQueryLineage, error) {
	documentIDText := strconv.FormatInt(documentID, 10)
	var latest threadstore.ThreadMeta
	found := false

	for _, meta := range s.threads {
		if meta.ParentThreadID != parentThreadID || strings.TrimSpace(meta.LastResponseID) == "" {
			continue
		}

		var metadata map[string]string
		if strings.TrimSpace(meta.MetadataJSON) == "" {
			continue
		}
		if err := json.Unmarshal([]byte(meta.MetadataJSON), &metadata); err != nil {
			s.t.Fatalf("json.Unmarshal(meta.MetadataJSON) error = %v", err)
		}
		if metadata["spawn_mode"] != "document_query" || metadata["document_id"] != documentIDText {
			continue
		}
		if meta.Status != threadstore.ThreadStatusCompleted && meta.Status != threadstore.ThreadStatusReady {
			continue
		}

		if !found || meta.UpdatedAt.After(latest.UpdatedAt) || (meta.UpdatedAt.Equal(latest.UpdatedAt) && meta.CreatedAt.After(latest.CreatedAt)) || (meta.UpdatedAt.Equal(latest.UpdatedAt) && meta.CreatedAt.Equal(latest.CreatedAt) && meta.ID > latest.ID) {
			latest = meta
			found = true
		}
	}

	if !found {
		return threadstore.DocumentQueryLineage{}, threadstore.ErrThreadNotFound
	}

	return threadstore.DocumentQueryLineage{
		ChildThreadID: latest.ID,
		ResponseID:    latest.LastResponseID,
		Model:         latest.Model,
	}, nil
}

func (s *fakeActorStore) SaveThread(_ context.Context, meta threadstore.ThreadMeta) error {
	s.threads[meta.ID] = meta
	s.savedThreads = append(s.savedThreads, meta)
	return nil
}

func (s *fakeActorStore) CommandProcessed(_ context.Context, _, _ string) (bool, error) {
	return false, nil
}

func (s *fakeActorStore) MarkCommandProcessed(_ context.Context, _, _ string) (bool, error) {
	return true, nil
}

func (s *fakeActorStore) ClaimOwnership(_ context.Context, _, _ string, _ time.Time) (threadstore.ClaimResult, error) {
	return threadstore.ClaimResult{Claimed: true, SocketGeneration: 1}, nil
}

func (s *fakeActorStore) RenewOwnership(_ context.Context, _, _ string, _ uint64, _ time.Time) (bool, error) {
	return true, nil
}

func (s *fakeActorStore) RotateOwnership(_ context.Context, _, _ string, currentGeneration uint64, _, _ time.Time) (uint64, bool, error) {
	return currentGeneration + 1, true, nil
}

func (s *fakeActorStore) ReleaseOwnership(_ context.Context, threadID, _ string, _ uint64) error {
	s.releasedThreads = append(s.releasedThreads, threadID)
	return nil
}

func (s *fakeActorStore) CreateOpenAISocketSession(_ context.Context, session threadstore.OpenAISocketSession) error {
	s.createdSockets = append(s.createdSockets, session)
	return nil
}

func (s *fakeActorStore) TouchOpenAISocketSession(_ context.Context, touch threadstore.OpenAISocketTouch) error {
	s.touchedSockets = append(s.touchedSockets, touch)
	return nil
}

func (s *fakeActorStore) DisconnectOpenAISocketSession(_ context.Context, socketID, _ string, _, _ time.Time) error {
	s.disconnectedSockets = append(s.disconnectedSockets, socketID)
	return nil
}

func (s *fakeActorStore) AppendItem(_ context.Context, entry threadstore.ItemLogEntry) (threadstore.ItemRecord, error) {
	s.appendedItems = append(s.appendedItems, entry)
	return threadstore.ItemRecord{
		Seq:        int64(len(s.appendedItems)),
		ResponseID: entry.ResponseID,
		ItemType:   entry.ItemType,
		Direction:  entry.Direction,
		Payload:    json.RawMessage(entry.PayloadJSON),
		CreatedAt:  entry.CreatedAt,
	}, nil
}

func (s *fakeActorStore) SaveResponseRaw(_ context.Context, _ string, responseID string, payload json.RawMessage) error {
	s.savedResponses[responseID] = append(json.RawMessage(nil), payload...)
	return nil
}

func (s *fakeActorStore) SaveResponseCreateCheckpoint(_ context.Context, threadID, _ string, payload json.RawMessage) error {
	s.latestClientCreateByID[threadID] = append(json.RawMessage(nil), payload...)
	return nil
}

func (s *fakeActorStore) LoadLatestResponseCreateCheckpoint(_ context.Context, threadID string) (json.RawMessage, error) {
	raw, ok := s.latestClientCreateByID[threadID]
	if !ok {
		return nil, threadhistory.ErrCheckpointNotFound
	}
	return append(json.RawMessage(nil), raw...), nil
}

func (s *fakeActorStore) AppendEvent(_ context.Context, entry threadstore.EventLogEntry, _ string) error {
	s.historyEvents = append(s.historyEvents, entry)
	return nil
}

func (s *fakeActorStore) ListItems(_ context.Context, threadID string, _ threadstore.ListOptions) ([]threadstore.ItemRecord, error) {
	var items []threadstore.ItemRecord
	for _, entry := range s.appendedItems {
		if entry.ThreadID != threadID {
			continue
		}
		items = append(items, threadstore.ItemRecord{
			Seq:        int64(len(items) + 1),
			ResponseID: entry.ResponseID,
			ItemType:   entry.ItemType,
			Direction:  entry.Direction,
			Payload:    json.RawMessage(entry.PayloadJSON),
			CreatedAt:  entry.CreatedAt,
		})
	}
	return items, nil
}

func (s *fakeActorStore) CreateSpawnGroup(_ context.Context, meta threadstore.SpawnGroupMeta, _ []string) error {
	s.spawnGroups[meta.ID] = meta
	return nil
}

func (s *fakeActorStore) LoadSpawnGroup(_ context.Context, spawnGroupID string) (threadstore.SpawnGroupMeta, error) {
	meta, ok := s.spawnGroups[spawnGroupID]
	if !ok {
		return threadstore.SpawnGroupMeta{}, threadstore.ErrThreadNotFound
	}
	return meta, nil
}

func (s *fakeActorStore) SaveSpawnGroup(_ context.Context, meta threadstore.SpawnGroupMeta) error {
	s.spawnGroups[meta.ID] = meta
	s.savedSpawnGroups = append(s.savedSpawnGroups, meta)
	return nil
}

func (s *fakeActorStore) ListSpawnResults(_ context.Context, spawnGroupID string) ([]threadstore.SpawnChildResult, error) {
	results := s.spawnResults[spawnGroupID]
	cloned := make([]threadstore.SpawnChildResult, len(results))
	copy(cloned, results)
	return cloned, nil
}

func (s *fakeActorStore) UpsertSpawnResult(_ context.Context, spawnGroupID string, result threadstore.SpawnChildResult) (bool, []threadstore.SpawnChildResult, error) {
	results := append(s.spawnResults[spawnGroupID], result)
	s.spawnResults[spawnGroupID] = results
	cloned := make([]threadstore.SpawnChildResult, len(results))
	copy(cloned, results)
	return true, cloned, nil
}

func newActorRecoveryHarness(t *testing.T, store *fakeActorStore, conn *actorTestConn) *threadActor {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	actor := &threadActor{
		threadID: "thread_parent",
		workerID: "worker-local-1",
		logger:   logger,
		store:    store,
		history:  store,
		cfg:      testOpenAIConfig(),
		publish:  func(context.Context, string, agentcmd.Command) error { return nil },
		ctx:      context.Background(),
	}

	if conn != nil {
		session := openaiws.NewSession(testOpenAIConfig(), &actorTestDialer{conn: conn})
		if err := session.Connect(context.Background()); err != nil {
			t.Fatalf("session.Connect() error = %v", err)
		}
		actor.session = session
		actor.sessionFactory = func() *openaiws.Session {
			t.Fatal("sessionFactory should not be called")
			return nil
		}
	} else {
		actor.sessionFactory = func() *openaiws.Session {
			t.Fatal("sessionFactory should not be called")
			return nil
		}
	}

	return actor
}

func TestActorRecoveryHarnessReconcileFromCheckpointReplaysLatestCreate(t *testing.T) {
	t.Parallel()

	store := newFakeActorStore(t)
	store.threads["thread_parent"] = threadstore.ThreadMeta{
		ID:               "thread_parent",
		Status:           threadstore.ThreadStatusReconciling,
		Model:            "gpt-5.4",
		ActiveResponseID: "resp_active",
	}
	store.latestClientCreateByID["thread_parent"] = json.RawMessage(`{
		"type":"response.create",
		"model":"gpt-5.4",
		"previous_response_id":"resp_prev",
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}],
		"store":true
	}`)

	conn := &actorTestConn{
		reads: [][]byte{
			[]byte(`{"type":"response.created","response":{"id":"resp_replayed"}}`),
			[]byte(`{"type":"response.completed","response":{"id":"resp_replayed"}}`),
		},
	}
	actor := newActorRecoveryHarness(t, store, conn)

	err := actor.reconcileFromCheckpoint(store.threads["thread_parent"], "cmd_reconcile")
	if err != nil {
		t.Fatalf("reconcileFromCheckpoint() error = %v", err)
	}

	if len(conn.writes) != 1 {
		t.Fatalf("writes = %d, want 1", len(conn.writes))
	}

	var sent map[string]any
	if err := json.Unmarshal(conn.writes[0], &sent); err != nil {
		t.Fatalf("json.Unmarshal(sent) error = %v", err)
	}
	if sent["previous_response_id"] != "resp_prev" {
		t.Fatalf("previous_response_id = %v, want resp_prev", sent["previous_response_id"])
	}

	if len(store.savedThreads) < 3 {
		t.Fatalf("savedThreads = %d, want at least 3 state transitions", len(store.savedThreads))
	}
	if store.savedThreads[0].ActiveResponseID != "" {
		t.Fatalf("first saved ActiveResponseID = %q, want empty", store.savedThreads[0].ActiveResponseID)
	}

	final := store.threads["thread_parent"]
	if final.Status != threadstore.ThreadStatusReady {
		t.Fatalf("final status = %q, want ready", final.Status)
	}
	if final.LastResponseID != "resp_replayed" {
		t.Fatalf("LastResponseID = %q, want resp_replayed", final.LastResponseID)
	}
	if final.ActiveResponseID != "" {
		t.Fatalf("ActiveResponseID = %q, want empty", final.ActiveResponseID)
	}
}

func TestActorRecoveryHarnessReconcileFromPreparedInputRefCheckpoint(t *testing.T) {
	t.Parallel()

	store := newFakeActorStore(t)
	store.threads["thread_parent"] = threadstore.ThreadMeta{
		ID:               "thread_parent",
		Status:           threadstore.ThreadStatusReconciling,
		Model:            "gpt-5.4",
		ActiveResponseID: "resp_active",
	}

	blob, err := blobstore.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal() error = %v", err)
	}
	preparedStore, err := preparedinput.NewStore(blob)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	ref, err := preparedStore.Write(context.Background(), "pi_reconcile", preparedinput.Artifact{
		Version: preparedinput.VersionV1,
		Input:   []byte(`[{"type":"message","role":"user","content":[{"type":"input_text","text":"replayed prepared"}]}]`),
	})
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	store.latestClientCreateByID["thread_parent"] = json.RawMessage(`{
		"type":"response.create",
		"model":"gpt-5.4",
		"prepared_input_ref":"` + ref + `",
		"store":true
	}`)

	conn := &actorTestConn{
		reads: [][]byte{
			[]byte(`{"type":"response.created","response":{"id":"resp_replayed"}}`),
			[]byte(`{"type":"response.completed","response":{"id":"resp_replayed"}}`),
		},
	}

	actor := newActorRecoveryHarness(t, store, conn)
	actor.blob = blob

	if err := actor.reconcileFromCheckpoint(store.threads["thread_parent"], "cmd_reconcile"); err != nil {
		t.Fatalf("reconcileFromCheckpoint() error = %v", err)
	}

	if len(conn.writes) != 1 {
		t.Fatalf("writes = %d, want 1", len(conn.writes))
	}

	var sent map[string]any
	if err := json.Unmarshal(conn.writes[0], &sent); err != nil {
		t.Fatalf("json.Unmarshal(sent) error = %v", err)
	}

	if _, exists := sent["prepared_input_ref"]; exists {
		t.Fatalf("prepared_input_ref should not be sent to OpenAI, got %#v", sent["prepared_input_ref"])
	}

	items, ok := sent["input"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("input = %#v, want one prepared item", sent["input"])
	}

	message, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("message = %#v, want object", items[0])
	}
	content, ok := message["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("content = %#v, want one text part", message["content"])
	}
	part, ok := content[0].(map[string]any)
	if !ok || part["text"] != "replayed prepared" {
		t.Fatalf("content[0] = %#v, want replayed prepared", content[0])
	}
}

func TestActorRecoveryHarnessReconcileFromCheckpointMissingCheckpointLeavesThreadPassive(t *testing.T) {
	t.Parallel()

	store := newFakeActorStore(t)
	store.threads["thread_parent"] = threadstore.ThreadMeta{
		ID:               "thread_parent",
		Status:           threadstore.ThreadStatusReconciling,
		Model:            "gpt-5.4",
		OwnerWorkerID:    "worker-local-1",
		SocketGeneration: 6,
		SocketExpiresAt:  time.Now().UTC().Add(time.Minute),
		ActiveResponseID: "resp_active",
		LastResponseID:   "resp_prev",
	}

	actor := newActorRecoveryHarness(t, store, nil)

	err := actor.recoverThread(store.threads["thread_parent"], "cmd_reconcile", true)
	if err != nil {
		t.Fatalf("recoverThread() error = %v", err)
	}

	final := store.threads["thread_parent"]
	if final.Status != threadstore.ThreadStatusIncomplete {
		t.Fatalf("final status = %q, want incomplete", final.Status)
	}
	if final.ActiveResponseID != "" {
		t.Fatalf("ActiveResponseID = %q, want empty", final.ActiveResponseID)
	}
	if final.OwnerWorkerID != "" {
		t.Fatalf("OwnerWorkerID = %q, want empty", final.OwnerWorkerID)
	}
	if !final.SocketExpiresAt.IsZero() {
		t.Fatalf("SocketExpiresAt = %s, want zero", final.SocketExpiresAt)
	}
	if final.LastResponseID != "resp_prev" {
		t.Fatalf("LastResponseID = %q, want resp_prev", final.LastResponseID)
	}
	if len(store.releasedThreads) != 1 || store.releasedThreads[0] != "thread_parent" {
		t.Fatalf("releasedThreads = %#v, want [thread_parent]", store.releasedThreads)
	}
}

func TestActorRecoveryHarnessRecoverWaitingChildrenKeepsBarrierOpen(t *testing.T) {
	t.Parallel()

	store := newFakeActorStore(t)
	store.threads["thread_parent"] = threadstore.ThreadMeta{
		ID:                 "thread_parent",
		Status:             threadstore.ThreadStatusWaitingChildren,
		ActiveSpawnGroupID: "sg_waiting",
		LastResponseID:     "resp_parent",
	}
	store.spawnGroups["sg_waiting"] = threadstore.SpawnGroupMeta{
		ID:             "sg_waiting",
		ParentThreadID: "thread_parent",
		Expected:       2,
		Status:         threadstore.SpawnGroupStatusWaiting,
	}
	store.spawnResults["sg_waiting"] = []threadstore.SpawnChildResult{
		{ChildThreadID: "thread_child_1", Status: "completed"},
	}

	actor := newActorRecoveryHarness(t, store, nil)

	err := actor.recoverWaitingChildren(store.threads["thread_parent"], "cmd_recover")
	if err != nil {
		t.Fatalf("recoverWaitingChildren() error = %v", err)
	}

	if len(store.savedSpawnGroups) == 0 {
		t.Fatal("expected spawn group to be resaved with updated counts")
	}
	spawn := store.spawnGroups["sg_waiting"]
	if spawn.Status != threadstore.SpawnGroupStatusWaiting {
		t.Fatalf("spawn status = %q, want waiting", spawn.Status)
	}
	if spawn.Completed != 1 || spawn.Failed != 0 || spawn.Cancelled != 0 {
		t.Fatalf("spawn counts = completed:%d failed:%d cancelled:%d, want 1/0/0", spawn.Completed, spawn.Failed, spawn.Cancelled)
	}

	final := store.threads["thread_parent"]
	if final.Status != threadstore.ThreadStatusWaitingChildren {
		t.Fatalf("final status = %q, want waiting_children", final.Status)
	}
	if len(store.appendedItems) != 0 {
		t.Fatalf("appendedItems = %d, want 0", len(store.appendedItems))
	}
}

func TestActorRecoveryHarnessRecoverWaitingChildrenResumesParentAfterBarrierCloses(t *testing.T) {
	t.Parallel()

	store := newFakeActorStore(t)
	store.threads["thread_parent"] = threadstore.ThreadMeta{
		ID:                 "thread_parent",
		Status:             threadstore.ThreadStatusWaitingChildren,
		ActiveSpawnGroupID: "sg_closed",
		LastResponseID:     "resp_parent",
		Model:              "gpt-5.4",
	}
	store.spawnGroups["sg_closed"] = threadstore.SpawnGroupMeta{
		ID:             "sg_closed",
		ParentThreadID: "thread_parent",
		ParentCallID:   "call_parent",
		Expected:       2,
		Status:         threadstore.SpawnGroupStatusWaiting,
	}
	store.spawnResults["sg_closed"] = []threadstore.SpawnChildResult{
		{ChildThreadID: "thread_child_1", Status: "completed", ChildResponseID: "resp_child_1", AssistantText: "One"},
		{ChildThreadID: "thread_child_2", Status: "completed", ChildResponseID: "resp_child_2", AssistantText: "Two"},
	}

	conn := &actorTestConn{
		reads: [][]byte{
			[]byte(`{"type":"response.created","response":{"id":"resp_after_barrier"}}`),
			[]byte(`{"type":"response.completed","response":{"id":"resp_after_barrier"}}`),
		},
	}
	actor := newActorRecoveryHarness(t, store, conn)

	err := actor.recoverWaitingChildren(store.threads["thread_parent"], "cmd_recover_waiting")
	if err != nil {
		t.Fatalf("recoverWaitingChildren() error = %v", err)
	}

	if len(store.appendedItems) != 1 {
		t.Fatalf("appendedItems = %d, want 1", len(store.appendedItems))
	}
	inputItem := store.appendedItems[0]
	if inputItem.ResponseID != "resp_parent" {
		t.Fatalf("input ResponseID = %q, want resp_parent", inputItem.ResponseID)
	}
	if inputItem.ItemType != "function_call_output" {
		t.Fatalf("input ItemType = %q, want function_call_output", inputItem.ItemType)
	}

	spawn := store.spawnGroups["sg_closed"]
	if spawn.AggregateSubmittedAt.IsZero() {
		t.Fatal("expected AggregateSubmittedAt to be set")
	}
	if spawn.AggregateCmdID != "cmd_recover_waiting" {
		t.Fatalf("AggregateCmdID = %q, want cmd_recover_waiting", spawn.AggregateCmdID)
	}

	if len(conn.writes) != 1 {
		t.Fatalf("writes = %d, want 1", len(conn.writes))
	}
	var sent map[string]any
	if err := json.Unmarshal(conn.writes[0], &sent); err != nil {
		t.Fatalf("json.Unmarshal(sent) error = %v", err)
	}
	if sent["previous_response_id"] != "resp_parent" {
		t.Fatalf("previous_response_id = %v, want resp_parent", sent["previous_response_id"])
	}

	final := store.threads["thread_parent"]
	if final.Status != threadstore.ThreadStatusReady {
		t.Fatalf("final status = %q, want ready", final.Status)
	}
	if final.LastResponseID != "resp_after_barrier" {
		t.Fatalf("LastResponseID = %q, want resp_after_barrier", final.LastResponseID)
	}
}
