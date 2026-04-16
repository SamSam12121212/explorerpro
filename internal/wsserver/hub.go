package wsserver

import (
	"errors"
	"log/slog"
	"sync"

	"explorer/internal/eventrelay"
)

type eventHub struct {
	logger *slog.Logger

	mu      sync.RWMutex
	clients map[*client]struct{}
}

func newEventHub(logger *slog.Logger) *eventHub {
	return &eventHub{
		logger:  logger,
		clients: map[*client]struct{}{},
	}
}

func (h *eventHub) HandleFrame(f eventrelay.Frame) {
	for _, recipient := range h.recipients() {
		if err := recipient.handleEvent(f); err != nil {
			var payloadErr clientPayloadError
			if errors.As(err, &payloadErr) {
				h.logger.Warn("failed to decode live event payload", "event_type", f.EventType, "error", err)
				continue
			}
			recipient.logStreamError(err)
			recipient.close(websocketCloseStatus(err), "stream closed")
			h.unregister(recipient)
		}
	}
}

func (h *eventHub) register(c *client) func() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clients[c] = struct{}{}
	return func() { h.unregister(c) }
}

func (h *eventHub) unregister(c *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.clients, c)
}

func (h *eventHub) recipients() []*client {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if len(h.clients) == 0 {
		return nil
	}
	out := make([]*client, 0, len(h.clients))
	for c := range h.clients {
		out = append(out, c)
	}
	return out
}
