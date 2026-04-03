package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"explorer/internal/config"
	apphttp "explorer/internal/httpserver"
	"explorer/internal/logutil"
	"explorer/internal/platform"
)

func main() {
	if err := run(); err != nil {
		slog.Error("application exited with error", "error", err)
		os.Exit(1)
	}
}

func run() (runErr error) {
	cfg, err := config.Load()
	if err != nil {
		return err
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

	server, err := apphttp.NewServer(cfg, logger, runtime)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)

	go func() {
		logger.Info("server starting", "addr", server.Addr)
		errCh <- server.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			return err
		}
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		return err
	}

	logger.Info("server stopped cleanly")
	return nil
}
