package worker

import (
	"testing"
	"time"

	"explorer/internal/threadstore"
)

func TestShouldRotateSocket(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 13, 18, 30, 0, 0, time.UTC)
	meta := threadstore.ThreadMeta{
		ID:               "thread_123",
		OwnerWorkerID:    "worker_a",
		Status:           threadstore.ThreadStatusReady,
		SocketGeneration: 3,
		SocketExpiresAt:  now.Add(4 * time.Minute),
	}

	if !shouldRotateSocket(meta, "worker_a", now) {
		t.Fatal("expected socket to be eligible for rotation")
	}

	meta.Status = threadstore.ThreadStatusRunning
	if shouldRotateSocket(meta, "worker_a", now) {
		t.Fatal("did not expect running thread to be eligible for rotation")
	}
}

func TestRecoverySweepStatusesExcludesPassiveThreads(t *testing.T) {
	t.Parallel()

	statuses := recoverySweepStatuses()
	seen := make(map[threadstore.ThreadStatus]bool, len(statuses))
	for _, status := range statuses {
		seen[status] = true
	}

	if seen[threadstore.ThreadStatusReady] {
		t.Fatal("did not expect ready threads to be recovered on startup")
	}
	if seen[threadstore.ThreadStatusWaitingTool] {
		t.Fatal("did not expect waiting_tool threads to be recovered on startup")
	}
	if !seen[threadstore.ThreadStatusWaitingChildren] {
		t.Fatal("expected waiting_children threads to be recovered on startup")
	}
	if !seen[threadstore.ThreadStatusRunning] {
		t.Fatal("expected running threads to be recovered on startup")
	}
	if !seen[threadstore.ThreadStatusReconciling] {
		t.Fatal("expected reconciling threads to be recovered on startup")
	}
}
