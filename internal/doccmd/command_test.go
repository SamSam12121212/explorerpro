package doccmd

import (
	"encoding/json"
	"testing"
)

func TestPrepareInputRequestRoundTrip(t *testing.T) {
	t.Parallel()

	req := PrepareInputRequest{
		RequestID:  "prep_123",
		Kind:       PrepareKindWarmup,
		ThreadID:   "thread_123",
		DocumentID: "doc_123",
		Task:       "warm document pages",
		InputItems: json.RawMessage(`[{"type":"message","role":"user"}]`),
	}

	data, err := EncodePrepareInputRequest(req)
	if err != nil {
		t.Fatalf("EncodePrepareInputRequest() error = %v", err)
	}

	decoded, err := DecodePrepareInputRequest(data)
	if err != nil {
		t.Fatalf("DecodePrepareInputRequest() error = %v", err)
	}

	if decoded.RequestID != req.RequestID {
		t.Fatalf("RequestID = %q, want %q", decoded.RequestID, req.RequestID)
	}
	if decoded.Kind != req.Kind {
		t.Fatalf("Kind = %q, want %q", decoded.Kind, req.Kind)
	}
	if decoded.DocumentID != req.DocumentID {
		t.Fatalf("DocumentID = %q, want %q", decoded.DocumentID, req.DocumentID)
	}
}

func TestPrepareInputRequestValidation(t *testing.T) {
	t.Parallel()

	tests := []PrepareInputRequest{
		{Kind: PrepareKindWarmup, DocumentID: "doc_123"},
		{RequestID: "prep_123", DocumentID: "doc_123"},
		{RequestID: "prep_123", Kind: PrepareKindWarmup},
	}

	for _, req := range tests {
		if _, err := EncodePrepareInputRequest(req); err == nil {
			t.Fatalf("expected validation error for request %#v", req)
		}
	}
}

func TestPrepareInputResponseRoundTrip(t *testing.T) {
	t.Parallel()

	resp := PrepareInputResponse{
		RequestID:        "prep_123",
		Status:           PrepareStatusOK,
		PreparedInputRef: "blob://prepared-inputs/pi_123.json",
	}

	data, err := EncodePrepareInputResponse(resp)
	if err != nil {
		t.Fatalf("EncodePrepareInputResponse() error = %v", err)
	}

	decoded, err := DecodePrepareInputResponse(data)
	if err != nil {
		t.Fatalf("DecodePrepareInputResponse() error = %v", err)
	}

	if decoded.PreparedInputRef != resp.PreparedInputRef {
		t.Fatalf("PreparedInputRef = %q, want %q", decoded.PreparedInputRef, resp.PreparedInputRef)
	}
}

func TestPrepareInputResponseValidation(t *testing.T) {
	t.Parallel()

	tests := []PrepareInputResponse{
		{Status: PrepareStatusError, Error: "boom"},
		{RequestID: "prep_123"},
	}

	for _, resp := range tests {
		if _, err := EncodePrepareInputResponse(resp); err == nil {
			t.Fatalf("expected validation error for response %#v", resp)
		}
	}
}
