package httpserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"explorer/internal/doccmd"
	"explorer/internal/docstore"
	"explorer/internal/idgen"
	"explorer/internal/platform"

	"github.com/nats-io/nats.go"
)

const maxDocUploadBytes = 50 << 20

type documentAPI struct {
	logger  *slog.Logger
	runtime *platform.Runtime
	store   *docstore.Store
}

func newDocumentAPI(logger *slog.Logger, runtime *platform.Runtime) *documentAPI {
	return &documentAPI{
		logger:  logger,
		runtime: runtime,
		store:   docstore.New(runtime.Postgres().Pool()),
	}
}

func (a *documentAPI) handleDocuments(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.handleListDocuments(w, r)
	case http.MethodPost:
		a.handleUploadDocument(w, r)
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

func (a *documentAPI) handleDocumentRoutes(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.Trim(strings.TrimPrefix(r.URL.Path, "/documents/"), "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) == 0 || parts[0] == "" || len(parts) > 2 {
		http.NotFound(w, r)
		return
	}

	docID := parts[0]

	switch {
	case len(parts) == 1:
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		a.handleGetDocument(w, r, docID)
	case len(parts) == 2 && parts[1] == "manifest":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		a.handleGetManifest(w, r, docID)
	default:
		http.NotFound(w, r)
	}
}

func (a *documentAPI) handleListDocuments(w http.ResponseWriter, r *http.Request) {
	docs, err := a.store.List(r.Context(), 100)
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("list documents: %v", err))
		return
	}

	presented := make([]map[string]any, 0, len(docs))
	for _, doc := range docs {
		presented = append(presented, presentDocument(doc))
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"documents": presented,
		"count":     len(presented),
	})
}

func (a *documentAPI) handleUploadDocument(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(maxDocUploadBytes); err != nil {
		writeErrorJSON(w, http.StatusBadRequest, fmt.Sprintf("parse multipart form: %v", err))
		return
	}

	file, _, err := r.FormFile("file")
	if err != nil {
		writeErrorJSON(w, http.StatusBadRequest, "file field is required")
		return
	}
	defer file.Close()

	payload := bytes.Buffer{}
	limited := http.MaxBytesReader(w, file, maxDocUploadBytes+1)
	if _, err := payload.ReadFrom(limited); err != nil {
		writeErrorJSON(w, http.StatusBadRequest, fmt.Sprintf("read document upload: %v", err))
		return
	}
	if payload.Len() == 0 {
		writeErrorJSON(w, http.StatusBadRequest, "uploaded document is empty")
		return
	}
	if payload.Len() > maxDocUploadBytes {
		writeErrorJSON(w, http.StatusBadRequest, fmt.Sprintf("uploaded document exceeds %d bytes", maxDocUploadBytes))
		return
	}

	if !bytes.HasPrefix(payload.Bytes(), []byte("%PDF")) {
		writeErrorJSON(w, http.StatusBadRequest, "uploaded file is not a PDF")
		return
	}

	docID, err := idgen.New("doc")
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, err.Error())
		return
	}

	cmdID, err := idgen.New("cmd")
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, err.Error())
		return
	}

	sourceRef := a.runtime.Blob().Ref("documents", docID, "source.pdf")
	if err := a.runtime.Blob().WriteRef(r.Context(), sourceRef, payload.Bytes()); err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("store uploaded document: %v", err))
		return
	}

	now := time.Now().UTC()
	doc := docstore.Document{
		ID:        docID,
		SourceRef: sourceRef,
		Status:    "pending",
		DPI:       doccmd.DefaultDPI,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := a.store.Create(r.Context(), doc); err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("create document: %v", err))
		return
	}

	splitCmd := doccmd.SplitCommand{
		CmdID:      cmdID,
		DocumentID: docID,
		SourceRef:  sourceRef,
		DPI:        doccmd.DefaultDPI,
	}

	cmdPayload, err := json.Marshal(splitCmd)
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("marshal split command: %v", err))
		return
	}

	msg := &nats.Msg{
		Subject: doccmd.SplitSubject,
		Header:  nats.Header{},
		Data:    cmdPayload,
	}
	msg.Header.Set("Nats-Msg-Id", cmdID)

	if _, err := a.runtime.NATS().JetStream().PublishMsg(msg); err != nil {
		writeErrorJSON(w, http.StatusServiceUnavailable, fmt.Sprintf("publish split command: %v", err))
		return
	}

	a.logger.Info("document split requested",
		"document_id", docID,
		"source_ref", sourceRef,
		"cmd_id", cmdID,
	)

	writeJSON(w, http.StatusAccepted, map[string]any{
		"document": presentDocument(doc),
		"cmd_id":   cmdID,
	})
}

func (a *documentAPI) handleGetDocument(w http.ResponseWriter, r *http.Request, docID string) {
	doc, err := a.store.Get(r.Context(), docID)
	if errors.Is(err, docstore.ErrDocumentNotFound) {
		writeErrorJSON(w, http.StatusNotFound, "document not found")
		return
	}
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("get document: %v", err))
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"document": presentDocument(doc),
	})
}

func (a *documentAPI) handleGetManifest(w http.ResponseWriter, r *http.Request, docID string) {
	doc, err := a.store.Get(r.Context(), docID)
	if errors.Is(err, docstore.ErrDocumentNotFound) {
		writeErrorJSON(w, http.StatusNotFound, "document not found")
		return
	}
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("get document: %v", err))
		return
	}

	if doc.Status != "ready" || doc.ManifestRef == "" {
		writeErrorJSON(w, http.StatusNotFound, "manifest not available")
		return
	}

	data, err := a.runtime.Blob().ReadRef(r.Context(), doc.ManifestRef)
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("read manifest: %v", err))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func presentDocument(d docstore.Document) map[string]any {
	entry := map[string]any{
		"id":         d.ID,
		"source_ref": d.SourceRef,
		"status":     d.Status,
		"page_count": d.PageCount,
		"dpi":        d.DPI,
		"created_at": d.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at": d.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if d.Error != "" {
		entry["error"] = d.Error
	}
	if d.ManifestRef != "" {
		entry["manifest_ref"] = d.ManifestRef
	}
	return entry
}
