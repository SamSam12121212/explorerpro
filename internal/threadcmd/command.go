package threadcmd

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const (
	StreamName            = "THREAD_CMD"
	DispatchQueue         = "thread-dispatchers"
	DispatchStartSubject  = "thread.dispatch.start"
	DispatchAdoptSubject  = "thread.dispatch.adopt"
	WorkerSubjectTemplate = "thread.worker.%s.cmd.%s"
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
	CmdID                    int64           `json:"cmd_id"`
	Kind                     Kind            `json:"kind"`
	ThreadID                 int64           `json:"thread_id"`
	RootThreadID             int64           `json:"root_thread_id"`
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
	PreviousWorkerID   int64  `json:"previous_worker_id,omitempty"`
	RequiredGeneration uint64 `json:"required_generation,omitempty"`
}

type ReconcileBody struct {
	PreviousWorkerID   int64  `json:"previous_worker_id,omitempty"`
	RequiredGeneration uint64 `json:"required_generation,omitempty"`
}

type ChildResultBody struct {
	SpawnGroupID    int64  `json:"spawn_group_id"`
	ChildThreadID   int64  `json:"child_thread_id"`
	ChildResponseID string `json:"child_response_id,omitempty"`
	Status          string `json:"status,omitempty"`
	AssistantText   string `json:"assistant_text,omitempty"`
	ResultRef       string `json:"result_ref,omitempty"`
	SummaryRef      string `json:"summary_ref,omitempty"`
	ErrorRef        string `json:"error_ref,omitempty"`
}

func InputKind(inputItems json.RawMessage, preparedInputRef string) string {
	if strings.TrimSpace(preparedInputRef) != "" {
		return "prepared_input"
	}

	items := decodeInputItems(inputItems)
	if len(items) == 0 {
		return "none"
	}

	itemType := strings.TrimSpace(stringValue(items[0]["type"]))
	switch itemType {
	case "message":
		if strings.TrimSpace(stringValue(items[0]["role"])) == "user" {
			return "user_message"
		}
		return "message"
	case "":
		return "input_items"
	default:
		return itemType
	}
}

func LogAttrs(cmd Command) []any {
	attrs := []any{
		"cmd_id", cmd.CmdID,
		"kind", cmd.Kind,
		"thread_id", cmd.ThreadID,
	}
	if rootThreadID := cmd.RootThreadID; rootThreadID > 0 {
		attrs = append(attrs, "root_thread_id", rootThreadID)
	}
	if causationID := strings.TrimSpace(cmd.CausationID); causationID != "" {
		attrs = append(attrs, "causation_id", causationID)
	}
	if correlationID := strings.TrimSpace(cmd.CorrelationID); correlationID != "" {
		attrs = append(attrs, "correlation_id", correlationID)
	}

	switch cmd.Kind {
	case KindThreadStart:
		if body, err := cmd.StartBody(); err == nil {
			attrs = append(attrs,
				"model", body.Model,
				"input_kind", InputKind(body.InitialInput, body.PreparedInputRef),
				"has_previous_response_id", strings.TrimSpace(body.PreviousResponseID) != "",
			)
		}
	case KindThreadResume:
		if body, err := cmd.ResumeBody(); err == nil {
			attrs = append(attrs, "input_kind", InputKind(body.InputItems, body.PreparedInputRef))
		}
	case KindThreadSubmitToolOutput:
		if body, err := cmd.SubmitToolOutputBody(); err == nil {
			callID := strings.TrimSpace(body.CallID)
			if callID == "" {
				callID = functionCallOutputCallID(body.OutputItem)
			}
			attrs = append(attrs, "input_kind", "function_call_output")
			if callID != "" {
				attrs = append(attrs, "call_id", callID)
			}
		}
	case KindThreadChildCompleted, KindThreadChildFailed:
		if body, err := cmd.ChildResultBody(); err == nil {
			if spawnGroupID := body.SpawnGroupID; spawnGroupID > 0 {
				attrs = append(attrs, "spawn_group_id", spawnGroupID)
			}
			if childThreadID := body.ChildThreadID; childThreadID > 0 {
				attrs = append(attrs, "child_thread_id", childThreadID)
			}
			childStatus := strings.TrimSpace(body.Status)
			if childStatus == "" {
				childStatus = childStatusForKind(cmd.Kind)
			}
			if childStatus != "" {
				attrs = append(attrs, "child_status", childStatus)
			}
			if childResponseID := strings.TrimSpace(body.ChildResponseID); childResponseID != "" {
				attrs = append(attrs, "child_response_id", childResponseID)
			}
		}
	}

	return attrs
}

func Decode(raw []byte) (Command, error) {
	var cmd Command
	if err := json.Unmarshal(raw, &cmd); err != nil {
		return Command{}, fmt.Errorf("decode command: %w", err)
	}

	if cmd.CmdID <= 0 {
		return Command{}, fmt.Errorf("command missing cmd_id")
	}

	if cmd.ThreadID <= 0 {
		return Command{}, fmt.Errorf("command missing thread_id")
	}

	if strings.TrimSpace(string(cmd.Kind)) == "" {
		return Command{}, fmt.Errorf("command missing kind")
	}

	if cmd.RootThreadID <= 0 {
		cmd.RootThreadID = cmd.ThreadID
	}

	return cmd, nil
}

func decodeInputItems(raw json.RawMessage) []map[string]any {
	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil
	}
	return items
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func childStatusForKind(kind Kind) string {
	switch kind {
	case KindThreadChildCompleted:
		return "completed"
	case KindThreadChildFailed:
		return "failed"
	default:
		return ""
	}
}

func functionCallOutputCallID(raw json.RawMessage) string {
	var payload struct {
		CallID string `json:"call_id"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ""
	}
	return strings.TrimSpace(payload.CallID)
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

	if body.SpawnGroupID <= 0 {
		return ChildResultBody{}, fmt.Errorf("child result missing spawn_group_id")
	}

	if body.ChildThreadID <= 0 {
		return ChildResultBody{}, fmt.Errorf("child result missing child_thread_id")
	}

	return body, nil
}

func WorkerCommandSubject(workerID int64, kind Kind) string {
	return fmt.Sprintf(WorkerSubjectTemplate, strconv.FormatInt(workerID, 10), kindSubjectSuffix(kind))
}

func DispatchSubject(kind Kind) string {
	return "thread.dispatch." + kindSubjectSuffix(kind)
}

func WorkerCommandWildcard(workerID int64) string {
	return fmt.Sprintf("thread.worker.%s.cmd.>", strconv.FormatInt(workerID, 10))
}

func DurableWorkerName(workerID int64) string {
	return "worker-" + sanitizeToken(strconv.FormatInt(workerID, 10))
}

func DurableDispatchName() string {
	return "dispatch-workers"
}

func SubjectToKind(subject string) Kind {
	parts := strings.Split(subject, ".")
	if len(parts) < 3 {
		return ""
	}

	var suffix string
	switch {
	case len(parts) >= 3 && parts[0] == "thread" && parts[1] == "dispatch":
		suffix = strings.Join(parts[2:], ".")
	case len(parts) >= 5 && parts[0] == "thread" && parts[1] == "worker" && parts[3] == "cmd":
		suffix = strings.Join(parts[4:], ".")
	default:
		return ""
	}

	if suffix == "" {
		return ""
	}
	if !strings.HasPrefix(suffix, "thread.") {
		suffix = "thread." + suffix
	}

	return Kind(suffix)
}

func kindSubjectSuffix(kind Kind) string {
	return strings.TrimPrefix(string(kind), "thread.")
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
