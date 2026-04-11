package worker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"explorer/internal/blobstore"
	"explorer/internal/docsplitter"
	"explorer/internal/docstore"
	"explorer/internal/openaiws"
	"explorer/internal/threadstore"
)

var defaultDocumentExecLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

type docExecDocStore interface {
	Get(ctx context.Context, id string) (docstore.Document, error)
	UpdateBaseLineage(ctx context.Context, id, baseResponseID, baseModel string) error
}

type docExecThreadDocStore interface {
	GetLineage(ctx context.Context, threadID, documentID string) (string, error)
	UpdateLineage(ctx context.Context, threadID, documentID, responseID string) error
}

type documentExecConfig struct {
	Logger         *slog.Logger
	Blob           *blobstore.LocalStore
	OpenAIConfig   openaiws.Config
	SessionFactory func() *openaiws.Session
	Docs           docExecDocStore
	ThreadDocs     docExecThreadDocStore
	SessionIdleTTL time.Duration
	SessionMaxTTL  time.Duration
	MaxParallel    int
	Now            func() time.Time
}

type documentExec struct {
	logger         *slog.Logger
	blob           *blobstore.LocalStore
	cfg            openaiws.Config
	sessionFactory func() *openaiws.Session
	docs           docExecDocStore
	threadDocs     docExecThreadDocStore
	sessionIdleTTL time.Duration
	sessionMaxTTL  time.Duration
	maxParallel    int
	now            func() time.Time

	sessionsMu sync.Mutex
	sessions   map[string]map[string]*documentSession
}

type documentSession struct {
	mu               sync.Mutex
	session          *openaiws.Session
	latestResponseID string
	connectedAt      time.Time
	lastUsedAt       time.Time
}

func newDocumentExec(cfg documentExecConfig) *documentExec {
	logger := cfg.Logger
	if logger == nil {
		logger = defaultDocumentExecLogger
	}
	now := cfg.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	maxParallel := cfg.MaxParallel
	if maxParallel <= 0 {
		maxParallel = 4
	}

	return &documentExec{
		logger:         logger,
		blob:           cfg.Blob,
		cfg:            cfg.OpenAIConfig,
		sessionFactory: cfg.SessionFactory,
		docs:           cfg.Docs,
		threadDocs:     cfg.ThreadDocs,
		sessionIdleTTL: cfg.SessionIdleTTL,
		sessionMaxTTL:  cfg.SessionMaxTTL,
		maxParallel:    maxParallel,
		now:            now,
		sessions:       map[string]map[string]*documentSession{},
	}
}

type docExecRequest struct {
	ThreadID    string
	DocumentIDs []string
	Task        string
	Model       string
}

type docExecResult struct {
	DocumentID   string `json:"document_id"`
	DocumentName string `json:"document_name,omitempty"`
	Answer       string `json:"answer,omitempty"`
	ResponseID   string `json:"response_id,omitempty"`
	Error        string `json:"error,omitempty"`
}

func (e *documentExec) Execute(ctx context.Context, req docExecRequest) []docExecResult {
	results := make([]docExecResult, len(req.DocumentIDs))
	if len(req.DocumentIDs) == 0 {
		return results
	}

	parallelism := e.maxParallel
	if parallelism <= 0 || parallelism > len(req.DocumentIDs) {
		parallelism = len(req.DocumentIDs)
	}

	sem := make(chan struct{}, parallelism)
	var wg sync.WaitGroup

	for index, docID := range req.DocumentIDs {
		wg.Add(1)
		go func(index int, docID string) {
			defer wg.Done()

			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				results[index] = docExecResult{
					DocumentID: docID,
					Error:      ctx.Err().Error(),
				}
				return
			}
			defer func() { <-sem }()

			results[index] = e.executeOne(ctx, req, docID)
		}(index, docID)
	}

	wg.Wait()
	return results
}

func (e *documentExec) executeOne(ctx context.Context, req docExecRequest, documentID string) docExecResult {
	log := e.logger.With("document_id", documentID, "thread_id", req.ThreadID)

	doc, err := e.docs.Get(ctx, documentID)
	if err != nil {
		log.Warn("failed to load document for query", "error", err)
		return docExecResult{DocumentID: documentID, Error: fmt.Sprintf("document not found: %v", err)}
	}

	if doc.ManifestRef == "" {
		return docExecResult{DocumentID: documentID, DocumentName: doc.Filename, Error: "document has no manifest (not yet split)"}
	}

	manifest, err := e.loadManifest(ctx, doc.ManifestRef)
	if err != nil {
		log.Warn("failed to load document manifest", "manifest_ref", doc.ManifestRef, "error", err)
		return docExecResult{DocumentID: documentID, DocumentName: doc.Filename, Error: fmt.Sprintf("manifest load failed: %v", err)}
	}

	entry := e.getSession(req.ThreadID, documentID)

	responseID, answer, err := e.executeOneWithSession(ctx, entry, req, doc, manifest, log)
	if err != nil {
		return docExecResult{DocumentID: documentID, DocumentName: doc.Filename, Error: err.Error()}
	}

	if err := e.threadDocs.UpdateLineage(ctx, req.ThreadID, documentID, responseID); err != nil {
		log.Warn("failed to persist thread document lineage", "error", err)
	}

	log.Info("document query completed", "response_id", responseID, "answer_length", len(answer))
	return docExecResult{
		DocumentID:   documentID,
		DocumentName: doc.Filename,
		Answer:       answer,
		ResponseID:   responseID,
	}
}

func (e *documentExec) resolveLineage(ctx context.Context, threadID, documentID string, doc docstore.Document) (string, error) {
	threadLineage, err := e.threadDocs.GetLineage(ctx, threadID, documentID)
	if err != nil {
		return "", fmt.Errorf("load thread document lineage: %w", err)
	}
	if threadLineage != "" {
		return threadLineage, nil
	}

	if doc.BaseResponseID != "" {
		return doc.BaseResponseID, nil
	}

	return "", nil
}

func (e *documentExec) loadManifest(ctx context.Context, manifestRef string) (docsplitter.Manifest, error) {
	data, err := e.blob.ReadRef(ctx, manifestRef)
	if err != nil {
		return docsplitter.Manifest{}, fmt.Errorf("read manifest blob: %w", err)
	}

	var manifest docsplitter.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return docsplitter.Manifest{}, fmt.Errorf("decode manifest json: %w", err)
	}
	return manifest, nil
}

func (e *documentExec) executeOneWithSession(
	ctx context.Context,
	entry *documentSession,
	req docExecRequest,
	doc docstore.Document,
	manifest docsplitter.Manifest,
	log *slog.Logger,
) (string, string, error) {
	entry.mu.Lock()
	defer entry.mu.Unlock()

	previousResponseID, err := e.resolveLineageLocked(ctx, entry, req.ThreadID, doc.ID, doc)
	if err != nil {
		log.Warn("failed to resolve document lineage", "error", err)
		return "", "", fmt.Errorf("lineage resolution failed: %w", err)
	}

	if previousResponseID == "" {
		log.Info("warming document (no existing lineage)", "page_count", manifest.PageCount)
		warmupResponseID, warmErr := e.warmDocumentLocked(ctx, entry, doc, manifest, req.Model)
		if warmErr != nil {
			log.Warn("document warmup failed", "error", warmErr)
			return "", "", fmt.Errorf("warmup failed: %w", warmErr)
		}
		if err := e.docs.UpdateBaseLineage(ctx, doc.ID, warmupResponseID, req.Model); err != nil {
			log.Warn("failed to persist document base lineage", "error", err)
		}
		previousResponseID = warmupResponseID
		log.Info("document warmup completed", "base_response_id", warmupResponseID)
	}

	log.Info("sending document query", "previous_response_id", previousResponseID)
	responseID, answer, err := e.queryDocumentLocked(ctx, entry, previousResponseID, req.Task, req.Model)
	if err == nil {
		return responseID, answer, nil
	}

	log.Warn("document query failed on live session, retrying on fresh connection", "error", err)
	e.dropSessionLocked(entry)

	responseID, answer, err = e.queryDocumentLocked(ctx, entry, previousResponseID, req.Task, req.Model)
	if err == nil {
		return responseID, answer, nil
	}

	log.Warn("document query failed after reconnect, rebuilding lineage", "error", err)
	entry.latestResponseID = ""

	warmupResponseID, warmErr := e.warmDocumentLocked(ctx, entry, doc, manifest, req.Model)
	if warmErr != nil {
		return "", "", fmt.Errorf("query failed after rebuild warmup: %w", warmErr)
	}
	if err := e.docs.UpdateBaseLineage(ctx, doc.ID, warmupResponseID, req.Model); err != nil {
		log.Warn("failed to persist rebuilt base lineage", "error", err)
	}

	responseID, answer, err = e.queryDocumentLocked(ctx, entry, warmupResponseID, req.Task, req.Model)
	if err != nil {
		return "", "", fmt.Errorf("query failed after rebuild: %w", err)
	}
	return responseID, answer, nil
}

func (e *documentExec) resolveLineageLocked(ctx context.Context, entry *documentSession, threadID, documentID string, doc docstore.Document) (string, error) {
	if entry.latestResponseID != "" {
		return entry.latestResponseID, nil
	}
	return e.resolveLineage(ctx, threadID, documentID, doc)
}

func (e *documentExec) warmDocumentLocked(ctx context.Context, entry *documentSession, doc docstore.Document, manifest docsplitter.Manifest, model string) (string, error) {
	pageContent, err := e.buildDocumentPageContent(ctx, doc, manifest)
	if err != nil {
		return "", fmt.Errorf("build page content for warmup: %w", err)
	}

	payload, err := buildResponseCreatePayloadObject(threadstore.ThreadMeta{}, map[string]any{
		"model": model,
		"input": []any{
			map[string]any{
				"type":    "message",
				"role":    "user",
				"content": pageContent,
			},
		},
		"store":    true,
		"generate": false,
	})
	if err != nil {
		return "", fmt.Errorf("build document warmup response.create payload: %w", err)
	}

	responseID, _, err := e.executePayloadLocked(ctx, entry, payload)
	if err != nil {
		return "", fmt.Errorf("warmup session failed: %w", err)
	}
	return responseID, nil
}

func (e *documentExec) warmDocument(ctx context.Context, doc docstore.Document, manifest docsplitter.Manifest, model string) (string, error) {
	entry := &documentSession{}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	return e.warmDocumentLocked(ctx, entry, doc, manifest, model)
}

func (e *documentExec) queryDocumentLocked(ctx context.Context, entry *documentSession, previousResponseID, task, model string) (string, string, error) {
	payload, err := buildResponseCreatePayloadObject(threadstore.ThreadMeta{}, map[string]any{
		"model": model,
		"input": []any{
			map[string]any{
				"type": "message",
				"role": "user",
				"content": []any{
					map[string]any{
						"type": "input_text",
						"text": task,
					},
				},
			},
		},
		"previous_response_id": previousResponseID,
		"store":                true,
	})
	if err != nil {
		return "", "", fmt.Errorf("build document query response.create payload: %w", err)
	}
	return e.executePayloadLocked(ctx, entry, payload)
}

func (e *documentExec) queryDocument(ctx context.Context, previousResponseID, task, model string) (string, string, error) {
	entry := &documentSession{}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	return e.queryDocumentLocked(ctx, entry, previousResponseID, task, model)
}

func (e *documentExec) openSessionAndQuery(ctx context.Context, payload map[string]any) (string, string, error) {
	session := e.sessionFactory()
	if err := session.Connect(ctx); err != nil {
		return "", "", fmt.Errorf("connect document session: %w", err)
	}
	defer session.Close()

	payloadJSON, err := marshalResponseCreatePayload(payload)
	if err != nil {
		return "", "", fmt.Errorf("marshal document response.create payload: %w", err)
	}

	event, err := openaiws.NewResponseCreateEvent("doc-query", payloadJSON)
	if err != nil {
		return "", "", fmt.Errorf("build document response.create event: %w", err)
	}

	if err := session.Send(ctx, event); err != nil {
		return "", "", fmt.Errorf("send document response.create: %w", err)
	}

	return streamDocumentResponse(ctx, session)
}

func (e *documentExec) executePayloadLocked(ctx context.Context, entry *documentSession, payload map[string]any) (string, string, error) {
	if err := e.ensureConnectedLocked(ctx, entry); err != nil {
		return "", "", err
	}

	payloadJSON, err := marshalResponseCreatePayload(payload)
	if err != nil {
		return "", "", fmt.Errorf("marshal document response.create payload: %w", err)
	}

	event, err := openaiws.NewResponseCreateEvent("doc-query", payloadJSON)
	if err != nil {
		return "", "", fmt.Errorf("build document response.create event: %w", err)
	}

	if err := entry.session.Send(ctx, event); err != nil {
		return "", "", fmt.Errorf("send document response.create: %w", err)
	}

	responseID, answer, err := streamDocumentResponse(ctx, entry.session)
	if err != nil {
		return "", "", err
	}

	now := e.currentTime()
	entry.latestResponseID = responseID
	entry.lastUsedAt = now
	if entry.connectedAt.IsZero() {
		entry.connectedAt = now
	}

	return responseID, answer, nil
}

func (e *documentExec) ensureConnectedLocked(ctx context.Context, entry *documentSession) error {
	now := e.currentTime()

	if entry.session == nil {
		entry.session = e.sessionFactory()
	}

	if entry.session == nil {
		return fmt.Errorf("document session factory is not configured")
	}

	if snapshot := entry.session.Snapshot(); snapshot.State == openaiws.SessionStateConnected {
		if e.sessionMaxTTL > 0 && !entry.connectedAt.IsZero() && now.Sub(entry.connectedAt) >= e.sessionMaxTTL {
			e.dropSessionLocked(entry)
		} else {
			return nil
		}
	}

	if entry.session == nil {
		entry.session = e.sessionFactory()
	}
	if entry.session == nil {
		return fmt.Errorf("document session factory is not configured")
	}

	if err := entry.session.Connect(ctx); err != nil {
		return fmt.Errorf("connect document session: %w", err)
	}

	entry.connectedAt = now
	return nil
}

func (e *documentExec) currentTime() time.Time {
	if e.now != nil {
		return e.now()
	}
	return time.Now().UTC()
}

func (e *documentExec) getSession(threadID, documentID string) *documentSession {
	e.sessionsMu.Lock()
	defer e.sessionsMu.Unlock()

	documentsByThread, ok := e.sessions[threadID]
	if !ok {
		documentsByThread = map[string]*documentSession{}
		e.sessions[threadID] = documentsByThread
	}

	entry, ok := documentsByThread[documentID]
	if ok {
		return entry
	}

	entry = &documentSession{}
	documentsByThread[documentID] = entry
	return entry
}

func (e *documentExec) CloseThread(threadID string) error {
	e.sessionsMu.Lock()
	documentsByThread := e.sessions[threadID]
	delete(e.sessions, threadID)
	e.sessionsMu.Unlock()

	var errs []error
	for _, entry := range documentsByThread {
		entry.mu.Lock()
		if err := e.closeSessionLocked(entry, true); err != nil {
			errs = append(errs, err)
		}
		entry.mu.Unlock()
	}

	return errors.Join(errs...)
}

func (e *documentExec) CloseIdleSessions(now time.Time) error {
	e.sessionsMu.Lock()
	entries := make([]*documentSession, 0)
	for _, documentsByThread := range e.sessions {
		for _, entry := range documentsByThread {
			entries = append(entries, entry)
		}
	}
	e.sessionsMu.Unlock()

	var errs []error
	for _, entry := range entries {
		entry.mu.Lock()
		if e.shouldCloseIdleLocked(entry, now) {
			if err := e.closeSessionLocked(entry, true); err != nil {
				errs = append(errs, err)
			}
		}
		entry.mu.Unlock()
	}

	return errors.Join(errs...)
}

func (e *documentExec) shouldCloseIdleLocked(entry *documentSession, now time.Time) bool {
	if entry.session == nil {
		return false
	}
	if e.sessionIdleTTL > 0 && !entry.lastUsedAt.IsZero() && now.Sub(entry.lastUsedAt) >= e.sessionIdleTTL {
		return true
	}
	if e.sessionMaxTTL > 0 && !entry.connectedAt.IsZero() && now.Sub(entry.connectedAt) >= e.sessionMaxTTL {
		return true
	}
	return false
}

func (e *documentExec) dropSessionLocked(entry *documentSession) {
	_ = e.closeSessionLocked(entry, false)
}

func (e *documentExec) closeSessionLocked(entry *documentSession, clearLineage bool) error {
	var err error
	if entry.session != nil {
		err = entry.session.Close()
		entry.session = nil
	}
	entry.connectedAt = time.Time{}
	if clearLineage {
		entry.latestResponseID = ""
		entry.lastUsedAt = time.Time{}
	}
	return err
}

func streamDocumentResponse(ctx context.Context, session *openaiws.Session) (string, string, error) {
	var responseID string
	var textParts []string

	for {
		event, err := session.Receive(ctx)
		if err != nil {
			return "", "", fmt.Errorf("receive document session event: %w", err)
		}

		if resolved := event.ResolvedResponseID(); resolved != "" {
			responseID = resolved
		}

		switch event.Type {
		case openaiws.EventTypeResponseOutputTextDelta:
			if delta := extractTextDelta(event.Raw); delta != "" {
				textParts = append(textParts, delta)
			}

		case openaiws.EventTypeResponseCompleted:
			return responseID, strings.Join(textParts, ""), nil

		case openaiws.EventTypeResponseFailed:
			errMsg := extractResponseFailedError(event.Raw)
			if errMsg == "" {
				errMsg = "document session response failed"
			}
			return "", "", fmt.Errorf("%s", errMsg)

		case openaiws.EventTypeResponseIncomplete:
			return responseID, strings.Join(textParts, ""), nil

		case openaiws.EventTypeError:
			errMsg := "document session error"
			if event.Error != nil && event.Error.Message != "" {
				errMsg = event.Error.Message
			}
			return "", "", fmt.Errorf("%s", errMsg)
		}
	}
}

func extractTextDelta(raw json.RawMessage) string {
	var payload struct {
		Delta string `json:"delta"`
	}
	if json.Unmarshal(raw, &payload) == nil {
		return payload.Delta
	}
	return ""
}

func extractResponseFailedError(raw json.RawMessage) string {
	var payload struct {
		Response struct {
			StatusDetails struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			} `json:"status_details"`
		} `json:"response"`
	}
	if json.Unmarshal(raw, &payload) == nil {
		return payload.Response.StatusDetails.Error.Message
	}
	return ""
}

func (e *documentExec) buildDocumentPageContent(ctx context.Context, doc docstore.Document, manifest docsplitter.Manifest) ([]any, error) {
	content := make([]any, 0, len(manifest.Pages)*3+2)

	content = append(content, map[string]any{
		"type": "input_text",
		"text": fmt.Sprintf(`<pdf name="%s" id="%s" page_count="%d">`,
			escapePromptAttribute(doc.Filename),
			escapePromptAttribute(doc.ID),
			manifest.PageCount),
	})

	for _, page := range manifest.Pages {
		content = append(content, map[string]any{
			"type": "input_text",
			"text": fmt.Sprintf(`<pdf_page number="%d">`, page.PageNumber),
		})

		imageData, err := e.blob.ReadRef(ctx, page.ImageRef)
		if err != nil {
			return nil, fmt.Errorf("read page %d image: %w", page.PageNumber, err)
		}

		contentType := page.ContentType
		if contentType == "" {
			contentType = http.DetectContentType(imageData)
		}

		dataURL := "data:" + contentType + ";base64," + base64.StdEncoding.EncodeToString(imageData)
		content = append(content, map[string]any{
			"type":      "input_image",
			"image_url": dataURL,
			"detail":    "high",
		})

		content = append(content, map[string]any{
			"type": "input_text",
			"text": "</pdf_page>",
		})
	}

	content = append(content, map[string]any{
		"type": "input_text",
		"text": "</pdf>",
	})

	return content, nil
}
