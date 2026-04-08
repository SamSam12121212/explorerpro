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
	"time"

	"explorer/internal/postgresstore"
	"explorer/internal/threadevents"
	"explorer/internal/threadstore"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/nats-io/nats.go"
)

const (
	batchMaxEvents     = 10
	batchFlushInterval = 50 * time.Millisecond
	heartbeatInterval  = 20 * time.Second
	itemsBatchLimit    = int64(200)
)

var wsOriginPatterns = []string{
	"localhost:*",
	"127.0.0.1:*",
	"[::1]:*",
}

type clientConfig struct {
	logger    *slog.Logger
	js        nats.JetStreamContext
	store     *threadstore.Store
	pg        *postgresstore.Store
	threadID  string
	afterItem string
}

type client struct {
	cfg clientConfig
}

func newClient(cfg clientConfig) *client {
	return &client{cfg: cfg}
}

// serve upgrades the HTTP connection to a WebSocket and runs the event loop.
func (c *client) serve(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: wsOriginPatterns,
	})
	if err != nil {
		c.cfg.logger.Warn("failed to accept websocket", "error", err)
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "stream closed")

	ctx := r.Context()

	meta, preferPostgres, err := c.loadThread(ctx)
	if err != nil {
		c.cfg.logger.Warn("failed to load thread", "error", err)
		return
	}

	if err := c.writeSnapshot(ctx, conn, meta); err != nil {
		c.cfg.logger.Warn("failed to write initial snapshot", "error", err)
		return
	}

	sub, err := c.cfg.js.SubscribeSync(
		threadevents.Subject(c.cfg.threadID),
		nats.DeliverNew(),
		nats.AckExplicit(),
		nats.AckWait(30*time.Second),
	)
	if err != nil {
		c.cfg.logger.Warn("nats subscribe failed", "error", err)
		return
	}
	defer func() { _ = sub.Unsubscribe() }()

	c.runEventLoop(ctx, conn, sub, preferPostgres)
}

// runEventLoop is the primary NATS-driven loop.
func (c *client) runEventLoop(ctx context.Context, conn *websocket.Conn, sub *nats.Subscription, preferPostgres bool) {
	heartbeatTicker := time.NewTicker(heartbeatInterval)
	defer heartbeatTicker.Stop()

	batchTimer := time.NewTimer(batchFlushInterval)
	batchTimer.Stop()
	defer batchTimer.Stop()

	afterItem := c.cfg.afterItem
	var batch []json.RawMessage
	batchActive := false

	if err := c.writeItemsDelta(ctx, conn, &afterItem, preferPostgres); err != nil {
		c.logStreamError(err)
		return
	}

	for {
		select {
		case <-ctx.Done():
			return

		case <-heartbeatTicker.C:
			if err := wsjson.Write(ctx, conn, map[string]any{
				"type":      "thread.heartbeat",
				"thread_id": c.cfg.threadID,
				"time":      time.Now().UTC().Format(time.RFC3339),
			}); err != nil {
				c.logStreamError(err)
				return
			}

		case <-batchTimer.C:
			if err := c.flushPendingEventBatch(ctx, conn, &batch, &batchActive, batchTimer); err != nil {
				c.logStreamError(err)
				return
			}

		default:
			msg, err := sub.NextMsg(50 * time.Millisecond)
			if err != nil {
				if errors.Is(err, nats.ErrTimeout) {
					continue
				}
				if errors.Is(err, nats.ErrBadSubscription) {
					return
				}
				c.cfg.logger.Warn("nats receive error", "error", err)
				continue
			}

			_ = msg.Ack()

			env, err := threadevents.Decode(msg.Data)
			if err != nil {
				c.cfg.logger.Warn("failed to decode nats event", "error", err)
				continue
			}

			switch env.EventType {
			case threadevents.EventTypeThreadItem:
				if err := c.flushPendingEventBatch(ctx, conn, &batch, &batchActive, batchTimer); err != nil {
					c.logStreamError(err)
					return
				}
				if err := c.writeStreamItem(ctx, conn, &afterItem, env.Payload); err != nil {
					c.logStreamError(err)
					return
				}
			case threadevents.EventTypeThreadSnapshot:
				if err := c.flushPendingEventBatch(ctx, conn, &batch, &batchActive, batchTimer); err != nil {
					c.logStreamError(err)
					return
				}
				if err := c.writeStreamSnapshot(ctx, conn, env.Payload); err != nil {
					c.logStreamError(err)
					return
				}
			default:
				batch = append(batch, env.Payload)

				if !batchActive {
					batchTimer.Reset(batchFlushInterval)
					batchActive = true
				}

				if len(batch) >= batchMaxEvents || isTerminalEventType(env.EventType) {
					if err := c.flushPendingEventBatch(ctx, conn, &batch, &batchActive, batchTimer); err != nil {
						c.logStreamError(err)
						return
					}
				}
			}
		}
	}
}

func (c *client) loadThread(ctx context.Context) (threadstore.ThreadMeta, bool, error) {
	meta, err := c.cfg.pg.LoadThread(ctx, c.cfg.threadID)
	if err == nil {
		return meta, true, nil
	}
	if !errors.Is(err, threadstore.ErrThreadNotFound) {
		return threadstore.ThreadMeta{}, false, err
	}
	meta, err = c.cfg.store.LoadThread(ctx, c.cfg.threadID)
	if err != nil {
		return threadstore.ThreadMeta{}, false, err
	}
	return meta, false, nil
}

func (c *client) writeSnapshot(ctx context.Context, conn *websocket.Conn, meta threadstore.ThreadMeta) error {
	msg := map[string]any{
		"type":      "thread.snapshot",
		"thread_id": meta.ID,
		"thread":    presentThreadMeta(meta),
	}

	if owner, err := c.cfg.store.LoadOwner(ctx, meta.ID); err == nil {
		msg["owner"] = presentOwner(owner)
	}

	if meta.ActiveSpawnGroupID != "" {
		if spawn, err := c.loadSpawnGroup(ctx, meta.ActiveSpawnGroupID); err == nil {
			msg["active_spawn_group"] = presentSpawnGroup(spawn)
		}
	}

	return wsjson.Write(ctx, conn, msg)
}

func (c *client) loadSpawnGroup(ctx context.Context, id string) (threadstore.SpawnGroupMeta, error) {
	spawn, err := c.cfg.pg.LoadSpawnGroup(ctx, id)
	if err == nil {
		return spawn, nil
	}
	if !errors.Is(err, threadstore.ErrThreadNotFound) {
		return threadstore.SpawnGroupMeta{}, err
	}
	return c.cfg.store.LoadSpawnGroup(ctx, id)
}

func (c *client) writeItemsDelta(ctx context.Context, conn *websocket.Conn, afterItem *string, preferPostgres bool) error {
	options := threadstore.ListOptions{
		Limit: itemsBatchLimit,
		After: *afterItem,
	}

	var items []threadstore.ItemRecord
	var err error

	if preferPostgres && supportsPostgresCursor(*afterItem) {
		items, err = c.cfg.pg.ListItems(ctx, c.cfg.threadID, options)
		if err != nil {
			return err
		}
	}

	if len(items) == 0 {
		items, err = c.cfg.store.ListItems(ctx, c.cfg.threadID, options)
		if err != nil {
			return err
		}
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
	*afterItem = cursor

	return wsjson.Write(ctx, conn, map[string]any{
		"type":      "thread.items.delta",
		"thread_id": c.cfg.threadID,
		"items":     presented,
		"page": map[string]any{
			"limit":        itemsBatchLimit,
			"after":        options.After,
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
	StreamID   string          `json:"stream_id,omitempty"`
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

func (c *client) writeStreamItem(ctx context.Context, conn *websocket.Conn, afterItem *string, raw json.RawMessage) error {
	var item streamItemPayload
	if err := json.Unmarshal(raw, &item); err != nil {
		return err
	}

	cursor := strings.TrimSpace(item.Cursor)
	if cursor == "" {
		cursor = strconv.FormatInt(item.Seq, 10)
		item.Cursor = cursor
	}

	if after := parseCursor(*afterItem); after > 0 && item.Seq <= after {
		return nil
	}

	pageAfter := *afterItem
	*afterItem = cursor

	return wsjson.Write(ctx, conn, map[string]any{
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

func (c *client) writeStreamSnapshot(ctx context.Context, conn *websocket.Conn, raw json.RawMessage) error {
	var snapshot streamSnapshotPayload
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return err
	}

	threadID := strings.TrimSpace(snapshot.ID)
	if threadID == "" {
		threadID = c.cfg.threadID
	}

	return wsjson.Write(ctx, conn, map[string]any{
		"type":      "thread.snapshot",
		"thread_id": threadID,
		"thread":    snapshot,
	})
}

func (c *client) flushPendingEventBatch(
	ctx context.Context,
	conn *websocket.Conn,
	batch *[]json.RawMessage,
	batchActive *bool,
	batchTimer *time.Timer,
) error {
	if *batchActive {
		if !batchTimer.Stop() {
			select {
			case <-batchTimer.C:
			default:
			}
		}
	}

	if len(*batch) > 0 {
		if err := c.flushEventBatch(ctx, conn, *batch); err != nil {
			return err
		}
		*batch = (*batch)[:0]
	}
	*batchActive = false
	return nil
}

func (c *client) flushEventBatch(ctx context.Context, conn *websocket.Conn, events []json.RawMessage) error {
	decoded := make([]any, 0, len(events))
	for _, raw := range events {
		var v any
		if err := json.Unmarshal(raw, &v); err == nil {
			decoded = append(decoded, v)
		}
	}

	return wsjson.Write(ctx, conn, map[string]any{
		"type":      "thread.events.delta",
		"thread_id": c.cfg.threadID,
		"events":    decoded,
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

func isTerminalEventType(eventType string) bool {
	switch eventType {
	case "response.completed", "response.failed", "response.incomplete":
		return true
	}
	return false
}

func supportsPostgresCursor(raw string) bool {
	if strings.TrimSpace(raw) == "" {
		return true
	}
	_, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	return err == nil
}

func parseCursor(raw string) int64 {
	value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0
	}
	return value
}

// Presentation helpers — mirrors internal/httpserver/command_api.go presenters.

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
	if item.StreamID != "" {
		response["stream_id"] = item.StreamID
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
