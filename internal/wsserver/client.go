package wsserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"explorer/internal/eventrelay"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

const (
	heartbeatInterval = 20 * time.Second
	writeTimeout      = 10 * time.Second
)

var wsOriginPatterns = []string{
	"localhost:*",
	"127.0.0.1:*",
	"[::1]:*",
}

type clientConfig struct {
	logger *slog.Logger
}

type client struct {
	cfg clientConfig

	mu        sync.Mutex
	ctx       context.Context
	cancel    context.CancelFunc
	conn      *websocket.Conn
	closeOnce sync.Once
}

type clientPayloadError struct {
	err error
}

func (e clientPayloadError) Error() string {
	return e.err.Error()
}

func (e clientPayloadError) Unwrap() error {
	return e.err
}

func newClient(cfg clientConfig) *client {
	return &client{cfg: cfg}
}

func (c *client) serve(w http.ResponseWriter, r *http.Request, hub *eventHub) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: wsOriginPatterns,
	})
	if err != nil {
		c.cfg.logger.Warn("failed to accept websocket", "error", err)
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	c.ctx = ctx
	c.cancel = cancel
	c.conn = conn
	defer c.close(websocket.StatusNormalClosure, "stream closed")

	unregister := hub.register(c)
	defer unregister()

	c.run()
}

func (c *client) run() {
	heartbeatTicker := time.NewTicker(heartbeatInterval)
	defer heartbeatTicker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-heartbeatTicker.C:
			if err := c.writeHeartbeat(); err != nil {
				c.logStreamError(err)
				return
			}
		}
	}
}

func (c *client) handleEvent(f eventrelay.Frame) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	trimmed := bytes.TrimSpace(f.Payload)
	if len(trimmed) < 2 || trimmed[0] != '{' {
		return clientPayloadError{err: fmt.Errorf("expected JSON object payload")}
	}

	return c.writeRawLocked(trimmed)
}

func (c *client) writeHeartbeat() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.writeJSONLocked(map[string]any{
		"type": "thread.heartbeat",
		"time": time.Now().UTC().Format(time.RFC3339),
	})
}

func (c *client) writeJSONLocked(payload any) error {
	writeCtx, cancel := context.WithTimeout(c.ctx, writeTimeout)
	defer cancel()
	return wsjson.Write(writeCtx, c.conn, payload)
}

func (c *client) writeRawLocked(data []byte) error {
	writeCtx, cancel := context.WithTimeout(c.ctx, writeTimeout)
	defer cancel()
	return c.conn.Write(writeCtx, websocket.MessageText, data)
}

func (c *client) close(code websocket.StatusCode, reason string) {
	c.closeOnce.Do(func() {
		if c.cancel != nil {
			c.cancel()
		}
		if c.conn != nil {
			_ = c.conn.Close(code, reason)
		}
	})
}

func (c *client) logStreamError(err error) {
	status := websocket.CloseStatus(err)
	if status == websocket.StatusNormalClosure || status == websocket.StatusGoingAway {
		return
	}
	if errors.Is(err, context.Canceled) {
		return
	}
	c.cfg.logger.Warn("websocket stream error", "error", err)
}

func websocketCloseStatus(err error) websocket.StatusCode {
	status := websocket.CloseStatus(err)
	if status != -1 {
		return status
	}
	if errors.Is(err, context.Canceled) {
		return websocket.StatusGoingAway
	}
	return websocket.StatusInternalError
}
