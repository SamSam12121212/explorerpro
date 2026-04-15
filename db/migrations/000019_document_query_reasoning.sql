ALTER TABLE documents
ADD COLUMN IF NOT EXISTS query_reasoning text NOT NULL DEFAULT 'medium',
ADD COLUMN IF NOT EXISTS base_reasoning text NOT NULL DEFAULT '';
