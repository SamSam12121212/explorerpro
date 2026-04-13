package threadstore

import (
	"encoding/json"
	"errors"
	"time"
)

var ErrThreadNotFound = errors.New("thread not found")

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
	Seq        int64
	ResponseID string
	ItemType   string
	Direction  string
	Payload    json.RawMessage
	CreatedAt  time.Time
}

type EventRecord struct {
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

type DocumentQueryLineage struct {
	ChildThreadID string
	ResponseID    string
	Model         string
}
