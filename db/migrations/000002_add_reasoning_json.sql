-- Add reasoning_json column to threads table
-- Stores the OpenAI reasoning configuration (effort, summary) as JSONB.

ALTER TABLE threads ADD COLUMN IF NOT EXISTS reasoning_json jsonb;
