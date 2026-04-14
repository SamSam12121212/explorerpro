package httpserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"explorer/internal/doccmd"
	"explorer/internal/docstore"
	"explorer/internal/platform"
	"explorer/internal/postgresstore"

	"github.com/nats-io/nats.go"
)

const maxDocUploadBytes = 50 << 20
const defaultDocumentQueryModel = "gpt-5.4"

var allowedDocumentQueryModels = map[string]struct{}{
	"gpt-5.4":      {},
	"gpt-5.4-mini": {},
	"gpt-5.4-nano": {},
}

type documentAPI struct {
	logger   *slog.Logger
	runtime  *platform.Runtime
	store    *docstore.Store
	commands *postgresstore.Store
}

type updateDocumentRequest struct {
	QueryModel        string `json:"query_model"`
	ClearBaseResponse bool   `json:"clear_base_response"`
}

func newDocumentAPI(logger *slog.Logger, runtime *platform.Runtime) *documentAPI {
	return &documentAPI{
		logger:   logger,
		runtime:  runtime,
		store:    docstore.New(runtime.Postgres().Pool()),
		commands: postgresstore.New(runtime.Postgres().Pool()),
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

	docID, err := parseDocumentID(parts[0])
	if err != nil {
		http.NotFound(w, r)
		return
	}

	switch {
	case len(parts) == 1:
		switch r.Method {
		case http.MethodGet:
			a.handleGetDocument(w, r, docID)
		case http.MethodPatch:
			a.handleUpdateDocument(w, r, docID)
		default:
			methodNotAllowed(w, http.MethodGet, http.MethodPatch)
		}
	case len(parts) == 2 && parts[1] == "source":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		a.handleDocumentSource(w, r, docID)
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

	file, header, err := r.FormFile("file")
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

	filename := strings.TrimSpace(filepath.Base(header.Filename))
	if filename == "." {
		filename = ""
	}

	docID, err := a.store.ReserveID(r.Context())
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, err.Error())
		return
	}

	cmdID, err := a.commands.ReserveCommandID(r.Context(), "doc", doccmd.SplitSubject)
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, err.Error())
		return
	}

	sourceRef := a.runtime.Blob().Ref("documents", formatDocumentID(docID), "source.pdf")
	if err := a.runtime.Blob().WriteRef(r.Context(), sourceRef, payload.Bytes()); err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("store uploaded document: %v", err))
		return
	}

	now := time.Now().UTC()
	doc := docstore.Document{
		ID:         docID,
		Filename:   filename,
		SourceRef:  sourceRef,
		Status:     "pending",
		DPI:        doccmd.DefaultDPI,
		QueryModel: defaultDocumentQueryModel,
		CreatedAt:  now,
		UpdatedAt:  now,
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
	msg.Header.Set("Nats-Msg-Id", strconv.FormatInt(cmdID, 10))

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

func (a *documentAPI) handleGetDocument(w http.ResponseWriter, r *http.Request, docID int64) {
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

func (a *documentAPI) handleUpdateDocument(w http.ResponseWriter, r *http.Request, docID int64) {
	var req updateDocumentRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeErrorJSON(w, http.StatusBadRequest, err.Error())
		return
	}

	queryModel, err := normalizeDocumentQueryModel(req.QueryModel)
	if err != nil {
		writeErrorJSON(w, http.StatusBadRequest, err.Error())
		return
	}

	doc, err := a.store.UpdateSettings(r.Context(), docID, queryModel, req.ClearBaseResponse)
	if errors.Is(err, docstore.ErrDocumentNotFound) {
		writeErrorJSON(w, http.StatusNotFound, "document not found")
		return
	}
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("update document: %v", err))
		return
	}

	a.logger.Info("document settings updated",
		"document_id", docID,
		"query_model", queryModel,
		"clear_base_response", req.ClearBaseResponse,
	)

	writeJSON(w, http.StatusOK, map[string]any{
		"document": presentDocument(doc),
	})
}

func (a *documentAPI) handleDocumentSource(w http.ResponseWriter, r *http.Request, docID int64) {
	doc, err := a.store.Get(r.Context(), docID)
	if errors.Is(err, docstore.ErrDocumentNotFound) {
		writeErrorJSON(w, http.StatusNotFound, "document not found")
		return
	}
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("get document: %v", err))
		return
	}

	if doc.SourceRef == "" {
		writeErrorJSON(w, http.StatusNotFound, "source not available")
		return
	}

	data, err := a.runtime.Blob().ReadRef(r.Context(), doc.SourceRef)
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("read source: %v", err))
		return
	}

	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (a *documentAPI) handleGetManifest(w http.ResponseWriter, r *http.Request, docID int64) {
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
		"id":          d.ID,
		"filename":    d.Filename,
		"source_ref":  d.SourceRef,
		"status":      d.Status,
		"page_count":  d.PageCount,
		"dpi":         d.DPI,
		"query_model": d.QueryModel,
		"created_at":  d.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":  d.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if d.Error != "" {
		entry["error"] = d.Error
	}
	if d.ManifestRef != "" {
		entry["manifest_ref"] = d.ManifestRef
	}
	if d.BaseResponseID != "" {
		entry["base_response_id"] = d.BaseResponseID
	}
	if d.BaseModel != "" {
		entry["base_model"] = d.BaseModel
	}
	if d.BaseInitializedAt != nil {
		entry["base_initialized_at"] = d.BaseInitializedAt.UTC().Format(time.RFC3339)
	}
	return entry
}

func normalizeDocumentQueryModel(raw string) (string, error) {
	model := strings.TrimSpace(raw)
	if model == "" {
		return "", fmt.Errorf("query_model is required")
	}
	if _, ok := allowedDocumentQueryModels[model]; !ok {
		return "", fmt.Errorf("unsupported query_model %q", model)
	}
	return model, nil
}

func parseDocumentID(raw string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid document id %q: %w", raw, err)
	}
	if id <= 0 {
		return 0, fmt.Errorf("document id must be greater than zero")
	}
	return id, nil
}

func formatDocumentID(id int64) string {
	return strconv.FormatInt(id, 10)
}
