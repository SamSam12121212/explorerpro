package ocrcmd

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	StreamName = "DOC_OCR"

	// SplitDoneSubject is emitted by docsplitter after a successful split and
	// consumed by dococr to drive OCR over every rendered page.
	SplitDoneSubject = "doc.split.done"
	SplitDoneQueue   = "ocr-workers"

	// OCRDoneSubject is emitted by dococr once every page of a document has
	// an OCR ref written back onto the manifest.
	OCRDoneSubject = "doc.ocr.done"
)

type SplitDoneEvent struct {
	DocumentID  int64  `json:"document_id"`
	ManifestRef string `json:"manifest_ref"`
	PageCount   int    `json:"page_count"`
}

type OCRDoneEvent struct {
	DocumentID int64 `json:"document_id"`
	PageCount  int   `json:"page_count"`
}

func EncodeSplitDone(evt SplitDoneEvent) ([]byte, error) {
	if err := validateSplitDone(evt); err != nil {
		return nil, err
	}
	data, err := json.Marshal(evt)
	if err != nil {
		return nil, fmt.Errorf("marshal split done event: %w", err)
	}
	return data, nil
}

func DecodeSplitDone(data []byte) (SplitDoneEvent, error) {
	var evt SplitDoneEvent
	if err := json.Unmarshal(data, &evt); err != nil {
		return SplitDoneEvent{}, fmt.Errorf("decode split done event: %w", err)
	}
	if err := validateSplitDone(evt); err != nil {
		return SplitDoneEvent{}, err
	}
	return evt, nil
}

func EncodeOCRDone(evt OCRDoneEvent) ([]byte, error) {
	if evt.DocumentID <= 0 {
		return nil, fmt.Errorf("ocr done event missing document_id")
	}
	data, err := json.Marshal(evt)
	if err != nil {
		return nil, fmt.Errorf("marshal ocr done event: %w", err)
	}
	return data, nil
}

func validateSplitDone(evt SplitDoneEvent) error {
	if evt.DocumentID <= 0 {
		return fmt.Errorf("split done event missing document_id")
	}
	if strings.TrimSpace(evt.ManifestRef) == "" {
		return fmt.Errorf("split done event missing manifest_ref")
	}
	if evt.PageCount <= 0 {
		return fmt.Errorf("split done event missing page_count")
	}
	return nil
}
