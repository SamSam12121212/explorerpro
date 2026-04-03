package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"explorer/internal/config"
	"explorer/internal/logutil"
	"explorer/internal/openaiws"
	"explorer/internal/platform"
	"explorer/internal/worker"
)

func main() {
	if err := run(); err != nil {
		slog.Error("worker exited with error", "error", err)
		os.Exit(1)
	}
}

func run() (runErr error) {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	if strings.TrimSpace(os.Getenv("SERVICE_NAME")) == "" {
		cfg.ServiceName = "explorer-worker"
	}
	if strings.TrimSpace(os.Getenv("NATS_CLIENT_NAME")) == "" {
		cfg.NATS.ClientName = cfg.ServiceName
	}

	logger := slog.New(logutil.NewHandler(os.Stdout, cfg.LogLevel))

	startupCtx, cancelStartup := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancelStartup()

	runtime, err := platform.New(startupCtx, cfg, logger)
	if err != nil {
		return err
	}
	defer func() {
		runErr = errors.Join(runErr, runtime.Close())
	}()

	service, err := worker.New(cfg, logger, runtime, openaiws.NewCoderDialer(openaiws.FromAppConfig(cfg.OpenAI)))
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := service.Run(ctx); err != nil {
		return err
	}

	logger.Info("worker stopped cleanly", "worker_id", service.WorkerID())
	return nil
}
