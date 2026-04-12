package doccmd

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	StreamName           = "DOC_CMD"
	SplitSubject         = "doc.split"
	SplitQueue           = "doc-workers"
	PrepareInputSubject  = "doc.prepare_input"
	PrepareInputQueue    = "documenthandlers"
	DefaultDPI           = 150
	PrepareKindWarmup    = "document_warmup"
	PrepareStatusOK      = "ok"
	PrepareStatusError   = "error"
	PrepareStatusNoop    = "noop"
	PrepareStatusPending = "pending"
)

type SplitCommand struct {
	CmdID      string `json:"cmd_id"`
	DocumentID string `json:"document_id"`
	SourceRef  string `json:"source_ref"`
	DPI        int    `json:"dpi,omitempty"`
}

type PrepareInputRequest struct {
	RequestID  string          `json:"request_id"`
	Kind       string          `json:"kind"`
	ThreadID   string          `json:"thread_id,omitempty"`
	DocumentID string          `json:"document_id"`
	Task       string          `json:"task,omitempty"`
	InputItems json.RawMessage `json:"input_items,omitempty"`
}

type PrepareInputResponse struct {
	RequestID        string `json:"request_id"`
	Status           string `json:"status"`
	PreparedInputRef string `json:"prepared_input_ref,omitempty"`
	Error            string `json:"error,omitempty"`
}

func DecodeSplit(data []byte) (SplitCommand, error) {
	var cmd SplitCommand
	if err := json.Unmarshal(data, &cmd); err != nil {
		return SplitCommand{}, fmt.Errorf("decode split command: %w", err)
	}
	if strings.TrimSpace(cmd.DocumentID) == "" {
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

func validatePrepareInputRequest(req PrepareInputRequest) error {
	if strings.TrimSpace(req.RequestID) == "" {
		return fmt.Errorf("prepare input request missing request_id")
	}
	if strings.TrimSpace(req.Kind) == "" {
		return fmt.Errorf("prepare input request missing kind")
	}
	if strings.TrimSpace(req.DocumentID) == "" {
		return fmt.Errorf("prepare input request missing document_id")
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
