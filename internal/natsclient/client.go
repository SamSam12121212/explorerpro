package natsclient

import (
	"context"
	"fmt"
	"time"

	"explorer/internal/config"

	"github.com/nats-io/nats.go"
)

type Client struct {
	conn *nats.Conn
	js   nats.JetStreamContext
}

func New(ctx context.Context, cfg config.NATSConfig) (*Client, error) {
	connectCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()

	conn, err := nats.Connect(
		cfg.URL,
		nats.Name(cfg.ClientName),
		nats.Timeout(cfg.ConnectTimeout),
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2*time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("connect to nats: %w", err)
	}

	js, err := conn.JetStream()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("create jetstream context: %w", err)
	}

	client := &Client{
		conn: conn,
		js:   js,
	}

	if err := client.Ping(connectCtx); err != nil {
		conn.Close()
		return nil, err
	}

	return client, nil
}

func (c *Client) Ping(ctx context.Context) error {
	if err := c.conn.FlushWithContext(ctx); err != nil {
		return fmt.Errorf("flush nats connection: %w", err)
	}

	if err := c.conn.LastError(); err != nil {
		return fmt.Errorf("nats connection error: %w", err)
	}

	return nil
}

func (c *Client) Close() error {
	c.conn.Close()
	return nil
}

func (c *Client) Conn() *nats.Conn {
	return c.conn
}

func (c *Client) JetStream() nats.JetStreamContext {
	return c.js
}
