package worker

import (
	"context"
	"strings"
	"testing"

	"explorer/internal/blobstore"
)

func TestLowerFunctionCallOutputListLowersImageRef(t *testing.T) {
	ctx := context.Background()
	blob := newPreparedPayloadTestBlob(t)

	imageRef := blob.Ref("documents", "15", "pages", "page-0004.png")
	if err := blob.WriteRef(ctx, imageRef, pngMagicBytes()); err != nil {
		t.Fatalf("WriteRef(image) error = %v", err)
	}

	item := map[string]any{
		"type":    "function_call_output",
		"call_id": "call_abc",
		"output": []any{
			map[string]any{"type": "input_text", "text": `<pdf name="report.pdf" id="15" page_count="20">`},
			map[string]any{"type": "input_text", "text": `<pdf_page number="4" width="1241" height="1754">`},
			map[string]any{
				"type":         "image_ref",
				"image_ref":    imageRef,
				"detail":       "high",
				"content_type": "image/png",
			},
			map[string]any{"type": "input_text", "text": "</pdf_page>"},
			map[string]any{"type": "input_text", "text": "</pdf>"},
		},
	}

	stats := &payloadLoweringStats{}
	lowered, err := lowerInputItemWithBlob(ctx, blob, item, stats)
	if err != nil {
		t.Fatalf("lowerInputItemWithBlob() error = %v", err)
	}

	output, ok := lowered["output"].([]any)
	if !ok {
		t.Fatalf("output type = %T, want []any", lowered["output"])
	}
	if len(output) != 5 {
		t.Fatalf("output length = %d, want 5", len(output))
	}

	image, ok := output[2].(map[string]any)
	if !ok {
		t.Fatalf("output[2] type = %T, want map[string]any", output[2])
	}
	if got := image["type"]; got != "input_image" {
		t.Errorf("image type = %v, want input_image (image_ref should have been lowered)", got)
	}
	imageURL, _ := image["image_url"].(string)
	if !strings.HasPrefix(imageURL, "data:image/png;base64,") {
		t.Errorf("image_url prefix = %q, want data:image/png;base64,...", imageURL)
	}
	if got := image["detail"]; got != "high" {
		t.Errorf("detail = %v, want high", got)
	}
	if _, hasRef := image["image_ref"]; hasRef {
		t.Error("lowered image item should not retain image_ref field")
	}

	// Surrounding text parts pass through untouched.
	for _, i := range []int{0, 1, 3, 4} {
		part, ok := output[i].(map[string]any)
		if !ok {
			t.Fatalf("output[%d] type = %T, want map[string]any", i, output[i])
		}
		if part["type"] != "input_text" {
			t.Errorf("output[%d].type = %v, want input_text (should pass through unchanged)", i, part["type"])
		}
	}

	if stats.LoweredImageInputs != 1 {
		t.Errorf("LoweredImageInputs = %d, want 1", stats.LoweredImageInputs)
	}
	if stats.LoweredBlobRefs != 1 {
		t.Errorf("LoweredBlobRefs = %d, want 1", stats.LoweredBlobRefs)
	}
}

func TestLowerFunctionCallOutputStringPassesThrough(t *testing.T) {
	ctx := context.Background()
	blob := newPreparedPayloadTestBlob(t)

	item := map[string]any{
		"type":    "function_call_output",
		"call_id": "call_abc",
		"output":  `{"status":"ok"}`,
	}

	stats := &payloadLoweringStats{}
	lowered, err := lowerInputItemWithBlob(ctx, blob, item, stats)
	if err != nil {
		t.Fatalf("lowerInputItemWithBlob() error = %v", err)
	}

	if got := lowered["output"]; got != `{"status":"ok"}` {
		t.Errorf("output = %v, want unchanged string", got)
	}
	if stats.LoweredImageInputs != 0 || stats.LoweredBlobRefs != 0 {
		t.Errorf("stats = %+v, want zero (string output has nothing to lower)", stats)
	}
}

func TestLowerMessageItemStillLowersImageRef(t *testing.T) {
	// Sanity check that extracting lowerContentPartsWithBlob didn't regress
	// the existing message path.
	ctx := context.Background()
	blob := newPreparedPayloadTestBlob(t)

	imageRef := blob.Ref("documents", "15", "pages", "page-0001.png")
	if err := blob.WriteRef(ctx, imageRef, pngMagicBytes()); err != nil {
		t.Fatalf("WriteRef(image) error = %v", err)
	}

	item := map[string]any{
		"type": "message",
		"role": "user",
		"content": []any{
			map[string]any{"type": "input_text", "text": "hello"},
			map[string]any{
				"type":         "image_ref",
				"image_ref":    imageRef,
				"detail":       "high",
				"content_type": "image/png",
			},
		},
	}

	stats := &payloadLoweringStats{}
	lowered, err := lowerInputItemWithBlob(ctx, blob, item, stats)
	if err != nil {
		t.Fatalf("lowerInputItemWithBlob() error = %v", err)
	}

	content, _ := lowered["content"].([]any)
	image, _ := content[1].(map[string]any)
	if image["type"] != "input_image" {
		t.Errorf("image type = %v, want input_image", image["type"])
	}
	if stats.LoweredImageInputs != 1 {
		t.Errorf("LoweredImageInputs = %d, want 1", stats.LoweredImageInputs)
	}
}

func newPreparedPayloadTestBlob(t *testing.T) *blobstore.LocalStore {
	t.Helper()
	store, err := blobstore.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("blobstore.NewLocal() error = %v", err)
	}
	return store
}

func pngMagicBytes() []byte {
	return []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}
}
