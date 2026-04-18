package docsplitter

import (
	"context"
	"encoding/json"
	"testing"

	"explorer/internal/blobstore"
)

func TestMergeExistingOCRRefs_PreservesOnMatchingSHA(t *testing.T) {
	ctx := context.Background()
	blob, err := blobstore.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("new blob: %v", err)
	}

	manifestRef := blob.Ref("documents", "7", "manifest.json")
	prev := Manifest{
		Pages: []PageEntry{
			{PageNumber: 1, SHA256: "aaa", OCRRef: "blob://documents/7/ocr/page-0001.json"},
			{PageNumber: 2, SHA256: "bbb", OCRRef: "blob://documents/7/ocr/page-0002.json"},
		},
	}
	data, _ := json.Marshal(prev)
	if err := blob.WriteRef(ctx, manifestRef, data); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	fresh := []PageEntry{
		{PageNumber: 1, SHA256: "aaa"},
		{PageNumber: 2, SHA256: "bbb"},
	}
	mergeExistingOCRRefs(ctx, blob, manifestRef, fresh)

	if fresh[0].OCRRef != "blob://documents/7/ocr/page-0001.json" {
		t.Errorf("page 1 ocr_ref not preserved: %q", fresh[0].OCRRef)
	}
	if fresh[1].OCRRef != "blob://documents/7/ocr/page-0002.json" {
		t.Errorf("page 2 ocr_ref not preserved: %q", fresh[1].OCRRef)
	}
}

func TestMergeExistingOCRRefs_DropsOnChangedSHA(t *testing.T) {
	ctx := context.Background()
	blob, err := blobstore.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("new blob: %v", err)
	}

	manifestRef := blob.Ref("documents", "7", "manifest.json")
	prev := Manifest{
		Pages: []PageEntry{
			{PageNumber: 1, SHA256: "aaa", OCRRef: "blob://documents/7/ocr/page-0001.json"},
		},
	}
	data, _ := json.Marshal(prev)
	_ = blob.WriteRef(ctx, manifestRef, data)

	fresh := []PageEntry{
		{PageNumber: 1, SHA256: "different"},
	}
	mergeExistingOCRRefs(ctx, blob, manifestRef, fresh)

	if fresh[0].OCRRef != "" {
		t.Errorf("page 1 ocr_ref should have been dropped on sha change, got %q", fresh[0].OCRRef)
	}
}

func TestMergeExistingOCRRefs_NoExistingManifest(t *testing.T) {
	ctx := context.Background()
	blob, err := blobstore.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("new blob: %v", err)
	}

	manifestRef := blob.Ref("documents", "7", "manifest.json")
	fresh := []PageEntry{
		{PageNumber: 1, SHA256: "aaa"},
	}
	mergeExistingOCRRefs(ctx, blob, manifestRef, fresh)

	if fresh[0].OCRRef != "" {
		t.Errorf("expected no ocr_ref when manifest absent, got %q", fresh[0].OCRRef)
	}
}
