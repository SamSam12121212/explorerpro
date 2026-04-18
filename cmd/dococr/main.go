package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"explorer/internal/blobstore"
	"explorer/internal/config"
	"explorer/internal/dococr"
	"explorer/internal/logutil"

	"github.com/nats-io/nats.go"
)

func main() {
	if err := run(); err != nil {
		slog.Error("dococr service exited with error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	logLevel := config.ParseLogLevel(os.Getenv("LOG_LEVEL"))
	logger := slog.New(logutil.NewHandler(os.Stdout, logLevel))

	natsURL := envOr("NATS_URL", "nats://localhost:4222")
	natsName := envOr("NATS_CLIENT_NAME", "explorer-dococr")
	blobDir := envOr("BLOB_STORAGE_DIR", "./blob-storage")

	paddleURL := strings.TrimSpace(os.Getenv("PADDLEOCR_URL"))
	if paddleURL == "" {
		return fmt.Errorf("PADDLEOCR_URL is required")
	}
	paddleBearer := strings.TrimSpace(os.Getenv("PADDLEOCR_BEARER_TOKEN"))

	requestTimeout := 60 * time.Second
	if raw := strings.TrimSpace(os.Getenv("PADDLEOCR_REQUEST_TIMEOUT_SECONDS")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			requestTimeout = time.Duration(v) * time.Second
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	nc, err := nats.Connect(
		natsURL,
		nats.Name(natsName),
		nats.Timeout(5*time.Second),
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2*time.Second),
	)
	if err != nil {
		return fmt.Errorf("connect nats: %w", err)
	}
	defer nc.Close()

	js, err := nc.JetStream()
	if err != nil {
		return fmt.Errorf("create jetstream context: %w", err)
	}

	blob, err := blobstore.NewLocal(blobDir)
	if err != nil {
		return fmt.Errorf("create blob store: %w", err)
	}

	svc := dococr.New(logger, js, blob, dococr.Config{
		PaddleOCRURL:    paddleURL,
		PaddleOCRBearer: paddleBearer,
		RequestTimeout:  requestTimeout,
	})

	logger.Info("dococr service starting",
		"nats", natsURL,
		"blob_dir", blobDir,
		"paddleocr_url", paddleURL,
		"paddleocr_auth", paddleBearer != "",
	)

	return svc.Run(ctx)
}

func envOr(key, fallback string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	return v
}
