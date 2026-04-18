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

	ch := make(chan *nats.Msg, 16)
	sub, err := s.js.ChanQueueSubscribe(
		ocrcmd.SplitDoneSubject,
		ocrcmd.SplitDoneQueue,
		ch,
		nats.BindStream(ocrcmd.StreamName),
		nats.ManualAck(),
		nats.AckWait(30*time.Minute),
		nats.MaxDeliver(3),
	)
	if err != nil {
		return fmt.Errorf("subscribe %s: %w", ocrcmd.SplitDoneSubject, err)
	}

	s.logger.Info("dococr service started",
		"subject", ocrcmd.SplitDoneSubject,
		"paddleocr_url", s.cfg.PaddleOCRURL,
	)

	for {
		select {
		case <-ctx.Done():
			_ = sub.Drain()
			s.logger.Info("dococr service stopping", "reason", ctx.Err())
			return nil
		case msg := <-ch:
			s.handleSplitDone(ctx, msg)
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

		ocrBytes, duration, err := s.callPaddleOCR(ctx, pageBytes, page.PageNumber)
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

func (s *Service) callPaddleOCR(ctx context.Context, pngBytes []byte, pageNumber int) ([]byte, time.Duration, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="image"; filename="page-%04d.png"`, pageNumber))
	header.Set("Content-Type", "image/png")
	part, err := writer.CreatePart(header)
	if err != nil {
		return nil, 0, fmt.Errorf("build multipart part: %w", err)
	}
	if _, err := part.Write(pngBytes); err != nil {
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

func trimForLog(data []byte) string {
	const max = 500
	s := strings.TrimSpace(string(data))
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}
