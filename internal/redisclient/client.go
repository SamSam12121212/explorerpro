package redisclient

import (
	"context"
	"fmt"

	"explorer/internal/config"

	"github.com/redis/go-redis/v9"
)

type Client struct {
	raw *redis.Client
}

func New(ctx context.Context, cfg config.RedisConfig) (*Client, error) {
	opts, err := redis.ParseURL(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}

	opts.DialTimeout = cfg.DialTimeout

	client := redis.NewClient(opts)

	pingCtx, cancel := context.WithTimeout(ctx, cfg.DialTimeout)
	defer cancel()

	wrapped := &Client{raw: client}
	if err := wrapped.Ping(pingCtx); err != nil {
		client.Close()
		return nil, err
	}

	return wrapped, nil
}

func (c *Client) Ping(ctx context.Context) error {
	if err := c.raw.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("ping redis: %w", err)
	}

	return nil
}

func (c *Client) Close() error {
	return c.raw.Close()
}

func (c *Client) Raw() *redis.Client {
	return c.raw
}
