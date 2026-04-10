package documenthandler

import (
	"context"
	"log/slog"

	"explorer/internal/blobstore"
	"explorer/internal/docstore"

	"github.com/nats-io/nats.go"
)

// Service is the future home for document-tool execution. For now it only
// owns lifecycle and dependency wiring so we can run it as a first-class
// backend service.
type Service struct {
	logger *slog.Logger
	js     nats.JetStreamContext
	docs   *docstore.Store
	blob   *blobstore.LocalStore
}

func New(logger *slog.Logger, js nats.JetStreamContext, docs *docstore.Store, blob *blobstore.LocalStore) *Service {
	return &Service{
		logger: logger,
		js:     js,
		docs:   docs,
		blob:   blob,
	}
}

func (s *Service) Run(ctx context.Context) error {
	s.logger.Info("document handler service started")

	<-ctx.Done()

	s.logger.Info("document handler service stopping", "reason", ctx.Err())
	return nil
}
