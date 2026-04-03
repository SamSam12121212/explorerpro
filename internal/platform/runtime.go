package platform

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"explorer/internal/blobstore"
	"explorer/internal/config"
	"explorer/internal/natsclient"
	"explorer/internal/postgresclient"
	"explorer/internal/redisclient"
)

type Runtime struct {
	cfg      config.Config
	logger   *slog.Logger
	blob     *blobstore.LocalStore
	nats     *natsclient.Client
	redis    *redisclient.Client
	postgres *postgresclient.Client
}

type DependencyStatus struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

type HealthSnapshot struct {
	Service      string                      `json:"service"`
	Status       string                      `json:"status"`
	Env          string                      `json:"env"`
	Time         string                      `json:"time"`
	BlobRoot     string                      `json:"blob_root"`
	Dependencies map[string]DependencyStatus `json:"dependencies"`
}

func New(ctx context.Context, cfg config.Config, logger *slog.Logger) (*Runtime, error) {
	blob, err := blobstore.NewLocal(cfg.Blob.StorageDir)
	if err != nil {
		return nil, err
	}

	natsClient, err := natsclient.New(ctx, cfg.NATS)
	if err != nil {
		return nil, err
	}

	redisClient, err := redisclient.New(ctx, cfg.Redis)
	if err != nil {
		_ = natsClient.Close()
		return nil, err
	}

	postgresClient, err := postgresclient.New(ctx, cfg.Postgres)
	if err != nil {
		_ = redisClient.Close()
		_ = natsClient.Close()
		return nil, err
	}

	return &Runtime{
		cfg:      cfg,
		logger:   logger,
		blob:     blob,
		nats:     natsClient,
		redis:    redisClient,
		postgres: postgresClient,
	}, nil
}

func (r *Runtime) Close() error {
	var errs []error

	if r.postgres != nil {
		errs = append(errs, r.postgres.Close())
	}

	if r.redis != nil {
		errs = append(errs, r.redis.Close())
	}

	if r.nats != nil {
		errs = append(errs, r.nats.Close())
	}

	return errors.Join(errs...)
}

func (r *Runtime) Health(ctx context.Context) HealthSnapshot {
	statuses := map[string]DependencyStatus{
		"blob":     probe(ctx, r.blob.Ping),
		"nats":     probe(ctx, r.nats.Ping),
		"redis":    probe(ctx, r.redis.Ping),
		"postgres": probe(ctx, r.postgres.Ping),
	}

	overall := "ok"
	for _, dependency := range statuses {
		if dependency.Status != "ok" {
			overall = "degraded"
			break
		}
	}

	return HealthSnapshot{
		Service:      r.cfg.ServiceName,
		Status:       overall,
		Env:          r.cfg.AppEnv,
		Time:         time.Now().UTC().Format(time.RFC3339),
		BlobRoot:     r.blob.Root(),
		Dependencies: statuses,
	}
}

func probe(ctx context.Context, fn func(context.Context) error) DependencyStatus {
	if err := fn(ctx); err != nil {
		return DependencyStatus{
			Status: "error",
			Error:  err.Error(),
		}
	}

	return DependencyStatus{Status: "ok"}
}

func (r *Runtime) Logger() *slog.Logger {
	return r.logger
}

func (r *Runtime) BlobRoot() string {
	return r.blob.Root()
}

func (r *Runtime) Blob() *blobstore.LocalStore {
	return r.blob
}

func (r *Runtime) NATS() *natsclient.Client {
	return r.nats
}

func (r *Runtime) Redis() *redisclient.Client {
	return r.redis
}

func (r *Runtime) Postgres() *postgresclient.Client {
	return r.postgres
}
