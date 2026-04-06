package httpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"explorer/internal/config"
	"explorer/internal/natsbootstrap"
	"explorer/internal/platform"
)

func NewServer(cfg config.Config, logger *slog.Logger, runtime *platform.Runtime) (*http.Server, error) {
	if err := natsbootstrap.EnsureAgentCommandStream(runtime.NATS().JetStream()); err != nil {
		return nil, fmt.Errorf("bootstrap agent command stream: %w", err)
	}
	if err := natsbootstrap.EnsureGitCommandStream(runtime.NATS().JetStream()); err != nil {
		return nil, fmt.Errorf("bootstrap git command stream: %w", err)
	}

	api := newCommandAPI(cfg, logger, runtime)
	repos := newRepoAPI(logger, runtime)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{
			"service":   cfg.ServiceName,
			"env":       cfg.AppEnv,
			"blob_root": runtime.BlobRoot(),
			"message":   "explorer backend shell is live",
		})
	})
	mux.HandleFunc("/images", api.handleImages)
	mux.HandleFunc("/images/", api.handleImageServe)
	mux.HandleFunc("/repos", repos.handleRepos)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), cfg.HTTP.HealthTimeout)
		defer cancel()

		health := runtime.Health(ctx)
		statusCode := http.StatusOK
		if health.Status != "ok" {
			statusCode = http.StatusServiceUnavailable
		}

		writeJSON(w, statusCode, health)
	})
	mux.HandleFunc("/threads", api.handleThreads)
	mux.HandleFunc("/threads/", api.handleThreadRoutes)

	return &http.Server{
		Addr:              ":" + cfg.HTTP.Port,
		Handler:           requestLogger(logger, mux),
		ReadHeaderTimeout: cfg.HTTP.ReadTimeout,
		ReadTimeout:       cfg.HTTP.ReadTimeout,
		WriteTimeout:      cfg.HTTP.WriteTimeout,
		IdleTimeout:       cfg.HTTP.IdleTimeout,
	}, nil
}

func requestLogger(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(recorder, r)
		logger.Info("request completed",
			"method", r.Method,
			"path", r.URL.Path,
			"status_code", recorder.statusCode,
			"remote_addr", r.RemoteAddr,
			"duration", time.Since(started),
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	statusCode int
}

func (r *statusRecorder) WriteHeader(statusCode int) {
	r.statusCode = statusCode
	r.ResponseWriter.WriteHeader(statusCode)
}

func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("http.ResponseWriter does not implement http.Hijacker")
	}
	return hijacker.Hijack()
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeErrorJSON(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
		},
	})
}
