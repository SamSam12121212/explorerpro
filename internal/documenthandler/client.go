package documenthandler

import (
	"context"
	"fmt"

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
		return doccmd.PrepareInputResponse{}, err
	}

	return resp, nil
}
