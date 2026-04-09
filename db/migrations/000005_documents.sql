CREATE TABLE IF NOT EXISTS documents (
    id            text PRIMARY KEY,
    source_ref    text NOT NULL,
    status        text NOT NULL DEFAULT 'pending',
    error         text NOT NULL DEFAULT '',
    manifest_ref  text NOT NULL DEFAULT '',
    page_count    integer NOT NULL DEFAULT 0,
    dpi           integer NOT NULL DEFAULT 150,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_documents_status ON documents (status);
CREATE INDEX IF NOT EXISTS idx_documents_created_at ON documents (created_at DESC);
