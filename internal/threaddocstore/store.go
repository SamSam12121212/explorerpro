package threaddocstore

import (
	"context"
	"fmt"

	"explorer/internal/docstore"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) AddDocuments(ctx context.Context, threadID string, documentIDs []string) error {
	if len(documentIDs) == 0 {
		return nil
	}

	_, err := s.pool.Exec(ctx, `
INSERT INTO thread_documents (thread_id, document_id)
SELECT $1, document_id
FROM unnest($2::text[]) AS document_id
ON CONFLICT (thread_id, document_id) DO NOTHING
`, threadID, documentIDs)
	if err != nil {
		return fmt.Errorf("insert thread documents for %s: %w", threadID, err)
	}

	return nil
}

func (s *Store) ListDocuments(ctx context.Context, threadID string, limit int64) ([]docstore.Document, error) {
	if limit <= 0 {
		limit = 100
	}

	rows, err := s.pool.Query(ctx, `
SELECT
    d.id,
    d.filename,
    d.source_ref,
    d.status,
    d.error,
    d.manifest_ref,
    d.page_count,
    d.dpi,
    d.query_model,
    d.base_response_id,
    d.base_model,
    d.base_initialized_at,
    d.created_at,
    d.updated_at
FROM thread_documents td
JOIN documents d ON d.id = td.document_id
WHERE td.thread_id = $1
ORDER BY td.created_at DESC, d.created_at DESC
LIMIT $2`, threadID, limit)
	if err != nil {
		return nil, fmt.Errorf("list thread documents for %s: %w", threadID, err)
	}
	defer rows.Close()

	documents := make([]docstore.Document, 0)
	for rows.Next() {
		var document docstore.Document
		if err := rows.Scan(
			&document.ID,
			&document.Filename,
			&document.SourceRef,
			&document.Status,
			&document.Error,
			&document.ManifestRef,
			&document.PageCount,
			&document.DPI,
			&document.QueryModel,
			&document.BaseResponseID,
			&document.BaseModel,
			&document.BaseInitializedAt,
			&document.CreatedAt,
			&document.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan thread document: %w", err)
		}
		documents = append(documents, document)
	}

	return documents, rows.Err()
}

func (s *Store) FilterAttached(ctx context.Context, threadID string, documentIDs []string) ([]string, error) {
	if len(documentIDs) == 0 {
		return nil, nil
	}

	rows, err := s.pool.Query(ctx, `
SELECT document_id
FROM thread_documents
WHERE thread_id = $1
  AND document_id = ANY($2::text[])`, threadID, documentIDs)
	if err != nil {
		return nil, fmt.Errorf("filter attached documents for %s: %w", threadID, err)
	}
	defer rows.Close()

	attached := make([]string, 0, len(documentIDs))
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan attached document id: %w", err)
		}
		attached = append(attached, id)
	}

	return attached, rows.Err()
}
