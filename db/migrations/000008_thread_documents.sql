CREATE TABLE IF NOT EXISTS thread_documents (
    thread_id    text NOT NULL REFERENCES threads(id) ON DELETE CASCADE,
    document_id  text NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (thread_id, document_id)
);

CREATE INDEX IF NOT EXISTS idx_thread_documents_document_id
    ON thread_documents (document_id);
