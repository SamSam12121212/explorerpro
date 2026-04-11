package collectionstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"explorer/internal/docstore"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrCollectionNotFound = errors.New("collection not found")
var ErrCollectionNameExists = errors.New("collection name already exists")

type Collection struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	DocumentCount int       `json:"document_count"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) Create(ctx context.Context, collection Collection) error {
	_, err := s.pool.Exec(ctx, `
INSERT INTO collections (id, name, created_at, updated_at)
VALUES ($1, $2, $3, $4)`,
		collection.ID, collection.Name, collection.CreatedAt, collection.UpdatedAt,
	)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrCollectionNameExists
	}
	if err != nil {
		return fmt.Errorf("insert collection: %w", err)
	}
	return nil
}

func (s *Store) Get(ctx context.Context, id string) (Collection, error) {
	var collection Collection
	err := s.pool.QueryRow(ctx, `
SELECT
    c.id,
    c.name,
    COUNT(cd.document_id)::integer AS document_count,
    c.created_at,
    c.updated_at
FROM collections c
LEFT JOIN collection_documents cd ON cd.collection_id = c.id
WHERE c.id = $1
GROUP BY c.id, c.name, c.created_at, c.updated_at`,
		id,
	).Scan(
		&collection.ID,
		&collection.Name,
		&collection.DocumentCount,
		&collection.CreatedAt,
		&collection.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Collection{}, ErrCollectionNotFound
	}
	if err != nil {
		return Collection{}, fmt.Errorf("get collection: %w", err)
	}
	return collection, nil
}

func (s *Store) List(ctx context.Context, limit int64) ([]Collection, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
SELECT
    c.id,
    c.name,
    COUNT(cd.document_id)::integer AS document_count,
    c.created_at,
    c.updated_at
FROM collections c
LEFT JOIN collection_documents cd ON cd.collection_id = c.id
GROUP BY c.id, c.name, c.created_at, c.updated_at
ORDER BY c.updated_at DESC, c.created_at DESC
LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list collections: %w", err)
	}
	defer rows.Close()

	var collections []Collection
	for rows.Next() {
		var collection Collection
		if err := rows.Scan(
			&collection.ID,
			&collection.Name,
			&collection.DocumentCount,
			&collection.CreatedAt,
			&collection.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan collection: %w", err)
		}
		collections = append(collections, collection)
	}

	return collections, rows.Err()
}

func (s *Store) ListDocuments(ctx context.Context, collectionID string, limit int64) ([]docstore.Document, error) {
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
FROM collection_documents cd
JOIN documents d ON d.id = cd.document_id
WHERE cd.collection_id = $1
ORDER BY cd.created_at DESC, d.created_at DESC
LIMIT $2`, collectionID, limit)
	if err != nil {
		return nil, fmt.Errorf("list collection documents: %w", err)
	}
	defer rows.Close()

	var documents []docstore.Document
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
			return nil, fmt.Errorf("scan collection document: %w", err)
		}
		documents = append(documents, document)
	}

	return documents, rows.Err()
}

func (s *Store) AddDocument(ctx context.Context, collectionID, documentID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin add collection document tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	tag, err := tx.Exec(ctx, `
INSERT INTO collection_documents (collection_id, document_id)
VALUES ($1, $2)
ON CONFLICT (collection_id, document_id) DO NOTHING`, collectionID, documentID)
	if err != nil {
		return fmt.Errorf("insert collection document: %w", err)
	}

	if tag.RowsAffected() > 0 {
		if _, err := tx.Exec(ctx, `
UPDATE collections
SET updated_at = now()
WHERE id = $1`, collectionID); err != nil {
			return fmt.Errorf("touch collection after add document: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit add collection document: %w", err)
	}

	return nil
}
