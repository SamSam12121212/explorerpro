package wsserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"explorer/internal/eventrelay"
	"explorer/internal/postgresstore"
	"explorer/internal/threaddocstore"
)

type Config struct {
	Port      string
	RelayAddr string
	Logger    *slog.Logger
	Store     *postgresstore.Store
	Docs      *threaddocstore.Store
}

type Server struct {
	cfg    Config
	hub    *eventHub
	relay  *eventrelay.Server
	logger *slog.Logger
	mux    *http.ServeMux
}

func New(cfg Config) *Server {
	hub := newEventHub(cfg.Logger)

	s := &Server{
		cfg:    cfg,
		hub:    hub,
		relay:  eventrelay.NewServer(cfg.Logger, hub.HandleFrame),
		logger: cfg.Logger,
		mux:    http.NewServeMux(),
	}

	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /connect", s.handleGlobalConnect)
	s.mux.HandleFunc("GET /threads/{thread_id}/connect", s.handleConnect)

	return s
}

func (s *Server) ListenAndServe(ctx context.Context) error {
	relayErrCh := make(chan error, 1)
	go func() {
		relayErrCh <- s.relay.ListenAndServe(ctx, s.cfg.RelayAddr)
	}()

	srv := &http.Server{
		Addr:              ":" + s.cfg.Port,
		Handler:           s.mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      0,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	s.logger.Info("wsserver listening", "port", s.cfg.Port, "relay", s.cfg.RelayAddr)

	httpErrCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			httpErrCh <- fmt.Errorf("wsserver listen: %w", err)
		}
		close(httpErrCh)
	}()

	select {
	case err := <-relayErrCh:
		if err != nil {
			return fmt.Errorf("event relay: %w", err)
		}
	case err := <-httpErrCh:
		if err != nil {
			return err
		}
	case <-ctx.Done():
	}

	return nil
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	threadID, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("thread_id")), 10, 64)
	if err != nil || threadID <= 0 {
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

func (s *Server) handleGlobalConnect(w http.ResponseWriter, r *http.Request) {
	client := newClient(clientConfig{
		logger: s.logger.With("scope", "global"),
		store:  s.cfg.Store,
		docs:   s.cfg.Docs,
	})

	client.serve(w, r, s.hub)
}
