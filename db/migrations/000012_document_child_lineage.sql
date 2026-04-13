CREATE INDEX IF NOT EXISTS idx_threads_parent_document_query_lineage
    ON threads (parent_thread_id, ((metadata_json ->> 'document_id')), updated_at DESC)
    WHERE parent_thread_id IS NOT NULL
      AND metadata_json ->> 'spawn_mode' = 'document_query'
      AND status = 'completed'
      AND last_response_id IS NOT NULL;

ALTER TABLE thread_documents
DROP COLUMN IF EXISTS latest_response_id,
DROP COLUMN IF EXISTS latest_model,
DROP COLUMN IF EXISTS initialized_at,
DROP COLUMN IF EXISTS last_used_at;
