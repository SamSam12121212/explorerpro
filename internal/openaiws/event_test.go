package openaiws

import "testing"

func TestEventTypeIsDelta(t *testing.T) {
	t.Parallel()

	if !EventTypeResponseOutputTextDelta.IsDelta() {
		t.Fatal("output text delta should report IsDelta")
	}
	if EventTypeResponseCompleted.IsDelta() {
		t.Fatal("completed event should not report IsDelta")
	}
}

func TestEventTypeIsToolCallDelta(t *testing.T) {
	t.Parallel()

	if !EventTypeResponseFunctionCallArgumentsDelta.IsToolCallDelta() {
		t.Fatal("function_call_arguments.delta should report IsToolCallDelta")
	}
	if EventTypeResponseFunctionCallArgumentsDone.IsToolCallDelta() {
		t.Fatal("function_call_arguments.done is not a delta")
	}
	if EventTypeResponseReasoningTextDelta.IsToolCallDelta() {
		t.Fatal("reasoning delta is not a tool-call delta")
	}
	if EventTypeResponseOutputTextDelta.IsToolCallDelta() {
		t.Fatal("output text delta is not a tool-call delta")
	}
}

func TestDecodeServerEventReasoningSummaryDelta(t *testing.T) {
	t.Parallel()

	event, err := DecodeServerEvent([]byte(`{
		"type":"response.reasoning_summary_text.delta",
		"response_id":"resp_123",
		"item_id":"rs_123",
		"output_index":0,
		"summary_index":0,
		"delta":"thinking..."
	}`))
	if err != nil {
		t.Fatalf("DecodeServerEvent() error = %v", err)
	}

	if event.Type != EventTypeResponseReasoningSummaryTextDelta {
		t.Fatalf("Type = %q, want %q", event.Type, EventTypeResponseReasoningSummaryTextDelta)
	}
	if event.ResponseID != "resp_123" {
		t.Fatalf("ResponseID = %q, want resp_123", event.ResponseID)
	}
	if string(event.Field("delta")) != `"thinking..."` {
		t.Fatalf("delta = %s, want %q", event.Field("delta"), `"thinking..."`)
	}
}
