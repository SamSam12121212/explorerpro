package dococr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"time"

	"explorer/internal/blobstore"
	"explorer/internal/docsplitter"
	"explorer/internal/natsbootstrap"
	"explorer/internal/ocrcmd"

	"github.com/nats-io/nats.go"
)

type Config struct {
	PaddleOCRURL    string
	PaddleOCRBearer string
	RequestTimeout  time.Duration
}

type Service struct {
	logger *slog.Logger
	js     nats.JetStreamContext
	blob   *blobstore.LocalStore
	http   *http.Client
	cfg    Config
}

func New(logger *slog.Logger, js nats.JetStreamContext, blob *blobstore.LocalStore, cfg Config) *Service {
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 60 * time.Second
	}
	return &Service{
		logger: logger,
		js:     js,
		blob:   blob,
		http:   &http.Client{Timeout: cfg.RequestTimeout},
		cfg:    cfg,
	}
}

func (s *Service) Run(ctx context.Context) error {
	if err := natsbootstrap.EnsureDocOCRStream(s.js); err != nil {
		return fmt.Errorf("bootstrap doc ocr stream: %w", err)
	}

	splitCh := make(chan *nats.Msg, 16)
	splitSub, err := s.js.ChanQueueSubscribe(
		ocrcmd.SplitDoneSubject,
		ocrcmd.SplitDoneQueue,
		splitCh,
		nats.BindStream(ocrcmd.StreamName),
		nats.ManualAck(),
		nats.AckWait(30*time.Minute),
		nats.MaxDeliver(3),
	)
	if err != nil {
		return fmt.Errorf("subscribe %s: %w", ocrcmd.SplitDoneSubject, err)
	}

	imageCh := make(chan *nats.Msg, 16)
	imageSub, err := s.js.ChanQueueSubscribe(
		ocrcmd.ImageOCRRequestedSubject,
		ocrcmd.ImageOCRRequestedQueue,
		imageCh,
		nats.BindStream(ocrcmd.StreamName),
		nats.ManualAck(),
		nats.AckWait(5*time.Minute),
		nats.MaxDeliver(3),
	)
	if err != nil {
		_ = splitSub.Drain()
		return fmt.Errorf("subscribe %s: %w", ocrcmd.ImageOCRRequestedSubject, err)
	}

	s.logger.Info("dococr service started",
		"split_subject", ocrcmd.SplitDoneSubject,
		"image_subject", ocrcmd.ImageOCRRequestedSubject,
		"paddleocr_url", s.cfg.PaddleOCRURL,
	)

	for {
		select {
		case <-ctx.Done():
			_ = splitSub.Drain()
			_ = imageSub.Drain()
			s.logger.Info("dococr service stopping", "reason", ctx.Err())
			return nil
		case msg := <-splitCh:
			s.handleSplitDone(ctx, msg)
		case msg := <-imageCh:
			s.handleImageOCRRequested(ctx, msg)
		}
	}
}

func (s *Service) handleSplitDone(ctx context.Context, msg *nats.Msg) {
	evt, err := ocrcmd.DecodeSplitDone(msg.Data)
	if err != nil {
		s.logger.Error("invalid split done event", "error", err)
		_ = msg.Term()
		return
	}

	s.logger.Info("processing ocr",
		"document_id", evt.DocumentID,
		"manifest_ref", evt.ManifestRef,
		"page_count", evt.PageCount,
	)

	manifestBytes, err := s.blob.ReadRef(ctx, evt.ManifestRef)
	if err != nil {
		s.logger.Error("failed to read manifest", "document_id", evt.DocumentID, "error", err)
		_ = msg.Nak()
		return
	}

	var manifest docsplitter.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		s.logger.Error("failed to parse manifest", "document_id", evt.DocumentID, "error", err)
		_ = msg.Term()
		return
	}

	start := time.Now()
	for i := range manifest.Pages {
		page := &manifest.Pages[i]
		if page.OCRRef != "" {
			continue
		}

		pageBytes, err := s.blob.ReadRef(ctx, page.ImageRef)
		if err != nil {
			s.logger.Error("failed to read page image",
				"document_id", evt.DocumentID,
				"page_number", page.PageNumber,
				"image_ref", page.ImageRef,
				"error", err,
			)
			_ = msg.Nak()
			return
		}

		pageFilename := fmt.Sprintf("page-%04d.png", page.PageNumber)
		ocrBytes, duration, err := s.callPaddleOCR(ctx, pageBytes, pageFilename, "image/png")
		if err != nil {
			s.logger.Error("paddleocr call failed",
				"document_id", evt.DocumentID,
				"page_number", page.PageNumber,
				"error", err,
			)
			_ = msg.Nak()
			return
		}

		ocrRef := s.blob.Ref("documents", fmt.Sprintf("%d", evt.DocumentID), "ocr", fmt.Sprintf("page-%04d.json", page.PageNumber))
		if err := s.blob.WriteRef(ctx, ocrRef, ocrBytes); err != nil {
			s.logger.Error("failed to write ocr blob",
				"document_id", evt.DocumentID,
				"page_number", page.PageNumber,
				"ocr_ref", ocrRef,
				"error", err,
			)
			_ = msg.Nak()
			return
		}

		page.OCRRef = ocrRef
		s.logger.Info("page ocr complete",
			"document_id", evt.DocumentID,
			"page_number", page.PageNumber,
			"ocr_ref", ocrRef,
			"paddleocr_ms", duration.Milliseconds(),
		)
	}

	updated, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		s.logger.Error("failed to marshal updated manifest", "document_id", evt.DocumentID, "error", err)
		_ = msg.Nak()
		return
	}
	if err := s.blob.WriteRef(ctx, evt.ManifestRef, updated); err != nil {
		s.logger.Error("failed to write updated manifest",
			"document_id", evt.DocumentID,
			"manifest_ref", evt.ManifestRef,
			"error", err,
		)
		_ = msg.Nak()
		return
	}

	if err := s.publishOCRDone(evt.DocumentID, len(manifest.Pages)); err != nil {
		s.logger.Error("failed to signal ocr done; will retry on redelivery",
			"document_id", evt.DocumentID,
			"error", err,
		)
		_ = msg.Nak()
		return
	}

	s.logger.Info("ocr complete",
		"document_id", evt.DocumentID,
		"page_count", len(manifest.Pages),
		"elapsed_ms", time.Since(start).Milliseconds(),
	)

	_ = msg.Ack()
}

func (s *Service) callPaddleOCR(ctx context.Context, imageBytes []byte, filename, contentType string) ([]byte, time.Duration, error) {
	if strings.TrimSpace(filename) == "" {
		filename = "image"
	}
	if strings.TrimSpace(contentType) == "" {
		contentType = "application/octet-stream"
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="image"; filename=%q`, filename))
	header.Set("Content-Type", contentType)
	part, err := writer.CreatePart(header)
	if err != nil {
		return nil, 0, fmt.Errorf("build multipart part: %w", err)
	}
	if _, err := part.Write(imageBytes); err != nil {
		return nil, 0, fmt.Errorf("write multipart body: %w", err)
	}
	if err := writer.Close(); err != nil {
		return nil, 0, fmt.Errorf("close multipart writer: %w", err)
	}

	endpoint := strings.TrimRight(s.cfg.PaddleOCRURL, "/") + "/ocr"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &body)
	if err != nil {
		return nil, 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	if token := strings.TrimSpace(s.cfg.PaddleOCRBearer); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	started := time.Now()
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("post /ocr: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, fmt.Errorf("read /ocr response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("paddleocr returned %d: %s", resp.StatusCode, trimForLog(data))
	}

	var probe struct {
		Lines *[]any `json:"lines"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, 0, fmt.Errorf("decode /ocr response: %w", err)
	}
	if probe.Lines == nil {
		return nil, 0, fmt.Errorf("paddleocr response missing lines field")
	}

	return data, time.Since(started), nil
}

func (s *Service) publishOCRDone(documentID int64, pageCount int) error {
	data, err := ocrcmd.EncodeOCRDone(ocrcmd.OCRDoneEvent{
		DocumentID: documentID,
		PageCount:  pageCount,
	})
	if err != nil {
		return fmt.Errorf("encode ocr done event: %w", err)
	}
	if _, err := s.js.Publish(ocrcmd.OCRDoneSubject, data); err != nil {
		return fmt.Errorf("publish %s: %w", ocrcmd.OCRDoneSubject, err)
	}
	return nil
}

func (s *Service) handleImageOCRRequested(ctx context.Context, msg *nats.Msg) {
	evt, err := ocrcmd.DecodeImageOCRRequested(msg.Data)
	if err != nil {
		s.logger.Error("invalid image ocr requested event", "error", err)
		_ = msg.Term()
		return
	}

	s.logger.Info("processing image ocr",
		"image_id", evt.ImageID,
		"image_ref", evt.ImageRef,
	)

	ocrRef := s.blob.Ref("images", evt.ImageID, "ocr.json")

	// Cache short-circuit: if OCR already landed (redelivery, manual retrigger),
	// skip the PaddleOCR call but still re-publish image.ocr.done so any
	// downstream consumer waiting on the first publish isn't stuck.
	if existing, err := s.blob.ReadRef(ctx, ocrRef); err == nil && len(existing) > 0 {
		s.logger.Info("image ocr already present; skipping paddleocr call",
			"image_id", evt.ImageID,
			"ocr_ref", ocrRef,
		)
		if err := s.publishImageOCRDone(evt.ImageID, ocrRef); err != nil {
			s.logger.Error("failed to re-publish image ocr done; will retry on redelivery",
				"image_id", evt.ImageID,
				"error", err,
			)
			_ = msg.Nak()
			return
		}
		_ = msg.Ack()
		return
	}

	imageBytes, err := s.blob.ReadRef(ctx, evt.ImageRef)
	if err != nil {
		s.logger.Error("failed to read image blob",
			"image_id", evt.ImageID,
			"image_ref", evt.ImageRef,
			"error", err,
		)
		_ = msg.Nak()
		return
	}

	start := time.Now()
	ocrBytes, duration, err := s.callPaddleOCR(ctx, imageBytes, imageUploadFilename(evt.ImageID, evt.ContentType), evt.ContentType)
	if err != nil {
		s.logger.Error("paddleocr call failed for image",
			"image_id", evt.ImageID,
			"error", err,
		)
		_ = msg.Nak()
		return
	}

	if err := s.blob.WriteRef(ctx, ocrRef, ocrBytes); err != nil {
		s.logger.Error("failed to write image ocr blob",
			"image_id", evt.ImageID,
			"ocr_ref", ocrRef,
			"error", err,
		)
		_ = msg.Nak()
		return
	}

	// Publish BEFORE ack so a Nak from publish failure redelivers the request;
	// redelivery hits the cache branch and re-publishes without redoing OCR.
	if err := s.publishImageOCRDone(evt.ImageID, ocrRef); err != nil {
		s.logger.Error("failed to signal image ocr done; will retry on redelivery",
			"image_id", evt.ImageID,
			"error", err,
		)
		_ = msg.Nak()
		return
	}

	s.logger.Info("image ocr complete",
		"image_id", evt.ImageID,
		"ocr_ref", ocrRef,
		"paddleocr_ms", duration.Milliseconds(),
		"elapsed_ms", time.Since(start).Milliseconds(),
	)

	_ = msg.Ack()
}

func (s *Service) publishImageOCRDone(imageID, ocrRef string) error {
	data, err := ocrcmd.EncodeImageOCRDone(ocrcmd.ImageOCRDoneEvent{
		ImageID: imageID,
		OCRRef:  ocrRef,
	})
	if err != nil {
		return fmt.Errorf("encode image ocr done event: %w", err)
	}
	if _, err := s.js.Publish(ocrcmd.ImageOCRDoneSubject, data); err != nil {
		return fmt.Errorf("publish %s: %w", ocrcmd.ImageOCRDoneSubject, err)
	}
	return nil
}

func imageUploadFilename(imageID, contentType string) string {
	ext := ""
	switch strings.ToLower(strings.TrimSpace(contentType)) {
	case "image/png":
		ext = ".png"
	case "image/jpeg", "image/jpg":
		ext = ".jpg"
	case "image/gif":
		ext = ".gif"
	case "image/webp":
		ext = ".webp"
	case "image/bmp":
		ext = ".bmp"
	case "image/tiff":
		ext = ".tiff"
	}
	return imageID + ext
}

func trimForLog(data []byte) string {
	const max = 500
	s := strings.TrimSpace(string(data))
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}
