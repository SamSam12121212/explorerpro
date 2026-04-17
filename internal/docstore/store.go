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
	ID                int64      `json:"id"`
	Filename          string     `json:"filename"`
	SourceRef         string     `json:"source_ref"`
	Status            string     `json:"status"`
	Error             string     `json:"error,omitempty"`
	ManifestRef       string     `json:"manifest_ref,omitempty"`
	PageCount         int        `json:"page_count"`
	DPI               int        `json:"dpi"`
	QueryModel        string     `json:"query_model,omitempty"`
	QueryReasoning    string     `json:"query_reasoning,omitempty"`
	BaseResponseID    string     `json:"base_response_id,omitempty"`
	BaseModel         string     `json:"base_model,omitempty"`
	BaseReasoning     string     `json:"base_reasoning,omitempty"`
	BaseInitializedAt *time.Time `json:"base_initialized_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) ReserveID(ctx context.Context) (int64, error) {
	var id int64
	if err := s.pool.QueryRow(ctx, `
	SELECT nextval(pg_get_serial_sequence('documents', 'id'))`).Scan(&id); err != nil {
		return 0, fmt.Errorf("reserve document id: %w", err)
	}
	return id, nil
}

func (s *Store) Create(ctx context.Context, doc Document) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO documents (id, filename, source_ref, status, dpi, query_model, query_reasoning, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, COALESCE(NULLIF($6, ''), 'gpt-5.4-mini'), COALESCE(NULLIF($7, ''), 'medium'), $8, $9)`,
		doc.ID, doc.Filename, doc.SourceRef, doc.Status, doc.DPI, doc.QueryModel, doc.QueryReasoning, doc.CreatedAt, doc.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert document: %w", err)
	}
	return nil
}

func (s *Store) Get(ctx context.Context, id int64) (Document, error) {
	var d Document
	err := s.pool.QueryRow(ctx, `
	SELECT id, filename, source_ref, status, error, manifest_ref, page_count, dpi,
	       query_model, query_reasoning,
	       base_response_id, base_model, base_reasoning, base_initialized_at,
	       created_at, updated_at
	FROM documents WHERE id = $1`, id).Scan(
		&d.ID, &d.Filename, &d.SourceRef, &d.Status, &d.Error, &d.ManifestRef, &d.PageCount, &d.DPI,
		&d.QueryModel, &d.QueryReasoning,
		&d.BaseResponseID, &d.BaseModel, &d.BaseReasoning, &d.BaseInitializedAt,
		&d.CreatedAt, &d.UpdatedAt,
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
	SELECT id, filename, source_ref, status, error, manifest_ref, page_count, dpi,
	       query_model, query_reasoning,
	       base_response_id, base_model, base_reasoning, base_initialized_at,
	       created_at, updated_at
	FROM documents ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list documents: %w", err)
	}
	defer rows.Close()

	var docs []Document
	for rows.Next() {
		var d Document
		if err := rows.Scan(
			&d.ID, &d.Filename, &d.SourceRef, &d.Status, &d.Error, &d.ManifestRef, &d.PageCount, &d.DPI,
			&d.QueryModel, &d.QueryReasoning,
			&d.BaseResponseID, &d.BaseModel, &d.BaseReasoning, &d.BaseInitializedAt,
			&d.CreatedAt, &d.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan document: %w", err)
		}
		docs = append(docs, d)
	}
	return docs, rows.Err()
}

func (s *Store) UpdateBaseLineage(ctx context.Context, id int64, baseResponseID, baseModel, baseReasoning string) error {
	_, err := s.pool.Exec(ctx, `
	UPDATE documents
	SET base_response_id = $2, base_model = $3, base_reasoning = $4, base_initialized_at = now(), updated_at = now()
	WHERE id = $1`, id, baseResponseID, baseModel, baseReasoning)
	if err != nil {
		return fmt.Errorf("update document base lineage: %w", err)
	}
	return nil
}

func (s *Store) UpdateSettings(ctx context.Context, id int64, queryModel, queryReasoning string, clearBase bool) (Document, error) {
	var d Document
	err := s.pool.QueryRow(ctx, `
	UPDATE documents
	SET query_model = $2,
	    query_reasoning = $3,
	    base_response_id = CASE WHEN $4 THEN '' ELSE base_response_id END,
	    base_model = CASE WHEN $4 THEN '' ELSE base_model END,
	    base_reasoning = CASE WHEN $4 THEN '' ELSE base_reasoning END,
	    base_initialized_at = CASE WHEN $4 THEN NULL ELSE base_initialized_at END,
	    updated_at = now()
	WHERE id = $1
	RETURNING id, filename, source_ref, status, error, manifest_ref, page_count, dpi,
	          query_model, query_reasoning,
	          base_response_id, base_model, base_reasoning, base_initialized_at,
	          created_at, updated_at`, id, queryModel, queryReasoning, clearBase).Scan(
		&d.ID, &d.Filename, &d.SourceRef, &d.Status, &d.Error, &d.ManifestRef, &d.PageCount, &d.DPI,
		&d.QueryModel, &d.QueryReasoning,
		&d.BaseResponseID, &d.BaseModel, &d.BaseReasoning, &d.BaseInitializedAt,
		&d.CreatedAt, &d.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Document{}, ErrDocumentNotFound
	}
	if err != nil {
		return Document{}, fmt.Errorf("update document settings: %w", err)
	}
	return d, nil
}

func (s *Store) Delete(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM documents WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete document: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrDocumentNotFound
	}
	return nil
}

func (s *Store) UpdateStatus(ctx context.Context, id int64, status, manifestRef string, pageCount int, errMsg string) error {
	_, err := s.pool.Exec(ctx, `
UPDATE documents SET status = $2, manifest_ref = $3, page_count = $4, error = $5, updated_at = now()
WHERE id = $1`, id, status, manifestRef, pageCount, errMsg)
	if err != nil {
		return fmt.Errorf("update document status: %w", err)
	}
	return nil
}
