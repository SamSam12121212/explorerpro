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

func TestPrepareInputBuildsPageReadArtifact(t *testing.T) {
	ctx := context.Background()
	blob := newTestBlobStore(t)
	const documentID int64 = 15

	imageRef := blob.Ref("documents", "15", "pages", "page-0004.png")
	if err := blob.WriteRef(ctx, imageRef, []byte("png")); err != nil {
		t.Fatalf("WriteRef(image) error = %v", err)
	}

	manifestJSON, err := json.Marshal(docsplitter.Manifest{
		DocumentID: documentID,
		Filename:   "report.pdf",
		PageCount:  10,
		Pages: []docsplitter.PageEntry{
			{PageNumber: 4, ImageRef: imageRef, Width: 1241, Height: 1754, ContentType: "image/png"},
		},
	})
	if err != nil {
		t.Fatalf("json.Marshal(manifest) error = %v", err)
	}

	manifestRef := blob.Ref("documents", "15", "manifest.json")
	if err := blob.WriteRef(ctx, manifestRef, manifestJSON); err != nil {
		t.Fatalf("WriteRef(manifest) error = %v", err)
	}

	svc := New(nil, nil, nil, &fakeDocStore{
		documents: map[int64]docstore.Document{
			15: {
				ID:          documentID,
				Filename:    "report.pdf",
				ManifestRef: manifestRef,
				PageCount:   10,
			},
		},
	}, &fakeThreadDocStore{}, &fakeThreadCollectionStore{}, blob)
	svc.now = func() time.Time { return time.Date(2026, 4, 18, 12, 0, 0, 0, time.UTC) }

	resp := svc.prepareInput(ctx, doccmd.PrepareInputRequest{
		RequestID:  "req_pr_15_4",
		Kind:       doccmd.PrepareKindPageRead,
		ThreadID:   456,
		DocumentID: documentID,
		PageNumber: 4,
		CallID:     "call_abc",
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
	if artifact.SourceKind != doccmd.PrepareKindPageRead {
		t.Fatalf("source_kind = %q, want %q", artifact.SourceKind, doccmd.PrepareKindPageRead)
	}

	var items []map[string]any
	if err := json.Unmarshal(artifact.Input, &items); err != nil {
		t.Fatalf("json.Unmarshal(input) error = %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items length = %d, want 1", len(items))
	}
	if items[0]["type"] != "function_call_output" {
		t.Fatalf("item type = %v, want function_call_output", items[0]["type"])
	}
	if items[0]["call_id"] != "call_abc" {
		t.Fatalf("call_id = %v, want call_abc", items[0]["call_id"])
	}
}

func TestBuildReadDocumentPageInputWrapsPageInFunctionCallOutput(t *testing.T) {
	doc := docstore.Document{ID: 15, Filename: "report.pdf"}
	manifest := docsplitter.Manifest{
		DocumentID: 15,
		Filename:   "report.pdf",
		PageCount:  20,
		Pages: []docsplitter.PageEntry{
			{PageNumber: 3, ImageRef: "blob://doc15/pages/page-0003.png", Width: 1241, Height: 1754, ContentType: "image/png"},
			{PageNumber: 4, ImageRef: "blob://doc15/pages/page-0004.png", Width: 1241, Height: 1754, ContentType: "image/png"},
		},
	}

	raw, err := buildReadDocumentPageInput(doc, manifest, 4, "call_xyz")
	if err != nil {
		t.Fatalf("buildReadDocumentPageInput() error = %v", err)
	}

	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items length = %d, want 1", len(items))
	}

	item := items[0]
	if got := item["type"]; got != "function_call_output" {
		t.Errorf("item type = %v, want function_call_output", got)
	}
	if got := item["call_id"]; got != "call_xyz" {
		t.Errorf("call_id = %v, want call_xyz", got)
	}

	output, ok := item["output"].([]any)
	if !ok {
		t.Fatalf("output type = %T, want []any", item["output"])
	}
	if len(output) != 5 {
		t.Fatalf("output length = %d, want 5 (<pdf>, <pdf_page>, image, </pdf_page>, </pdf>)", len(output))
	}

	openTag, _ := output[0].(map[string]any)
	if openTag["text"] != `<pdf name="report.pdf" id="15" page_count="20">` {
		t.Errorf("output[0].text = %v, want full <pdf> open tag with page_count=20", openTag["text"])
	}
	pageTag, _ := output[1].(map[string]any)
	if pageTag["text"] != `<pdf_page number="4" width="1241" height="1754">` {
		t.Errorf("output[1].text = %v, want <pdf_page> for page 4", pageTag["text"])
	}
	image, _ := output[2].(map[string]any)
	if image["type"] != "image_ref" {
		t.Errorf("output[2].type = %v, want image_ref", image["type"])
	}
	if image["image_ref"] != "blob://doc15/pages/page-0004.png" {
		t.Errorf("output[2].image_ref = %v, want page-0004 ref", image["image_ref"])
	}
}

func TestBuildReadDocumentPageInputRejectsMissingPage(t *testing.T) {
	doc := docstore.Document{ID: 15, Filename: "report.pdf"}
	manifest := docsplitter.Manifest{
		DocumentID: 15,
		PageCount:  2,
		Pages: []docsplitter.PageEntry{
			{PageNumber: 1, ImageRef: "blob://doc15/pages/page-0001.png", Width: 1241, Height: 1754},
			{PageNumber: 2, ImageRef: "blob://doc15/pages/page-0002.png", Width: 1241, Height: 1754},
		},
	}

	_, err := buildReadDocumentPageInput(doc, manifest, 99, "call_xyz")
	if err == nil {
		t.Fatal("buildReadDocumentPageInput() error = nil, want error for page out of range")
	}
	if !strings.Contains(err.Error(), "not found in manifest") {
		t.Errorf("error = %v, want 'not found in manifest'", err)
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
	if len(tools) != 3 {
		t.Fatalf("tools length = %d, want 3 (lookup + query_document + read_document_page for root thread)", len(tools))
	}
	if tools[1]["name"] != doccmd.ToolNameQueryDocument {
		t.Fatalf("tools[1] name = %v, want %q", tools[1]["name"], doccmd.ToolNameQueryDocument)
	}
	if tools[2]["name"] != doccmd.ToolNameReadDocumentPage {
		t.Fatalf("tools[2] name = %v, want %q", tools[2]["name"], doccmd.ToolNameReadDocumentPage)
	}
}

func TestRuntimeContextOmitsReadDocumentPageForChildThreads(t *testing.T) {
	svc := New(nil, nil, nil, &fakeDocStore{}, &fakeThreadDocStore{
		documentsByThread: map[int64][]docstore.Document{
			456: {
				{ID: 1, Filename: "report.pdf"},
			},
		},
	}, &fakeThreadCollectionStore{}, newTestBlobStore(t))

	resp := svc.runtimeContext(context.Background(), doccmd.RuntimeContextRequest{
		RequestID:      "docctx_456",
		ThreadID:       456,
		ParentThreadID: 123,
		Instructions:   "Be concise.",
		Tools:          json.RawMessage(`[{"type":"function","name":"lookup"}]`),
	})

	if resp.Status != doccmd.PrepareStatusOK {
		t.Fatalf("status = %q, want %q (error=%q)", resp.Status, doccmd.PrepareStatusOK, resp.Error)
	}

	var tools []map[string]any
	if err := json.Unmarshal(resp.Tools, &tools); err != nil {
		t.Fatalf("json.Unmarshal(tools) error = %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("tools length = %d, want 2 (lookup + query_document; no read_document_page on child threads)", len(tools))
	}
	for _, tool := range tools {
		if tool["name"] == doccmd.ToolNameReadDocumentPage {
			t.Fatalf("child thread should not receive %q tool", doccmd.ToolNameReadDocumentPage)
		}
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
