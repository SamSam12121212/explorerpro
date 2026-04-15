ALTER TABLE threads
    ADD COLUMN archived_at timestamptz;

CREATE INDEX idx_threads_root_unarchived_updated_at
    ON threads (updated_at DESC, id DESC)
    WHERE parent_thread_id IS NULL
      AND archived_at IS NULL;
