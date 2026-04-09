package collectionstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrCollectionNotFound = errors.New("collection not found")

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
