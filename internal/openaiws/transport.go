package openaiws

import (
	"context"
	"net/http"
)

type CloseCode int

const (
	CloseCodeNormal          CloseCode = 1000
	CloseCodePolicyViolation CloseCode = 1008
	CloseCodeInternalError   CloseCode = 1011
	CloseCodeServiceRestart  CloseCode = 1012
	CloseCodeTryAgainLater   CloseCode = 1013
	CloseCodeBadGateway      CloseCode = 1014
)

type DialRequest struct {
	URL    string
	Header http.Header
}

type Dialer interface {
	Dial(ctx context.Context, req DialRequest) (Conn, error)
}

type Conn interface {
	Read(ctx context.Context) ([]byte, error)
	Write(ctx context.Context, payload []byte) error
	Ping(ctx context.Context) error
	Close(code CloseCode, reason string) error
}
