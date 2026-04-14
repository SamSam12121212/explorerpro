CREATE TABLE openai_socket_sessions (
    id text PRIMARY KEY,
    thread_id text NOT NULL REFERENCES threads (id) ON DELETE CASCADE,
    root_thread_id text NOT NULL REFERENCES threads (id) ON DELETE CASCADE,
    parent_thread_id text REFERENCES threads (id) ON DELETE CASCADE,
    worker_id text NOT NULL,
    thread_socket_generation bigint NOT NULL,
    state text NOT NULL,
    connected_at timestamptz NOT NULL,
    last_read_at timestamptz,
    last_write_at timestamptz,
    last_heartbeat_at timestamptz NOT NULL,
    heartbeat_expires_at timestamptz,
    disconnected_at timestamptz,
    disconnect_reason text,
    expires_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT openai_socket_sessions_id_not_blank CHECK (btrim(id) <> ''),
    CONSTRAINT openai_socket_sessions_worker_id_not_blank CHECK (btrim(worker_id) <> ''),
    CONSTRAINT openai_socket_sessions_thread_socket_generation_positive CHECK (thread_socket_generation > 0),
    CONSTRAINT openai_socket_sessions_state_valid CHECK (
        state IN ('connected', 'disconnected')
    ),
    CONSTRAINT openai_socket_sessions_last_heartbeat_after_connect CHECK (
        last_heartbeat_at >= connected_at
    ),
    CONSTRAINT openai_socket_sessions_last_read_after_connect CHECK (
        last_read_at IS NULL OR last_read_at >= connected_at
    ),
    CONSTRAINT openai_socket_sessions_last_write_after_connect CHECK (
        last_write_at IS NULL OR last_write_at >= connected_at
    ),
    CONSTRAINT openai_socket_sessions_disconnected_after_connect CHECK (
        disconnected_at IS NULL OR disconnected_at >= connected_at
    ),
    CONSTRAINT openai_socket_sessions_connected_shape CHECK (
        (
            state = 'connected'
            AND heartbeat_expires_at IS NOT NULL
            AND disconnected_at IS NULL
            AND expires_at IS NULL
            AND btrim(COALESCE(disconnect_reason, '')) = ''
        )
        OR (
            state = 'disconnected'
            AND disconnected_at IS NOT NULL
            AND expires_at IS NOT NULL
        )
    )
);

CREATE INDEX idx_openai_socket_sessions_thread_connected_at
    ON openai_socket_sessions (thread_id, connected_at DESC);

CREATE INDEX idx_openai_socket_sessions_worker_state_connected_at
    ON openai_socket_sessions (worker_id, state, connected_at DESC);

CREATE INDEX idx_openai_socket_sessions_state_heartbeat_expires_at
    ON openai_socket_sessions (state, heartbeat_expires_at);

CREATE INDEX idx_openai_socket_sessions_state_expires_at
    ON openai_socket_sessions (state, expires_at);

CREATE TRIGGER trg_openai_socket_sessions_set_updated_at
BEFORE UPDATE ON openai_socket_sessions
FOR EACH ROW
EXECUTE FUNCTION set_updated_at();
