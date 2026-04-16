package wsserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"explorer/internal/eventrelay"
)

type Config struct {
	Port      string
	RelayAddr string
	Logger    *slog.Logger
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
	s.mux.HandleFunc("GET /connect", s.handleConnect)

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
	client := newClient(clientConfig{
		logger: s.logger,
	})
	client.serve(w, r, s.hub)
}
