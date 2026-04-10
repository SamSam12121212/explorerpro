package httpserver

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"explorer/internal/threadstore"
)

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

	t.Run("legacy stream cursor stays accepted", func(t *testing.T) {
		t.Parallel()

		req := httptest.NewRequest("GET", "/threads/thread_123/items?limit=999&after=123-0", nil)
		query, err := parseListQuery(req)
		if err != nil {
			t.Fatalf("parseListQuery() error = %v", err)
		}

		if query.Limit != 500 {
			t.Fatalf("limit = %d, want 500", query.Limit)
		}
		if query.After != "123-0" {
			t.Fatalf("after = %q, want 123-0", query.After)
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
		StreamID:  "1740000000000-0",
		ItemType:  "message",
		Direction: "output",
		CreatedAt: time.Date(2026, 3, 14, 9, 0, 0, 0, time.UTC),
		Payload:   json.RawMessage(`{"ok":true}`),
	}

	presented := presentItem(item)
	if presented["cursor"] != "7" {
		t.Fatalf("cursor = %v, want 7", presented["cursor"])
	}
	if presented["stream_id"] != "1740000000000-0" {
		t.Fatalf("stream_id = %v, want Redis stream id", presented["stream_id"])
	}
}

func TestPresentPageUsesCursorBounds(t *testing.T) {
	t.Parallel()

	bounds := itemPageBounds([]threadstore.ItemRecord{
		{Seq: 3, StreamID: "1740000000000-0"},
		{Seq: 4, StreamID: "1740000000100-0"},
	})
	page := presentPage(listQuery{Limit: 10, After: "2"}, bounds, 2)

	if page["first_cursor"] != "3" {
		t.Fatalf("first_cursor = %v, want 3", page["first_cursor"])
	}
	if page["last_cursor"] != "4" {
		t.Fatalf("last_cursor = %v, want 4", page["last_cursor"])
	}
	if page["first_stream_id"] != "1740000000000-0" {
		t.Fatalf("first_stream_id = %v, want first Redis stream id", page["first_stream_id"])
	}
}

func TestNormalizeAttachedDocumentIDs(t *testing.T) {
	t.Parallel()

	t.Run("dedupes and trims", func(t *testing.T) {
		t.Parallel()

		got, err := normalizeAttachedDocumentIDs([]string{" doc_1 ", "doc_1", "doc_2"})
		if err != nil {
			t.Fatalf("normalizeAttachedDocumentIDs() error = %v", err)
		}
		if stringJSON(got) != `["doc_1","doc_2"]` {
			t.Fatalf("got %s, want [\"doc_1\",\"doc_2\"]", stringJSON(got))
		}
	})

	t.Run("rejects blank ids", func(t *testing.T) {
		t.Parallel()

		if _, err := normalizeAttachedDocumentIDs([]string{"doc_1", " "}); err == nil {
			t.Fatal("expected blank attached document id to fail")
		}
	})
}

func TestNormalizeResumeBodyPreservesAttachedDocumentIDs(t *testing.T) {
	t.Parallel()

	got, err := normalizeResumeBody(json.RawMessage(`{
		"input_items":"hello",
		"attached_document_ids":["doc_1","doc_2"]
	}`))
	if err != nil {
		t.Fatalf("normalizeResumeBody() error = %v", err)
	}

	ids, err := extractAttachedDocumentIDs(got)
	if err != nil {
		t.Fatalf("extractAttachedDocumentIDs() error = %v", err)
	}
	if stringJSON(ids) != `["doc_1","doc_2"]` {
		t.Fatalf("attached_document_ids = %s, want [\"doc_1\",\"doc_2\"]", stringJSON(ids))
	}
}

func stringJSON(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(data)
}
