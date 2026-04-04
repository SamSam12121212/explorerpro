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

	"explorer/internal/config"
	"explorer/internal/gitservice"
	"explorer/internal/logutil"
	"explorer/internal/repostore"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
)

func main() {
	if err := run(); err != nil {
		slog.Error("git service exited with error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	logLevel := config.ParseLogLevel(os.Getenv("LOG_LEVEL"))
	logger := slog.New(logutil.NewHandler(os.Stdout, logLevel))

	natsURL := envOr("NATS_URL", "nats://localhost:4222")
	natsName := envOr("NATS_CLIENT_NAME", "explorer-git")
	postgresDSN := envOr("POSTGRES_DSN", "postgres://explorer:explorer@localhost:5432/explorer?sslmode=disable")
	dataDir := envOr("GIT_DATA_DIR", "/data/git")

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

	githubToken := envOr("GITHUB_TOKEN", "")

	repos := repostore.New(pool)
	svc := gitservice.New(logger, js, repos, dataDir, githubToken)

	logger.Info("git service starting",
		"nats", natsURL,
		"data_dir", dataDir,
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
