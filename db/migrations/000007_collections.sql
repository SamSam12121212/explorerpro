CREATE TABLE IF NOT EXISTS collections (
    id          text PRIMARY KEY,
    name        text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_collections_updated_at ON collections (updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_collections_created_at ON collections (created_at DESC);

CREATE TABLE IF NOT EXISTS collection_documents (
    collection_id  text NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
    document_id    text NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    created_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (collection_id, document_id)
);

CREATE INDEX IF NOT EXISTS idx_collection_documents_document_id ON collection_documents (document_id);
