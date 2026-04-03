package blobstore

import (
	"context"
	"testing"
)

func TestWriteAndReadRef(t *testing.T) {
	t.Parallel()

	store, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal() error = %v", err)
	}

	ref := store.Ref("images", "img_test", "source.png")
	want := []byte("hello image")
	if err := store.WriteRef(context.Background(), ref, want); err != nil {
		t.Fatalf("WriteRef() error = %v", err)
	}

	got, err := store.ReadRef(context.Background(), ref)
	if err != nil {
		t.Fatalf("ReadRef() error = %v", err)
	}

	if string(got) != string(want) {
		t.Fatalf("ReadRef() = %q, want %q", string(got), string(want))
	}
}

func TestResolveRefRejectsTraversal(t *testing.T) {
	t.Parallel()

	store, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal() error = %v", err)
	}

	if _, err := store.ResolveRef("blob://../../etc/passwd"); err == nil {
		t.Fatal("expected traversal ref to fail")
	}
}
