package wsserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"explorer/internal/docstore"
	"explorer/internal/postgresstore"
	"explorer/internal/threaddocstore"
	"explorer/internal/threadevents"
	"explorer/internal/threadstore"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

const (
	heartbeatInterval = 20 * time.Second
	itemsBatchLimit   = int64(200)
	writeTimeout      = 10 * time.Second
)

var wsOriginPatterns = []string{
	"localhost:*",
	"127.0.0.1:*",
	"[::1]:*",
}

type clientConfig struct {
	logger    *slog.Logger
	store     *postgresstore.Store
	docs      *threaddocstore.Store
	threadID  string
	afterItem string
}

type client struct {
	cfg clientConfig

	mu        sync.Mutex
	ctx       context.Context
	cancel    context.CancelFunc
	conn      *websocket.Conn
	afterItem string
	closeOnce sync.Once
}

type clientPayloadError struct {
	err error
}

func (e clientPayloadError) Error() string {
	return e.err.Error()
}

func (e clientPayloadError) Unwrap() error {
	return e.err
}

func newClient(cfg clientConfig) *client {
	return &client{
		cfg:       cfg,
		afterItem: cfg.afterItem,
	}
}

func (c *client) serve(w http.ResponseWriter, r *http.Request, hub *eventHub) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: wsOriginPatterns,
	})
	if err != nil {
		c.cfg.logger.Warn("failed to accept websocket", "error", err)
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	c.ctx = ctx
	c.cancel = cancel
	c.conn = conn
	defer c.close(websocket.StatusNormalClosure, "stream closed")

	meta, err := c.loadThread(ctx)
	if err != nil {
		c.cfg.logger.Warn("failed to load thread", "error", err)
		return
	}

	if err := c.writeSnapshot(ctx, meta); err != nil {
		c.cfg.logger.Warn("failed to write initial snapshot", "error", err)
		return
	}

	if err := c.writeItemsDelta(ctx); err != nil {
		c.logStreamError(err)
		return
	}

	unregister := hub.register(c.cfg.threadID, c)
	defer unregister()

	// Close the registration gap with one last catch-up pass before we idle on heartbeats.
	if err := c.writeItemsDelta(ctx); err != nil {
		c.logStreamError(err)
		return
	}

	c.run()
}

func (c *client) run() {
	heartbeatTicker := time.NewTicker(heartbeatInterval)
	defer heartbeatTicker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-heartbeatTicker.C:
			if err := c.writeHeartbeat(); err != nil {
				c.logStreamError(err)
				return
			}
		}
	}
}

func (c *client) handleEvent(env threadevents.EventEnvelope) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	switch env.EventType {
	case threadevents.EventTypeThreadItem:
		return c.writeStreamItemLocked(env.Payload)
	case threadevents.EventTypeThreadSnapshot:
		return c.writeStreamSnapshotLocked(env.Payload)
	default:
		return c.writeStreamEventLocked(env.Payload)
	}
}

func (c *client) loadThread(ctx context.Context) (threadstore.ThreadMeta, error) {
	return c.cfg.store.LoadThread(ctx, c.cfg.threadID)
}

func (c *client) writeSnapshot(ctx context.Context, meta threadstore.ThreadMeta) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	attachedDocuments, err := c.loadAttachedDocuments(ctx, meta.ID)
	if err != nil {
		return err
	}

	msg := map[string]any{
		"type":               "thread.snapshot",
		"thread_id":          meta.ID,
		"thread":             presentThreadMeta(meta),
		"attached_documents": attachedDocuments,
	}

	if owner, err := c.cfg.store.LoadOwner(ctx, meta.ID); err == nil {
		msg["owner"] = presentOwner(owner)
	}

	if meta.ActiveSpawnGroupID != "" {
		if spawn, err := c.loadSpawnGroup(ctx, meta.ActiveSpawnGroupID); err == nil {
			msg["active_spawn_group"] = presentSpawnGroup(spawn)
		}
	}

	return c.writeJSONLocked(msg)
}

func (c *client) loadSpawnGroup(ctx context.Context, id string) (threadstore.SpawnGroupMeta, error) {
	return c.cfg.store.LoadSpawnGroup(ctx, id)
}

func (c *client) writeItemsDelta(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	options := threadstore.ListOptions{
		Limit: itemsBatchLimit,
		After: c.afterItem,
	}
	items, err := c.cfg.store.ListItems(ctx, c.cfg.threadID, options)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return nil
	}

	presented := make([]map[string]any, 0, len(items))
	for _, item := range items {
		presented = append(presented, presentItem(item))
	}

	last := items[len(items)-1]
	cursor := strconv.FormatInt(last.Seq, 10)
	pageAfter := c.afterItem
	c.afterItem = cursor

	return c.writeJSONLocked(map[string]any{
		"type":      "thread.items.delta",
		"thread_id": c.cfg.threadID,
		"items":     presented,
		"page": map[string]any{
			"limit":        itemsBatchLimit,
			"after":        pageAfter,
			"count":        len(items),
			"first_cursor": strconv.FormatInt(items[0].Seq, 10),
			"last_cursor":  cursor,
		},
	})
}

type streamItemPayload struct {
	Cursor     string          `json:"cursor"`
	Seq        int64           `json:"seq"`
	ResponseID string          `json:"response_id,omitempty"`
	ItemType   string          `json:"item_type"`
	Direction  string          `json:"direction"`
	CreatedAt  string          `json:"created_at"`
	Payload    json.RawMessage `json:"payload,omitempty"`
}

type streamSnapshotPayload struct {
	ID                 string `json:"id"`
	Status             string `json:"status,omitempty"`
	Model              string `json:"model,omitempty"`
	LastResponseID     string `json:"last_response_id,omitempty"`
	ActiveResponseID   string `json:"active_response_id,omitempty"`
	ActiveSpawnGroupID string `json:"active_spawn_group_id,omitempty"`
	UpdatedAt          string `json:"updated_at,omitempty"`
}

func (c *client) writeStreamItemLocked(raw json.RawMessage) error {
	var item streamItemPayload
	if err := json.Unmarshal(raw, &item); err != nil {
		return clientPayloadError{err: err}
	}

	cursor := strings.TrimSpace(item.Cursor)
	if cursor == "" {
		cursor = strconv.FormatInt(item.Seq, 10)
		item.Cursor = cursor
	}

	if after := parseCursor(c.afterItem); after > 0 && item.Seq <= after {
		return nil
	}

	pageAfter := c.afterItem
	c.afterItem = cursor

	return c.writeJSONLocked(map[string]any{
		"type":      "thread.items.delta",
		"thread_id": c.cfg.threadID,
		"items":     []streamItemPayload{item},
		"page": map[string]any{
			"limit":        itemsBatchLimit,
			"after":        pageAfter,
			"count":        1,
			"first_cursor": cursor,
			"last_cursor":  cursor,
		},
	})
}

func (c *client) writeStreamSnapshotLocked(raw json.RawMessage) error {
	var snapshot streamSnapshotPayload
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return clientPayloadError{err: err}
	}

	threadID := strings.TrimSpace(snapshot.ID)
	if threadID == "" {
		threadID = c.cfg.threadID
	}

	attachedDocuments, err := c.loadAttachedDocuments(c.ctx, threadID)
	if err != nil {
		return err
	}

	return c.writeJSONLocked(map[string]any{
		"type":               "thread.snapshot",
		"thread_id":          threadID,
		"thread":             snapshot,
		"attached_documents": attachedDocuments,
	})
}

func (c *client) loadAttachedDocuments(ctx context.Context, threadID string) ([]map[string]any, error) {
	if c.cfg.docs == nil {
		return []map[string]any{}, nil
	}

	documents, err := c.cfg.docs.ListDocuments(ctx, threadID, 200)
	if err != nil {
		return nil, err
	}

	return presentAttachedDocuments(documents), nil
}

func (c *client) writeStreamEventLocked(raw json.RawMessage) error {
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return clientPayloadError{err: err}
	}

	return c.writeJSONLocked(map[string]any{
		"type":      "thread.events.delta",
		"thread_id": c.cfg.threadID,
		"events":    []any{decoded},
	})
}

func (c *client) writeHeartbeat() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.writeJSONLocked(map[string]any{
		"type":      "thread.heartbeat",
		"thread_id": c.cfg.threadID,
		"time":      time.Now().UTC().Format(time.RFC3339),
	})
}

func (c *client) writeJSONLocked(payload any) error {
	writeCtx, cancel := context.WithTimeout(c.ctx, writeTimeout)
	defer cancel()
	return wsjson.Write(writeCtx, c.conn, payload)
}

func (c *client) close(code websocket.StatusCode, reason string) {
	c.closeOnce.Do(func() {
		if c.cancel != nil {
			c.cancel()
		}
		if c.conn != nil {
			_ = c.conn.Close(code, reason)
		}
	})
}

func (c *client) logStreamError(err error) {
	status := websocket.CloseStatus(err)
	if status == websocket.StatusNormalClosure || status == websocket.StatusGoingAway {
		return
	}
	if errors.Is(err, context.Canceled) {
		return
	}
	c.cfg.logger.Warn("websocket stream error", "error", err)
}

func websocketCloseStatus(err error) websocket.StatusCode {
	status := websocket.CloseStatus(err)
	if status != -1 {
		return status
	}
	if errors.Is(err, context.Canceled) {
		return websocket.StatusGoingAway
	}
	return websocket.StatusInternalError
}

func parseCursor(raw string) int64 {
	value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0
	}
	return value
}

func presentThreadMeta(meta threadstore.ThreadMeta) map[string]any {
	response := map[string]any{
		"id":                    meta.ID,
		"root_thread_id":        meta.RootThreadID,
		"parent_thread_id":      meta.ParentThreadID,
		"parent_call_id":        meta.ParentCallID,
		"depth":                 meta.Depth,
		"status":                meta.Status,
		"model":                 meta.Model,
		"instructions":          meta.Instructions,
		"owner_worker_id":       meta.OwnerWorkerID,
		"socket_generation":     meta.SocketGeneration,
		"last_response_id":      meta.LastResponseID,
		"active_response_id":    meta.ActiveResponseID,
		"active_spawn_group_id": meta.ActiveSpawnGroupID,
		"created_at":            meta.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":            meta.UpdatedAt.UTC().Format(time.RFC3339),
	}

	if !meta.SocketExpiresAt.IsZero() {
		response["socket_expires_at"] = meta.SocketExpiresAt.UTC().Format(time.RFC3339)
	}

	if decoded, err := decodeOptionalJSON(meta.MetadataJSON); err == nil && decoded != nil {
		response["metadata"] = decoded
	}
	if decoded, err := decodeOptionalJSON(meta.ToolsJSON); err == nil && decoded != nil {
		response["tools"] = decoded
	}
	if decoded, err := decodeOptionalJSON(meta.ToolChoiceJSON); err == nil && decoded != nil {
		response["tool_choice"] = decoded
	}

	return response
}

func presentAttachedDocuments(documents []docstore.Document) []map[string]any {
	presented := make([]map[string]any, 0, len(documents))
	for _, document := range documents {
		presented = append(presented, map[string]any{
			"id":         document.ID,
			"filename":   document.Filename,
			"status":     document.Status,
			"page_count": document.PageCount,
		})
	}
	return presented
}

func presentOwner(owner threadstore.OwnerRecord) map[string]any {
	response := map[string]any{
		"worker_id":         owner.WorkerID,
		"socket_generation": owner.SocketGeneration,
	}
	if !owner.LeaseUntil.IsZero() {
		response["lease_until"] = owner.LeaseUntil.UTC().Format(time.RFC3339)
	}
	if !owner.ClaimedAt.IsZero() {
		response["claimed_at"] = owner.ClaimedAt.UTC().Format(time.RFC3339)
	}
	if !owner.UpdatedAt.IsZero() {
		response["updated_at"] = owner.UpdatedAt.UTC().Format(time.RFC3339)
	}
	return response
}

func presentSpawnGroup(spawn threadstore.SpawnGroupMeta) map[string]any {
	response := map[string]any{
		"id":                     spawn.ID,
		"parent_thread_id":       spawn.ParentThreadID,
		"parent_call_id":         spawn.ParentCallID,
		"expected":               spawn.Expected,
		"completed":              spawn.Completed,
		"failed":                 spawn.Failed,
		"cancelled":              spawn.Cancelled,
		"status":                 spawn.Status,
		"aggregate_cmd_id":       spawn.AggregateCmdID,
		"created_at":             spawn.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":             spawn.UpdatedAt.UTC().Format(time.RFC3339),
		"aggregate_submitted_at": "",
	}
	if !spawn.AggregateSubmittedAt.IsZero() {
		response["aggregate_submitted_at"] = spawn.AggregateSubmittedAt.UTC().Format(time.RFC3339)
	}
	return response
}

func presentItem(item threadstore.ItemRecord) map[string]any {
	response := map[string]any{
		"cursor":      strconv.FormatInt(item.Seq, 10),
		"seq":         item.Seq,
		"response_id": item.ResponseID,
		"item_type":   item.ItemType,
		"direction":   item.Direction,
		"created_at":  item.CreatedAt.UTC().Format(time.RFC3339),
	}
	if decoded, err := decodeRawJSON(item.Payload); err == nil && decoded != nil {
		response["payload"] = decoded
	}
	return response
}

func decodeOptionalJSON(raw string) (any, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	return decodeRawJSON(json.RawMessage(raw))
}

func decodeRawJSON(raw json.RawMessage) (any, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, nil
	}
	var decoded any
	if err := json.Unmarshal(trimmed, &decoded); err != nil {
		return string(trimmed), nil
	}
	return decoded, nil
}
