package wsserver

import (
	"errors"
	"log/slog"
	"sync"

	"explorer/internal/eventrelay"
)

type eventHub struct {
	logger *slog.Logger

	mu            sync.RWMutex
	clients       map[int64]map[*client]struct{}
	globalClients map[*client]struct{}
}

func newEventHub(logger *slog.Logger) *eventHub {
	return &eventHub{
		logger:        logger,
		clients:       map[int64]map[*client]struct{}{},
		globalClients: map[*client]struct{}{},
	}
}

func (h *eventHub) HandleFrame(f eventrelay.Frame) {
	for _, recipient := range h.recipients(f.ThreadID) {
		if err := recipient.handleEvent(f); err != nil {
			var payloadErr clientPayloadError
			if errors.As(err, &payloadErr) {
				h.logger.Warn("failed to decode live thread payload", "thread_id", f.ThreadID, "event_type", f.EventType, "error", err)
				continue
			}
			recipient.logStreamError(err)
			recipient.close(websocketCloseStatus(err), "stream closed")
			h.unregister(f.ThreadID, recipient)
		}
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
