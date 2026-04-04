package httpserver

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"explorer/internal/gitcmd"
	"explorer/internal/idgen"
	"explorer/internal/platform"
	"explorer/internal/repostore"

	"github.com/nats-io/nats.go"
)

type repoAPI struct {
	logger  *slog.Logger
	runtime *platform.Runtime
	store   *repostore.Store
}

type addRepoRequest struct {
	URL string `json:"url"`
	Ref string `json:"ref"`
}

func newRepoAPI(logger *slog.Logger, runtime *platform.Runtime) *repoAPI {
	return &repoAPI{
		logger:  logger,
		runtime: runtime,
		store:   repostore.New(runtime.Postgres().Pool()),
	}
}

func (a *repoAPI) handleRepos(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.handleListRepos(w, r)
	case http.MethodPost:
		a.handleAddRepo(w, r)
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

func (a *repoAPI) handleListRepos(w http.ResponseWriter, r *http.Request) {
	repos, err := a.store.List(r.Context(), 100)
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("list repos: %v", err))
		return
	}

	presented := make([]map[string]any, 0, len(repos))
	for _, repo := range repos {
		presented = append(presented, presentRepo(repo))
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"repos": presented,
		"count": len(presented),
	})
}

func (a *repoAPI) handleAddRepo(w http.ResponseWriter, r *http.Request) {
	var req addRepoRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeErrorJSON(w, http.StatusBadRequest, err.Error())
		return
	}

	repoURL := strings.TrimSpace(req.URL)
	if repoURL == "" {
		writeErrorJSON(w, http.StatusBadRequest, "url is required")
		return
	}

	parsed, err := url.Parse(repoURL)
	if err != nil || parsed.Host == "" {
		writeErrorJSON(w, http.StatusBadRequest, "invalid url")
		return
	}

	repoName := strings.TrimSuffix(path.Base(parsed.Path), ".git")
	if repoName == "" || repoName == "." {
		writeErrorJSON(w, http.StatusBadRequest, "could not determine repo name from url")
		return
	}

	ref := strings.TrimSpace(req.Ref)
	if ref == "" {
		ref = "main"
	}

	repoID, err := idgen.New("repo")
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, err.Error())
		return
	}

	cmdID, err := idgen.New("cmd")
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, err.Error())
		return
	}

	now := time.Now().UTC()
	repo := repostore.Repo{
		ID:        repoID,
		URL:       repoURL,
		Ref:       ref,
		Name:      repoName,
		Status:    "cloning",
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := a.store.Create(r.Context(), repo); err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("create repo: %v", err))
		return
	}

	cloneCmd := gitcmd.CloneCommand{
		CmdID:  cmdID,
		RepoID: repoID,
		URL:    repoURL,
		Ref:    ref,
		Name:   repoName,
	}

	payload, err := json.Marshal(cloneCmd)
	if err != nil {
		writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("marshal clone command: %v", err))
		return
	}

	msg := &nats.Msg{
		Subject: gitcmd.CloneSubject,
		Header:  nats.Header{},
		Data:    payload,
	}
	msg.Header.Set("Nats-Msg-Id", cmdID)

	if _, err := a.runtime.NATS().JetStream().PublishMsg(msg); err != nil {
		writeErrorJSON(w, http.StatusServiceUnavailable, fmt.Sprintf("publish clone command: %v", err))
		return
	}

	a.logger.Info("repo clone requested",
		"repo_id", repoID,
		"url", repoURL,
		"ref", ref,
		"cmd_id", cmdID,
	)

	writeJSON(w, http.StatusAccepted, map[string]any{
		"repo":   presentRepo(repo),
		"cmd_id": cmdID,
	})
}

func presentRepo(r repostore.Repo) map[string]any {
	entry := map[string]any{
		"id":         r.ID,
		"url":        r.URL,
		"ref":        r.Ref,
		"name":       r.Name,
		"status":     r.Status,
		"created_at": r.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at": r.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if r.Error != "" {
		entry["error"] = r.Error
	}
	if r.CommitSHA != "" {
		entry["commit_sha"] = r.CommitSHA
	}
	return entry
}
