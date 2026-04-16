CREATE TABLE IF NOT EXISTS thread_collections (
    thread_id      bigint NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
    collection_id  text NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
    created_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (thread_id, collection_id)
);

CREATE INDEX IF NOT EXISTS idx_thread_collections_collection_id
    ON thread_collections (collection_id);
