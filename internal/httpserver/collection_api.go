package httpserver

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"explorer/internal/collectionstore"
	"explorer/internal/idgen"
	"explorer/internal/platform"
)

const maxCollectionNameLength = 120

type collectionAPI struct {
	logger  *slog.Logger
	runtime *platform.Runtime
	store   *collectionstore.Store
}

type createCollectionRequest struct {
	Name string `json:"name"`
}

func newCollectionAPI(logger *slog.Logger, runtime *platform.Runtime) *collectionAPI {
	return &collectionAPI{
		logger:  logger,
		runtime: runtime,
		store:   collectionstore.New(runtime.Postgres().Pool()),
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
	trimmed := strings.Trim(strings.TrimPrefix(r.URL.Path, "/collections/"), "/")
	if trimmed == "" || strings.Contains(trimmed, "/") {
		http.NotFound(w, r)
		return
	}

	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}

	a.handleGetCollection(w, r, trimmed)
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

	writeJSON(w, http.StatusOK, map[string]any{
		"collection": presentCollection(collection),
	})
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
