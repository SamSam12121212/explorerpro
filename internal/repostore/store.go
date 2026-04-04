package repostore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrRepoNotFound = errors.New("repo not found")

type Repo struct {
	ID        string    `json:"id"`
	URL       string    `json:"url"`
	Ref       string    `json:"ref"`
	Name      string    `json:"name"`
	Status    string    `json:"status"`
	Error     string    `json:"error,omitempty"`
	ClonePath string    `json:"clone_path,omitempty"`
	CommitSHA string    `json:"commit_sha,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) Create(ctx context.Context, repo Repo) error {
	_, err := s.pool.Exec(ctx, `
INSERT INTO repos (id, url, ref, name, status, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		repo.ID, repo.URL, repo.Ref, repo.Name, repo.Status, repo.CreatedAt, repo.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert repo: %w", err)
	}
	return nil
}

func (s *Store) Get(ctx context.Context, id string) (Repo, error) {
	var r Repo
	err := s.pool.QueryRow(ctx, `
SELECT id, url, ref, name, status, error, clone_path, commit_sha, created_at, updated_at
FROM repos WHERE id = $1`, id).Scan(
		&r.ID, &r.URL, &r.Ref, &r.Name, &r.Status, &r.Error, &r.ClonePath, &r.CommitSHA, &r.CreatedAt, &r.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Repo{}, ErrRepoNotFound
	}
	if err != nil {
		return Repo{}, fmt.Errorf("get repo: %w", err)
	}
	return r, nil
}

func (s *Store) List(ctx context.Context, limit int64) ([]Repo, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
SELECT id, url, ref, name, status, error, clone_path, commit_sha, created_at, updated_at
FROM repos ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list repos: %w", err)
	}
	defer rows.Close()

	var repos []Repo
	for rows.Next() {
		var r Repo
		if err := rows.Scan(&r.ID, &r.URL, &r.Ref, &r.Name, &r.Status, &r.Error, &r.ClonePath, &r.CommitSHA, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan repo: %w", err)
		}
		repos = append(repos, r)
	}
	return repos, rows.Err()
}

func (s *Store) UpdateStatus(ctx context.Context, id, status, clonePath, commitSHA, errMsg string) error {
	_, err := s.pool.Exec(ctx, `
UPDATE repos SET status = $2, clone_path = $3, commit_sha = $4, error = $5, updated_at = now()
WHERE id = $1`, id, status, clonePath, commitSHA, errMsg)
	if err != nil {
		return fmt.Errorf("update repo status: %w", err)
	}
	return nil
}

func (s *Store) Delete(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM repos WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete repo: %w", err)
	}
	return nil
}
