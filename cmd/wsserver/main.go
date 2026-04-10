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
	"explorer/internal/logutil"
	"explorer/internal/natsbootstrap"
	"explorer/internal/postgresstore"
	"explorer/internal/threaddocstore"
	"explorer/internal/threadstore"
	"explorer/internal/wsserver"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
)

func main() {
	if err := run(); err != nil {
		slog.Error("wsserver exited with error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	logLevel := config.ParseLogLevel(os.Getenv("LOG_LEVEL"))
	logger := slog.New(logutil.NewHandler(os.Stdout, logLevel))

	port := envOr("PORT", "8081")
	natsURL := envOr("NATS_URL", "nats://localhost:4222")
	natsName := envOr("NATS_CLIENT_NAME", "explorer-wsserver")
	redisURL := envOr("REDIS_URL", "redis://localhost:6379/0")
	postgresDSN := envOr("POSTGRES_DSN", "postgres://explorer:explorer@localhost:5432/explorer?sslmode=disable")

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

	if err := natsbootstrap.EnsureThreadEventsStream(js); err != nil {
		return fmt.Errorf("ensure thread events stream: %w", err)
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

	rdb, err := wsserver.NewRedisClient(redisURL)
	if err != nil {
		return fmt.Errorf("parse redis url: %w", err)
	}
	defer rdb.Close()

	pg := postgresstore.New(pool)
	docs := threaddocstore.New(pool)
	store := threadstore.New(rdb, pg)

	srv := wsserver.New(wsserver.Config{
		Port:   port,
		Logger: logger,
		JS:     js,
		Store:  store,
		PG:     pg,
		Docs:   docs,
	})

	logger.Info("wsserver starting",
		"port", port,
		"nats", natsURL,
	)

	return srv.ListenAndServe(ctx)
}

func envOr(key, fallback string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	return v
}
