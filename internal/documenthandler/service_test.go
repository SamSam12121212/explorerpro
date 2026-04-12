package documenthandler

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"explorer/internal/blobstore"
	"explorer/internal/doccmd"
	"explorer/internal/docsplitter"
	"explorer/internal/docstore"
	"explorer/internal/preparedinput"
)

func TestPrepareInputBuildsWarmupArtifact(t *testing.T) {
	ctx := context.Background()
	blob := newTestBlobStore(t)

	imageRef := blob.Ref("documents", "doc_123", "pages", "page-0001.png")
	if err := blob.WriteRef(ctx, imageRef, []byte("png")); err != nil {
		t.Fatalf("WriteRef(image) error = %v", err)
	}

	manifestJSON, err := json.Marshal(docsplitter.Manifest{
		DocumentID: "doc_123",
		Filename:   "report.pdf",
		PageCount:  1,
		Pages: []docsplitter.PageEntry{
			{PageNumber: 1, ImageRef: imageRef, ContentType: "image/png"},
		},
	})
	if err != nil {
		t.Fatalf("json.Marshal(manifest) error = %v", err)
	}

	manifestRef := blob.Ref("documents", "doc_123", "manifest.json")
	if err := blob.WriteRef(ctx, manifestRef, manifestJSON); err != nil {
		t.Fatalf("WriteRef(manifest) error = %v", err)
	}

	svc := New(nil, nil, nil, &fakeDocStore{
		documents: map[string]docstore.Document{
			"doc_123": {
				ID:          "doc_123",
				Filename:    "report.pdf",
				ManifestRef: manifestRef,
				PageCount:   1,
			},
		},
	}, blob)
	svc.now = func() time.Time { return time.Date(2026, 4, 12, 10, 0, 0, 0, time.UTC) }

	resp := svc.prepareInput(ctx, doccmd.PrepareInputRequest{
		RequestID:  "req_123",
		Kind:       doccmd.PrepareKindWarmup,
		ThreadID:   "thread_123",
		DocumentID: "doc_123",
	})

	if resp.Status != doccmd.PrepareStatusOK {
		t.Fatalf("status = %q, want %q (error=%q)", resp.Status, doccmd.PrepareStatusOK, resp.Error)
	}
	if resp.PreparedInputRef == "" {
		t.Fatal("prepared_input_ref is empty")
	}

	store, err := preparedinput.NewStore(blob)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	artifact, err := store.Read(ctx, resp.PreparedInputRef)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}

	if artifact.SourceKind != doccmd.PrepareKindWarmup {
		t.Fatalf("source_kind = %q, want %q", artifact.SourceKind, doccmd.PrepareKindWarmup)
	}
	if artifact.CreatedAt != "2026-04-12T10:00:00Z" {
		t.Fatalf("created_at = %q, want %q", artifact.CreatedAt, "2026-04-12T10:00:00Z")
	}

	var input []map[string]any
	if err := json.Unmarshal(artifact.Input, &input); err != nil {
		t.Fatalf("json.Unmarshal(input) error = %v", err)
	}
	if len(input) != 1 {
		t.Fatalf("input length = %d, want 1", len(input))
	}

	content, ok := input[0]["content"].([]any)
	if !ok {
		t.Fatalf("content type = %T, want []any", input[0]["content"])
	}
	if len(content) != 5 {
		t.Fatalf("content length = %d, want 5", len(content))
	}

	imageItem, ok := content[2].(map[string]any)
	if !ok {
		t.Fatalf("content[2] type = %T, want map[string]any", content[2])
	}
	if got := imageItem["type"]; got != "image_ref" {
		t.Fatalf("image item type = %v, want %q", got, "image_ref")
	}
	if got := imageItem["image_ref"]; got != imageRef {
		t.Fatalf("image_ref = %v, want %q", got, imageRef)
	}
	if got := imageItem["detail"]; got != "high" {
		t.Fatalf("detail = %v, want %q", got, "high")
	}
}

func TestPrepareInputRejectsUnsupportedKind(t *testing.T) {
	svc := New(nil, nil, nil, &fakeDocStore{}, newTestBlobStore(t))

	resp := svc.prepareInput(context.Background(), doccmd.PrepareInputRequest{
		RequestID:  "req_unsupported",
		Kind:       "repo_warmup",
		DocumentID: "doc_123",
	})

	if resp.Status != doccmd.PrepareStatusError {
		t.Fatalf("status = %q, want %q", resp.Status, doccmd.PrepareStatusError)
	}
	if resp.Error == "" {
		t.Fatal("error is empty")
	}
}

type fakeDocStore struct {
	documents map[string]docstore.Document
}

func (s *fakeDocStore) Get(_ context.Context, id string) (docstore.Document, error) {
	doc, ok := s.documents[id]
	if !ok {
		return docstore.Document{}, docstore.ErrDocumentNotFound
	}
	return doc, nil
}

func newTestBlobStore(t *testing.T) *blobstore.LocalStore {
	t.Helper()

	store, err := blobstore.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal() error = %v", err)
	}
	return store
}
