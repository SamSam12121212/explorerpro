CREATE TABLE IF NOT EXISTS repos (
    id          text PRIMARY KEY,
    url         text NOT NULL,
    ref         text NOT NULL DEFAULT 'main',
    name        text NOT NULL,
    status      text NOT NULL DEFAULT 'pending',
    error       text NOT NULL DEFAULT '',
    clone_path  text NOT NULL DEFAULT '',
    commit_sha  text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_repos_status ON repos (status);
CREATE INDEX IF NOT EXISTS idx_repos_created_at ON repos (created_at DESC);
