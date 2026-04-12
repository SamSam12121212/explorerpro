package documenthandler

import (
	"context"
	"fmt"
	"log/slog"

	"explorer/internal/blobstore"
	"explorer/internal/doccmd"
	"explorer/internal/docstore"

	"github.com/nats-io/nats.go"
)

// Service currently exists only as a lifecycle/dependency shell for
// document-adjacent backend work. It is intentionally not the OpenAI
// document executor; worker-owned document execution lives in the worker.
type Service struct {
	logger *slog.Logger
	nc     *nats.Conn
	js     nats.JetStreamContext
	docs   *docstore.Store
	blob   *blobstore.LocalStore
}

func New(logger *slog.Logger, nc *nats.Conn, js nats.JetStreamContext, docs *docstore.Store, blob *blobstore.LocalStore) *Service {
	return &Service{
		logger: logger,
		nc:     nc,
		js:     js,
		docs:   docs,
		blob:   blob,
	}
}

func (s *Service) Run(ctx context.Context) error {
	if s.nc == nil {
		return fmt.Errorf("document handler nats connection is required")
	}

	ch := make(chan *nats.Msg, 64)
	sub, err := s.nc.ChanQueueSubscribe(doccmd.PrepareInputSubject, doccmd.PrepareInputQueue, ch)
	if err != nil {
		return fmt.Errorf("subscribe %s: %w", doccmd.PrepareInputSubject, err)
	}

	s.logger.Info("document handler service started", "subject", doccmd.PrepareInputSubject)

	for {
		select {
		case <-ctx.Done():
			_ = sub.Drain()
			s.logger.Info("document handler service stopping", "reason", ctx.Err())
			return nil
		case msg := <-ch:
			s.handlePrepareInput(msg)
		}
	}
}

func (s *Service) handlePrepareInput(msg *nats.Msg) {
	req, err := doccmd.DecodePrepareInputRequest(msg.Data)
	if err != nil {
		s.logger.Error("invalid prepare input request", "error", err)
		s.respondPrepareInput(msg, doccmd.PrepareInputResponse{
			RequestID: "unknown",
			Status:    doccmd.PrepareStatusError,
			Error:     err.Error(),
		})
		return
	}

	s.logger.Info("prepare input request received",
		"request_id", req.RequestID,
		"kind", req.Kind,
		"thread_id", req.ThreadID,
		"document_id", req.DocumentID,
	)

	s.respondPrepareInput(msg, doccmd.PrepareInputResponse{
		RequestID: req.RequestID,
		Status:    doccmd.PrepareStatusNoop,
		Error:     "document prepared-input materialization is not implemented yet",
	})
}

func (s *Service) respondPrepareInput(msg *nats.Msg, resp doccmd.PrepareInputResponse) {
	if msg == nil || msg.Reply == "" {
		s.logger.Warn("prepare input request has no reply subject",
			"request_id", resp.RequestID,
			"status", resp.Status,
			"error", resp.Error,
		)
		return
	}

	data, err := doccmd.EncodePrepareInputResponse(resp)
	if err != nil {
		s.logger.Error("failed to encode prepare input response",
			"request_id", resp.RequestID,
			"status", resp.Status,
			"error", err,
		)
		return
	}
	if err := msg.Respond(data); err != nil {
		s.logger.Error("failed to publish prepare input response",
			"request_id", resp.RequestID,
			"status", resp.Status,
			"error", err,
		)
	}
}
