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
		ThreadID:   123,
		DocumentID: 123,
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
		t.Fatalf("DocumentID = %d, want %d", decoded.DocumentID, req.DocumentID)
	}
}

func TestPrepareInputRequestValidation(t *testing.T) {
	t.Parallel()

	tests := []PrepareInputRequest{
		{Kind: PrepareKindWarmup, DocumentID: 123},
		{RequestID: "prep_123", DocumentID: 123},
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

func TestRuntimeContextRequestRoundTrip(t *testing.T) {
	t.Parallel()

	req := RuntimeContextRequest{
		RequestID:    "docctx_123",
		ThreadID:     123,
		Instructions: "Be concise.",
		Tools:        json.RawMessage(`[{"type":"function","name":"lookup"}]`),
	}

	data, err := EncodeRuntimeContextRequest(req)
	if err != nil {
		t.Fatalf("EncodeRuntimeContextRequest() error = %v", err)
	}

	decoded, err := DecodeRuntimeContextRequest(data)
	if err != nil {
		t.Fatalf("DecodeRuntimeContextRequest() error = %v", err)
	}

	if decoded.RequestID != req.RequestID {
		t.Fatalf("RequestID = %q, want %q", decoded.RequestID, req.RequestID)
	}
	if decoded.ThreadID != req.ThreadID {
		t.Fatalf("ThreadID = %d, want %d", decoded.ThreadID, req.ThreadID)
	}
	if string(decoded.Tools) != string(req.Tools) {
		t.Fatalf("Tools = %s, want %s", string(decoded.Tools), string(req.Tools))
	}
}

func TestRuntimeContextRequestValidation(t *testing.T) {
	t.Parallel()

	tests := []RuntimeContextRequest{
		{ThreadID: 123},
		{RequestID: "docctx_123"},
	}

	for _, req := range tests {
		if _, err := EncodeRuntimeContextRequest(req); err == nil {
			t.Fatalf("expected validation error for request %#v", req)
		}
	}
}

func TestRuntimeContextResponseRoundTrip(t *testing.T) {
	t.Parallel()

	resp := RuntimeContextResponse{
		RequestID:    "docctx_123",
		Status:       PrepareStatusOK,
		Instructions: "Be concise.\n\n<available_documents>\n</available_documents>",
		Tools:        json.RawMessage(`[{"type":"function","name":"query_document"}]`),
	}

	data, err := EncodeRuntimeContextResponse(resp)
	if err != nil {
		t.Fatalf("EncodeRuntimeContextResponse() error = %v", err)
	}

	decoded, err := DecodeRuntimeContextResponse(data)
	if err != nil {
		t.Fatalf("DecodeRuntimeContextResponse() error = %v", err)
	}

	if decoded.Instructions != resp.Instructions {
		t.Fatalf("Instructions = %q, want %q", decoded.Instructions, resp.Instructions)
	}
	if string(decoded.Tools) != string(resp.Tools) {
		t.Fatalf("Tools = %s, want %s", string(decoded.Tools), string(resp.Tools))
	}
}

func TestRuntimeContextResponseValidation(t *testing.T) {
	t.Parallel()

	tests := []RuntimeContextResponse{
		{Status: PrepareStatusError, Error: "boom"},
		{RequestID: "docctx_123"},
	}

	for _, resp := range tests {
		if _, err := EncodeRuntimeContextResponse(resp); err == nil {
			t.Fatalf("expected validation error for response %#v", resp)
		}
	}
}

func TestQueryDocumentToolDefinition(t *testing.T) {
	t.Parallel()

	def := QueryDocumentToolDefinition()
	if def["name"] != ToolNameQueryDocument {
		t.Fatalf("name = %v, want %q", def["name"], ToolNameQueryDocument)
	}
	if def["strict"] != true {
		t.Fatalf("strict = %v, want true", def["strict"])
	}
	params, ok := def["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("parameters = %#v, want map[string]any", def["parameters"])
	}
	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties = %#v, want map[string]any", params["properties"])
	}
	docIDProp, ok := props["document_id"].(map[string]any)
	if !ok {
		t.Fatalf("document_id property missing or wrong shape: %#v", props["document_id"])
	}
	if docIDProp["type"] != "integer" {
		t.Fatalf("document_id type = %v, want integer", docIDProp["type"])
	}
	if _, ok := props["task"]; !ok {
		t.Fatal("task property missing")
	}
	if _, ok := props["document_ids"]; ok {
		t.Fatal("legacy document_ids property should be gone after refactor")
	}
	required, ok := params["required"].([]string)
	if !ok {
		t.Fatalf("required = %#v, want []string", params["required"])
	}
	wantRequired := map[string]bool{"document_id": true, "task": true}
	for _, r := range required {
		delete(wantRequired, r)
	}
	if len(wantRequired) != 0 {
		t.Fatalf("required missing fields: %v (got %v)", wantRequired, required)
	}
}
