package doccmd

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	StreamName   = "DOC_CMD"
	SplitSubject = "doc.split"
	SplitQueue   = "doc-workers"
	DefaultDPI   = 150
)

type SplitCommand struct {
	CmdID      string `json:"cmd_id"`
	DocumentID string `json:"document_id"`
	SourceRef  string `json:"source_ref"`
	DPI        int    `json:"dpi,omitempty"`
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
