package documenthandler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"explorer/internal/doccmd"
	"explorer/internal/docsplitter"
	"explorer/internal/docstore"
	"explorer/internal/preparedinput"
	"explorer/internal/threadcollectionstore"

	"github.com/nats-io/nats.go"
)

type documentStore interface {
	Get(ctx context.Context, id int64) (docstore.Document, error)
}

type threadDocumentStore interface {
	ListDocuments(ctx context.Context, threadID int64, limit int64) ([]docstore.Document, error)
}

type threadCollectionStore interface {
	ListAttached(ctx context.Context, threadID int64, limit int64) ([]threadcollectionstore.AttachedCollection, error)
}

type documentBlobStore interface {
	Ref(parts ...string) string
	ReadRef(ctx context.Context, ref string) ([]byte, error)
	WriteRef(ctx context.Context, ref string, data []byte) error
}

// Service currently exists only as a lifecycle/dependency shell for
// document-adjacent backend work. It is intentionally not the OpenAI
// document executor; worker-owned document execution lives in the worker.
type Service struct {
	logger            *slog.Logger
	nc                *nats.Conn
	js                nats.JetStreamContext
	docs              documentStore
	threadDocs        threadDocumentStore
	threadCollections threadCollectionStore
	blob              documentBlobStore
	now               func() time.Time
}

func New(logger *slog.Logger, nc *nats.Conn, js nats.JetStreamContext, docs documentStore, threadDocs threadDocumentStore, threadCollections threadCollectionStore, blob documentBlobStore) *Service {
	return &Service{
		logger:            logger,
		nc:                nc,
		js:                js,
		docs:              docs,
		threadDocs:        threadDocs,
		threadCollections: threadCollections,
		blob:              blob,
		now:               func() time.Time { return time.Now().UTC() },
	}
}

func (s *Service) Run(ctx context.Context) error {
	if s.nc == nil {
		return fmt.Errorf("document handler nats connection is required")
	}

	ch := make(chan *nats.Msg, 64)
	sub, err := s.nc.ChanQueueSubscribe(doccmd.PrepareInputSubject, doccmd.PrepareInputQueue, ch)
	if err != nil {
		return fmt.Errorf("subscribe %s: %w", doccmd.PrepareInputSubject, err)
	}
	defer sub.Unsubscribe()

	runtimeCh := make(chan *nats.Msg, 64)
	runtimeSub, err := s.nc.ChanQueueSubscribe(doccmd.RuntimeContextSubject, doccmd.RuntimeContextQueue, runtimeCh)
	if err != nil {
		return fmt.Errorf("subscribe %s: %w", doccmd.RuntimeContextSubject, err)
	}
	defer runtimeSub.Unsubscribe()

	s.logger.Info("document handler service started",
		"prepare_input_subject", doccmd.PrepareInputSubject,
		"runtime_context_subject", doccmd.RuntimeContextSubject,
	)

	for {
		select {
		case <-ctx.Done():
			_ = sub.Drain()
			_ = runtimeSub.Drain()
			s.logger.Info("document handler service stopping", "reason", ctx.Err())
			return nil
		case msg := <-ch:
			s.handlePrepareInput(ctx, msg)
		case msg := <-runtimeCh:
			s.handleRuntimeContext(ctx, msg)
		}
	}
}

func (s *Service) handlePrepareInput(ctx context.Context, msg *nats.Msg) {
	req, err := doccmd.DecodePrepareInputRequest(msg.Data)
	if err != nil {
		s.logger.Error("invalid prepare input request", "error", err)
		s.respondPrepareInput(msg, doccmd.PrepareInputResponse{
			RequestID: "unknown",
			Status:    doccmd.PrepareStatusError,
			Error:     err.Error(),
		})
		return
	}

	s.logger.Info("prepare input request received",
		"request_id", req.RequestID,
		"kind", req.Kind,
		"thread_id", req.ThreadID,
		"document_id", req.DocumentID,
	)

	s.respondPrepareInput(msg, s.prepareInput(ctx, req))
}

func (s *Service) handleRuntimeContext(ctx context.Context, msg *nats.Msg) {
	req, err := doccmd.DecodeRuntimeContextRequest(msg.Data)
	if err != nil {
		s.logger.Error("invalid runtime context request", "error", err)
		s.respondRuntimeContext(msg, doccmd.RuntimeContextResponse{
			RequestID: "unknown",
			Status:    doccmd.PrepareStatusError,
			Error:     err.Error(),
		})
		return
	}

	s.logger.Info("runtime context request received",
		"request_id", req.RequestID,
		"thread_id", req.ThreadID,
	)

	s.respondRuntimeContext(msg, s.runtimeContext(ctx, req))
}

func (s *Service) respondPrepareInput(msg *nats.Msg, resp doccmd.PrepareInputResponse) {
	if msg == nil || msg.Reply == "" {
		s.logger.Warn("prepare input request has no reply subject",
			"request_id", resp.RequestID,
			"status", resp.Status,
			"error", resp.Error,
		)
		return
	}

	data, err := doccmd.EncodePrepareInputResponse(resp)
	if err != nil {
		s.logger.Error("failed to encode prepare input response",
			"request_id", resp.RequestID,
			"status", resp.Status,
			"error", err,
		)
		return
	}
	if err := msg.Respond(data); err != nil {
		s.logger.Error("failed to publish prepare input response",
			"request_id", resp.RequestID,
			"status", resp.Status,
			"error", err,
		)
	}
}

func (s *Service) respondRuntimeContext(msg *nats.Msg, resp doccmd.RuntimeContextResponse) {
	if msg == nil || msg.Reply == "" {
		s.logger.Warn("runtime context request has no reply subject",
			"request_id", resp.RequestID,
			"status", resp.Status,
			"error", resp.Error,
		)
		return
	}

	data, err := doccmd.EncodeRuntimeContextResponse(resp)
	if err != nil {
		s.logger.Error("failed to encode runtime context response",
			"request_id", resp.RequestID,
			"status", resp.Status,
			"error", err,
		)
		return
	}
	if err := msg.Respond(data); err != nil {
		s.logger.Error("failed to publish runtime context response",
			"request_id", resp.RequestID,
			"status", resp.Status,
			"error", err,
		)
	}
}

func (s *Service) prepareInput(ctx context.Context, req doccmd.PrepareInputRequest) doccmd.PrepareInputResponse {
	var ref string
	var err error

	switch strings.TrimSpace(req.Kind) {
	case doccmd.PrepareKindWarmup:
		ref, err = s.prepareWarmupInput(ctx, req)
	case doccmd.PrepareKindDocumentQuery:
		ref, err = s.prepareDocumentQueryInput(ctx, req)
	default:
		return doccmd.PrepareInputResponse{
			RequestID: req.RequestID,
			Status:    doccmd.PrepareStatusError,
			Error:     fmt.Sprintf("unsupported prepare input kind %q", req.Kind),
		}
	}

	if err != nil {
		return doccmd.PrepareInputResponse{
			RequestID: req.RequestID,
			Status:    doccmd.PrepareStatusError,
			Error:     err.Error(),
		}
	}

	return doccmd.PrepareInputResponse{
		RequestID:        req.RequestID,
		Status:           doccmd.PrepareStatusOK,
		PreparedInputRef: ref,
	}
}

func (s *Service) prepareWarmupInput(ctx context.Context, req doccmd.PrepareInputRequest) (string, error) {
	doc, err := s.docs.Get(ctx, req.DocumentID)
	if err != nil {
		return "", fmt.Errorf("load document: %w", err)
	}
	if strings.TrimSpace(doc.ManifestRef) == "" {
		return "", fmt.Errorf("document has no manifest (not yet split)")
	}

	manifest, err := s.loadManifest(ctx, doc.ManifestRef)
	if err != nil {
		return "", err
	}

	inputJSON, err := buildWarmupInput(doc, manifest)
	if err != nil {
		return "", err
	}

	store, err := preparedinput.NewStore(s.blob)
	if err != nil {
		return "", err
	}

	ref, err := store.Write(ctx, req.RequestID, preparedinput.Artifact{
		Version:    preparedinput.VersionV1,
		Input:      inputJSON,
		SourceKind: doccmd.PrepareKindWarmup,
		CreatedAt:  s.now().Format(time.RFC3339),
	})
	if err != nil {
		return "", err
	}

	return ref, nil
}

func (s *Service) prepareDocumentQueryInput(ctx context.Context, req doccmd.PrepareInputRequest) (string, error) {
	doc, err := s.docs.Get(ctx, req.DocumentID)
	if err != nil {
		return "", fmt.Errorf("load document: %w", err)
	}
	if strings.TrimSpace(doc.ManifestRef) == "" {
		return "", fmt.Errorf("document has no manifest (not yet split)")
	}

	manifest, err := s.loadManifest(ctx, doc.ManifestRef)
	if err != nil {
		return "", err
	}

	inputJSON, err := buildDocumentQueryInput(doc, manifest, req.Task)
	if err != nil {
		return "", err
	}

	store, err := preparedinput.NewStore(s.blob)
	if err != nil {
		return "", err
	}

	ref, err := store.Write(ctx, req.RequestID, preparedinput.Artifact{
		Version:    preparedinput.VersionV1,
		Input:      inputJSON,
		SourceKind: doccmd.PrepareKindDocumentQuery,
		CreatedAt:  s.now().Format(time.RFC3339),
	})
	if err != nil {
		return "", err
	}

	return ref, nil
}

func (s *Service) runtimeContext(ctx context.Context, req doccmd.RuntimeContextRequest) doccmd.RuntimeContextResponse {
	instructions := req.Instructions
	tools := append(json.RawMessage(nil), req.Tools...)

	if s.threadDocs == nil {
		return doccmd.RuntimeContextResponse{
			RequestID:    req.RequestID,
			Status:       doccmd.PrepareStatusOK,
			Instructions: instructions,
			Tools:        tools,
		}
	}

	documents, err := s.threadDocs.ListDocuments(ctx, req.ThreadID, 200)
	if err != nil {
		return doccmd.RuntimeContextResponse{
			RequestID: req.RequestID,
			Status:    doccmd.PrepareStatusError,
			Error:     fmt.Sprintf("list attached documents: %v", err),
		}
	}

	var collections []threadcollectionstore.AttachedCollection
	if s.threadCollections != nil {
		collections, err = s.threadCollections.ListAttached(ctx, req.ThreadID, 200)
		if err != nil {
			return doccmd.RuntimeContextResponse{
				RequestID: req.RequestID,
				Status:    doccmd.PrepareStatusError,
				Error:     fmt.Sprintf("list attached collections: %v", err),
			}
		}
	}

	if len(documents) == 0 && len(collections) == 0 {
		return doccmd.RuntimeContextResponse{
			RequestID:    req.RequestID,
			Status:       doccmd.PrepareStatusOK,
			Instructions: instructions,
			Tools:        tools,
		}
	}

	instructions = appendAvailableDocumentsBlock(instructions, documents, collections)
	tools, err = appendQueryDocumentTool(tools)
	if err != nil {
		return doccmd.RuntimeContextResponse{
			RequestID: req.RequestID,
			Status:    doccmd.PrepareStatusError,
			Error:     err.Error(),
		}
	}

	return doccmd.RuntimeContextResponse{
		RequestID:    req.RequestID,
		Status:       doccmd.PrepareStatusOK,
		Instructions: instructions,
		Tools:        tools,
	}
}

func (s *Service) loadManifest(ctx context.Context, manifestRef string) (docsplitter.Manifest, error) {
	data, err := s.blob.ReadRef(ctx, manifestRef)
	if err != nil {
		return docsplitter.Manifest{}, fmt.Errorf("read manifest blob: %w", err)
	}

	var manifest docsplitter.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return docsplitter.Manifest{}, fmt.Errorf("decode manifest json: %w", err)
	}

	return manifest, nil
}

func buildWarmupInput(doc docstore.Document, manifest docsplitter.Manifest) (json.RawMessage, error) {
	content := make([]any, 0, len(manifest.Pages)*3+2)

	content = append(content, map[string]any{
		"type": "input_text",
		"text": fmt.Sprintf(`<pdf name="%s" id="%s" page_count="%d">`,
			escapePromptAttribute(doc.Filename),
			escapePromptAttribute(formatDocumentID(doc.ID)),
			manifest.PageCount),
	})

	for _, page := range manifest.Pages {
		content = append(content, map[string]any{
			"type": "input_text",
			"text": fmt.Sprintf(`<pdf_page number="%d">`, page.PageNumber),
		})

		image := map[string]any{
			"type":      "image_ref",
			"image_ref": page.ImageRef,
			"detail":    "high",
		}
		if strings.TrimSpace(page.ContentType) != "" {
			image["content_type"] = page.ContentType
		}
		content = append(content, image)

		content = append(content, map[string]any{
			"type": "input_text",
			"text": "</pdf_page>",
		})
	}

	content = append(content, map[string]any{
		"type": "input_text",
		"text": "</pdf>",
	})

	inputJSON, err := json.Marshal([]any{
		map[string]any{
			"type":    "message",
			"role":    "user",
			"content": content,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("marshal document warmup input: %w", err)
	}

	return inputJSON, nil
}

func buildDocumentQueryInput(doc docstore.Document, manifest docsplitter.Manifest, task string) (json.RawMessage, error) {
	content := make([]any, 0, len(manifest.Pages)*3+3)

	content = append(content, map[string]any{
		"type": "input_text",
		"text": fmt.Sprintf(`<pdf name="%s" id="%s" page_count="%d">`,
			escapePromptAttribute(doc.Filename),
			escapePromptAttribute(formatDocumentID(doc.ID)),
			manifest.PageCount),
	})

	for _, page := range manifest.Pages {
		content = append(content, map[string]any{
			"type": "input_text",
			"text": fmt.Sprintf(`<pdf_page number="%d">`, page.PageNumber),
		})

		image := map[string]any{
			"type":      "image_ref",
			"image_ref": page.ImageRef,
			"detail":    "high",
		}
		if strings.TrimSpace(page.ContentType) != "" {
			image["content_type"] = page.ContentType
		}
		content = append(content, image)

		content = append(content, map[string]any{
			"type": "input_text",
			"text": "</pdf_page>",
		})
	}

	content = append(content, map[string]any{
		"type": "input_text",
		"text": "</pdf>",
	})

	if strings.TrimSpace(task) != "" {
		content = append(content, map[string]any{
			"type": "input_text",
			"text": task,
		})
	}

	inputJSON, err := json.Marshal([]any{
		map[string]any{
			"type":    "message",
			"role":    "user",
			"content": content,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("marshal document query input: %w", err)
	}

	return inputJSON, nil
}

func escapePromptAttribute(value string) string {
	replacer := strings.NewReplacer(
		"&", "&amp;",
		`"`, "&quot;",
		"<", "&lt;",
		">", "&gt;",
		"\n", "&#10;",
		"\r", "&#13;",
		"\t", "&#9;",
	)
	return replacer.Replace(value)
}

func appendAvailableDocumentsBlock(base string, documents []docstore.Document, collections []threadcollectionstore.AttachedCollection) string {
	block := formatAvailableDocumentsBlock(documents, collections)
	if block == "" {
		return base
	}

	trimmedBase := strings.TrimRight(base, "\n")
	if strings.TrimSpace(trimmedBase) == "" {
		return block
	}

	return trimmedBase + "\n\n" + block
}

func formatAvailableDocumentsBlock(documents []docstore.Document, collections []threadcollectionstore.AttachedCollection) string {
	var parts []string

	if standalone := formatDocumentsBlock(documents, "<available_documents>", "</available_documents>"); standalone != "" {
		parts = append(parts, standalone)
	}

	for _, collection := range collections {
		parts = append(parts, formatCollectionBlock(collection))
	}

	return strings.Join(parts, "\n")
}

func formatDocumentsBlock(documents []docstore.Document, openTag, closeTag string) string {
	var builder strings.Builder
	count := 0

	for _, document := range documents {
		if document.ID <= 0 {
			continue
		}
		id := formatDocumentID(document.ID)

		name := strings.TrimSpace(document.Filename)
		if name == "" {
			name = id
		}

		if count == 0 {
			builder.WriteString(openTag)
			builder.WriteByte('\n')
		}
		builder.WriteString(`<document id="`)
		builder.WriteString(escapePromptAttribute(id))
		builder.WriteString(`" name="`)
		builder.WriteString(escapePromptAttribute(name))
		builder.WriteString(`" />`)
		builder.WriteByte('\n')
		count++
	}

	if count == 0 {
		return ""
	}

	builder.WriteString(closeTag)
	return builder.String()
}

func formatCollectionBlock(collection threadcollectionstore.AttachedCollection) string {
	name := strings.TrimSpace(collection.Name)
	if name == "" {
		name = collection.ID
	}

	var builder strings.Builder
	builder.WriteString(`<collection name="`)
	builder.WriteString(escapePromptAttribute(name))
	builder.WriteString(`">` + "\n")
	if inner := formatDocumentsBlock(collection.Documents, "<available_documents>", "</available_documents>"); inner != "" {
		builder.WriteString(inner)
		builder.WriteByte('\n')
	} else {
		// Empty collections are still surfaced so the model knows the
		// collection exists (the user may add documents to it mid-thread).
		builder.WriteString("<available_documents></available_documents>\n")
	}
	builder.WriteString("</collection>")
	return builder.String()
}

func formatDocumentID(id int64) string {
	return strconv.FormatInt(id, 10)
}

func appendQueryDocumentTool(raw json.RawMessage) (json.RawMessage, error) {
	var tools []map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &tools); err != nil {
			return nil, fmt.Errorf("decode runtime context tools: %w", err)
		}
	}

	for _, tool := range tools {
		name, _ := tool["name"].(string)
		if name == doccmd.ToolNameQueryDocument {
			return raw, nil
		}
	}

	tools = append(tools, doccmd.QueryDocumentToolDefinition())
	encoded, err := json.Marshal(tools)
	if err != nil {
		return nil, fmt.Errorf("marshal runtime context tools: %w", err)
	}
	return encoded, nil
}
