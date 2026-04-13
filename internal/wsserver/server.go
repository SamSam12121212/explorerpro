package wsserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"explorer/internal/postgresstore"
	"explorer/internal/threaddocstore"

	"github.com/nats-io/nats.go"
)

type Config struct {
	Port   string
	Logger *slog.Logger
	JS     nats.JetStreamContext
	Store  *postgresstore.Store
	Docs   *threaddocstore.Store
}

type Server struct {
	cfg    Config
	hub    *eventHub
	logger *slog.Logger
	mux    *http.ServeMux
}

func New(cfg Config) *Server {
	s := &Server{
		cfg:    cfg,
		hub:    newEventHub(cfg.Logger, cfg.JS),
		logger: cfg.Logger,
		mux:    http.NewServeMux(),
	}

	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /threads/{thread_id}/connect", s.handleConnect)

	return s
}

func (s *Server) ListenAndServe(ctx context.Context) error {
	if err := s.hub.Start(ctx); err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              ":" + s.cfg.Port,
		Handler:           s.mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      0, // unlimited for websocket
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	s.logger.Info("wsserver listening", "port", s.cfg.Port)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("wsserver listen: %w", err)
	}
	return nil
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("thread_id")
	if strings.TrimSpace(threadID) == "" {
		http.Error(w, "missing thread_id", http.StatusBadRequest)
		return
	}

	afterItem := strings.TrimSpace(r.URL.Query().Get("after_item"))

	client := newClient(clientConfig{
		logger:    s.logger.With("thread_id", threadID),
		store:     s.cfg.Store,
		docs:      s.cfg.Docs,
		threadID:  threadID,
		afterItem: afterItem,
	})

	client.serve(w, r, s.hub)
}
