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

	// ImageOCRRequestedSubject is emitted by the HTTP image upload handler
	// after an image blob lands, and consumed by dococr to drive a single
	// PaddleOCR call against the uploaded image.
	ImageOCRRequestedSubject = "image.ocr.requested"
	ImageOCRRequestedQueue   = "image-ocr-workers"

	// ImageOCRDoneSubject is emitted by dococr once an uploaded image has an
	// OCR JSON blob persisted next to its source.
	ImageOCRDoneSubject = "image.ocr.done"
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

type ImageOCRRequestedEvent struct {
	ImageID     string `json:"image_id"`
	ImageRef    string `json:"image_ref"`
	ContentType string `json:"content_type,omitempty"`
}

type ImageOCRDoneEvent struct {
	ImageID string `json:"image_id"`
	OCRRef  string `json:"ocr_ref"`
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

func EncodeImageOCRRequested(evt ImageOCRRequestedEvent) ([]byte, error) {
	if err := validateImageOCRRequested(evt); err != nil {
		return nil, err
	}
	data, err := json.Marshal(evt)
	if err != nil {
		return nil, fmt.Errorf("marshal image ocr requested event: %w", err)
	}
	return data, nil
}

func DecodeImageOCRRequested(data []byte) (ImageOCRRequestedEvent, error) {
	var evt ImageOCRRequestedEvent
	if err := json.Unmarshal(data, &evt); err != nil {
		return ImageOCRRequestedEvent{}, fmt.Errorf("decode image ocr requested event: %w", err)
	}
	if err := validateImageOCRRequested(evt); err != nil {
		return ImageOCRRequestedEvent{}, err
	}
	return evt, nil
}

func EncodeImageOCRDone(evt ImageOCRDoneEvent) ([]byte, error) {
	if strings.TrimSpace(evt.ImageID) == "" {
		return nil, fmt.Errorf("image ocr done event missing image_id")
	}
	if strings.TrimSpace(evt.OCRRef) == "" {
		return nil, fmt.Errorf("image ocr done event missing ocr_ref")
	}
	data, err := json.Marshal(evt)
	if err != nil {
		return nil, fmt.Errorf("marshal image ocr done event: %w", err)
	}
	return data, nil
}

func validateImageOCRRequested(evt ImageOCRRequestedEvent) error {
	if strings.TrimSpace(evt.ImageID) == "" {
		return fmt.Errorf("image ocr requested event missing image_id")
	}
	if strings.TrimSpace(evt.ImageRef) == "" {
		return fmt.Errorf("image ocr requested event missing image_ref")
	}
	return nil
}
