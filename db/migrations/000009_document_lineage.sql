ALTER TABLE documents
ADD COLUMN IF NOT EXISTS base_response_id text NOT NULL DEFAULT '',
ADD COLUMN IF NOT EXISTS base_model text NOT NULL DEFAULT '',
ADD COLUMN IF NOT EXISTS base_initialized_at timestamptz;

ALTER TABLE thread_documents
ADD COLUMN IF NOT EXISTS latest_response_id text NOT NULL DEFAULT '',
ADD COLUMN IF NOT EXISTS initialized_at timestamptz,
ADD COLUMN IF NOT EXISTS last_used_at timestamptz;
