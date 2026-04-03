-- Add include_json column to threads table.
-- Stores the OpenAI Responses include contract, including reasoning.encrypted_content.

ALTER TABLE threads ADD COLUMN IF NOT EXISTS include_json jsonb;
