package doccmd

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	StreamName                     = "DOC_CMD"
	SplitSubject                   = "doc.split"
	SplitQueue                     = "doc-workers"
	PrepareInputSubject            = "doc.prepare_input"
	PrepareInputQueue              = "documenthandlers"
	RuntimeContextSubject          = "doc.runtime_context"
	RuntimeContextQueue            = "documenthandlers"
	DefaultDPI                     = 150
	PrepareKindWarmup              = "document_warmup"
	PrepareKindDocumentQuery       = "document_query"
	PrepareKindPageRead            = "document_page_read"
	PrepareStatusOK                = "ok"
	PrepareStatusError             = "error"
	PrepareStatusNoop              = "noop"
	PrepareStatusPending           = "pending"
	ToolNameQueryDocument    = "query_document"
	ToolNameReadDocumentPage = "read_document_page"
)

type SplitCommand struct {
	CmdID      int64  `json:"cmd_id"`
	DocumentID int64  `json:"document_id"`
	SourceRef  string `json:"source_ref"`
	DPI        int    `json:"dpi,omitempty"`
}

type PrepareInputRequest struct {
	RequestID  string          `json:"request_id"`
	Kind       string          `json:"kind"`
	ThreadID   int64           `json:"thread_id,omitempty"`
	DocumentID int64           `json:"document_id"`
	Task       string          `json:"task,omitempty"`
	InputItems json.RawMessage `json:"input_items,omitempty"`
	// PageNumber is 1-indexed, required for PrepareKindPageRead.
	PageNumber int `json:"page_number,omitempty"`
	// CallID is the tool call id echoed back on the function_call_output item,
	// required for PrepareKindPageRead.
	CallID string `json:"call_id,omitempty"`
}

type PrepareInputResponse struct {
	RequestID        string `json:"request_id"`
	Status           string `json:"status"`
	PreparedInputRef string `json:"prepared_input_ref,omitempty"`
	Error            string `json:"error,omitempty"`
}

type RuntimeContextRequest struct {
	RequestID string `json:"request_id"`
	ThreadID  int64  `json:"thread_id"`
	// ParentThreadID is 0 for root threads. Populated so the service can gate
	// root-only tools (e.g. read_document_page) off of child threads.
	ParentThreadID int64           `json:"parent_thread_id,omitempty"`
	Instructions   string          `json:"instructions,omitempty"`
	Tools          json.RawMessage `json:"tools,omitempty"`
}

type RuntimeContextResponse struct {
	RequestID    string          `json:"request_id"`
	Status       string          `json:"status"`
	Instructions string          `json:"instructions,omitempty"`
	Tools        json.RawMessage `json:"tools,omitempty"`
	Error        string          `json:"error,omitempty"`
}

func DecodeSplit(data []byte) (SplitCommand, error) {
	var cmd SplitCommand
	if err := json.Unmarshal(data, &cmd); err != nil {
		return SplitCommand{}, fmt.Errorf("decode split command: %w", err)
	}
	if cmd.CmdID <= 0 {
		return SplitCommand{}, fmt.Errorf("split command missing cmd_id")
	}
	if cmd.DocumentID <= 0 {
		return SplitCommand{}, fmt.Errorf("split command missing document_id")
	}
	if strings.TrimSpace(cmd.SourceRef) == "" {
		return SplitCommand{}, fmt.Errorf("split command missing source_ref")
	}
	if cmd.DPI <= 0 {
		cmd.DPI = DefaultDPI
	}
	return cmd, nil
}

func EncodePrepareInputRequest(req PrepareInputRequest) ([]byte, error) {
	if err := validatePrepareInputRequest(req); err != nil {
		return nil, err
	}

	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal prepare input request: %w", err)
	}

	return data, nil
}

func DecodePrepareInputRequest(data []byte) (PrepareInputRequest, error) {
	var req PrepareInputRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return PrepareInputRequest{}, fmt.Errorf("decode prepare input request: %w", err)
	}

	if err := validatePrepareInputRequest(req); err != nil {
		return PrepareInputRequest{}, err
	}

	return req, nil
}

func EncodePrepareInputResponse(resp PrepareInputResponse) ([]byte, error) {
	if err := validatePrepareInputResponse(resp); err != nil {
		return nil, err
	}

	data, err := json.Marshal(resp)
	if err != nil {
		return nil, fmt.Errorf("marshal prepare input response: %w", err)
	}

	return data, nil
}

func DecodePrepareInputResponse(data []byte) (PrepareInputResponse, error) {
	var resp PrepareInputResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return PrepareInputResponse{}, fmt.Errorf("decode prepare input response: %w", err)
	}

	if err := validatePrepareInputResponse(resp); err != nil {
		return PrepareInputResponse{}, err
	}

	return resp, nil
}

func EncodeRuntimeContextRequest(req RuntimeContextRequest) ([]byte, error) {
	if err := validateRuntimeContextRequest(req); err != nil {
		return nil, err
	}

	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal runtime context request: %w", err)
	}

	return data, nil
}

func DecodeRuntimeContextRequest(data []byte) (RuntimeContextRequest, error) {
	var req RuntimeContextRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return RuntimeContextRequest{}, fmt.Errorf("decode runtime context request: %w", err)
	}

	if err := validateRuntimeContextRequest(req); err != nil {
		return RuntimeContextRequest{}, err
	}

	return req, nil
}

func EncodeRuntimeContextResponse(resp RuntimeContextResponse) ([]byte, error) {
	if err := validateRuntimeContextResponse(resp); err != nil {
		return nil, err
	}

	data, err := json.Marshal(resp)
	if err != nil {
		return nil, fmt.Errorf("marshal runtime context response: %w", err)
	}

	return data, nil
}

func DecodeRuntimeContextResponse(data []byte) (RuntimeContextResponse, error) {
	var resp RuntimeContextResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return RuntimeContextResponse{}, fmt.Errorf("decode runtime context response: %w", err)
	}

	if err := validateRuntimeContextResponse(resp); err != nil {
		return RuntimeContextResponse{}, err
	}

	return resp, nil
}

func validatePrepareInputRequest(req PrepareInputRequest) error {
	if strings.TrimSpace(req.RequestID) == "" {
		return fmt.Errorf("prepare input request missing request_id")
	}
	if strings.TrimSpace(req.Kind) == "" {
		return fmt.Errorf("prepare input request missing kind")
	}
	if req.DocumentID <= 0 {
		return fmt.Errorf("prepare input request missing document_id")
	}
	if req.Kind == PrepareKindPageRead {
		if req.PageNumber <= 0 {
			return fmt.Errorf("prepare input request missing page_number for kind %q", req.Kind)
		}
		if strings.TrimSpace(req.CallID) == "" {
			return fmt.Errorf("prepare input request missing call_id for kind %q", req.Kind)
		}
	}
	return nil
}

func validatePrepareInputResponse(resp PrepareInputResponse) error {
	if strings.TrimSpace(resp.RequestID) == "" {
		return fmt.Errorf("prepare input response missing request_id")
	}
	if strings.TrimSpace(resp.Status) == "" {
		return fmt.Errorf("prepare input response missing status")
	}
	return nil
}

func validateRuntimeContextRequest(req RuntimeContextRequest) error {
	if strings.TrimSpace(req.RequestID) == "" {
		return fmt.Errorf("runtime context request missing request_id")
	}
	if req.ThreadID <= 0 {
		return fmt.Errorf("runtime context request missing thread_id")
	}
	return nil
}

func validateRuntimeContextResponse(resp RuntimeContextResponse) error {
	if strings.TrimSpace(resp.RequestID) == "" {
		return fmt.Errorf("runtime context response missing request_id")
	}
	if strings.TrimSpace(resp.Status) == "" {
		return fmt.Errorf("runtime context response missing status")
	}
	return nil
}

func ReadDocumentPageToolDefinition() map[string]any {
	return map[string]any{
		"type":        "function",
		"name":        ToolNameReadDocumentPage,
		"description": "Load a single page of an attached document into your context as an image. Use this when you already know the specific page you need to inspect directly. The page is returned wrapped in <pdf>/<pdf_page> tags carrying its width and height in manifest pixels (top-left origin) so you can reason about coordinates. Call in parallel when you need multiple pages in one turn.",
		"strict":      true,
		"parameters": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"document_id": map[string]any{
					"type":        "integer",
					"description": "ID of the attached document to load a page from.",
				},
				"page_number": map[string]any{
					"type":        "integer",
					"description": "1-indexed page number to load.",
				},
			},
			"required": []string{"document_id", "page_number"},
		},
	}
}

func QueryDocumentToolDefinition() map[string]any {
	return map[string]any{
		"type":        "function",
		"name":        ToolNameQueryDocument,
		"description": "Query a single attached document. Each document has all of its pages already loaded into a separate analysis session. Call this tool in parallel — once per document — when you need to ask multiple documents different questions in the same turn. The available document IDs and filenames are listed in the <available_documents> block. Describe what you need in the task field; mention specific page numbers there if needed. Up to 50 query_document calls are accepted per turn; calls beyond the cap are rejected, so split into follow-up turns if you need more parallelism.",
		"strict":      true,
		"parameters": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"document_id": map[string]any{
					"type":        "integer",
					"description": "ID of the attached document to query.",
				},
				"task": map[string]any{
					"type":        "string",
					"description": "What to look for or ask about in this document.",
				},
			},
			"required": []string{"document_id", "task"},
		},
	}
}
