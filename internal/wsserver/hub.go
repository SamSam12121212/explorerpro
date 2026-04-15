package wsserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"explorer/internal/threadevents"

	"github.com/nats-io/nats.go"
)

const wsEventAckWait = 30 * time.Second

type eventHub struct {
	logger *slog.Logger
	js     nats.JetStreamContext

	mu            sync.RWMutex
	clients       map[int64]map[*client]struct{}
	globalClients map[*client]struct{}
}

func newEventHub(logger *slog.Logger, js nats.JetStreamContext) *eventHub {
	return &eventHub{
		logger:        logger,
		js:            js,
		clients:       map[int64]map[*client]struct{}{},
		globalClients: map[*client]struct{}{},
	}
}

func (h *eventHub) Start(ctx context.Context) error {
	if h == nil || h.js == nil {
		return fmt.Errorf("ws event hub is not configured")
	}

	eventsCh := make(chan *nats.Msg, 256)
	if _, err := h.js.ChanSubscribe(
		threadevents.SubjectWildcard,
		eventsCh,
		nats.BindStream(threadevents.StreamName),
		nats.Durable(threadevents.ConsumerName),
		nats.ManualAck(),
		nats.AckExplicit(),
		nats.AckWait(wsEventAckWait),
		nats.DeliverAll(),
	); err != nil {
		return fmt.Errorf("subscribe thread events stream: %w", err)
	}

	go h.consume(ctx, eventsCh)
	return nil
}

func (h *eventHub) consume(ctx context.Context, eventsCh <-chan *nats.Msg) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-eventsCh:
			if !ok {
				return
			}
			h.handleMessage(msg)
		}
	}
}

func (h *eventHub) handleMessage(msg *nats.Msg) {
	env, err := threadevents.Decode(msg.Data)
	if err != nil {
		h.logger.Warn("failed to decode thread event envelope", "subject", msg.Subject, "error", err)
		h.ack(msg)
		return
	}

	for _, recipient := range h.recipients(env.ThreadID) {
		if err := recipient.handleEvent(env); err != nil {
			var payloadErr clientPayloadError
			if errors.As(err, &payloadErr) {
				h.logger.Warn("failed to decode live thread payload", "thread_id", env.ThreadID, "event_type", env.EventType, "error", err)
				continue
			}
			recipient.logStreamError(err)
			recipient.close(websocketCloseStatus(err), "stream closed")
			h.unregister(env.ThreadID, recipient)
		}
	}

	h.ack(msg)
}

func (h *eventHub) ack(msg *nats.Msg) {
	if err := msg.Ack(); err != nil {
		h.logger.Warn("failed to ack thread event", "subject", msg.Subject, "error", err)
	}
}

func (h *eventHub) register(threadID int64, c *client) func() {
	h.mu.Lock()
	defer h.mu.Unlock()

	if threadID <= 0 {
		h.globalClients[c] = struct{}{}
		return func() {
			h.unregister(threadID, c)
		}
	}

	if h.clients[threadID] == nil {
		h.clients[threadID] = map[*client]struct{}{}
	}
	h.clients[threadID][c] = struct{}{}

	return func() {
		h.unregister(threadID, c)
	}
}

func (h *eventHub) unregister(threadID int64, c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if threadID <= 0 {
		delete(h.globalClients, c)
		return
	}

	recipients := h.clients[threadID]
	if recipients == nil {
		return
	}
	delete(recipients, c)
	if len(recipients) == 0 {
		delete(h.clients, threadID)
	}
}

func (h *eventHub) recipients(threadID int64) []*client {
	h.mu.RLock()
	defer h.mu.RUnlock()

	recipients := h.clients[threadID]
	if len(recipients) == 0 && len(h.globalClients) == 0 {
		return nil
	}

	out := make([]*client, 0, len(recipients)+len(h.globalClients))
	for recipient := range recipients {
		out = append(out, recipient)
	}
	for recipient := range h.globalClients {
		out = append(out, recipient)
	}
	return out
}
