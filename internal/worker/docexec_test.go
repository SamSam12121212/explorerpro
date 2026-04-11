package worker

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"explorer/internal/blobstore"
	"explorer/internal/docsplitter"
	"explorer/internal/docstore"
	"explorer/internal/openaiws"
)

func TestWarmDocumentSendsGenerateFalseWithoutInstructionsOrPrompt(t *testing.T) {
	ctx := context.Background()
	blob := newTestBlobStore(t)
	imageRef := blob.Ref("pages", "page-1.png")
	if err := blob.WriteRef(ctx, imageRef, []byte("png")); err != nil {
		t.Fatalf("WriteRef() error = %v", err)
	}

	conn := &fakeDocConn{
		reads: [][]byte{
			[]byte(`{"type":"response.created","response":{"id":"resp_warm"}}`),
			[]byte(`{"type":"response.completed","response_id":"resp_warm"}`),
		},
	}

	exec := &documentExec{
		blob: blob,
		sessionFactory: func() *openaiws.Session {
			return openaiws.NewSession(testOpenAIWSConfig(), &fakeDocDialer{conn: conn})
		},
	}

	gotID, err := exec.warmDocument(ctx, docstore.Document{
		ID:       "doc_123",
		Filename: "report.pdf",
	}, docsplitter.Manifest{
		PageCount: 1,
		Pages: []docsplitter.PageEntry{
			{PageNumber: 1, ImageRef: imageRef, ContentType: "image/png"},
		},
	}, "gpt-5.4")
	if err != nil {
		t.Fatalf("warmDocument() error = %v", err)
	}

	if gotID != "resp_warm" {
		t.Fatalf("warmDocument() response ID = %q, want %q", gotID, "resp_warm")
	}

	payload := decodeSentResponseCreate(t, conn)

	if got := payload["generate"]; got != false {
		t.Fatalf("generate = %v, want false", got)
	}
	if _, exists := payload["instructions"]; exists {
		t.Fatalf("instructions should be omitted, got %#v", payload["instructions"])
	}

	content := decodeSingleMessageContent(t, payload)
	if len(content) != 5 {
		t.Fatalf("content length = %d, want 5", len(content))
	}

	const removedPrompt = "The document above has been loaded. Confirm receipt and readiness."
	for _, item := range content {
		itemMap, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("content item type = %T, want map[string]any", item)
		}
		if text, _ := itemMap["text"].(string); text == removedPrompt {
			t.Fatalf("warmup content still includes removed prompt")
		}
	}
}

func TestQueryDocumentOmitsInstructions(t *testing.T) {
	conn := &fakeDocConn{
		reads: [][]byte{
			[]byte(`{"type":"response.created","response":{"id":"resp_query"}}`),
			[]byte(`{"type":"response.output_text.delta","delta":"answer"}`),
			[]byte(`{"type":"response.completed","response_id":"resp_query"}`),
		},
	}

	exec := &documentExec{
		sessionFactory: func() *openaiws.Session {
			return openaiws.NewSession(testOpenAIWSConfig(), &fakeDocDialer{conn: conn})
		},
	}

	responseID, answer, err := exec.queryDocument(context.Background(), "resp_prev", "What is on page 1?", "gpt-5.4")
	if err != nil {
		t.Fatalf("queryDocument() error = %v", err)
	}

	if responseID != "resp_query" {
		t.Fatalf("queryDocument() response ID = %q, want %q", responseID, "resp_query")
	}
	if answer != "answer" {
		t.Fatalf("queryDocument() answer = %q, want %q", answer, "answer")
	}

	payload := decodeSentResponseCreate(t, conn)

	if got := payload["previous_response_id"]; got != "resp_prev" {
		t.Fatalf("previous_response_id = %v, want %q", got, "resp_prev")
	}
	if _, exists := payload["instructions"]; exists {
		t.Fatalf("instructions should be omitted, got %#v", payload["instructions"])
	}
	if _, exists := payload["generate"]; exists {
		t.Fatalf("generate should be omitted for document queries, got %#v", payload["generate"])
	}

	content := decodeSingleMessageContent(t, payload)
	if len(content) != 1 {
		t.Fatalf("content length = %d, want 1", len(content))
	}
	itemMap, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("content item type = %T, want map[string]any", content[0])
	}
	if got := itemMap["text"]; got != "What is on page 1?" {
		t.Fatalf("task text = %v, want %q", got, "What is on page 1?")
	}
}

func newTestBlobStore(t *testing.T) *blobstore.LocalStore {
	t.Helper()

	store, err := blobstore.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal() error = %v", err)
	}
	return store
}

func testOpenAIWSConfig() openaiws.Config {
	return openaiws.Config{
		APIKey:             "test-key",
		BaseURL:            "https://api.openai.com/v1",
		ResponsesSocketURL: "wss://api.openai.com/v1/responses",
		DialTimeout:        50 * time.Millisecond,
		ReadTimeout:        50 * time.Millisecond,
		WriteTimeout:       50 * time.Millisecond,
		PingInterval:       50 * time.Millisecond,
		MaxMessageBytes:    1024,
	}
}

func decodeSentResponseCreate(t *testing.T, conn *fakeDocConn) map[string]any {
	t.Helper()

	if len(conn.writes) != 1 {
		t.Fatalf("writes = %d, want 1", len(conn.writes))
	}

	var payload map[string]any
	if err := json.Unmarshal(conn.writes[0], &payload); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if got := payload["type"]; got != "response.create" {
		t.Fatalf("type = %v, want %q", got, "response.create")
	}
	return payload
}

func decodeSingleMessageContent(t *testing.T, payload map[string]any) []any {
	t.Helper()

	input, ok := payload["input"].([]any)
	if !ok {
		t.Fatalf("input type = %T, want []any", payload["input"])
	}
	if len(input) != 1 {
		t.Fatalf("input length = %d, want 1", len(input))
	}

	message, ok := input[0].(map[string]any)
	if !ok {
		t.Fatalf("message type = %T, want map[string]any", input[0])
	}
	content, ok := message["content"].([]any)
	if !ok {
		t.Fatalf("content type = %T, want []any", message["content"])
	}
	return content
}

type fakeDocDialer struct {
	conn openaiws.Conn
}

func (d *fakeDocDialer) Dial(context.Context, openaiws.DialRequest) (openaiws.Conn, error) {
	return d.conn, nil
}

type fakeDocConn struct {
	reads    [][]byte
	writes   [][]byte
	closed   bool
	closedCh chan struct{}
}

func (c *fakeDocConn) Read(ctx context.Context) ([]byte, error) {
	if len(c.reads) > 0 {
		payload := append([]byte(nil), c.reads[0]...)
		c.reads = c.reads[1:]
		return payload, nil
	}

	if c.closedCh == nil {
		c.closedCh = make(chan struct{})
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.closedCh:
		return nil, context.Canceled
	}
}

func (c *fakeDocConn) Write(_ context.Context, payload []byte) error {
	c.writes = append(c.writes, append([]byte(nil), payload...))
	return nil
}

func (c *fakeDocConn) Ping(context.Context) error {
	return nil
}

func (c *fakeDocConn) Close(openaiws.CloseCode, string) error {
	c.closed = true
	if c.closedCh != nil {
		close(c.closedCh)
		c.closedCh = nil
	}
	return nil
}
