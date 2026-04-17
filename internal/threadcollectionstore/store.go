package threadcollectionstore

import (
	"context"
	"fmt"

	"explorer/internal/docstore"

	"github.com/jackc/pgx/v5/pgxpool"
)

// AttachedCollection is a collection linked to a thread together with its
// current (live) member documents. The documents are re-resolved every time
// ListAttached is called so that adding or removing a document from a
// collection is reflected on the thread's next turn without any explicit
// re-attach.
type AttachedCollection struct {
	ID        string
	Name      string
	Documents []docstore.Document
}

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// AddCollections links the given collection IDs to the thread. Duplicate
// (thread_id, collection_id) pairs are ignored. Caller is responsible for
// validating the collections exist before calling.
func (s *Store) AddCollections(ctx context.Context, threadID int64, collectionIDs []string) error {
	if len(collectionIDs) == 0 {
		return nil
	}

	_, err := s.pool.Exec(ctx, `
INSERT INTO thread_collections (thread_id, collection_id)
SELECT $1, collection_id
FROM unnest($2::text[]) AS collection_id
ON CONFLICT (thread_id, collection_id) DO NOTHING
	`, threadID, collectionIDs)
	if err != nil {
		return fmt.Errorf("insert thread collections for %d: %w", threadID, err)
	}

	return nil
}

// ListAttached returns every collection attached to the thread along with the
// collection's current member documents. Ordering is stable — attached
// collections by thread_collections.created_at ascending, members by
// collection_documents.created_at ascending — so that an unchanged attachment
// yields a byte-identical runtime context between turns (keeps the prompt
// cache warm).
//
// Uses two queries regardless of N: one for the attached collections, one
// batched query that fetches every member document across all of them
// (filtered by ANY($collection_ids)). Grouping and in-collection ordering
// happen in Go.
func (s *Store) ListAttached(ctx context.Context, threadID int64, limit int64) ([]AttachedCollection, error) {
	if limit <= 0 {
		limit = 100
	}

	collectionRows, err := s.pool.Query(ctx, `
SELECT c.id, c.name
FROM thread_collections tc
JOIN collections c ON c.id = tc.collection_id
WHERE tc.thread_id = $1
ORDER BY tc.created_at ASC, c.id ASC
LIMIT $2`, threadID, limit)
	if err != nil {
		return nil, fmt.Errorf("list thread collections for %d: %w", threadID, err)
	}
	defer collectionRows.Close()

	var attached []AttachedCollection
	indexByID := map[string]int{}
	for collectionRows.Next() {
		var c AttachedCollection
		if err := collectionRows.Scan(&c.ID, &c.Name); err != nil {
			return nil, fmt.Errorf("scan thread collection: %w", err)
		}
		c.Documents = []docstore.Document{}
		indexByID[c.ID] = len(attached)
		attached = append(attached, c)
	}
	if err := collectionRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate thread collections: %w", err)
	}

	if len(attached) == 0 {
		return attached, nil
	}

	collectionIDs := make([]string, len(attached))
	for i, c := range attached {
		collectionIDs[i] = c.ID
	}

	// One batched fetch for every member across every attached collection.
	// Ordering inside each collection is stable (cd.created_at ASC, d.id ASC)
	// and grouping is done in Go by reading rows in collection_id, cd.created_at
	// order.
	docRows, err := s.pool.Query(ctx, `
SELECT
    cd.collection_id,
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
WHERE cd.collection_id = ANY($1::text[])
ORDER BY cd.collection_id ASC, cd.created_at ASC, d.id ASC`, collectionIDs)
	if err != nil {
		return nil, fmt.Errorf("list collection documents for thread %d: %w", threadID, err)
	}
	defer docRows.Close()

	for docRows.Next() {
		var (
			collectionID string
			document     docstore.Document
		)
		if err := docRows.Scan(
			&collectionID,
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

		idx, ok := indexByID[collectionID]
		if !ok {
			// Shouldn't happen — WHERE clause is filtered to these IDs — but
			// skip defensively rather than panic.
			continue
		}
		attached[idx].Documents = append(attached[idx].Documents, document)
	}
	if err := docRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate collection documents: %w", err)
	}

	return attached, nil
}
