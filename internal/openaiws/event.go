package openaiws

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

type EventType string

const (
	EventTypeError                             EventType = "error"
	EventTypeResponseCreate                    EventType = "response.create"
	EventTypeResponseCreated                   EventType = "response.created"
	EventTypeResponseInProgress                EventType = "response.in_progress"
	EventTypeResponseOutputItemAdded           EventType = "response.output_item.added"
	EventTypeResponseOutputItemDone            EventType = "response.output_item.done"
	EventTypeResponseReasoningSummaryPartAdded EventType = "response.reasoning_summary_part.added"
	EventTypeResponseReasoningSummaryPartDone  EventType = "response.reasoning_summary_part.done"
	EventTypeResponseReasoningSummaryTextDelta EventType = "response.reasoning_summary_text.delta"
	EventTypeResponseReasoningSummaryTextDone  EventType = "response.reasoning_summary_text.done"
	EventTypeResponseReasoningTextDelta        EventType = "response.reasoning_text.delta"
	EventTypeResponseReasoningTextDone         EventType = "response.reasoning_text.done"
	EventTypeResponseOutputTextDelta           EventType = "response.output_text.delta"
	EventTypeResponseOutputTextDone            EventType = "response.output_text.done"
	EventTypeResponseCompleted                 EventType = "response.completed"
	EventTypeResponseFailed                    EventType = "response.failed"
	EventTypeResponseIncomplete                EventType = "response.incomplete"
	EventTypeResponseRefusalDone               EventType = "response.refusal.done"
	EventTypeResponseFunctionArgsDone          EventType = "response.function_call_arguments.done"
)

func (t EventType) IsTerminal() bool {
	switch t {
	case EventTypeResponseCompleted, EventTypeResponseFailed, EventTypeResponseIncomplete:
		return true
	default:
		return false
	}
}

func (t EventType) IsDelta() bool {
	return strings.HasSuffix(string(t), ".delta")
}

type ClientEvent struct {
	Type    EventType       `json:"type"`
	EventID string          `json:"-"`
	Payload json.RawMessage `json:"-"`
}

func NewResponseCreateEvent(eventID string, payload json.RawMessage) (ClientEvent, error) {
	trimmed := bytes.TrimSpace(payload)
	if err := validateJSONObjectPayload(trimmed); err != nil {
		return ClientEvent{}, fmt.Errorf("validate response payload: %w", err)
	}

	return ClientEvent{
		Type:    EventTypeResponseCreate,
		EventID: eventID,
		Payload: append(json.RawMessage(nil), trimmed...),
	}, nil
}

func (e ClientEvent) Bytes() ([]byte, error) {
	payload := bytes.TrimSpace(e.Payload)
	if err := validateJSONObjectPayload(payload); err != nil {
		return nil, fmt.Errorf("validate client event payload: %w", err)
	}

	encodedType, err := json.Marshal(e.Type)
	if err != nil {
		return nil, fmt.Errorf("marshal client event type: %w", err)
	}

	inner := bytes.TrimSpace(payload[1 : len(payload)-1])
	encoded := make([]byte, 0, len(payload)+len(encodedType)+16)
	encoded = append(encoded, '{')
	encoded = append(encoded, []byte(`"type":`)...)
	encoded = append(encoded, encodedType...)
	if len(inner) > 0 {
		encoded = append(encoded, ',')
		encoded = append(encoded, inner...)
	}
	encoded = append(encoded, '}')

	return encoded, nil
}

func validateJSONObjectPayload(payload json.RawMessage) error {
	if len(payload) == 0 {
		return fmt.Errorf("payload is required")
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return fmt.Errorf("payload must be a JSON object: %w", err)
	}

	return nil
}

type EventError struct {
	Type    string `json:"type,omitempty"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	Param   string `json:"param,omitempty"`
}

type ServerEvent struct {
	Type       EventType
	EventID    string
	ResponseID string
	Error      *EventError
	Fields     map[string]json.RawMessage
	Raw        json.RawMessage
}

func DecodeServerEvent(payload []byte) (ServerEvent, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return ServerEvent{}, fmt.Errorf("decode raw event map: %w", err)
	}

	var header struct {
		Type       EventType   `json:"type"`
		EventID    string      `json:"event_id,omitempty"`
		ResponseID string      `json:"response_id,omitempty"`
		Error      *EventError `json:"error,omitempty"`
	}
	if err := json.Unmarshal(payload, &header); err != nil {
		return ServerEvent{}, fmt.Errorf("decode event header: %w", err)
	}

	if header.Type == "" {
		return ServerEvent{}, fmt.Errorf("event missing type")
	}

	return ServerEvent{
		Type:       header.Type,
		EventID:    header.EventID,
		ResponseID: header.ResponseID,
		Error:      header.Error,
		Fields:     fields,
		Raw:        append(json.RawMessage(nil), payload...),
	}, nil
}

func (e ServerEvent) Field(name string) json.RawMessage {
	return e.Fields[name]
}

func (e ServerEvent) ResponsePayload() json.RawMessage {
	return e.Field("response")
}

func (e ServerEvent) ResolvedResponseID() string {
	if e.ResponseID != "" {
		return e.ResponseID
	}

	response := e.ResponsePayload()
	if len(response) == 0 {
		return ""
	}

	var payload struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(response, &payload); err != nil {
		return ""
	}

	return payload.ID
}
