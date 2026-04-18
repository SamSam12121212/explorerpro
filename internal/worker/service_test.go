package worker

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"explorer/internal/threadstore"
)

func TestDecodeSpawnRequestRejectsExcessChildren(t *testing.T) {
	t.Parallel()

	children := make([]map[string]any, maxChildrenPerSpawn+1)
	for i := range children {
		children[i] = map[string]any{"prompt": "go"}
	}
	args, err := json.Marshal(map[string]any{"children": children})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	_, err = decodeSpawnRequest(string(args), threadstore.ThreadMeta{ID: 7, Depth: 0})
	if err == nil {
		t.Fatal("expected decodeSpawnRequest to reject more than max children")
	}
	if !strings.Contains(err.Error(), "per-turn cap") {
		t.Fatalf("error %q does not mention per-turn cap", err)
	}
}

func TestDecodeSpawnRequestAcceptsCapBoundary(t *testing.T) {
	t.Parallel()

	children := make([]map[string]any, maxChildrenPerSpawn)
	for i := range children {
		children[i] = map[string]any{"prompt": "go"}
	}
	args, err := json.Marshal(map[string]any{"children": children})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	req, err := decodeSpawnRequest(string(args), threadstore.ThreadMeta{ID: 7, Depth: 0})
	if err != nil {
		t.Fatalf("decodeSpawnRequest at cap returned error: %v", err)
	}
	if len(req.Children) != maxChildrenPerSpawn {
		t.Fatalf("got %d children, want %d", len(req.Children), maxChildrenPerSpawn)
	}
}

func TestShouldRotateSocket(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 13, 18, 30, 0, 0, time.UTC)
	meta := threadstore.ThreadMeta{
		ID:               123,
		OwnerWorkerID:    tid("worker_a"),
		Status:           threadstore.ThreadStatusReady,
		SocketGeneration: 3,
		SocketExpiresAt:  now.Add(4 * time.Minute),
	}

	if !shouldRotateSocket(meta, tid("worker_a"), now) {
		t.Fatal("expected socket to be eligible for rotation")
	}

	meta.Status = threadstore.ThreadStatusRunning
	if shouldRotateSocket(meta, tid("worker_a"), now) {
		t.Fatal("did not expect running thread to be eligible for rotation")
	}
}

func TestSocketBudgetReserveAndRelease(t *testing.T) {
	t.Parallel()

	s := &Service{socketBudgetMax: 3}

	for i := 0; i < 3; i++ {
		if err := s.reserveSocketSlot(); err != nil {
			t.Fatalf("reserve %d: %v", i, err)
		}
	}

	if err := s.reserveSocketSlot(); err == nil {
		t.Fatal("expected reserveSocketSlot to fail when budget is exhausted")
	}

	used, max := s.socketBudgetSnapshot()
	if used != 3 || max != 3 {
		t.Fatalf("snapshot used=%d max=%d, want 3/3", used, max)
	}

	s.releaseSocketSlot()
	if err := s.reserveSocketSlot(); err != nil {
		t.Fatalf("reserve after release: %v", err)
	}

	// Releasing past zero must not panic or underflow.
	for i := 0; i < 10; i++ {
		s.releaseSocketSlot()
	}
	used, _ = s.socketBudgetSnapshot()
	if used != 0 {
		t.Fatalf("snapshot used=%d after over-release, want 0", used)
	}
}

func TestSocketBudgetUnboundedWhenMaxNonPositive(t *testing.T) {
	t.Parallel()

	s := &Service{socketBudgetMax: 0}
	for i := 0; i < 1000; i++ {
		if err := s.reserveSocketSlot(); err != nil {
			t.Fatalf("reserve %d unexpectedly failed: %v", i, err)
		}
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
