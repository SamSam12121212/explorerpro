package documenthandler

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"explorer/internal/blobstore"
	"explorer/internal/doccmd"
	"explorer/internal/docsplitter"
	"explorer/internal/docstore"
	"explorer/internal/preparedinput"
	"explorer/internal/threadcollectionstore"
)

func TestPrepareInputBuildsWarmupArtifact(t *testing.T) {
	ctx := context.Background()
	blob := newTestBlobStore(t)
	const documentID int64 = 123

	imageRef := blob.Ref("documents", "123", "pages", "page-0001.png")
	if err := blob.WriteRef(ctx, imageRef, []byte("png")); err != nil {
		t.Fatalf("WriteRef(image) error = %v", err)
	}

	manifestJSON, err := json.Marshal(docsplitter.Manifest{
		DocumentID: documentID,
		Filename:   "report.pdf",
		PageCount:  1,
		Pages: []docsplitter.PageEntry{
			{PageNumber: 1, ImageRef: imageRef, ContentType: "image/png"},
		},
	})
	if err != nil {
		t.Fatalf("json.Marshal(manifest) error = %v", err)
	}

	manifestRef := blob.Ref("documents", "123", "manifest.json")
	if err := blob.WriteRef(ctx, manifestRef, manifestJSON); err != nil {
		t.Fatalf("WriteRef(manifest) error = %v", err)
	}

	svc := New(nil, nil, nil, &fakeDocStore{
		documents: map[int64]docstore.Document{
			123: {
				ID:          documentID,
				Filename:    "report.pdf",
				ManifestRef: manifestRef,
				PageCount:   1,
			},
		},
	}, &fakeThreadDocStore{}, &fakeThreadCollectionStore{}, blob)
	svc.now = func() time.Time { return time.Date(2026, 4, 12, 10, 0, 0, 0, time.UTC) }

	resp := svc.prepareInput(ctx, doccmd.PrepareInputRequest{
		RequestID:  "req_123",
		Kind:       doccmd.PrepareKindWarmup,
		ThreadID:   123,
		DocumentID: documentID,
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

func TestAppendPdfEnvelopeItemsSinglePageSubset(t *testing.T) {
	doc := docstore.Document{ID: 15, Filename: "report.pdf"}
	manifest := docsplitter.Manifest{
		DocumentID: 15,
		Filename:   "report.pdf",
		PageCount:  20,
		Pages: []docsplitter.PageEntry{
			{PageNumber: 4, ImageRef: "blob://doc15/pages/page-0004.png", Width: 1241, Height: 1754, ContentType: "image/png"},
		},
	}

	content := appendPdfEnvelopeItems(nil, doc, manifest, manifest.Pages)

	if len(content) != 5 {
		t.Fatalf("content length = %d, want 5 (<pdf>, <pdf_page>, image, </pdf_page>, </pdf>)", len(content))
	}

	wantTexts := []string{
		`<pdf name="report.pdf" id="15" page_count="20">`,
		`<pdf_page number="4" width="1241" height="1754">`,
		"", // image item, asserted separately
		"</pdf_page>",
		"</pdf>",
	}
	for i, want := range wantTexts {
		if want == "" {
			continue
		}
		m, ok := content[i].(map[string]any)
		if !ok {
			t.Fatalf("content[%d] type = %T, want map[string]any", i, content[i])
		}
		if got, _ := m["text"].(string); got != want {
			t.Errorf("content[%d].text = %q, want %q", i, got, want)
		}
	}

	image, ok := content[2].(map[string]any)
	if !ok {
		t.Fatalf("content[2] type = %T, want map[string]any (image item)", content[2])
	}
	if got := image["type"]; got != "image_ref" {
		t.Errorf("image type = %v, want %q", got, "image_ref")
	}
	if got := image["image_ref"]; got != "blob://doc15/pages/page-0004.png" {
		t.Errorf("image_ref = %v, want blob://doc15/pages/page-0004.png", got)
	}
	if got := image["detail"]; got != "high" {
		t.Errorf("detail = %v, want %q", got, "high")
	}
	if got := image["content_type"]; got != "image/png" {
		t.Errorf("content_type = %v, want %q", got, "image/png")
	}
}

func TestBuildWarmupInputEmitsPageDimensions(t *testing.T) {
	doc := docstore.Document{ID: 42, Filename: "report.pdf"}
	manifest := docsplitter.Manifest{
		DocumentID: 42,
		Filename:   "report.pdf",
		PageCount:  2,
		Pages: []docsplitter.PageEntry{
			{PageNumber: 1, ImageRef: "blob:page-1", Width: 1241, Height: 1754, ContentType: "image/png"},
			{PageNumber: 2, ImageRef: "blob:page-2", Width: 2480, Height: 3508, ContentType: "image/png"},
		},
	}

	raw, err := buildWarmupInput(doc, manifest)
	if err != nil {
		t.Fatalf("buildWarmupInput() error = %v", err)
	}

	assertPageTagsIncludeDimensions(t, raw, []string{
		`<pdf_page number="1" width="1241" height="1754">`,
		`<pdf_page number="2" width="2480" height="3508">`,
	})
}

func TestBuildDocumentQueryInputEmitsPageDimensions(t *testing.T) {
	doc := docstore.Document{ID: 42, Filename: "report.pdf"}
	manifest := docsplitter.Manifest{
		DocumentID: 42,
		Filename:   "report.pdf",
		PageCount:  1,
		Pages: []docsplitter.PageEntry{
			{PageNumber: 1, ImageRef: "blob:page-1", Width: 1241, Height: 1754, ContentType: "image/png"},
		},
	}

	raw, err := buildDocumentQueryInput(doc, manifest, "summarize the cat")
	if err != nil {
		t.Fatalf("buildDocumentQueryInput() error = %v", err)
	}

	assertPageTagsIncludeDimensions(t, raw, []string{
		`<pdf_page number="1" width="1241" height="1754">`,
	})
}

func assertPageTagsIncludeDimensions(t *testing.T, raw json.RawMessage, wantTags []string) {
	t.Helper()

	var input []map[string]any
	if err := json.Unmarshal(raw, &input); err != nil {
		t.Fatalf("json.Unmarshal(input) error = %v", err)
	}
	if len(input) != 1 {
		t.Fatalf("input length = %d, want 1", len(input))
	}
	content, ok := input[0]["content"].([]any)
	if !ok {
		t.Fatalf("content type = %T, want []any", input[0]["content"])
	}

	var pageTags []string
	for _, item := range content {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		text, _ := m["text"].(string)
		if strings.HasPrefix(text, "<pdf_page ") {
			pageTags = append(pageTags, text)
		}
	}

	if len(pageTags) != len(wantTags) {
		t.Fatalf("pdf_page tags = %d (%v), want %d", len(pageTags), pageTags, len(wantTags))
	}
	for i, want := range wantTags {
		if pageTags[i] != want {
			t.Errorf("pdf_page[%d] = %q, want %q", i, pageTags[i], want)
		}
	}
}

func TestPrepareInputRejectsUnsupportedKind(t *testing.T) {
	svc := New(nil, nil, nil, &fakeDocStore{}, &fakeThreadDocStore{}, &fakeThreadCollectionStore{}, newTestBlobStore(t))

	resp := svc.prepareInput(context.Background(), doccmd.PrepareInputRequest{
		RequestID:  "req_unsupported",
		Kind:       "repo_warmup",
		DocumentID: 123,
	})

	if resp.Status != doccmd.PrepareStatusError {
		t.Fatalf("status = %q, want %q", resp.Status, doccmd.PrepareStatusError)
	}
	if resp.Error == "" {
		t.Fatal("error is empty")
	}
}

func TestRuntimeContextAppendsAvailableDocumentsAndTool(t *testing.T) {
	svc := New(nil, nil, nil, &fakeDocStore{}, &fakeThreadDocStore{
		documentsByThread: map[int64][]docstore.Document{
			123: {
				{ID: 1, Filename: `Quarterly "Report" <Draft>.pdf`},
			},
		},
	}, &fakeThreadCollectionStore{}, newTestBlobStore(t))

	resp := svc.runtimeContext(context.Background(), doccmd.RuntimeContextRequest{
		RequestID:    "docctx_123",
		ThreadID:     123,
		Instructions: "Be concise.",
		Tools:        json.RawMessage(`[{"type":"function","name":"lookup"}]`),
	})

	if resp.Status != doccmd.PrepareStatusOK {
		t.Fatalf("status = %q, want %q (error=%q)", resp.Status, doccmd.PrepareStatusOK, resp.Error)
	}

	wantInstructions := "Be concise.\n\n<available_documents>\n" +
		`<document id="1" name="Quarterly &quot;Report&quot; &lt;Draft&gt;.pdf" />` + "\n" +
		"</available_documents>"
	if resp.Instructions != wantInstructions {
		t.Fatalf("instructions = %q, want %q", resp.Instructions, wantInstructions)
	}

	var tools []map[string]any
	if err := json.Unmarshal(resp.Tools, &tools); err != nil {
		t.Fatalf("json.Unmarshal(tools) error = %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("tools length = %d, want 2", len(tools))
	}
	if tools[1]["name"] != doccmd.ToolNameQueryDocument {
		t.Fatalf("tool name = %v, want %q", tools[1]["name"], doccmd.ToolNameQueryDocument)
	}
}

func TestRuntimeContextLeavesBaseWhenNoDocumentsAttached(t *testing.T) {
	svc := New(nil, nil, nil, &fakeDocStore{}, &fakeThreadDocStore{}, &fakeThreadCollectionStore{}, newTestBlobStore(t))

	resp := svc.runtimeContext(context.Background(), doccmd.RuntimeContextRequest{
		RequestID:    "docctx_123",
		ThreadID:     123,
		Instructions: "Be concise.",
		Tools:        json.RawMessage(`[{"type":"function","name":"lookup"}]`),
	})

	if resp.Status != doccmd.PrepareStatusOK {
		t.Fatalf("status = %q, want %q (error=%q)", resp.Status, doccmd.PrepareStatusOK, resp.Error)
	}
	if resp.Instructions != "Be concise." {
		t.Fatalf("instructions = %q, want %q", resp.Instructions, "Be concise.")
	}
	if string(resp.Tools) != `[{"type":"function","name":"lookup"}]` {
		t.Fatalf("tools = %s, want base tools", string(resp.Tools))
	}
}

type fakeDocStore struct {
	documents map[int64]docstore.Document
}

func (s *fakeDocStore) Get(_ context.Context, id int64) (docstore.Document, error) {
	doc, ok := s.documents[id]
	if !ok {
		return docstore.Document{}, docstore.ErrDocumentNotFound
	}
	return doc, nil
}

type fakeThreadDocStore struct {
	documentsByThread map[int64][]docstore.Document
	err               error
}

func (s *fakeThreadDocStore) ListDocuments(_ context.Context, threadID int64, _ int64) ([]docstore.Document, error) {
	if s.err != nil {
		return nil, s.err
	}

	documents := s.documentsByThread[threadID]
	cloned := make([]docstore.Document, len(documents))
	copy(cloned, documents)
	return cloned, nil
}

type fakeThreadCollectionStore struct {
	collectionsByThread map[int64][]threadcollectionstore.AttachedCollection
	err                 error
}

func (s *fakeThreadCollectionStore) ListAttached(_ context.Context, threadID int64, _ int64) ([]threadcollectionstore.AttachedCollection, error) {
	if s.err != nil {
		return nil, s.err
	}

	collections := s.collectionsByThread[threadID]
	cloned := make([]threadcollectionstore.AttachedCollection, len(collections))
	copy(cloned, collections)
	return cloned, nil
}

func newTestBlobStore(t *testing.T) *blobstore.LocalStore {
	t.Helper()

	store, err := blobstore.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal() error = %v", err)
	}
	return store
}
