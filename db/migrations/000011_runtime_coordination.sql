CREATE TABLE thread_owners (
    thread_id text PRIMARY KEY REFERENCES threads (id) ON DELETE CASCADE,
    worker_id text NOT NULL,
    lease_until timestamptz NOT NULL,
    socket_generation bigint NOT NULL,
    claimed_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT thread_owners_worker_id_not_blank CHECK (btrim(worker_id) <> ''),
    CONSTRAINT thread_owners_socket_generation_positive CHECK (socket_generation > 0)
);

CREATE INDEX idx_thread_owners_worker_lease
    ON thread_owners (worker_id, lease_until DESC);

CREATE TRIGGER trg_thread_owners_set_updated_at
BEFORE UPDATE ON thread_owners
FOR EACH ROW
EXECUTE FUNCTION set_updated_at();

CREATE TABLE thread_processed_commands (
    thread_id text NOT NULL REFERENCES threads (id) ON DELETE CASCADE,
    cmd_id text NOT NULL,
    processed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (thread_id, cmd_id),
    CONSTRAINT thread_processed_commands_cmd_id_not_blank CHECK (btrim(cmd_id) <> '')
);

CREATE INDEX idx_thread_processed_commands_processed_at
    ON thread_processed_commands (processed_at DESC);
