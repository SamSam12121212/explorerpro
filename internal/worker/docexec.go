package worker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"explorer/internal/blobstore"
	"explorer/internal/docsplitter"
	"explorer/internal/docstore"
	"explorer/internal/openaiws"
)

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
}

type documentExec struct {
	logger         *slog.Logger
	blob           *blobstore.LocalStore
	cfg            openaiws.Config
	sessionFactory func() *openaiws.Session
	docs           docExecDocStore
	threadDocs     docExecThreadDocStore
}

func newDocumentExec(cfg documentExecConfig) *documentExec {
	return &documentExec{
		logger:         cfg.Logger,
		blob:           cfg.Blob,
		cfg:            cfg.OpenAIConfig,
		sessionFactory: cfg.SessionFactory,
		docs:           cfg.Docs,
		threadDocs:     cfg.ThreadDocs,
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
	results := make([]docExecResult, 0, len(req.DocumentIDs))
	for _, docID := range req.DocumentIDs {
		result := e.executeOne(ctx, req, docID)
		results = append(results, result)
	}
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

	previousResponseID, err := e.resolveLineage(ctx, req.ThreadID, documentID, doc)
	if err != nil {
		log.Warn("failed to resolve document lineage", "error", err)
		return docExecResult{DocumentID: documentID, DocumentName: doc.Filename, Error: fmt.Sprintf("lineage resolution failed: %v", err)}
	}

	if previousResponseID == "" {
		log.Info("warming document (no existing lineage)", "page_count", manifest.PageCount)
		warmupResponseID, warmErr := e.warmDocument(ctx, doc, manifest, req.Model)
		if warmErr != nil {
			log.Warn("document warmup failed", "error", warmErr)
			return docExecResult{DocumentID: documentID, DocumentName: doc.Filename, Error: fmt.Sprintf("warmup failed: %v", warmErr)}
		}
		if err := e.docs.UpdateBaseLineage(ctx, documentID, warmupResponseID, req.Model); err != nil {
			log.Warn("failed to persist document base lineage", "error", err)
		}
		previousResponseID = warmupResponseID
		log.Info("document warmup completed", "base_response_id", warmupResponseID)
	}

	log.Info("sending document query", "previous_response_id", previousResponseID)
	responseID, answer, err := e.queryDocument(ctx, previousResponseID, req.Task, req.Model)
	if err != nil {
		log.Warn("document query failed, retrying with fresh lineage", "error", err)
		responseID, answer, err = e.retryWithFreshLineage(ctx, log, doc, manifest, req)
		if err != nil {
			return docExecResult{DocumentID: documentID, DocumentName: doc.Filename, Error: fmt.Sprintf("query failed after rebuild: %v", err)}
		}
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

func (e *documentExec) retryWithFreshLineage(ctx context.Context, log *slog.Logger, doc docstore.Document, manifest docsplitter.Manifest, req docExecRequest) (string, string, error) {
	warmupResponseID, warmErr := e.warmDocument(ctx, doc, manifest, req.Model)
	if warmErr != nil {
		return "", "", fmt.Errorf("rebuild warmup failed: %w", warmErr)
	}
	if err := e.docs.UpdateBaseLineage(ctx, doc.ID, warmupResponseID, req.Model); err != nil {
		log.Warn("failed to persist rebuilt base lineage", "error", err)
	}
	return e.queryDocument(ctx, warmupResponseID, req.Task, req.Model)
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

func (e *documentExec) warmDocument(ctx context.Context, doc docstore.Document, manifest docsplitter.Manifest, model string) (string, error) {
	pageContent, err := e.buildDocumentPageContent(ctx, doc, manifest)
	if err != nil {
		return "", fmt.Errorf("build page content for warmup: %w", err)
	}

	payload := map[string]any{
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
	}

	responseID, _, err := e.openSessionAndQuery(ctx, payload)
	if err != nil {
		return "", fmt.Errorf("warmup session failed: %w", err)
	}
	return responseID, nil
}

func (e *documentExec) queryDocument(ctx context.Context, previousResponseID, task, model string) (string, string, error) {
	payload := map[string]any{
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
	}
	return e.openSessionAndQuery(ctx, payload)
}

func (e *documentExec) openSessionAndQuery(ctx context.Context, payload map[string]any) (string, string, error) {
	session := e.sessionFactory()
	if err := session.Connect(ctx); err != nil {
		return "", "", fmt.Errorf("connect document session: %w", err)
	}
	defer session.Close()

	event, err := openaiws.NewResponseCreateEvent("doc-query", payload)
	if err != nil {
		return "", "", fmt.Errorf("build document response.create event: %w", err)
	}

	if err := session.Send(ctx, event); err != nil {
		return "", "", fmt.Errorf("send document response.create: %w", err)
	}

	return streamDocumentResponse(ctx, session)
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
