package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"explorer/internal/blobstore"
	"explorer/internal/docsplitter"
	"explorer/internal/docstore"
	"explorer/internal/openaiws"
	"explorer/internal/threaddocstore"
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

func TestExecuteReusesThreadDocumentSession(t *testing.T) {
	ctx := context.Background()
	blob := newTestBlobStore(t)

	doc := writeReadyTestDocument(t, ctx, blob, "doc_1", "report.pdf")

	conn := &fakeDocConn{
		reads: [][]byte{
			[]byte(`{"type":"response.created","response":{"id":"resp_warm"}}`),
			[]byte(`{"type":"response.completed","response_id":"resp_warm"}`),
			[]byte(`{"type":"response.created","response":{"id":"resp_query_1"}}`),
			[]byte(`{"type":"response.output_text.delta","delta":"first"}`),
			[]byte(`{"type":"response.completed","response_id":"resp_query_1"}`),
			[]byte(`{"type":"response.created","response":{"id":"resp_query_2"}}`),
			[]byte(`{"type":"response.output_text.delta","delta":"second"}`),
			[]byte(`{"type":"response.completed","response_id":"resp_query_2"}`),
		},
	}
	dialer := &countingDocDialer{conn: conn}

	docs := &fakeExecDocStore{
		documents: map[string]docstore.Document{
			doc.ID: doc,
		},
	}
	threadDocs := &fakeExecThreadDocStore{
		lineage: map[string]string{},
	}

	exec := newDocumentExec(documentExecConfig{
		Blob: blob,
		SessionFactory: func() *openaiws.Session {
			return openaiws.NewSession(testOpenAIWSConfig(), dialer)
		},
		Docs:           docs,
		ThreadDocs:     threadDocs,
		SessionIdleTTL: time.Hour,
		SessionMaxTTL:  time.Hour,
		MaxParallel:    2,
	})

	first := exec.Execute(ctx, docExecRequest{
		ThreadID:    "thread_1",
		DocumentIDs: []string{doc.ID},
		Task:        "Summarize the document.",
		Model:       "gpt-5.4",
	})
	second := exec.Execute(ctx, docExecRequest{
		ThreadID:    "thread_1",
		DocumentIDs: []string{doc.ID},
		Task:        "What changed in the follow-up?",
		Model:       "gpt-5.4",
	})

	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("unexpected result lengths: first=%d second=%d", len(first), len(second))
	}
	if first[0].Answer != "first" || first[0].ResponseID != "resp_query_1" {
		t.Fatalf("first result = %#v, want answer=first response_id=resp_query_1", first[0])
	}
	if second[0].Answer != "second" || second[0].ResponseID != "resp_query_2" {
		t.Fatalf("second result = %#v, want answer=second response_id=resp_query_2", second[0])
	}
	if got := dialer.Dials(); got != 1 {
		t.Fatalf("dial count = %d, want 1 reused session", got)
	}
	if got := threadDocs.Lineage("thread_1", doc.ID); got != "resp_query_2" {
		t.Fatalf("stored lineage = %q, want %q", got, "resp_query_2")
	}
	if got := threadDocs.LineageModel("thread_1", doc.ID); got != "gpt-5.4" {
		t.Fatalf("stored lineage model = %q, want %q", got, "gpt-5.4")
	}
	if got := docs.Document(doc.ID).BaseResponseID; got != "resp_warm" {
		t.Fatalf("base_response_id = %q, want %q", got, "resp_warm")
	}

	if len(conn.writes) != 3 {
		t.Fatalf("writes = %d, want 3 (warmup + 2 queries)", len(conn.writes))
	}

	warmPayload := decodeSentResponseCreateAt(t, conn, 0)
	if got := warmPayload["generate"]; got != false {
		t.Fatalf("warmup generate = %v, want false", got)
	}

	firstQueryPayload := decodeSentResponseCreateAt(t, conn, 1)
	if got := firstQueryPayload["previous_response_id"]; got != "resp_warm" {
		t.Fatalf("first query previous_response_id = %v, want %q", got, "resp_warm")
	}

	secondQueryPayload := decodeSentResponseCreateAt(t, conn, 2)
	if got := secondQueryPayload["previous_response_id"]; got != "resp_query_1" {
		t.Fatalf("second query previous_response_id = %v, want %q", got, "resp_query_1")
	}
}

func TestExecuteUsesDocumentQueryModel(t *testing.T) {
	ctx := context.Background()
	blob := newTestBlobStore(t)

	doc := writeReadyTestDocument(t, ctx, blob, "doc_1", "report.pdf")
	doc.QueryModel = "gpt-5.4-mini"

	conn := &fakeDocConn{
		reads: [][]byte{
			[]byte(`{"type":"response.created","response":{"id":"resp_warm"}}`),
			[]byte(`{"type":"response.completed","response_id":"resp_warm"}`),
			[]byte(`{"type":"response.created","response":{"id":"resp_query"}}`),
			[]byte(`{"type":"response.output_text.delta","delta":"mini"}`),
			[]byte(`{"type":"response.completed","response_id":"resp_query"}`),
		},
	}

	exec := newDocumentExec(documentExecConfig{
		Blob: blob,
		SessionFactory: func() *openaiws.Session {
			return openaiws.NewSession(testOpenAIWSConfig(), &fakeDocDialer{conn: conn})
		},
		Docs: &fakeExecDocStore{
			documents: map[string]docstore.Document{
				doc.ID: doc,
			},
		},
		ThreadDocs:     &fakeExecThreadDocStore{lineage: map[string]string{}, models: map[string]string{}},
		SessionIdleTTL: time.Hour,
		SessionMaxTTL:  time.Hour,
		MaxParallel:    1,
	})

	results := exec.Execute(ctx, docExecRequest{
		ThreadID:    "thread_1",
		DocumentIDs: []string{doc.ID},
		Task:        "Summarize the document.",
		Model:       "gpt-5.4",
	})

	if len(results) != 1 || results[0].Error != "" {
		t.Fatalf("unexpected results = %#v", results)
	}

	warmPayload := decodeSentResponseCreateAt(t, conn, 0)
	if got := warmPayload["model"]; got != "gpt-5.4-mini" {
		t.Fatalf("warmup model = %v, want %q", got, "gpt-5.4-mini")
	}

	queryPayload := decodeSentResponseCreateAt(t, conn, 1)
	if got := queryPayload["model"]; got != "gpt-5.4-mini" {
		t.Fatalf("query model = %v, want %q", got, "gpt-5.4-mini")
	}
}

func TestExecuteKeepsExistingThreadLineageModelAfterDocumentModelChange(t *testing.T) {
	ctx := context.Background()
	blob := newTestBlobStore(t)

	doc := writeReadyTestDocument(t, ctx, blob, "doc_1", "report.pdf")
	doc.QueryModel = "gpt-5.4-nano"

	conn := &fakeDocConn{
		reads: [][]byte{
			[]byte(`{"type":"response.created","response":{"id":"resp_query"}}`),
			[]byte(`{"type":"response.output_text.delta","delta":"still-mini"}`),
			[]byte(`{"type":"response.completed","response_id":"resp_query"}`),
		},
	}

	threadDocs := &fakeExecThreadDocStore{
		lineage: map[string]string{
			threadDocKey("thread_1", doc.ID): "resp_prev",
		},
		models: map[string]string{
			threadDocKey("thread_1", doc.ID): "gpt-5.4-mini",
		},
	}

	exec := newDocumentExec(documentExecConfig{
		Blob: blob,
		SessionFactory: func() *openaiws.Session {
			return openaiws.NewSession(testOpenAIWSConfig(), &fakeDocDialer{conn: conn})
		},
		Docs: &fakeExecDocStore{
			documents: map[string]docstore.Document{
				doc.ID: doc,
			},
		},
		ThreadDocs:     threadDocs,
		SessionIdleTTL: time.Hour,
		SessionMaxTTL:  time.Hour,
		MaxParallel:    1,
	})

	results := exec.Execute(ctx, docExecRequest{
		ThreadID:    "thread_1",
		DocumentIDs: []string{doc.ID},
		Task:        "Continue the existing conversation.",
		Model:       "gpt-5.4",
	})

	if len(results) != 1 || results[0].Error != "" {
		t.Fatalf("unexpected results = %#v", results)
	}

	queryPayload := decodeSentResponseCreateAt(t, conn, 0)
	if got := queryPayload["model"]; got != "gpt-5.4-mini" {
		t.Fatalf("query model = %v, want %q", got, "gpt-5.4-mini")
	}
	if got := queryPayload["previous_response_id"]; got != "resp_prev" {
		t.Fatalf("previous_response_id = %v, want %q", got, "resp_prev")
	}
}

func TestExecuteQueriesDocumentsInParallel(t *testing.T) {
	ctx := context.Background()
	blob := newTestBlobStore(t)

	doc1 := writeReadyTestDocument(t, ctx, blob, "doc_1", "doc-1.pdf")
	doc2 := writeReadyTestDocument(t, ctx, blob, "doc_2", "doc-2.pdf")

	conn1 := &delayedDocConn{
		fakeDocConn: &fakeDocConn{
			reads: [][]byte{
				[]byte(`{"type":"response.created","response":{"id":"resp_doc_1"}}`),
				[]byte(`{"type":"response.output_text.delta","delta":"alpha"}`),
				[]byte(`{"type":"response.completed","response_id":"resp_doc_1"}`),
			},
		},
		delay: 150 * time.Millisecond,
	}
	conn2 := &delayedDocConn{
		fakeDocConn: &fakeDocConn{
			reads: [][]byte{
				[]byte(`{"type":"response.created","response":{"id":"resp_doc_2"}}`),
				[]byte(`{"type":"response.output_text.delta","delta":"beta"}`),
				[]byte(`{"type":"response.completed","response_id":"resp_doc_2"}`),
			},
		},
		delay: 150 * time.Millisecond,
	}
	dialer := &queueDocDialer{
		conns: []openaiws.Conn{conn1, conn2},
	}

	exec := newDocumentExec(documentExecConfig{
		Blob: blob,
		SessionFactory: func() *openaiws.Session {
			return openaiws.NewSession(testOpenAIWSConfig(), dialer)
		},
		Docs: &fakeExecDocStore{
			documents: map[string]docstore.Document{
				doc1.ID: doc1,
				doc2.ID: doc2,
			},
		},
		ThreadDocs: &fakeExecThreadDocStore{
			lineage: map[string]string{
				threadDocKey("thread_parallel", doc1.ID): "resp_prev_1",
				threadDocKey("thread_parallel", doc2.ID): "resp_prev_2",
			},
		},
		SessionIdleTTL: time.Hour,
		SessionMaxTTL:  time.Hour,
		MaxParallel:    2,
	})

	start := time.Now()
	results := exec.Execute(ctx, docExecRequest{
		ThreadID:    "thread_parallel",
		DocumentIDs: []string{doc1.ID, doc2.ID},
		Task:        "Summarize the attached document.",
		Model:       "gpt-5.4",
	})
	elapsed := time.Since(start)

	if elapsed >= 250*time.Millisecond {
		t.Fatalf("Execute() took %s, want parallel execution under 250ms", elapsed)
	}
	if len(results) != 2 {
		t.Fatalf("results length = %d, want 2", len(results))
	}
	if results[0].DocumentID != doc1.ID || results[1].DocumentID != doc2.ID {
		t.Fatalf("results document order = %#v, want [%s %s]", results, doc1.ID, doc2.ID)
	}
	gotAnswers := map[string]bool{
		results[0].Answer: true,
		results[1].Answer: true,
	}
	if !gotAnswers["alpha"] || !gotAnswers["beta"] {
		t.Fatalf("answers = %#v, want alpha and beta", results)
	}
	if got := dialer.Dials(); got != 2 {
		t.Fatalf("dial count = %d, want 2", got)
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

func decodeSentResponseCreateAt(t *testing.T, conn *fakeDocConn, index int) map[string]any {
	t.Helper()

	if len(conn.writes) <= index {
		t.Fatalf("writes = %d, want index %d", len(conn.writes), index)
	}

	var payload map[string]any
	if err := json.Unmarshal(conn.writes[index], &payload); err != nil {
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

type countingDocDialer struct {
	mu    sync.Mutex
	conn  openaiws.Conn
	dials int
}

func (d *countingDocDialer) Dial(context.Context, openaiws.DialRequest) (openaiws.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dials++
	return d.conn, nil
}

func (d *countingDocDialer) Dials() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dials
}

type queueDocDialer struct {
	mu    sync.Mutex
	conns []openaiws.Conn
	dials int
}

func (d *queueDocDialer) Dial(context.Context, openaiws.DialRequest) (openaiws.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if len(d.conns) == 0 {
		return nil, fmt.Errorf("no queued doc connections")
	}

	conn := d.conns[0]
	d.conns = d.conns[1:]
	d.dials++
	return conn, nil
}

func (d *queueDocDialer) Dials() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dials
}

type delayedDocConn struct {
	*fakeDocConn
	delay     time.Duration
	readCount int
	mu        sync.Mutex
}

func (c *delayedDocConn) Read(ctx context.Context) ([]byte, error) {
	c.mu.Lock()
	shouldDelay := c.readCount == 0 && c.delay > 0
	c.readCount++
	c.mu.Unlock()

	if shouldDelay {
		time.Sleep(c.delay)
	}

	return c.fakeDocConn.Read(ctx)
}

type fakeExecDocStore struct {
	mu        sync.Mutex
	documents map[string]docstore.Document
}

func (s *fakeExecDocStore) Get(_ context.Context, id string) (docstore.Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	doc, ok := s.documents[id]
	if !ok {
		return docstore.Document{}, docstore.ErrDocumentNotFound
	}
	return doc, nil
}

func (s *fakeExecDocStore) UpdateBaseLineage(_ context.Context, id, baseResponseID, baseModel string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	doc, ok := s.documents[id]
	if !ok {
		return docstore.ErrDocumentNotFound
	}
	doc.BaseResponseID = baseResponseID
	doc.BaseModel = baseModel
	s.documents[id] = doc
	return nil
}

func (s *fakeExecDocStore) Document(id string) docstore.Document {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.documents[id]
}

type fakeExecThreadDocStore struct {
	mu      sync.Mutex
	lineage map[string]string
	models  map[string]string
}

func (s *fakeExecThreadDocStore) GetLineage(_ context.Context, threadID, documentID string) (threaddocstore.DocumentLineage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := threadDocKey(threadID, documentID)
	return threaddocstore.DocumentLineage{
		ResponseID: s.lineage[key],
		Model:      s.models[key],
	}, nil
}

func (s *fakeExecThreadDocStore) UpdateLineage(_ context.Context, threadID, documentID, responseID, model string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lineage == nil {
		s.lineage = map[string]string{}
	}
	if s.models == nil {
		s.models = map[string]string{}
	}
	key := threadDocKey(threadID, documentID)
	s.lineage[key] = responseID
	s.models[key] = model
	return nil
}

func (s *fakeExecThreadDocStore) Lineage(threadID, documentID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lineage[threadDocKey(threadID, documentID)]
}

func (s *fakeExecThreadDocStore) LineageModel(threadID, documentID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.models[threadDocKey(threadID, documentID)]
}

func threadDocKey(threadID, documentID string) string {
	return threadID + "::" + documentID
}

func writeReadyTestDocument(t *testing.T, ctx context.Context, blob *blobstore.LocalStore, docID, filename string) docstore.Document {
	t.Helper()

	imageRef := blob.Ref("documents", docID, "pages", "page-0001.png")
	if err := blob.WriteRef(ctx, imageRef, []byte("png")); err != nil {
		t.Fatalf("WriteRef(image) error = %v", err)
	}

	manifest := docsplitter.Manifest{
		DocumentID: docID,
		Filename:   filename,
		PageCount:  1,
		Pages: []docsplitter.PageEntry{
			{PageNumber: 1, ImageRef: imageRef, ContentType: "image/png"},
		},
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("json.Marshal(manifest) error = %v", err)
	}

	manifestRef := blob.Ref("documents", docID, "manifest.json")
	if err := blob.WriteRef(ctx, manifestRef, manifestJSON); err != nil {
		t.Fatalf("WriteRef(manifest) error = %v", err)
	}

	return docstore.Document{
		ID:          docID,
		Filename:    filename,
		ManifestRef: manifestRef,
		Status:      "ready",
		QueryModel:  "gpt-5.4",
	}
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
