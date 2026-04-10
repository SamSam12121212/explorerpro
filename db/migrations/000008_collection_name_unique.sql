CREATE UNIQUE INDEX IF NOT EXISTS idx_collections_name_unique
ON collections (lower(name));
