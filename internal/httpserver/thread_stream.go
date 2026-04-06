package httpserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"explorer/internal/threadstore"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

const (
	threadStreamPollInterval      = 350 * time.Millisecond
	threadStreamSnapshotInterval  = 2 * time.Second
	threadStreamHeartbeatInterval = 20 * time.Second
	threadStreamBatchLimit        = int64(200)
)

var threadStreamOriginPatterns = []string{
	"localhost:*",
	"127.0.0.1:*",
	"[::1]:*",
}

type threadSnapshotMessage struct {
	Type             string         `json:"type"`
	ThreadID         string         `json:"thread_id"`
	Thread           map[string]any `json:"thread"`
	Owner            map[string]any `json:"owner,omitempty"`
	ActiveSpawnGroup map[string]any `json:"active_spawn_group,omitempty"`
}

type threadItemsDeltaMessage struct {
	Type     string           `json:"type"`
	ThreadID string           `json:"thread_id"`
	Items    []map[string]any `json:"items"`
	Page     map[string]any   `json:"page"`
}

type threadEventsDeltaMessage struct {
	Type     string           `json:"type"`
	ThreadID string           `json:"thread_id"`
	Events   []map[string]any `json:"events"`
	Page     map[string]any   `json:"page"`
}

type threadHeartbeatMessage struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	Time     string `json:"time"`
}

func (a *commandAPI) handleThreadConnect(w http.ResponseWriter, r *http.Request, threadID string) {
	meta, preferPostgres, err := a.loadThreadForRead(r.Context(), threadID)
	if err != nil {
		if errors.Is(err, threadstore.ErrThreadNotFound) {
			writeErrorJSON(w, http.StatusNotFound, fmt.Sprintf("thread %s not found", threadID))
			return
		}
		writeErrorJSON(w, http.StatusInternalServerError, fmt.Sprintf("load thread %s: %v", threadID, err))
		return
	}

	afterItem := strings.TrimSpace(r.URL.Query().Get("after_item"))
	if err := validateListCursor(afterItem); err != nil {
		writeErrorJSON(w, http.StatusBadRequest, fmt.Sprintf("invalid after_item: %v", err))
		return
	}

	afterEvent := strings.TrimSpace(r.URL.Query().Get("after_event"))
	if err := validateListCursor(afterEvent); err != nil {
		writeErrorJSON(w, http.StatusBadRequest, fmt.Sprintf("invalid after_event: %v", err))
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: threadStreamOriginPatterns,
	})
	if err != nil {
		a.logger.Warn("failed to accept thread websocket",
			"thread_id", threadID,
			"error", err,
		)
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "thread stream closed")

	ctx := r.Context()

	if err := a.writeThreadSnapshot(ctx, conn, meta); err != nil {
		a.logger.Warn("failed to write initial thread snapshot",
			"thread_id", threadID,
			"error", err,
		)
		return
	}

	pollTicker := time.NewTicker(threadStreamPollInterval)
	defer pollTicker.Stop()

	snapshotTicker := time.NewTicker(threadStreamSnapshotInterval)
	defer snapshotTicker.Stop()

	heartbeatTicker := time.NewTicker(threadStreamHeartbeatInterval)
	defer heartbeatTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-pollTicker.C:
			meta, err = a.loadAndWriteThreadDeltas(ctx, conn, threadID, preferPostgres, &afterItem, &afterEvent)
			if err != nil {
				a.logThreadStreamError(threadID, err)
				return
			}
		case <-snapshotTicker.C:
			meta, err = a.loadThreadForStream(ctx, threadID)
			if err != nil {
				a.logThreadStreamError(threadID, err)
				return
			}
			if err := a.writeThreadSnapshot(ctx, conn, meta); err != nil {
				a.logThreadStreamError(threadID, err)
				return
			}
		case <-heartbeatTicker.C:
			if err := wsjson.Write(ctx, conn, threadHeartbeatMessage{
				Type:     "thread.heartbeat",
				ThreadID: threadID,
				Time:     time.Now().UTC().Format(time.RFC3339),
			}); err != nil {
				a.logThreadStreamError(threadID, err)
				return
			}
		}
	}
}

func (a *commandAPI) loadAndWriteThreadDeltas(
	ctx context.Context,
	conn *websocket.Conn,
	threadID string,
	preferPostgres bool,
	afterItem *string,
	afterEvent *string,
) (threadstore.ThreadMeta, error) {
	items, err := a.listItemsForRead(ctx, threadID, listQuery{
		Limit: threadStreamBatchLimit,
		After: *afterItem,
	}, preferPostgres)
	if err != nil {
		return threadstore.ThreadMeta{}, err
	}

	if len(items) > 0 {
		presented := make([]map[string]any, 0, len(items))
		for _, item := range items {
			presented = append(presented, presentItem(item))
		}

		message := threadItemsDeltaMessage{
			Type:     "thread.items.delta",
			ThreadID: threadID,
			Items:    presented,
			Page: presentPage(listQuery{
				Limit: threadStreamBatchLimit,
				After: *afterItem,
			}, itemPageBounds(items), len(items)),
		}
		if err := wsjson.Write(ctx, conn, message); err != nil {
			return threadstore.ThreadMeta{}, err
		}

		*afterItem = strconvFormatInt(items[len(items)-1].Seq)
	}

	events, err := a.listEventsForRead(ctx, threadID, listQuery{
		Limit: threadStreamBatchLimit,
		After: *afterEvent,
	}, preferPostgres)
	if err != nil {
		return threadstore.ThreadMeta{}, err
	}

	if len(events) > 0 {
		presented := make([]map[string]any, 0, len(events))
		for _, event := range events {
			presented = append(presented, presentEvent(event))
		}

		message := threadEventsDeltaMessage{
			Type:     "thread.events.delta",
			ThreadID: threadID,
			Events:   presented,
			Page: presentPage(listQuery{
				Limit: threadStreamBatchLimit,
				After: *afterEvent,
			}, eventPageBounds(events), len(events)),
		}
		if err := wsjson.Write(ctx, conn, message); err != nil {
			return threadstore.ThreadMeta{}, err
		}

		*afterEvent = strconvFormatInt(events[len(events)-1].EventSeq)
	}

	if len(items) == 0 && len(events) == 0 {
		return threadstore.ThreadMeta{}, nil
	}

	meta, err := a.loadThreadForStream(ctx, threadID)
	if err != nil {
		return threadstore.ThreadMeta{}, err
	}
	if err := a.writeThreadSnapshot(ctx, conn, meta); err != nil {
		return threadstore.ThreadMeta{}, err
	}
	return meta, nil
}

func (a *commandAPI) loadThreadForStream(ctx context.Context, threadID string) (threadstore.ThreadMeta, error) {
	meta, _, err := a.loadThreadForRead(ctx, threadID)
	return meta, err
}

func (a *commandAPI) writeThreadSnapshot(ctx context.Context, conn *websocket.Conn, meta threadstore.ThreadMeta) error {
	message := threadSnapshotMessage{
		Type:     "thread.snapshot",
		ThreadID: meta.ID,
		Thread:   presentThreadMeta(meta),
	}

	if owner, err := a.store.LoadOwner(ctx, meta.ID); err == nil {
		message.Owner = presentOwner(owner)
	} else if !errors.Is(err, threadstore.ErrThreadNotFound) {
		return err
	}

	if meta.ActiveSpawnGroupID != "" {
		spawn, err := a.loadSpawnGroupForRead(ctx, meta.ActiveSpawnGroupID)
		if err != nil && !errors.Is(err, threadstore.ErrThreadNotFound) {
			return err
		}
		if err == nil {
			message.ActiveSpawnGroup = presentSpawnGroup(spawn)
		}
	}

	return wsjson.Write(ctx, conn, message)
}

func (a *commandAPI) logThreadStreamError(threadID string, err error) {
	status := websocket.CloseStatus(err)
	if status == websocket.StatusNormalClosure || status == websocket.StatusGoingAway {
		a.logger.Info("thread websocket closed",
			"thread_id", threadID,
			"status", status,
		)
		return
	}
	if errors.Is(err, context.Canceled) {
		return
	}
	a.logger.Warn("thread websocket stream ended",
		"thread_id", threadID,
		"error", err,
		"close_status", status,
	)
}

func strconvFormatInt(value int64) string {
	return fmt.Sprintf("%d", value)
}
