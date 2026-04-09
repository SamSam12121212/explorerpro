package docstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrDocumentNotFound = errors.New("document not found")

type Document struct {
	ID          string    `json:"id"`
	Filename    string    `json:"filename"`
	SourceRef   string    `json:"source_ref"`
	Status      string    `json:"status"`
	Error       string    `json:"error,omitempty"`
	ManifestRef string    `json:"manifest_ref,omitempty"`
	PageCount   int       `json:"page_count"`
	DPI         int       `json:"dpi"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) Create(ctx context.Context, doc Document) error {
	_, err := s.pool.Exec(ctx, `
	INSERT INTO documents (id, filename, source_ref, status, dpi, created_at, updated_at)
	VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		doc.ID, doc.Filename, doc.SourceRef, doc.Status, doc.DPI, doc.CreatedAt, doc.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert document: %w", err)
	}
	return nil
}

func (s *Store) Get(ctx context.Context, id string) (Document, error) {
	var d Document
	err := s.pool.QueryRow(ctx, `
	SELECT id, filename, source_ref, status, error, manifest_ref, page_count, dpi, created_at, updated_at
	FROM documents WHERE id = $1`, id).Scan(
		&d.ID, &d.Filename, &d.SourceRef, &d.Status, &d.Error, &d.ManifestRef, &d.PageCount, &d.DPI, &d.CreatedAt, &d.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Document{}, ErrDocumentNotFound
	}
	if err != nil {
		return Document{}, fmt.Errorf("get document: %w", err)
	}
	return d, nil
}

func (s *Store) List(ctx context.Context, limit int64) ([]Document, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
	SELECT id, filename, source_ref, status, error, manifest_ref, page_count, dpi, created_at, updated_at
	FROM documents ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list documents: %w", err)
	}
	defer rows.Close()

	var docs []Document
	for rows.Next() {
		var d Document
		if err := rows.Scan(&d.ID, &d.Filename, &d.SourceRef, &d.Status, &d.Error, &d.ManifestRef, &d.PageCount, &d.DPI, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan document: %w", err)
		}
		docs = append(docs, d)
	}
	return docs, rows.Err()
}

func (s *Store) UpdateStatus(ctx context.Context, id, status, manifestRef string, pageCount int, errMsg string) error {
	_, err := s.pool.Exec(ctx, `
UPDATE documents SET status = $2, manifest_ref = $3, page_count = $4, error = $5, updated_at = now()
WHERE id = $1`, id, status, manifestRef, pageCount, errMsg)
	if err != nil {
		return fmt.Errorf("update document status: %w", err)
	}
	return nil
}
