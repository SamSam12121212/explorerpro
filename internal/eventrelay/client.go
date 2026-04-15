package eventrelay

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"
)

type Client struct {
	logger *slog.Logger
	addr   string

	mu   sync.Mutex
	conn net.Conn
	bw   *bufio.Writer
}

func NewClient(logger *slog.Logger, addr string) *Client {
	return &Client{
		logger: logger,
		addr:   addr,
	}
}

func (c *Client) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn != nil {
		return nil
	}

	return c.connectLocked(ctx)
}

func (c *Client) connectLocked(ctx context.Context) error {
	dialer := net.Dialer{Timeout: 5 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", c.addr)
	if err != nil {
		return fmt.Errorf("event relay connect to %s: %w", c.addr, err)
	}

	c.conn = conn
	c.bw = bufio.NewWriterSize(conn, 256*1024)
	c.logger.Info("event relay connected", "addr", c.addr)
	return nil
}

func (c *Client) Send(ctx context.Context, threadID int64, eventType string, payload json.RawMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil {
		if err := c.connectLocked(ctx); err != nil {
			return err
		}
	}

	err := WriteFrame(c.bw, Frame{
		ThreadID:  threadID,
		EventType: eventType,
		Payload:   payload,
	})
	if err != nil {
		c.closeLocked()
		return fmt.Errorf("event relay write: %w", err)
	}

	if err := c.bw.Flush(); err != nil {
		c.closeLocked()
		return fmt.Errorf("event relay flush: %w", err)
	}

	return nil
}

func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeLocked()
}

func (c *Client) closeLocked() error {
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	c.bw = nil
	return err
}
