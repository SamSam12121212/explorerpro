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
	"explorer/internal/postgresstore"
	"explorer/internal/threaddocstore"
	"explorer/internal/wsserver"

	"github.com/jackc/pgx/v5/pgxpool"
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
	relayAddr := envOr("EVENT_RELAY_ADDR", ":9090")
	postgresDSN := envOr("POSTGRES_DSN", "postgres://explorer:explorer@localhost:5432/explorer?sslmode=disable")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	poolCfg, err := pgxpool.ParseConfig(postgresDSN)
	if err != nil {
		return fmt.Errorf("parse postgres config: %w", err)
	}
	poolCfg.ConnConfig.ConnectTimeout = 5 * time.Second
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return fmt.Errorf("create postgres pool: %w", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping postgres: %w", err)
	}

	store := postgresstore.New(pool)
	docs := threaddocstore.New(pool)

	srv := wsserver.New(wsserver.Config{
		Port:      port,
		RelayAddr: relayAddr,
		Logger:    logger,
		Store:     store,
		Docs:      docs,
	})

	logger.Info("wsserver starting",
		"port", port,
		"relay_addr", relayAddr,
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
