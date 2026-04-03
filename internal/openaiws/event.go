package openaiws

import (
	"encoding/json"
	"fmt"
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

type ClientEvent struct {
	Type    EventType      `json:"type"`
	EventID string         `json:"-"`
	Payload map[string]any `json:"-"`
}

func NewResponseCreateEvent(eventID string, response any) (ClientEvent, error) {
	payload, err := json.Marshal(response)
	if err != nil {
		return ClientEvent{}, fmt.Errorf("marshal response payload: %w", err)
	}

	fields := map[string]any{}
	if err := json.Unmarshal(payload, &fields); err != nil {
		return ClientEvent{}, fmt.Errorf("decode response payload object: %w", err)
	}

	return ClientEvent{
		Type:    EventTypeResponseCreate,
		EventID: eventID,
		Payload: fields,
	}, nil
}

func (e ClientEvent) Bytes() ([]byte, error) {
	payload := make(map[string]any, len(e.Payload)+2)
	payload["type"] = e.Type
	for key, value := range e.Payload {
		payload[key] = value
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal client event: %w", err)
	}

	return encoded, nil
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
