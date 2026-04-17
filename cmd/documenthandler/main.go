package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"explorer/internal/blobstore"
	"explorer/internal/config"
	"explorer/internal/docstore"
	"explorer/internal/documenthandler"
	"explorer/internal/logutil"
	"explorer/internal/threadcollectionstore"
	"explorer/internal/threaddocstore"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
)

func main() {
	if err := run(); err != nil {
		slog.Error("document handler service exited with error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	logLevel := config.ParseLogLevel(os.Getenv("LOG_LEVEL"))
	logger := slog.New(logutil.NewHandler(os.Stdout, logLevel))

	natsURL := envOr("NATS_URL", "nats://localhost:4222")
	natsName := envOr("NATS_CLIENT_NAME", "explorer-documenthandler")
	postgresDSN := envOr("POSTGRES_DSN", "postgres://explorer:explorer@localhost:5432/explorer?sslmode=disable")
	blobDir := envOr("BLOB_STORAGE_DIR", "./blob-storage")

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

	poolCfg, err := pgxpool.ParseConfig(postgresDSN)
	if err != nil {
		return fmt.Errorf("parse postgres config: %w", err)
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return fmt.Errorf("create postgres pool: %w", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping postgres: %w", err)
	}

	blob, err := blobstore.NewLocal(blobDir)
	if err != nil {
		return fmt.Errorf("create blob store: %w", err)
	}

	docs := docstore.New(pool)
	threadDocs := threaddocstore.New(pool)
	threadCollections := threadcollectionstore.New(pool)
	svc := documenthandler.New(logger, nc, js, docs, threadDocs, threadCollections, blob)

	logger.Info("document handler service starting",
		"nats", natsURL,
		"blob_dir", blobDir,
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
