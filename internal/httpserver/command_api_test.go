package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"explorer/internal/threadstore"
)

type fakeEventHistoryStore struct {
	events []threadstore.EventRecord
	err    error
}

func (s fakeEventHistoryStore) ListEvents(_ context.Context, threadID string, options threadstore.ListOptions) ([]threadstore.EventRecord, error) {
	if s.err != nil {
		return nil, s.err
	}
	if threadID != "thread_123" {
		return nil, errors.New("unexpected thread id")
	}
	if options.Limit != 2 || options.After != "41" || options.Before != "" {
		return nil, errors.New("unexpected list options")
	}
	return s.events, nil
}

func TestNormalizeResponseInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  json.RawMessage
		want string
	}{
		{
			name: "array stays array",
			raw:  json.RawMessage(`[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]`),
			want: `[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]`,
		},
		{
			name: "object wraps into array",
			raw:  json.RawMessage(`{"type":"function_call_output","call_id":"call_123","output":{"ok":true}}`),
			want: `[{"call_id":"call_123","output":{"ok":true},"type":"function_call_output"}]`,
		},
		{
			name: "string becomes input_text message",
			raw:  json.RawMessage(`"hello world"`),
			want: `[{"content":[{"text":"hello world","type":"input_text"}],"role":"user","type":"message"}]`,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := normalizeResponseInput(tt.raw)
			if err != nil {
				t.Fatalf("normalizeResponseInput returned error: %v", err)
			}

			if string(got) != tt.want {
				t.Fatalf("normalizeResponseInput mismatch\nwant: %s\ngot:  %s", tt.want, string(got))
			}
		})
	}
}

func TestParseThreadRoute(t *testing.T) {
	t.Parallel()

	route, ok := parseThreadRoute("/threads/thread_123/commands")
	if !ok {
		t.Fatal("expected path to parse")
	}
	if route.ThreadID != "thread_123" {
		t.Fatalf("unexpected thread id: %s", route.ThreadID)
	}
	if route.Resource != "commands" {
		t.Fatalf("unexpected resource: %s", route.Resource)
	}

	route, ok = parseThreadRoute("/threads/thread_123")
	if !ok {
		t.Fatal("expected thread path to parse")
	}
	if route.ThreadID != "thread_123" || route.Resource != "" {
		t.Fatalf("unexpected route: %+v", route)
	}

	route, ok = parseThreadRoute("/threads/thread_123/responses/resp_123")
	if !ok {
		t.Fatal("expected response path to parse")
	}
	if route.ThreadID != "thread_123" || route.Resource != "responses" || route.ResourceID != "resp_123" {
		t.Fatalf("unexpected response route: %+v", route)
	}

	if _, ok := parseThreadRoute("/threads/thread_123/commands/extra"); ok {
		t.Fatal("expected invalid path to fail")
	}
	if _, ok := parseThreadRoute("/threads/thread_123/items/extra"); ok {
		t.Fatal("expected invalid items path to fail")
	}
	route, ok = parseThreadRoute("/threads/thread_123/spawn-groups/sg_123")
	if !ok {
		t.Fatal("expected spawn group path to parse")
	}
	if route.ThreadID != "thread_123" || route.Resource != "spawn-groups" || route.ResourceID != "sg_123" || route.Subresource != "" {
		t.Fatalf("unexpected spawn group route: %+v", route)
	}
	route, ok = parseThreadRoute("/threads/thread_123/spawn-groups/sg_123/results")
	if !ok {
		t.Fatal("expected spawn group results path to parse")
	}
	if route.ThreadID != "thread_123" || route.Resource != "spawn-groups" || route.ResourceID != "sg_123" || route.Subresource != "results" {
		t.Fatalf("unexpected spawn group results route: %+v", route)
	}
}

func TestParseListQuery(t *testing.T) {
	t.Parallel()

	t.Run("non numeric cursor is rejected", func(t *testing.T) {
		t.Parallel()

		req := httptest.NewRequest("GET", "/threads/thread_123/items?limit=999&after=123-0", nil)
		if _, err := parseListQuery(req); err == nil {
			t.Fatal("expected parseListQuery to reject non numeric cursor")
		}
	})

	t.Run("numeric cursor stays accepted", func(t *testing.T) {
		t.Parallel()

		req := httptest.NewRequest("GET", "/threads/thread_123/items?after=42", nil)
		query, err := parseListQuery(req)
		if err != nil {
			t.Fatalf("parseListQuery() error = %v", err)
		}
		if query.After != "42" {
			t.Fatalf("after = %q, want 42", query.After)
		}
	})

	t.Run("invalid numeric cursor is rejected", func(t *testing.T) {
		t.Parallel()

		req := httptest.NewRequest("GET", "/threads/thread_123/items?after=0", nil)
		if _, err := parseListQuery(req); err == nil {
			t.Fatal("expected parseListQuery to reject zero cursor")
		}
	})
}

func TestPresentItemUsesNormalizedCursor(t *testing.T) {
	t.Parallel()

	item := threadstore.ItemRecord{
		Seq:       7,
		ItemType:  "message",
		Direction: "output",
		CreatedAt: time.Date(2026, 3, 14, 9, 0, 0, 0, time.UTC),
		Payload:   json.RawMessage(`{"ok":true}`),
	}

	presented := presentItem(item)
	if got := stringJSON(presented); got != `{"created_at":"2026-03-14T09:00:00Z","cursor":"7","direction":"output","item_type":"message","payload":{"ok":true},"response_id":"","seq":7}` {
		t.Fatalf("presentItem() = %s", got)
	}
}

func TestPresentPageUsesCursorBounds(t *testing.T) {
	t.Parallel()

	bounds := itemPageBounds([]threadstore.ItemRecord{
		{Seq: 3},
		{Seq: 4},
	})
	page := presentPage(listQuery{Limit: 10, After: "2"}, bounds, 2)

	if got := stringJSON(page); got != `{"after":"2","before":"","count":2,"first_cursor":"3","last_cursor":"4","limit":10}` {
		t.Fatalf("presentPage() = %s", got)
	}
}

func TestNormalizeAttachedDocumentIDs(t *testing.T) {
	t.Parallel()

	t.Run("dedupes and trims", func(t *testing.T) {
		t.Parallel()

		got, err := normalizeAttachedDocumentIDs([]int64{1, 1, 2})
		if err != nil {
			t.Fatalf("normalizeAttachedDocumentIDs() error = %v", err)
		}
		if stringJSON(got) != `[1,2]` {
			t.Fatalf("got %s, want [1,2]", stringJSON(got))
		}
	})

	t.Run("rejects non-positive ids", func(t *testing.T) {
		t.Parallel()

		if _, err := normalizeAttachedDocumentIDs([]int64{1, 0}); err == nil {
			t.Fatal("expected invalid attached document id to fail")
		}
	})
}

func TestNormalizeResumeBodyPreservesAttachedDocumentIDs(t *testing.T) {
	t.Parallel()

	got, err := normalizeResumeBody(json.RawMessage(`{
		"input_items":"hello",
		"attached_document_ids":[1,2]
	}`))
	if err != nil {
		t.Fatalf("normalizeResumeBody() error = %v", err)
	}

	ids, err := extractAttachedDocumentIDs(got)
	if err != nil {
		t.Fatalf("extractAttachedDocumentIDs() error = %v", err)
	}
	if stringJSON(ids) != `[1,2]` {
		t.Fatalf("attached_document_ids = %s, want [1,2]", stringJSON(ids))
	}
}

func TestNormalizeResumeBodyNormalizesReasoning(t *testing.T) {
	t.Parallel()

	got, err := normalizeResumeBody(json.RawMessage(`{
		"input_items":"hello",
		"reasoning":{"effort":"high","summary":"detailed","generate_summary":"concise"}
	}`))
	if err != nil {
		t.Fatalf("normalizeResumeBody() error = %v", err)
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(got, &payload); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	var reasoning map[string]any
	if err := json.Unmarshal(payload["reasoning"], &reasoning); err != nil {
		t.Fatalf("json.Unmarshal(reasoning) error = %v", err)
	}

	if reasoning["effort"] != "high" {
		t.Fatalf("effort = %v, want high", reasoning["effort"])
	}
	if _, exists := reasoning["summary"]; exists {
		t.Fatalf("summary should be omitted, got %#v", reasoning["summary"])
	}
	if _, exists := reasoning["generate_summary"]; exists {
		t.Fatalf("generate_summary should be omitted, got %#v", reasoning["generate_summary"])
	}
}

func TestNormalizeResumeBodyRejectsPreparedInputRef(t *testing.T) {
	t.Parallel()

	_, err := normalizeResumeBody(json.RawMessage(`{
		"prepared_input_ref":"blob://prepared-inputs/pi_456.json"
	}`))
	if err == nil {
		t.Fatal("expected normalizeResumeBody() to reject prepared_input_ref")
	}
	if !strings.Contains(err.Error(), "internal-only") {
		t.Fatalf("error = %v, want internal-only message", err)
	}
}

func TestListEventsForReadUsesHistoryStore(t *testing.T) {
	t.Parallel()

	api := &commandAPI{
		history: fakeEventHistoryStore{
			events: []threadstore.EventRecord{
				{EventSeq: 42, EventType: "client.response.create", CreatedAt: time.Unix(42, 0).UTC()},
				{EventSeq: 43, EventType: "response.completed", CreatedAt: time.Unix(43, 0).UTC()},
			},
		},
	}

	events, err := api.listEventsForRead(context.Background(), "thread_123", listQuery{
		Limit: 2,
		After: "41",
	})
	if err != nil {
		t.Fatalf("listEventsForRead() error = %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}
	if events[0].EventSeq != 42 || events[1].EventSeq != 43 {
		t.Fatalf("events = %#v, want sequences 42 and 43", events)
	}
}

func stringJSON(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(data)
}
