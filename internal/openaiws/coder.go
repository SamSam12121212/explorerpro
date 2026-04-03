package openaiws

import (
	"context"
	"fmt"
	"net/http"

	coderws "github.com/coder/websocket"
)

type CoderDialer struct {
	httpClient      *http.Client
	maxMessageBytes int64
}

func NewCoderDialer(cfg Config) *CoderDialer {
	return &CoderDialer{
		httpClient: &http.Client{
			Timeout: cfg.DialTimeout,
		},
		maxMessageBytes: cfg.MaxMessageBytes,
	}
}

func (d *CoderDialer) Dial(ctx context.Context, req DialRequest) (Conn, error) {
	conn, _, err := coderws.Dial(ctx, req.URL, &coderws.DialOptions{
		HTTPClient:   d.httpClient,
		HTTPHeader:   req.Header,
		Subprotocols: nil,
	})
	if err != nil {
		return nil, fmt.Errorf("coder websocket dial: %w", err)
	}

	conn.SetReadLimit(d.maxMessageBytes)

	return &coderConn{conn: conn}, nil
}

type coderConn struct {
	conn *coderws.Conn
}

func (c *coderConn) Read(ctx context.Context) ([]byte, error) {
	messageType, payload, err := c.conn.Read(ctx)
	if err != nil {
		return nil, err
	}

	if messageType != coderws.MessageText {
		return nil, fmt.Errorf("unexpected websocket message type: %v", messageType)
	}

	return payload, nil
}

func (c *coderConn) Write(ctx context.Context, payload []byte) error {
	return c.conn.Write(ctx, coderws.MessageText, payload)
}

func (c *coderConn) Ping(ctx context.Context) error {
	return c.conn.Ping(ctx)
}

func (c *coderConn) Close(code CloseCode, reason string) error {
	return c.conn.Close(coderws.StatusCode(code), reason)
}
