package httpserver

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"explorer/internal/collectionstore"
	"explorer/internal/docstore"
	"explorer/internal/idgen"
	"explorer/internal/platform"
)

const maxCollectionNameLength = 120

type collectionAPI struct {
	logger  *slog.Logger
	runtime *platform.Runtime
	store   *collectionstore.Store
	docs    *docstore.Store
}

type createCollectionRequest struct {
	Name string `json:"name"`
}

type addCollectionDocumentRequest struct {
	DocumentID int64 `json:"document_id"`
}

type collectionRoute struct {
	CollectionID string
	Resource     string
}

func newCollectionAPI(logger *slog.Logger, runtime *platform.Runtime) *collectionAPI {
	return &collectionAPI{
		logger:  logger,
		runtime: runtime,
		store:   collectionstore.New(runtime.Postgres().Pool()),
		docs:    docstore.New(runtime.Postgres().Pool()),
	}
}

func (a *collectionAPI) handleCollections(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.handleListCollections(w, r)
	case http.MethodPost:
		a.handleCreateCollection(w, r)
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

func (a *collectionAPI) handleCollectionRoutes(w http.ResponseWriter, r *http.Request) {
	route, ok := parseCollectionRoute(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}

	switch route.Resource {
	case "":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, http.MethodGet)
			return
		}
		a.handleGetCollection(w, r, route.CollectionID)
	case "documents":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		a.handleAddCollectionDocument(w, r, route.CollectionID)
	default:
		http.NotFound(w, r)
	}
}

func (a *collectionAPI) handleListCollections(w http.ResponseWriter, r *http.Request) {
	collections, err := a.store.List(r.Context(), 100)
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("list collections: %v", err))
		return
	}

	presented := make([]map[string]any, 0, len(collections))
	for _, collection := range collections {
		presented = append(presented, presentCollection(collection))
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"collections": presented,
		"count":       len(presented),
	})
}

func (a *collectionAPI) handleCreateCollection(w http.ResponseWriter, r *http.Request) {
	var req createCollectionRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeErrorJSON(w, http.StatusBadRequest, err.Error())
		return
	}

	name := strings.TrimSpace(req.Name)
	switch {
	case name == "":
		writeErrorJSON(w, http.StatusBadRequest, "name is required")
		return
	case len(name) > maxCollectionNameLength:
		writeErrorJSON(w, http.StatusBadRequest, fmt.Sprintf("name must be %d characters or fewer", maxCollectionNameLength))
		return
	}

	collectionID, err := idgen.New("col")
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, err.Error())
		return
	}

	now := time.Now().UTC()
	collection := collectionstore.Collection{
		ID:        collectionID,
		Name:      name,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := a.store.Create(r.Context(), collection); err != nil {
		if errors.Is(err, collectionstore.ErrCollectionNameExists) {
			writeErrorJSON(w, http.StatusConflict, "collection name already exists")
			return
		}
		writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("create collection: %v", err))
		return
	}

	a.logger.Info("collection created",
		"collection_id", collectionID,
		"name", name,
	)

	writeJSON(w, http.StatusCreated, map[string]any{
		"collection": presentCollection(collection),
	})
}

func (a *collectionAPI) handleGetCollection(w http.ResponseWriter, r *http.Request, collectionID string) {
	collection, err := a.store.Get(r.Context(), collectionID)
	if errors.Is(err, collectionstore.ErrCollectionNotFound) {
		writeErrorJSON(w, http.StatusNotFound, "collection not found")
		return
	}
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("get collection: %v", err))
		return
	}

	documents, err := a.store.ListDocuments(r.Context(), collectionID, 100)
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("list collection documents: %v", err))
		return
	}

	presentedDocuments := make([]map[string]any, 0, len(documents))
	for _, document := range documents {
		presentedDocuments = append(presentedDocuments, presentDocument(document))
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"collection": presentCollection(collection),
		"documents":  presentedDocuments,
	})
}

func (a *collectionAPI) handleAddCollectionDocument(w http.ResponseWriter, r *http.Request, collectionID string) {
	var req addCollectionDocumentRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeErrorJSON(w, http.StatusBadRequest, err.Error())
		return
	}

	documentID := req.DocumentID
	if documentID <= 0 {
		writeErrorJSON(w, http.StatusBadRequest, "document_id is required")
		return
	}

	if _, err := a.store.Get(r.Context(), collectionID); errors.Is(err, collectionstore.ErrCollectionNotFound) {
		writeErrorJSON(w, http.StatusNotFound, "collection not found")
		return
	} else if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("get collection: %v", err))
		return
	}

	if _, err := a.docs.Get(r.Context(), documentID); errors.Is(err, docstore.ErrDocumentNotFound) {
		writeErrorJSON(w, http.StatusNotFound, "document not found")
		return
	} else if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("get document: %v", err))
		return
	}

	if err := a.store.AddDocument(r.Context(), collectionID, documentID); err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("add document to collection: %v", err))
		return
	}

	collection, err := a.store.Get(r.Context(), collectionID)
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("get updated collection: %v", err))
		return
	}
	documents, err := a.store.ListDocuments(r.Context(), collectionID, 100)
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("list updated collection documents: %v", err))
		return
	}

	presentedDocuments := make([]map[string]any, 0, len(documents))
	for _, document := range documents {
		presentedDocuments = append(presentedDocuments, presentDocument(document))
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"collection": presentCollection(collection),
		"documents":  presentedDocuments,
	})
}

func parseCollectionRoute(path string) (collectionRoute, bool) {
	var trimmed string
	switch {
	case strings.HasPrefix(path, "/collections/"):
		trimmed = strings.Trim(strings.TrimPrefix(path, "/collections/"), "/")
	case strings.HasPrefix(path, "/api/collections/"):
		trimmed = strings.Trim(strings.TrimPrefix(path, "/api/collections/"), "/")
	default:
		return collectionRoute{}, false
	}

	if trimmed == "" {
		return collectionRoute{}, false
	}

	parts := strings.Split(trimmed, "/")
	if len(parts) == 0 || parts[0] == "" || len(parts) > 2 {
		return collectionRoute{}, false
	}

	route := collectionRoute{CollectionID: parts[0]}
	if len(parts) == 2 {
		route.Resource = parts[1]
	}
	return route, true
}

func presentCollection(collection collectionstore.Collection) map[string]any {
	return map[string]any{
		"id":             collection.ID,
		"name":           collection.Name,
		"document_count": collection.DocumentCount,
		"created_at":     collection.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":     collection.UpdatedAt.UTC().Format(time.RFC3339),
	}
}
