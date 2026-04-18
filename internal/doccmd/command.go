package doccmd

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	StreamName                      = "DOC_CMD"
	SplitSubject                    = "doc.split"
	SplitQueue                      = "doc-workers"
	PrepareInputSubject             = "doc.prepare_input"
	PrepareInputQueue               = "documenthandlers"
	RuntimeContextSubject           = "doc.runtime_context"
	RuntimeContextQueue             = "documenthandlers"
	DefaultDPI                      = 150
	PrepareKindWarmup               = "document_warmup"
	PrepareKindDocumentQuery        = "document_query"
	PrepareKindPageRead             = "document_page_read"
	PrepareKindStoreCitationSpawn   = "store_citation_spawn"
	PrepareKindStoreCitationFinalize = "store_citation_finalize"
	PrepareStatusOK                 = "ok"
	PrepareStatusError              = "error"
	PrepareStatusNoop               = "noop"
	PrepareStatusPending            = "pending"
	ToolNameQueryDocument           = "query_document"
	ToolNameReadDocumentPage        = "read_document_page"
	ToolNameStoreCitation           = "store_citation"
	// ToolNameEmitBboxes is the forced function the evidence-locator child
	// thread must call. The main thread never sees this tool — it is only
	// injected into the locator's runtime context.
	ToolNameEmitBboxes = "emit_bboxes"
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
	// required for PrepareKindPageRead, PrepareKindStoreCitationSpawn, and
	// PrepareKindStoreCitationFinalize.
	CallID string `json:"call_id,omitempty"`
	// Pages is the 1-indexed set of pages the citation covers. Exactly 1 or 2
	// entries; when 2, they must be consecutive (N, N+1). Required for
	// PrepareKindStoreCitationSpawn and PrepareKindStoreCitationFinalize.
	Pages []int `json:"pages,omitempty"`
	// Instruction is the caller's natural-language description of what the
	// evidence locator should highlight (may include quoted fragments as
	// anchors). Required for PrepareKindStoreCitationSpawn.
	Instruction string `json:"instruction,omitempty"`
	// IncludeImages asks the locator to look at page images in addition to
	// OCR data — useful when the document is a noisy scan. Optional; defaults
	// to false.
	IncludeImages bool `json:"include_images,omitempty"`
	// ToolCallArgsJSON is the forced-tool-call arguments emitted by the
	// evidence locator child thread. Required for PrepareKindStoreCitationFinalize.
	ToolCallArgsJSON string `json:"tool_call_args_json,omitempty"`
	// ChildThreadID is the spawned locator thread's id, carried through for
	// audit on PrepareKindStoreCitationFinalize.
	ChildThreadID int64 `json:"child_thread_id,omitempty"`
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
	switch req.Kind {
	case PrepareKindPageRead:
		if req.PageNumber <= 0 {
			return fmt.Errorf("prepare input request missing page_number for kind %q", req.Kind)
		}
		if strings.TrimSpace(req.CallID) == "" {
			return fmt.Errorf("prepare input request missing call_id for kind %q", req.Kind)
		}
	case PrepareKindStoreCitationSpawn:
		if err := validateCitationPages(req.Pages); err != nil {
			return fmt.Errorf("%s: %w", req.Kind, err)
		}
		if strings.TrimSpace(req.Instruction) == "" {
			return fmt.Errorf("prepare input request missing instruction for kind %q", req.Kind)
		}
		if strings.TrimSpace(req.CallID) == "" {
			return fmt.Errorf("prepare input request missing call_id for kind %q", req.Kind)
		}
		if req.ThreadID <= 0 {
			return fmt.Errorf("prepare input request missing thread_id for kind %q", req.Kind)
		}
	case PrepareKindStoreCitationFinalize:
		if err := validateCitationPages(req.Pages); err != nil {
			return fmt.Errorf("%s: %w", req.Kind, err)
		}
		if strings.TrimSpace(req.CallID) == "" {
			return fmt.Errorf("prepare input request missing call_id for kind %q", req.Kind)
		}
		if req.ThreadID <= 0 {
			return fmt.Errorf("prepare input request missing thread_id for kind %q", req.Kind)
		}
		if strings.TrimSpace(req.ToolCallArgsJSON) == "" {
			return fmt.Errorf("prepare input request missing tool_call_args_json for kind %q", req.Kind)
		}
	}
	return nil
}

// validateCitationPages enforces the structural invariant: 1 or 2 pages,
// positive, and when 2 they must be consecutive (N, N+1). The 2-page rule
// captures paragraphs and values that flow across a page break without
// allowing the model to stitch together unrelated evidence from
// arbitrary pages under one citation.
func validateCitationPages(pages []int) error {
	switch len(pages) {
	case 1:
		if pages[0] <= 0 {
			return fmt.Errorf("pages[0] must be positive, got %d", pages[0])
		}
	case 2:
		if pages[0] <= 0 || pages[1] <= 0 {
			return fmt.Errorf("pages must be positive, got %v", pages)
		}
		if pages[1] != pages[0]+1 {
			return fmt.Errorf("pages must be consecutive (N, N+1), got %v", pages)
		}
	default:
		return fmt.Errorf("pages must have 1 or 2 entries, got %d", len(pages))
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

func StoreCitationToolDefinition() map[string]any {
	return map[string]any{
		"type":        "function",
		"name":        ToolNameStoreCitation,
		"description": "Record a citation pointing at specific evidence on one or two consecutive pages of an attached document. Use this after reading the page(s) to verify what you want to cite. An evidence-locator agent you do not see will find the exact bboxes based on your instruction and the OCR data for the page(s); it returns a citation_id that you embed in your reply as [display text][citation_id] — the frontend renders that as a clickable chip that jumps to the document and highlights the evidence. One citation per contiguous region; call again for a separate region, even in the same document. Call in parallel when you need multiple citations in one turn.",
		"strict":      true,
		"parameters": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"document_id": map[string]any{
					"type":        "integer",
					"description": "ID of the attached document the citation points at.",
				},
				"pages": map[string]any{
					"type":        "array",
					"description": "1-indexed page numbers. Exactly 1 entry for a single-page citation, or 2 consecutive entries (e.g. [17, 18]) when the cited region flows across a page break.",
					"items":       map[string]any{"type": "integer"},
					"minItems":    1,
					"maxItems":    2,
				},
				"instruction": map[string]any{
					"type":        "string",
					"description": "Natural-language description of what to highlight, including the authoritative text fragments you want anchored. Examples: 'The whole 4.7.1.10 paragraph including the section header — Serotonin syndrome (SS) can be caused by... reported for all antipsychotics', or 'The Trauma row in the mortality table: rank 1, 405 (12.2), median 3.0, IQR 1.1-9.0'. OCR text may have errors — your description is authoritative.",
				},
				"include_images": map[string]any{
					"type":        "boolean",
					"description": "Whether the locator should look at page images in addition to OCR. Default usage is false. Set true only when the document is a noisy or low-quality scan and OCR reliability is in doubt; otherwise the extra image tokens are wasted.",
				},
			},
			"required": []string{"document_id", "pages", "instruction", "include_images"},
		},
	}
}

// CitationLocatorInstructions is the system prompt for the evidence-locator
// child thread. Lives here because both documenthandler (which builds the
// locator's initial input) and worker (which sets it on the child start
// body) reference it.
const CitationLocatorInstructions = `You are an evidence locator for a document workspace.

You receive:
- Per-page OCR data (line text + bbox + poly) for one or two consecutive pages of a document.
- A natural-language instruction from the caller describing what evidence on the page(s) to highlight, often including authoritative text fragments (treat those fragments as ground truth — the OCR text may have recognition errors).
- Optionally, the page images (for scans where OCR reliability is in doubt).

You must call the emit_bboxes function with the OCR line indices whose unioned bounding boxes visually mark the evidence on the page(s).

Guidance:
- Indices are zero-based into the OCR lines array you were given for each page.
- When the caller cites a value, include adjacent label lines too (e.g. "Patient weight:" next to "75 kg", a column header next to its row value).
- When the caller cites a paragraph and mentions a section header, include the header line.
- When the caller passes two pages, only emit a page entry for pages that actually contain matching evidence. It is fine to return one page if the evidence ends before the second.
- Be precise: extra lines dilute the highlight; missing lines break the citation. Err on the side of matching what the caller asked for.

You MUST emit your answer via the emit_bboxes function only. Do not write any user-visible text.`

// EmitBboxesToolDefinition is the forced tool the evidence-locator child
// thread must call. The locator's response is structured as this tool's
// arguments; the main thread never sees this tool.
func EmitBboxesToolDefinition() map[string]any {
	return map[string]any{
		"type":        "function",
		"name":        ToolNameEmitBboxes,
		"description": "Return the OCR line indices whose unioned bboxes visually highlight the caller's described evidence. Use 1 entry per page in `pages`; include 2 entries only when evidence genuinely spans a page break.",
		"strict":      true,
		"parameters": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"pages": map[string]any{
					"type":        "array",
					"description": "Per-page OCR line indices. Order by page number.",
					"minItems":    1,
					"maxItems":    2,
					"items": map[string]any{
						"type":                 "object",
						"additionalProperties": false,
						"properties": map[string]any{
							"page": map[string]any{
								"type":        "integer",
								"description": "1-indexed page number.",
							},
							"line_indices": map[string]any{
								"type":        "array",
								"description": "OCR `lines[]` array indices for the lines whose bboxes should be unioned to highlight the evidence on this page. Include adjacent label lines when citing form values (e.g. the 'Patient weight:' label alongside the value).",
								"items":       map[string]any{"type": "integer"},
								"minItems":    1,
							},
						},
						"required": []string{"page", "line_indices"},
					},
				},
			},
			"required": []string{"pages"},
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
