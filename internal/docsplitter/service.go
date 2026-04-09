package docsplitter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	_ "image/png"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"explorer/internal/blobstore"
	"explorer/internal/doccmd"
	"explorer/internal/docstore"
	"explorer/internal/natsbootstrap"

	"github.com/nats-io/nats.go"
)

type Service struct {
	logger *slog.Logger
	js     nats.JetStreamContext
	docs   *docstore.Store
	blob   *blobstore.LocalStore
}

func New(logger *slog.Logger, js nats.JetStreamContext, docs *docstore.Store, blob *blobstore.LocalStore) *Service {
	return &Service{
		logger: logger,
		js:     js,
		docs:   docs,
		blob:   blob,
	}
}

func (s *Service) Run(ctx context.Context) error {
	if err := natsbootstrap.EnsureDocCommandStream(s.js); err != nil {
		return fmt.Errorf("bootstrap doc command stream: %w", err)
	}

	ch := make(chan *nats.Msg, 64)
	sub, err := s.js.ChanQueueSubscribe(
		doccmd.SplitSubject,
		doccmd.SplitQueue,
		ch,
		nats.BindStream(doccmd.StreamName),
		nats.ManualAck(),
		nats.AckWait(10*time.Minute),
		nats.MaxDeliver(3),
	)
	if err != nil {
		return fmt.Errorf("subscribe %s: %w", doccmd.SplitSubject, err)
	}

	s.logger.Info("docsplitter service started", "subject", doccmd.SplitSubject)

	for {
		select {
		case <-ctx.Done():
			_ = sub.Drain()
			s.logger.Info("docsplitter service stopping", "reason", ctx.Err())
			return nil
		case msg := <-ch:
			s.handleSplit(ctx, msg)
		}
	}
}

func (s *Service) handleSplit(ctx context.Context, msg *nats.Msg) {
	cmd, err := doccmd.DecodeSplit(msg.Data)
	if err != nil {
		s.logger.Error("invalid split command", "error", err)
		_ = msg.Term()
		return
	}

	s.logger.Info("processing split",
		"document_id", cmd.DocumentID,
		"source_ref", cmd.SourceRef,
		"dpi", cmd.DPI,
	)

	if err := s.docs.UpdateStatus(ctx, cmd.DocumentID, "splitting", "", 0, ""); err != nil {
		s.logger.Error("failed to update document status to splitting", "document_id", cmd.DocumentID, "error", err)
		_ = msg.Nak()
		return
	}

	manifestRef, pageCount, err := s.splitPDF(ctx, cmd)
	if err != nil {
		errMsg := err.Error()
		s.logger.Error("split failed", "document_id", cmd.DocumentID, "error", errMsg)
		if updateErr := s.docs.UpdateStatus(ctx, cmd.DocumentID, "failed", "", 0, errMsg); updateErr != nil {
			s.logger.Error("failed to update document status to failed", "document_id", cmd.DocumentID, "error", updateErr)
		}
		_ = msg.Ack()
		return
	}

	if err := s.docs.UpdateStatus(ctx, cmd.DocumentID, "ready", manifestRef, pageCount, ""); err != nil {
		s.logger.Error("failed to update document status to ready", "document_id", cmd.DocumentID, "error", err)
		_ = msg.Nak()
		return
	}

	s.logger.Info("split complete",
		"document_id", cmd.DocumentID,
		"manifest_ref", manifestRef,
		"page_count", pageCount,
	)

	_ = msg.Ack()
}

func (s *Service) splitPDF(ctx context.Context, cmd doccmd.SplitCommand) (string, int, error) {
	pdfData, err := s.blob.ReadRef(ctx, cmd.SourceRef)
	if err != nil {
		return "", 0, fmt.Errorf("read source pdf: %w", err)
	}

	tmpDir, err := os.MkdirTemp("", "docsplit-*")
	if err != nil {
		return "", 0, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	inputPath := filepath.Join(tmpDir, "input.pdf")
	if err := os.WriteFile(inputPath, pdfData, 0o644); err != nil {
		return "", 0, fmt.Errorf("write temp pdf: %w", err)
	}

	splitCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	outputPrefix := filepath.Join(tmpDir, "page")
	args := []string{"-png", "-r", strconv.Itoa(cmd.DPI), inputPath, outputPrefix}
	splitCmd := exec.CommandContext(splitCtx, "pdftocairo", args...)

	var stderr strings.Builder
	splitCmd.Stderr = &stderr

	if err := splitCmd.Run(); err != nil {
		return "", 0, fmt.Errorf("pdftocairo: %s: %w", strings.TrimSpace(stderr.String()), err)
	}

	pngFiles, err := filepath.Glob(filepath.Join(tmpDir, "page-*.png"))
	if err != nil {
		return "", 0, fmt.Errorf("glob output pngs: %w", err)
	}
	sort.Strings(pngFiles)

	if len(pngFiles) == 0 {
		return "", 0, fmt.Errorf("pdftocairo produced no output pages")
	}

	backendVersion := detectPopplerVersion(ctx)

	now := time.Now().UTC()
	pages := make([]PageEntry, 0, len(pngFiles))

	for i, pngPath := range pngFiles {
		pageNum := i + 1

		data, err := os.ReadFile(pngPath)
		if err != nil {
			return "", 0, fmt.Errorf("read page %d: %w", pageNum, err)
		}

		hash := sha256.Sum256(data)
		hexHash := hex.EncodeToString(hash[:])

		width, height, err := pngDimensions(data)
		if err != nil {
			return "", 0, fmt.Errorf("decode page %d dimensions: %w", pageNum, err)
		}

		pageRef := s.blob.Ref("documents", cmd.DocumentID, "pages", fmt.Sprintf("page-%04d.png", pageNum))
		if err := s.blob.WriteRef(ctx, pageRef, data); err != nil {
			return "", 0, fmt.Errorf("write page %d to blob: %w", pageNum, err)
		}

		pages = append(pages, PageEntry{
			PageNumber:  pageNum,
			ImageRef:    pageRef,
			Width:       width,
			Height:      height,
			ContentType: "image/png",
			SHA256:      hexHash,
		})
	}

	manifest := Manifest{
		Version:              "v1",
		DocumentID:           cmd.DocumentID,
		CreatedAt:            now.Format(time.RFC3339),
		PageCount:            len(pages),
		AssetsRootRef:        s.blob.Ref("documents", cmd.DocumentID),
		RenderBackend:        "poppler/pdftocairo",
		RenderBackendVersion: backendVersion,
		RenderParams: RenderParams{
			Format: "png",
			DPI:    cmd.DPI,
		},
		Pages: pages,
	}

	manifestJSON, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", 0, fmt.Errorf("marshal manifest: %w", err)
	}

	manifestRef := s.blob.Ref("documents", cmd.DocumentID, "manifest.json")
	if err := s.blob.WriteRef(ctx, manifestRef, manifestJSON); err != nil {
		return "", 0, fmt.Errorf("write manifest to blob: %w", err)
	}

	return manifestRef, len(pages), nil
}

func pngDimensions(data []byte) (int, int, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0, err
	}
	return cfg.Width, cfg.Height, nil
}

func detectPopplerVersion(ctx context.Context) string {
	vCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(vCtx, "pdftocairo", "-v")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "unknown"
	}

	// Output is typically: "pdftocairo version 26.03.0"
	line := strings.TrimSpace(string(out))
	if idx := strings.LastIndex(line, " "); idx >= 0 {
		return line[idx+1:]
	}
	return line
}
