package documenthandler

import (
	"context"
	"fmt"
	"strings"

	"explorer/internal/doccmd"

	"github.com/nats-io/nats.go"
)

type Client struct {
	nc *nats.Conn
}

func NewClient(nc *nats.Conn) *Client {
	return &Client{nc: nc}
}

func (c *Client) PrepareInput(ctx context.Context, req doccmd.PrepareInputRequest) (doccmd.PrepareInputResponse, error) {
	if c == nil || c.nc == nil {
		return doccmd.PrepareInputResponse{}, fmt.Errorf("document handler nats connection is required")
	}

	data, err := doccmd.EncodePrepareInputRequest(req)
	if err != nil {
		return doccmd.PrepareInputResponse{}, err
	}

	msg, err := c.nc.RequestWithContext(ctx, doccmd.PrepareInputSubject, data)
	if err != nil {
		return doccmd.PrepareInputResponse{}, fmt.Errorf("request prepared input: %w", err)
	}

	resp, err := doccmd.DecodePrepareInputResponse(msg.Data)
	if err != nil {
		return doccmd.PrepareInputResponse{}, fmt.Errorf("%w (body=%s)", err, summarizeResponseData(msg.Data))
	}

	return resp, nil
}

func (c *Client) RuntimeContext(ctx context.Context, req doccmd.RuntimeContextRequest) (doccmd.RuntimeContextResponse, error) {
	if c == nil || c.nc == nil {
		return doccmd.RuntimeContextResponse{}, fmt.Errorf("document handler nats connection is required")
	}

	data, err := doccmd.EncodeRuntimeContextRequest(req)
	if err != nil {
		return doccmd.RuntimeContextResponse{}, err
	}

	msg, err := c.nc.RequestWithContext(ctx, doccmd.RuntimeContextSubject, data)
	if err != nil {
		return doccmd.RuntimeContextResponse{}, fmt.Errorf("request runtime context: %w", err)
	}

	resp, err := doccmd.DecodeRuntimeContextResponse(msg.Data)
	if err != nil {
		return doccmd.RuntimeContextResponse{}, fmt.Errorf("%w (body=%s)", err, summarizeResponseData(msg.Data))
	}

	return resp, nil
}

func summarizeResponseData(data []byte) string {
	text := strings.TrimSpace(string(data))
	if text == "" {
		return `""`
	}
	const maxLen = 256
	if len(text) > maxLen {
		text = text[:maxLen] + "..."
	}
	return fmt.Sprintf("%q", text)
}
