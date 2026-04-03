package openaiws

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestNewResponseCreateEvent(t *testing.T) {
	event, err := NewResponseCreateEvent("evt_123", map[string]any{
		"model": "gpt-5.4",
		"input": "hello",
	})
	if err != nil {
		t.Fatalf("NewResponseCreateEvent() error = %v", err)
	}

	payload, err := event.Bytes()
	if err != nil {
		t.Fatalf("event.Bytes() error = %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	if got := decoded["type"]; got != string(EventTypeResponseCreate) {
		t.Fatalf("type = %v, want %q", got, EventTypeResponseCreate)
	}
	if _, exists := decoded["event_id"]; exists {
		t.Fatalf("event_id should not be sent over the websocket, got %#v", decoded["event_id"])
	}

	if got := decoded["model"]; got != "gpt-5.4" {
		t.Fatalf("model = %v, want %q", got, "gpt-5.4")
	}
}

func TestDecodeServerEvent(t *testing.T) {
	event, err := DecodeServerEvent([]byte(`{"type":"response.output_text.delta","response_id":"resp_123","delta":"hello"}`))
	if err != nil {
		t.Fatalf("DecodeServerEvent() error = %v", err)
	}

	if event.Type != EventTypeResponseOutputTextDelta {
		t.Fatalf("Type = %q, want %q", event.Type, EventTypeResponseOutputTextDelta)
	}

	if event.ResponseID != "resp_123" {
		t.Fatalf("ResponseID = %q, want %q", event.ResponseID, "resp_123")
	}

	if got := string(event.Field("delta")); got != `"hello"` {
		t.Fatalf("delta field = %s, want %q", got, `"hello"`)
	}

	if event.Type.IsTerminal() {
		t.Fatalf("output_text.delta should not be terminal")
	}
}

func TestSessionConnectSendReceiveClose(t *testing.T) {
	conn := &fakeConn{
		reads: [][]byte{
			[]byte(`{"type":"response.completed","response_id":"resp_123"}`),
		},
	}
	dialer := &fakeDialer{conn: conn}

	session := NewSession(Config{
		APIKey:             "test-key",
		BaseURL:            "https://api.openai.com/v1",
		ResponsesSocketURL: "wss://api.openai.com/v1/responses",
		DialTimeout:        testTimeout,
		ReadTimeout:        testTimeout,
		WriteTimeout:       testTimeout,
		PingInterval:       testTimeout,
		MaxMessageBytes:    1024,
	}, dialer)

	if err := session.Connect(context.Background()); err != nil {
		t.Fatalf("Connect() error = %v", err)
	}

	if got := dialer.request.Header.Get("Authorization"); got != "Bearer test-key" {
		t.Fatalf("Authorization header = %q, want %q", got, "Bearer test-key")
	}

	event, err := NewResponseCreateEvent("evt_123", map[string]any{"model": "gpt-5.4"})
	if err != nil {
		t.Fatalf("NewResponseCreateEvent() error = %v", err)
	}

	if err := session.Send(context.Background(), event); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	if len(conn.writes) != 1 {
		t.Fatalf("writes = %d, want 1", len(conn.writes))
	}

	inbound, err := session.Receive(context.Background())
	if err != nil {
		t.Fatalf("Receive() error = %v", err)
	}

	if inbound.Type != EventTypeResponseCompleted {
		t.Fatalf("Receive().Type = %q, want %q", inbound.Type, EventTypeResponseCompleted)
	}

	if err := session.Ping(context.Background()); err != nil {
		t.Fatalf("Ping() error = %v", err)
	}

	if conn.pings != 1 {
		t.Fatalf("pings = %d, want 1", conn.pings)
	}

	snapshot := session.Snapshot()
	if snapshot.SocketGeneration != 1 {
		t.Fatalf("SocketGeneration = %d, want 1", snapshot.SocketGeneration)
	}

	if snapshot.State != SessionStateConnected {
		t.Fatalf("State = %q, want %q", snapshot.State, SessionStateConnected)
	}

	if err := session.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	if !conn.closed {
		t.Fatalf("connection was not closed")
	}

	if conn.closeCode != CloseCodeNormal {
		t.Fatalf("close code = %d, want %d", conn.closeCode, CloseCodeNormal)
	}
}

func TestSessionSendDisconnectsOnTransportError(t *testing.T) {
	conn := &fakeConn{writeErr: errors.New("boom")}
	dialer := &fakeDialer{conn: conn}

	session := NewSession(Config{
		APIKey:             "test-key",
		BaseURL:            "https://api.openai.com/v1",
		ResponsesSocketURL: "wss://api.openai.com/v1/responses",
		DialTimeout:        testTimeout,
		ReadTimeout:        testTimeout,
		WriteTimeout:       testTimeout,
		PingInterval:       testTimeout,
		MaxMessageBytes:    1024,
	}, dialer)

	if err := session.Connect(context.Background()); err != nil {
		t.Fatalf("Connect() error = %v", err)
	}

	event, err := NewResponseCreateEvent("evt_123", map[string]any{"model": "gpt-5.4"})
	if err != nil {
		t.Fatalf("NewResponseCreateEvent() error = %v", err)
	}

	if err := session.Send(context.Background(), event); err == nil {
		t.Fatalf("Send() error = nil, want error")
	}

	if got := session.Snapshot().State; got != SessionStateDisconnected {
		t.Fatalf("State = %q, want %q", got, SessionStateDisconnected)
	}
}

const testTimeout = 50 * time.Millisecond

type fakeDialer struct {
	conn    Conn
	request DialRequest
}

func (d *fakeDialer) Dial(_ context.Context, req DialRequest) (Conn, error) {
	d.request = req
	return d.conn, nil
}

type fakeConn struct {
	mu          sync.Mutex
	reads       [][]byte
	writes      [][]byte
	writeErr    error
	pings       int
	closed      bool
	closeCode   CloseCode
	closeReason string
	closedCh    chan struct{}
}

func (c *fakeConn) Read(_ context.Context) ([]byte, error) {
	c.mu.Lock()
	if c.closedCh == nil {
		c.closedCh = make(chan struct{})
	}
	if len(c.reads) > 0 {
		payload := append([]byte(nil), c.reads[0]...)
		c.reads = c.reads[1:]
		c.mu.Unlock()
		return payload, nil
	}
	closedCh := c.closedCh
	c.mu.Unlock()

	<-closedCh
	return nil, context.Canceled
}

func (c *fakeConn) Write(_ context.Context, payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.writeErr != nil {
		return c.writeErr
	}
	c.writes = append(c.writes, append([]byte(nil), payload...))
	return nil
}

func (c *fakeConn) Ping(_ context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pings++
	return nil
}

func (c *fakeConn) Close(code CloseCode, reason string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.closed = true
	c.closeCode = code
	c.closeReason = reason
	if c.closedCh == nil {
		c.closedCh = make(chan struct{})
	}
	select {
	case <-c.closedCh:
	default:
		close(c.closedCh)
	}
	return nil
}
