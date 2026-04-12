package preparedinput

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

const VersionV1 = "v1"

type Artifact struct {
	Version    string          `json:"version"`
	Input      json.RawMessage `json:"input"`
	SourceKind string          `json:"source_kind,omitempty"`
	CreatedAt  string          `json:"created_at,omitempty"`
	ExpiresAt  string          `json:"expires_at,omitempty"`
	SourceHash string          `json:"source_hash,omitempty"`
}

type blobStore interface {
	Ref(parts ...string) string
	ReadRef(ctx context.Context, ref string) ([]byte, error)
	WriteRef(ctx context.Context, ref string, data []byte) error
}

type Store struct {
	blob blobStore
}

func NewStore(blob blobStore) (*Store, error) {
	if blob == nil {
		return nil, fmt.Errorf("prepared input blob store is required")
	}
	return &Store{blob: blob}, nil
}

func (a Artifact) Validate() error {
	version := strings.TrimSpace(a.Version)
	if version == "" {
		return fmt.Errorf("prepared input version is required")
	}
	if version != VersionV1 {
		return fmt.Errorf("unsupported prepared input version %q", a.Version)
	}

	if err := validateInput(a.Input); err != nil {
		return err
	}

	return nil
}

func Encode(artifact Artifact) ([]byte, error) {
	if err := artifact.Validate(); err != nil {
		return nil, err
	}

	data, err := json.Marshal(artifact)
	if err != nil {
		return nil, fmt.Errorf("marshal prepared input artifact: %w", err)
	}

	return data, nil
}

func Decode(data []byte) (Artifact, error) {
	var artifact Artifact
	if err := json.Unmarshal(data, &artifact); err != nil {
		return Artifact{}, fmt.Errorf("decode prepared input artifact: %w", err)
	}

	if err := artifact.Validate(); err != nil {
		return Artifact{}, err
	}

	artifact.Input = copyRawMessage(artifact.Input)
	return artifact, nil
}

func (s *Store) Ref(id string) (string, error) {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return "", fmt.Errorf("prepared input id is required")
	}

	return s.blob.Ref("prepared-inputs", trimmed+".json"), nil
}

func (s *Store) Write(ctx context.Context, id string, artifact Artifact) (string, error) {
	ref, err := s.Ref(id)
	if err != nil {
		return "", err
	}
	if err := s.WriteRef(ctx, ref, artifact); err != nil {
		return "", err
	}
	return ref, nil
}

func (s *Store) WriteRef(ctx context.Context, ref string, artifact Artifact) error {
	data, err := Encode(artifact)
	if err != nil {
		return err
	}

	if err := s.blob.WriteRef(ctx, ref, data); err != nil {
		return fmt.Errorf("write prepared input artifact: %w", err)
	}

	return nil
}

func (s *Store) Read(ctx context.Context, ref string) (Artifact, error) {
	data, err := s.blob.ReadRef(ctx, ref)
	if err != nil {
		return Artifact{}, fmt.Errorf("read prepared input artifact: %w", err)
	}

	artifact, err := Decode(data)
	if err != nil {
		return Artifact{}, err
	}

	return artifact, nil
}

func validateInput(raw json.RawMessage) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return fmt.Errorf("prepared input is required")
	}

	var items []json.RawMessage
	if err := json.Unmarshal(trimmed, &items); err != nil {
		return fmt.Errorf("prepared input must be a JSON array: %w", err)
	}
	if len(items) == 0 {
		return fmt.Errorf("prepared input must contain at least one item")
	}

	for index, item := range items {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(item, &object); err != nil {
			return fmt.Errorf("prepared input item %d must be a JSON object: %w", index, err)
		}
	}

	return nil
}

func copyRawMessage(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return append(json.RawMessage(nil), bytes.TrimSpace(raw)...)
}
