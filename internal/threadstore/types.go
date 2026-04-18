package threadstore

import (
	"encoding/json"
	"errors"
	"time"
)

var ErrThreadNotFound = errors.New("thread not found")
var ErrThreadBusy = errors.New("thread busy")
var ErrThreadNotRoot = errors.New("thread is not a root thread")

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
	ID                 int64
	RootThreadID       int64
	ParentThreadID     int64
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
	OwnerWorkerID      int64
	SocketGeneration   uint64
	SocketExpiresAt    time.Time
	LastResponseID     string
	ActiveResponseID   string
	ActiveSpawnGroupID int64
	ChildKind          string
	DocumentID         int64
	DocumentPhase      string
	// OneShot marks a child thread that must emit a single response.create
	// and then release its socket without entering the warm/idle loop.
	// Used today by citation_locator children; general-purpose so other
	// short-lived child flavours can reuse the behaviour.
	OneShot    bool
	ArchivedAt time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

type ItemLogEntry struct {
	ThreadID    int64
	ResponseID  string
	ItemType    string
	Direction   string
	PayloadJSON string
	CreatedAt   time.Time
}

type EventLogEntry struct {
	ThreadID         int64
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
	PreviousWorkerID int64
}

type SpawnGroupStatus string

const (
	SpawnGroupStatusWaiting   SpawnGroupStatus = "waiting"
	SpawnGroupStatusClosed    SpawnGroupStatus = "closed"
	SpawnGroupStatusCancelled SpawnGroupStatus = "cancelled"
)

type SpawnGroupMeta struct {
	ID                   int64
	ParentThreadID       int64
	ParentCallID         string
	GroupKind            string
	StableKey            string
	Expected             int
	Completed            int
	Failed               int
	Cancelled            int
	Status               SpawnGroupStatus
	AggregateSubmittedAt time.Time
	AggregateCmdID       int64
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

type SpawnChildResult struct {
	ChildThreadID   int64     `json:"child_thread_id"`
	DocumentID      int64     `json:"-"`
	Status          string    `json:"status"`
	ChildResponseID string    `json:"child_response_id,omitempty"`
	AssistantText   string    `json:"assistant_text,omitempty"`
	ResultRef       string    `json:"result_ref,omitempty"`
	SummaryRef      string    `json:"summary_ref,omitempty"`
	ErrorRef        string    `json:"error_ref,omitempty"`
	// ToolCallArgsJSON is the forced-tool-call arguments emitted by a
	// one-shot child (e.g. the evidence locator). Only populated for
	// child kinds that force structured output via tool_choice.
	ToolCallArgsJSON string    `json:"tool_call_args_json,omitempty"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type OwnerRecord struct {
	WorkerID         int64
	SocketGeneration uint64
	LeaseUntil       time.Time
	ClaimedAt        time.Time
	UpdatedAt        time.Time
}

type WorkerRecord struct {
	ID              int64
	ServiceName     string
	NATSClientName  string
	ResponsesWSURL  string
	LeaseUntil      time.Time
	StartedAt       time.Time
	LastHeartbeatAt time.Time
	StoppedAt       time.Time
	StopReason      string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

type DocumentQueryLineage struct {
	ChildThreadID int64
	ResponseID    string
	Model         string
}

type OpenAISocketState string

const (
	OpenAISocketStateConnected    OpenAISocketState = "connected"
	OpenAISocketStateDisconnected OpenAISocketState = "disconnected"
)

type OpenAISocketSession struct {
	ID                     string
	ThreadID               int64
	RootThreadID           int64
	ParentThreadID         int64
	WorkerID               int64
	ThreadSocketGeneration uint64
	State                  OpenAISocketState
	ConnectedAt            time.Time
	LastReadAt             time.Time
	LastWriteAt            time.Time
	LastHeartbeatAt        time.Time
	HeartbeatExpiresAt     time.Time
	DisconnectedAt         time.Time
	DisconnectReason       string
	ExpiresAt              time.Time
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

type OpenAISocketTouch struct {
	ID                 string
	LastReadAt         time.Time
	LastWriteAt        time.Time
	LastHeartbeatAt    time.Time
	HeartbeatExpiresAt time.Time
}
