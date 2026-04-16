package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"explorer/internal/config"
	"explorer/internal/logutil"
	"explorer/internal/wsserver"
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := wsserver.New(wsserver.Config{
		Port:      port,
		RelayAddr: relayAddr,
		Logger:    logger,
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
