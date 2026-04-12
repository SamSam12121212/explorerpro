package agentcmd

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

const (
	StreamName            = "AGENT_CMD"
	DispatchQueue         = "agent-dispatchers"
	DispatchStartSubject  = "agent.dispatch.thread.start"
	DispatchAdoptSubject  = "agent.dispatch.thread.adopt"
	WorkerSubjectTemplate = "agent.worker.%s.cmd.%s"
)

type Kind string

const (
	KindThreadStart            Kind = "thread.start"
	KindThreadResume           Kind = "thread.resume"
	KindThreadSubmitToolOutput Kind = "thread.submit_tool_output"
	KindThreadChildCompleted   Kind = "thread.child_completed"
	KindThreadChildFailed      Kind = "thread.child_failed"
	KindThreadCancel           Kind = "thread.cancel"
	KindThreadDisconnectSocket Kind = "thread.disconnect_socket"
	KindThreadRotateSocket     Kind = "thread.rotate_socket"
	KindThreadReconcile        Kind = "thread.reconcile"
	KindThreadAdopt            Kind = "thread.adopt"
)

var durableSanitizer = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

type Command struct {
	CmdID                    string          `json:"cmd_id"`
	Kind                     Kind            `json:"kind"`
	ThreadID                 string          `json:"thread_id"`
	RootThreadID             string          `json:"root_thread_id"`
	CausationID              string          `json:"causation_id,omitempty"`
	CorrelationID            string          `json:"correlation_id,omitempty"`
	ExpectedStatus           string          `json:"expected_status,omitempty"`
	ExpectedLastResponseID   string          `json:"expected_last_response_id,omitempty"`
	ExpectedSocketGeneration uint64          `json:"expected_socket_generation,omitempty"`
	Attempt                  int             `json:"attempt,omitempty"`
	CreatedAt                string          `json:"created_at,omitempty"`
	Body                     json.RawMessage `json:"body,omitempty"`
}

type StartBody struct {
	InitialInput       json.RawMessage `json:"initial_input"`
	PreparedInputRef   string          `json:"prepared_input_ref,omitempty"`
	Model              string          `json:"model"`
	Instructions       string          `json:"instructions,omitempty"`
	Metadata           json.RawMessage `json:"metadata,omitempty"`
	Include            json.RawMessage `json:"include,omitempty"`
	Tools              json.RawMessage `json:"tools,omitempty"`
	ToolChoice         json.RawMessage `json:"tool_choice,omitempty"`
	Reasoning          json.RawMessage `json:"reasoning,omitempty"`
	Store              *bool           `json:"store,omitempty"`
	PreviousResponseID string          `json:"previous_response_id,omitempty"`
}

type ResumeBody struct {
	InputItems       json.RawMessage `json:"input_items"`
	PreparedInputRef string          `json:"prepared_input_ref,omitempty"`
	Reasoning        json.RawMessage `json:"reasoning,omitempty"`
}

type SubmitToolOutputBody struct {
	CallID     string          `json:"call_id,omitempty"`
	OutputItem json.RawMessage `json:"output_item"`
}

type RotateSocketBody struct {
	Reason      string `json:"reason,omitempty"`
	ScheduledAt string `json:"scheduled_at,omitempty"`
}

type CancelBody struct {
	Reason  string `json:"reason,omitempty"`
	Cascade bool   `json:"cascade,omitempty"`
}

type AdoptBody struct {
	PreviousWorkerID   string `json:"previous_worker_id,omitempty"`
	RequiredGeneration uint64 `json:"required_generation,omitempty"`
}

type ReconcileBody struct {
	PreviousWorkerID   string `json:"previous_worker_id,omitempty"`
	RequiredGeneration uint64 `json:"required_generation,omitempty"`
}

type ChildResultBody struct {
	SpawnGroupID    string `json:"spawn_group_id"`
	ChildThreadID   string `json:"child_thread_id"`
	ChildResponseID string `json:"child_response_id,omitempty"`
	Status          string `json:"status,omitempty"`
	ResultRef       string `json:"result_ref,omitempty"`
	SummaryRef      string `json:"summary_ref,omitempty"`
	ErrorRef        string `json:"error_ref,omitempty"`
}

func Decode(raw []byte) (Command, error) {
	var cmd Command
	if err := json.Unmarshal(raw, &cmd); err != nil {
		return Command{}, fmt.Errorf("decode command: %w", err)
	}

	if strings.TrimSpace(cmd.CmdID) == "" {
		return Command{}, fmt.Errorf("command missing cmd_id")
	}

	if strings.TrimSpace(cmd.ThreadID) == "" {
		return Command{}, fmt.Errorf("command missing thread_id")
	}

	if strings.TrimSpace(string(cmd.Kind)) == "" {
		return Command{}, fmt.Errorf("command missing kind")
	}

	if strings.TrimSpace(cmd.RootThreadID) == "" {
		cmd.RootThreadID = cmd.ThreadID
	}

	return cmd, nil
}

func (c Command) StartBody() (StartBody, error) {
	var body StartBody
	if err := decodeBody(c.Body, &body); err != nil {
		return StartBody{}, err
	}

	if strings.TrimSpace(body.Model) == "" {
		return StartBody{}, fmt.Errorf("thread.start missing model")
	}

	if len(body.InitialInput) == 0 && strings.TrimSpace(body.PreparedInputRef) == "" {
		return StartBody{}, fmt.Errorf("thread.start missing initial_input or prepared_input_ref")
	}

	return body, nil
}

func (c Command) ResumeBody() (ResumeBody, error) {
	var body ResumeBody
	if err := decodeBody(c.Body, &body); err != nil {
		return ResumeBody{}, err
	}

	if len(body.InputItems) == 0 && strings.TrimSpace(body.PreparedInputRef) == "" {
		return ResumeBody{}, fmt.Errorf("thread.resume missing input_items or prepared_input_ref")
	}

	return body, nil
}

func (c Command) SubmitToolOutputBody() (SubmitToolOutputBody, error) {
	var body SubmitToolOutputBody
	if err := decodeBody(c.Body, &body); err != nil {
		return SubmitToolOutputBody{}, err
	}

	if len(body.OutputItem) == 0 {
		return SubmitToolOutputBody{}, fmt.Errorf("thread.submit_tool_output missing output_item")
	}

	var item struct {
		Type   string `json:"type"`
		CallID string `json:"call_id,omitempty"`
	}
	if err := json.Unmarshal(body.OutputItem, &item); err != nil {
		return SubmitToolOutputBody{}, fmt.Errorf("decode output_item: %w", err)
	}

	if item.Type != "function_call_output" {
		return SubmitToolOutputBody{}, fmt.Errorf("output_item type must be function_call_output")
	}

	if strings.TrimSpace(body.CallID) != "" && strings.TrimSpace(item.CallID) != "" && strings.TrimSpace(body.CallID) != strings.TrimSpace(item.CallID) {
		return SubmitToolOutputBody{}, fmt.Errorf("output_item call_id does not match body call_id")
	}

	return body, nil
}

func (c Command) RotateSocketBody() (RotateSocketBody, error) {
	var body RotateSocketBody
	if err := decodeBody(c.Body, &body); err != nil {
		return RotateSocketBody{}, err
	}

	return body, nil
}

func (c Command) CancelBody() (CancelBody, error) {
	var body CancelBody
	if err := decodeBody(c.Body, &body); err != nil {
		return CancelBody{}, err
	}

	return body, nil
}

func (c Command) AdoptBody() (AdoptBody, error) {
	var body AdoptBody
	if err := decodeBody(c.Body, &body); err != nil {
		return AdoptBody{}, err
	}

	return body, nil
}

func (c Command) ReconcileBody() (ReconcileBody, error) {
	var body ReconcileBody
	if err := decodeBody(c.Body, &body); err != nil {
		return ReconcileBody{}, err
	}

	return body, nil
}

func (c Command) ChildResultBody() (ChildResultBody, error) {
	var body ChildResultBody
	if err := decodeBody(c.Body, &body); err != nil {
		return ChildResultBody{}, err
	}

	if strings.TrimSpace(body.SpawnGroupID) == "" {
		return ChildResultBody{}, fmt.Errorf("child result missing spawn_group_id")
	}

	if strings.TrimSpace(body.ChildThreadID) == "" {
		return ChildResultBody{}, fmt.Errorf("child result missing child_thread_id")
	}

	return body, nil
}

func WorkerCommandSubject(workerID string, kind Kind) string {
	suffix := strings.ReplaceAll(string(kind), ".", ".")
	return fmt.Sprintf(WorkerSubjectTemplate, workerID, suffix)
}

func DispatchSubject(kind Kind) string {
	return "agent.dispatch." + strings.ReplaceAll(string(kind), ".", ".")
}

func WorkerCommandWildcard(workerID string) string {
	return fmt.Sprintf("agent.worker.%s.cmd.>", workerID)
}

func DurableWorkerName(workerID string) string {
	return "worker-" + sanitizeToken(workerID)
}

func DurableDispatchName() string {
	return "dispatch-workers"
}

func SubjectToKind(subject string) Kind {
	switch subject {
	case DispatchStartSubject:
		return KindThreadStart
	case DispatchAdoptSubject:
		return KindThreadAdopt
	}

	parts := strings.Split(subject, ".")
	if len(parts) < 5 {
		return ""
	}

	return Kind(strings.Join(parts[4:], "."))
}

func sanitizeToken(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "unnamed"
	}

	sanitized := durableSanitizer.ReplaceAllString(trimmed, "-")
	sanitized = strings.Trim(sanitized, "-")
	if sanitized == "" {
		return "unnamed"
	}

	return sanitized
}

func decodeBody(raw json.RawMessage, dst any) error {
	if len(raw) == 0 {
		return fmt.Errorf("command body is required")
	}

	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("decode command body: %w", err)
	}

	return nil
}
