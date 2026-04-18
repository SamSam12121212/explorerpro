// Package citationstore persists evidence-chain citations. A citation is a
// thread-scoped pointer from a root thread into a specific document page
// (or two consecutive pages). Each citation has 1–2 bbox rows — one per
// page — carrying the OCR line indices whose unioned envelope highlights
// the evidence on that page.
//
// The line indices are the canonical identity; bbox_x/y/width/height are
// materialized for paint-time speed. poly_json preserves the OCR poly
// points for future rotation-aware rendering.
package citationstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrCitationNotFound = errors.New("citation not found")

type Citation struct {
	ID           int64     `json:"id"`
	RootThreadID int64     `json:"root_thread_id"`
	DocumentID   int64     `json:"document_id"`
	Instruction  string    `json:"instruction"`
	CreatedAt    time.Time `json:"created_at"`
	Bboxes       []Bbox    `json:"bboxes"`
}

type Bbox struct {
	ID          int64           `json:"id"`
	Page        int             `json:"page"`
	LineIndices []int32         `json:"line_indices"`
	X           int             `json:"x"`
	Y           int             `json:"y"`
	Width       int             `json:"width"`
	Height      int             `json:"height"`
	PolyJSON    json.RawMessage `json:"poly_json,omitempty"`
}

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Create inserts the citation and its per-page bbox rows atomically,
// returning the new citation id. Bboxes slice must have 1 or 2 entries;
// caller is responsible for enforcing the consecutive-pages rule (happens
// earlier in documenthandler before we get here).
func (s *Store) Create(ctx context.Context, c Citation) (int64, error) {
	if c.RootThreadID <= 0 {
		return 0, fmt.Errorf("root_thread_id is required")
	}
	if c.DocumentID <= 0 {
		return 0, fmt.Errorf("document_id is required")
	}
	if len(c.Bboxes) == 0 {
		return 0, fmt.Errorf("at least one bbox is required")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin citation tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	createdAt := c.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}

	var citationID int64
	err = tx.QueryRow(ctx, `
INSERT INTO citations (root_thread_id, document_id, instruction, created_at)
VALUES ($1, $2, $3, $4)
RETURNING id
`, c.RootThreadID, c.DocumentID, c.Instruction, createdAt).Scan(&citationID)
	if err != nil {
		return 0, fmt.Errorf("insert citation: %w", err)
	}

	for _, b := range c.Bboxes {
		if b.Page <= 0 {
			return 0, fmt.Errorf("bbox page must be positive, got %d", b.Page)
		}
		if len(b.LineIndices) == 0 {
			return 0, fmt.Errorf("bbox line_indices must not be empty (page %d)", b.Page)
		}
		var poly any
		if len(b.PolyJSON) > 0 {
			poly = []byte(b.PolyJSON)
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO citation_bboxes (citation_id, page, line_indices, bbox_x, bbox_y, bbox_width, bbox_height, poly_json, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
`, citationID, b.Page, b.LineIndices, b.X, b.Y, b.Width, b.Height, poly, createdAt); err != nil {
			return 0, fmt.Errorf("insert citation bbox (page %d): %w", b.Page, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit citation tx: %w", err)
	}

	return citationID, nil
}

// Get loads a single citation with its bboxes.
func (s *Store) Get(ctx context.Context, id int64) (Citation, error) {
	var c Citation
	err := s.pool.QueryRow(ctx, `
SELECT id, root_thread_id, document_id, instruction, created_at
FROM citations
WHERE id = $1
`, id).Scan(&c.ID, &c.RootThreadID, &c.DocumentID, &c.Instruction, &c.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Citation{}, ErrCitationNotFound
	}
	if err != nil {
		return Citation{}, fmt.Errorf("get citation: %w", err)
	}

	bboxes, err := s.loadBboxes(ctx, []int64{id})
	if err != nil {
		return Citation{}, err
	}
	c.Bboxes = bboxes[id]
	return c, nil
}

// ListForThread returns all citations attached to a root thread, with their
// bboxes, ordered by creation time ascending.
func (s *Store) ListForThread(ctx context.Context, rootThreadID int64) ([]Citation, error) {
	if rootThreadID <= 0 {
		return nil, fmt.Errorf("root_thread_id is required")
	}

	rows, err := s.pool.Query(ctx, `
SELECT id, root_thread_id, document_id, instruction, created_at
FROM citations
WHERE root_thread_id = $1
ORDER BY created_at ASC, id ASC
`, rootThreadID)
	if err != nil {
		return nil, fmt.Errorf("list citations for thread: %w", err)
	}
	defer rows.Close()

	var citations []Citation
	var ids []int64
	for rows.Next() {
		var c Citation
		if err := rows.Scan(&c.ID, &c.RootThreadID, &c.DocumentID, &c.Instruction, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan citation: %w", err)
		}
		citations = append(citations, c)
		ids = append(ids, c.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate citations: %w", err)
	}

	if len(ids) == 0 {
		return citations, nil
	}

	bboxes, err := s.loadBboxes(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range citations {
		citations[i].Bboxes = bboxes[citations[i].ID]
	}
	return citations, nil
}

// loadBboxes returns bboxes grouped by citation_id, ordered by page then id
// within each group.
func (s *Store) loadBboxes(ctx context.Context, citationIDs []int64) (map[int64][]Bbox, error) {
	out := make(map[int64][]Bbox, len(citationIDs))
	if len(citationIDs) == 0 {
		return out, nil
	}

	rows, err := s.pool.Query(ctx, `
SELECT id, citation_id, page, line_indices, bbox_x, bbox_y, bbox_width, bbox_height, poly_json
FROM citation_bboxes
WHERE citation_id = ANY($1)
ORDER BY citation_id ASC, page ASC, id ASC
`, citationIDs)
	if err != nil {
		return nil, fmt.Errorf("list citation bboxes: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var b Bbox
		var citationID int64
		var poly []byte
		if err := rows.Scan(&b.ID, &citationID, &b.Page, &b.LineIndices, &b.X, &b.Y, &b.Width, &b.Height, &poly); err != nil {
			return nil, fmt.Errorf("scan citation bbox: %w", err)
		}
		if len(poly) > 0 {
			b.PolyJSON = json.RawMessage(poly)
		}
		out[citationID] = append(out[citationID], b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate citation bboxes: %w", err)
	}
	return out, nil
}
