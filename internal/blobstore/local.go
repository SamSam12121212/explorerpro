package blobstore

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

type LocalStore struct {
	root string
}

func NewLocal(root string) (*LocalStore, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve blob storage path: %w", err)
	}

	if err := os.MkdirAll(absRoot, 0o755); err != nil {
		return nil, fmt.Errorf("create blob storage dir: %w", err)
	}

	return &LocalStore{root: absRoot}, nil
}

func (s *LocalStore) Root() string {
	return s.root
}

func (s *LocalStore) Ping(context.Context) error {
	info, err := os.Stat(s.root)
	if err != nil {
		return fmt.Errorf("stat blob storage dir: %w", err)
	}

	if !info.IsDir() {
		return fmt.Errorf("blob storage path is not a directory")
	}

	return nil
}

func (s *LocalStore) Ref(parts ...string) string {
	joined := path.Join(parts...)
	trimmed := strings.TrimPrefix(joined, "/")
	return "blob://" + trimmed
}

func (s *LocalStore) ResolveRef(ref string) (string, error) {
	trimmed := strings.TrimSpace(ref)
	if !strings.HasPrefix(trimmed, "blob://") {
		return "", fmt.Errorf("unsupported blob ref %q", ref)
	}

	relative := strings.TrimPrefix(trimmed, "blob://")
	cleanRelative := path.Clean(relative)
	if cleanRelative == "." || cleanRelative == "" {
		return "", fmt.Errorf("blob ref %q does not identify a file", ref)
	}
	if cleanRelative == ".." || strings.HasPrefix(cleanRelative, "../") {
		return "", fmt.Errorf("blob ref %q escapes blob root", ref)
	}

	localPath := filepath.Join(s.root, filepath.FromSlash(cleanRelative))
	relToRoot, err := filepath.Rel(s.root, localPath)
	if err != nil {
		return "", fmt.Errorf("resolve blob ref %q: %w", ref, err)
	}
	if relToRoot == ".." || strings.HasPrefix(relToRoot, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("blob ref %q escapes blob root", ref)
	}

	return localPath, nil
}

func (s *LocalStore) WriteRef(ctx context.Context, ref string, data []byte) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	localPath, err := s.ResolveRef(ref)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		return fmt.Errorf("create blob parent dir: %w", err)
	}
	if err := os.WriteFile(localPath, data, 0o644); err != nil {
		return fmt.Errorf("write blob ref %q: %w", ref, err)
	}

	return nil
}

func (s *LocalStore) ReadRef(ctx context.Context, ref string) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	localPath, err := s.ResolveRef(ref)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(localPath)
	if err != nil {
		return nil, fmt.Errorf("read blob ref %q: %w", ref, err)
	}

	return data, nil
}
