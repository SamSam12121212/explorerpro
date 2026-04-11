ALTER TABLE documents
ADD COLUMN IF NOT EXISTS query_model text NOT NULL DEFAULT 'gpt-5.4';

UPDATE documents
SET query_model = base_model
WHERE base_model <> '';

ALTER TABLE thread_documents
ADD COLUMN IF NOT EXISTS latest_model text NOT NULL DEFAULT '';
