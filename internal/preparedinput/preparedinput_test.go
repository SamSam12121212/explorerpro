package preparedinput

import (
	"context"
	"testing"

	"explorer/internal/blobstore"
)

func TestArtifactRoundTrip(t *testing.T) {
	t.Parallel()

	artifact := Artifact{
		Version: VersionV1,
		Input:   []byte(`[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]`),
	}

	data, err := Encode(artifact)
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}

	decoded, err := Decode(data)
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}

	if decoded.Version != VersionV1 {
		t.Fatalf("Version = %q, want %q", decoded.Version, VersionV1)
	}
	if string(decoded.Input) != string(artifact.Input) {
		t.Fatalf("Input = %s, want %s", string(decoded.Input), string(artifact.Input))
	}
}

func TestArtifactValidateRejectsInvalidShape(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		artifact Artifact
	}{
		{
			name: "missing version",
			artifact: Artifact{
				Input: []byte(`[{"type":"message"}]`),
			},
		},
		{
			name: "unsupported version",
			artifact: Artifact{
				Version: "v2",
				Input:   []byte(`[{"type":"message"}]`),
			},
		},
		{
			name: "missing input",
			artifact: Artifact{
				Version: VersionV1,
			},
		},
		{
			name: "input is object",
			artifact: Artifact{
				Version: VersionV1,
				Input:   []byte(`{"type":"message"}`),
			},
		},
		{
			name: "input is empty array",
			artifact: Artifact{
				Version: VersionV1,
				Input:   []byte(`[]`),
			},
		},
		{
			name: "input item is not object",
			artifact: Artifact{
				Version: VersionV1,
				Input:   []byte(`["hello"]`),
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if err := tt.artifact.Validate(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestStoreWriteAndRead(t *testing.T) {
	t.Parallel()

	blob, err := blobstore.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal() error = %v", err)
	}

	store, err := NewStore(blob)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}

	artifact := Artifact{
		Version:    VersionV1,
		Input:      []byte(`[{"type":"message","role":"user"}]`),
		SourceKind: "document",
	}

	ref, err := store.Write(context.Background(), "artifact_123", artifact)
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	if ref != "blob://prepared-inputs/artifact_123.json" {
		t.Fatalf("ref = %q, want %q", ref, "blob://prepared-inputs/artifact_123.json")
	}

	loaded, err := store.Read(context.Background(), ref)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}

	if loaded.SourceKind != artifact.SourceKind {
		t.Fatalf("SourceKind = %q, want %q", loaded.SourceKind, artifact.SourceKind)
	}
	if string(loaded.Input) != string(artifact.Input) {
		t.Fatalf("Input = %s, want %s", string(loaded.Input), string(artifact.Input))
	}
}

func TestStoreReadRejectsInvalidArtifact(t *testing.T) {
	t.Parallel()

	blob, err := blobstore.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal() error = %v", err)
	}

	store, err := NewStore(blob)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}

	ref, err := store.Ref("bad_artifact")
	if err != nil {
		t.Fatalf("Ref() error = %v", err)
	}

	if err := blob.WriteRef(context.Background(), ref, []byte(`{"version":"v1","input":{}}`)); err != nil {
		t.Fatalf("WriteRef() error = %v", err)
	}

	if _, err := store.Read(context.Background(), ref); err == nil {
		t.Fatal("expected Read() to fail for invalid artifact")
	}
}
