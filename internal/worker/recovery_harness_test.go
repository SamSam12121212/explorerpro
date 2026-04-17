package worker

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"explorer/internal/threadcmd"
	"explorer/internal/threadstore"
)

type fakeSweepStore struct {
	threads                map[int64]threadstore.ThreadMeta
	owners                 map[int64]threadstore.OwnerRecord
	disconnectBatchResults []int64
	pruneBatchResults      []int64
	disconnectCalls        int
	pruneCalls             int
	nextReservedCommandID  int64
	stableCommandIDs       map[string]int64
}

func newFakeSweepStore() *fakeSweepStore {
	return &fakeSweepStore{
		threads:               map[int64]threadstore.ThreadMeta{},
		owners:                map[int64]threadstore.OwnerRecord{},
		nextReservedCommandID: 1000,
		stableCommandIDs:      map[string]int64{},
	}
}

func (s *fakeSweepStore) ListThreadIDsByStatus(_ context.Context, status threadstore.ThreadStatus) ([]int64, error) {
	ids := make([]int64, 0, len(s.threads))
	for id, meta := range s.threads {
		if meta.Status == status {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func (s *fakeSweepStore) LoadThread(_ context.Context, threadID int64) (threadstore.ThreadMeta, error) {
	meta, ok := s.threads[threadID]
	if !ok {
		return threadstore.ThreadMeta{}, threadstore.ErrThreadNotFound
	}
	return meta, nil
}

func (s *fakeSweepStore) LoadOwner(_ context.Context, threadID int64) (threadstore.OwnerRecord, error) {
	owner, ok := s.owners[threadID]
	if !ok {
		return threadstore.OwnerRecord{}, threadstore.ErrThreadNotFound
	}
	return owner, nil
}

func (s *fakeSweepStore) LoadOrCreateCommandID(_ context.Context, bus, kind, dedupeKey string) (int64, error) {
	key := bus + "|" + kind + "|" + dedupeKey
	if commandID, ok := s.stableCommandIDs[key]; ok {
		return commandID, nil
	}
	commandID := s.nextReservedCommandID
	s.nextReservedCommandID++
	s.stableCommandIDs[key] = commandID
	return commandID, nil
}

func (s *fakeSweepStore) DisconnectExpiredOpenAISocketSessions(_ context.Context, _, _ time.Time, _ int) (int64, error) {
	s.disconnectCalls++
	if len(s.disconnectBatchResults) == 0 {
		return 0, nil
	}
	result := s.disconnectBatchResults[0]
	s.disconnectBatchResults = s.disconnectBatchResults[1:]
	return result, nil
}

func (s *fakeSweepStore) PruneExpiredOpenAISocketSessions(_ context.Context, _ time.Time, _ int) (int64, error) {
	s.pruneCalls++
	if len(s.pruneBatchResults) == 0 {
		return 0, nil
	}
	result := s.pruneBatchResults[0]
	s.pruneBatchResults = s.pruneBatchResults[1:]
	return result, nil
}

type publishedCommand struct {
	subject string
	cmd     threadcmd.Command
}

type recoveryHarness struct {
	t         *testing.T
	ctx       context.Context
	service   *Service
	store     *fakeSweepStore
	published []publishedCommand
}

func newRecoveryHarness(t *testing.T, workerID int64) *recoveryHarness {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := newFakeSweepStore()
	h := &recoveryHarness{
		t:     t,
		ctx:   context.Background(),
		store: store,
	}
	h.service = &Service{
		logger:     logger,
		workerID:   workerID,
		sweepStore: store,
		actors:     map[int64]*threadActor{},
	}
	h.service.publishFn = func(_ context.Context, subject string, cmd threadcmd.Command) error {
		h.published = append(h.published, publishedCommand{
			subject: subject,
			cmd:     cmd,
		})
		return nil
	}
	return h
}

func (h *recoveryHarness) addThread(meta threadstore.ThreadMeta) {
	h.t.Helper()
	if meta.ID <= 0 {
		h.t.Fatal("thread id is required")
	}
	if meta.RootThreadID <= 0 {
		meta.RootThreadID = meta.ID
	}
	h.store.threads[meta.ID] = meta
}

func (h *recoveryHarness) addOwner(threadID int64, owner threadstore.OwnerRecord) {
	h.t.Helper()
	h.store.owners[threadID] = owner
}

func (h *recoveryHarness) addLiveActor(threadID int64) {
	h.t.Helper()
	h.service.actors[threadID] = &threadActor{done: make(chan struct{})}
}

func (h *recoveryHarness) recover(threadID int64) {
	h.t.Helper()
	if err := h.service.recoverThread(h.ctx, threadID); err != nil {
		h.t.Fatalf("recoverThread(%d) error = %v", threadID, err)
	}
}

func (h *recoveryHarness) rotateSweep() {
	h.t.Helper()
	h.service.scheduleSocketRotations(h.ctx)
}

func (h *recoveryHarness) requireSinglePublish() publishedCommand {
	h.t.Helper()
	if len(h.published) != 1 {
		h.t.Fatalf("published count = %d, want 1", len(h.published))
	}
	return h.published[0]
}

func TestRecoveryHarnessReconcilesExpiredRunningThread(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	h := newRecoveryHarness(t, tid("worker-local-1"))
	h.addThread(threadstore.ThreadMeta{
		ID:               100,
		RootThreadID:     100,
		Status:           threadstore.ThreadStatusRunning,
		SocketGeneration: 7,
		ActiveResponseID: "resp_active",
	})
	h.addOwner(100, threadstore.OwnerRecord{
		WorkerID:         tid("worker-dead"),
		SocketGeneration: 7,
		LeaseUntil:       now.Add(-time.Minute),
	})

	h.recover(100)

	published := h.requireSinglePublish()
	if published.subject != threadcmd.DispatchSubject(threadcmd.KindThreadReconcile) {
		t.Fatalf("subject = %q, want %q", published.subject, threadcmd.DispatchSubject(threadcmd.KindThreadReconcile))
	}
	if published.cmd.Kind != threadcmd.KindThreadReconcile {
		t.Fatalf("kind = %q, want %q", published.cmd.Kind, threadcmd.KindThreadReconcile)
	}
	if published.cmd.ExpectedSocketGeneration != 7 {
		t.Fatalf("ExpectedSocketGeneration = %d, want 7", published.cmd.ExpectedSocketGeneration)
	}

	body, err := published.cmd.ReconcileBody()
	if err != nil {
		t.Fatalf("ReconcileBody() error = %v", err)
	}
	if body.PreviousWorkerID != tid("worker-dead") {
		t.Fatalf("PreviousWorkerID = %d, want %d", body.PreviousWorkerID, tid("worker-dead"))
	}
	if body.RequiredGeneration != 7 {
		t.Fatalf("RequiredGeneration = %d, want 7", body.RequiredGeneration)
	}
}

func TestRecoveryHarnessAdoptsExpiredWaitingChildrenThread(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	h := newRecoveryHarness(t, tid("worker-local-1"))
	h.addThread(threadstore.ThreadMeta{
		ID:                 200,
		Status:             threadstore.ThreadStatusWaitingChildren,
		SocketGeneration:   3,
		ActiveSpawnGroupID: tid("sg_123"),
	})
	h.addOwner(200, threadstore.OwnerRecord{
		WorkerID:         tid("worker-dead"),
		SocketGeneration: 3,
		LeaseUntil:       now.Add(-time.Minute),
	})

	h.recover(200)

	published := h.requireSinglePublish()
	if published.subject != threadcmd.DispatchAdoptSubject {
		t.Fatalf("subject = %q, want %q", published.subject, threadcmd.DispatchAdoptSubject)
	}
	if published.cmd.Kind != threadcmd.KindThreadAdopt {
		t.Fatalf("kind = %q, want %q", published.cmd.Kind, threadcmd.KindThreadAdopt)
	}

	body, err := published.cmd.AdoptBody()
	if err != nil {
		t.Fatalf("AdoptBody() error = %v", err)
	}
	if body.PreviousWorkerID != tid("worker-dead") {
		t.Fatalf("PreviousWorkerID = %d, want %d", body.PreviousWorkerID, tid("worker-dead"))
	}
}

func TestRecoveryHarnessSkipsForeignLiveOwner(t *testing.T) {
	t.Parallel()

	h := newRecoveryHarness(t, tid("worker-local-1"))
	h.addThread(threadstore.ThreadMeta{
		ID:               300,
		Status:           threadstore.ThreadStatusRunning,
		SocketGeneration: 5,
		ActiveResponseID: "resp_active",
	})
	h.addOwner(300, threadstore.OwnerRecord{
		WorkerID:         tid("worker-other"),
		SocketGeneration: 5,
		LeaseUntil:       time.Now().UTC().Add(time.Minute),
	})

	h.recover(300)

	if len(h.published) != 0 {
		t.Fatalf("published count = %d, want 0", len(h.published))
	}
}

func TestRecoveryHarnessSweepsOpenAISocketSessionsUntilBatchDrains(t *testing.T) {
	t.Parallel()

	h := newRecoveryHarness(t, tid("worker-local-1"))
	h.store.disconnectBatchResults = []int64{socketSweepBatch, 3}
	h.store.pruneBatchResults = []int64{socketSweepBatch, 2}

	h.service.sweepOpenAISocketSessions(h.ctx)

	if h.store.disconnectCalls != 2 {
		t.Fatalf("disconnectCalls = %d, want 2", h.store.disconnectCalls)
	}
	if h.store.pruneCalls != 2 {
		t.Fatalf("pruneCalls = %d, want 2", h.store.pruneCalls)
	}
}

func TestRecoveryHarnessRequeuesOwnedThreadToSameWorkerOnRestart(t *testing.T) {
	t.Parallel()

	h := newRecoveryHarness(t, tid("worker-local-1"))
	h.addThread(threadstore.ThreadMeta{
		ID:               400,
		Status:           threadstore.ThreadStatusReconciling,
		SocketGeneration: 9,
		ActiveResponseID: "resp_active",
	})
	h.addOwner(400, threadstore.OwnerRecord{
		WorkerID:         tid("worker-local-1"),
		SocketGeneration: 9,
		LeaseUntil:       time.Now().UTC().Add(time.Minute),
	})

	h.recover(400)

	published := h.requireSinglePublish()
	wantSubject := threadcmd.WorkerCommandSubject(tid("worker-local-1"), threadcmd.KindThreadReconcile)
	if published.subject != wantSubject {
		t.Fatalf("subject = %q, want %q", published.subject, wantSubject)
	}
	if published.cmd.Kind != threadcmd.KindThreadReconcile {
		t.Fatalf("kind = %q, want %q", published.cmd.Kind, threadcmd.KindThreadReconcile)
	}
}

func TestRecoveryHarnessSkipsOwnedThreadWhenLiveActorExists(t *testing.T) {
	t.Parallel()

	h := newRecoveryHarness(t, tid("worker-local-1"))
	h.addThread(threadstore.ThreadMeta{
		ID:               500,
		Status:           threadstore.ThreadStatusRunning,
		SocketGeneration: 4,
		ActiveResponseID: "resp_active",
	})
	h.addOwner(500, threadstore.OwnerRecord{
		WorkerID:         tid("worker-local-1"),
		SocketGeneration: 4,
		LeaseUntil:       time.Now().UTC().Add(time.Minute),
	})
	h.addLiveActor(500)

	h.recover(500)

	if len(h.published) != 0 {
		t.Fatalf("published count = %d, want 0", len(h.published))
	}
}

func TestRecoveryHarnessSuppressesReconcileAfterAttemptCap(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	h := newRecoveryHarness(t, tid("worker-local-1"))
	h.addThread(threadstore.ThreadMeta{
		ID:               700,
		RootThreadID:     700,
		Status:           threadstore.ThreadStatusRunning,
		SocketGeneration: 1,
		ActiveResponseID: "resp_active",
	})
	h.addOwner(700, threadstore.OwnerRecord{
		WorkerID:         tid("worker-dead"),
		SocketGeneration: 1,
		LeaseUntil:       now.Add(-time.Minute),
	})

	for i := 0; i < maxRecoveryAttemptsPerThread+3; i++ {
		h.recover(700)
	}

	if len(h.published) != maxRecoveryAttemptsPerThread {
		t.Fatalf("published count = %d, want %d", len(h.published), maxRecoveryAttemptsPerThread)
	}
}

func TestRecoveryHarnessSchedulesRotationOnlyForLocalLiveActors(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	h := newRecoveryHarness(t, tid("worker-local-1"))
	h.addThread(threadstore.ThreadMeta{
		ID:               600,
		Status:           threadstore.ThreadStatusReady,
		OwnerWorkerID:    tid("worker-local-1"),
		SocketGeneration: 11,
		SocketExpiresAt:  now.Add(4 * time.Minute),
		LastResponseID:   "resp_latest",
	})
	h.addThread(threadstore.ThreadMeta{
		ID:               601,
		Status:           threadstore.ThreadStatusReady,
		OwnerWorkerID:    tid("worker-local-1"),
		SocketGeneration: 12,
		SocketExpiresAt:  now.Add(4 * time.Minute),
	})
	h.addThread(threadstore.ThreadMeta{
		ID:               602,
		Status:           threadstore.ThreadStatusReady,
		OwnerWorkerID:    tid("worker-other"),
		SocketGeneration: 13,
		SocketExpiresAt:  now.Add(4 * time.Minute),
	})
	h.addLiveActor(600)

	h.rotateSweep()

	published := h.requireSinglePublish()
	wantSubject := threadcmd.WorkerCommandSubject(tid("worker-local-1"), threadcmd.KindThreadRotateSocket)
	if published.subject != wantSubject {
		t.Fatalf("subject = %q, want %q", published.subject, wantSubject)
	}
	if published.cmd.Kind != threadcmd.KindThreadRotateSocket {
		t.Fatalf("kind = %q, want %q", published.cmd.Kind, threadcmd.KindThreadRotateSocket)
	}
	if published.cmd.ThreadID != 600 {
		t.Fatalf("ThreadID = %d, want 600", published.cmd.ThreadID)
	}
	if published.cmd.ExpectedSocketGeneration != 11 {
		t.Fatalf("ExpectedSocketGeneration = %d, want 11", published.cmd.ExpectedSocketGeneration)
	}
}
