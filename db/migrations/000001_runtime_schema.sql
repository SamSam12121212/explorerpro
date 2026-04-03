-- Explorer runtime persistence v1
--
-- Important boundary:
-- Redis remains the live execution truth.
-- Postgres stores durable snapshots and append-only history.

CREATE OR REPLACE FUNCTION set_updated_at()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$;

CREATE TABLE threads (
    id text PRIMARY KEY,
    root_thread_id text NOT NULL,
    parent_thread_id text REFERENCES threads (id) ON DELETE RESTRICT,
    parent_call_id text,
    depth integer NOT NULL DEFAULT 0,
    status text NOT NULL,
    model text NOT NULL,
    instructions text NOT NULL DEFAULT '',
    metadata_json jsonb NOT NULL DEFAULT '{}'::jsonb,
    tools_json jsonb,
    tool_choice_json jsonb,
    owner_worker_id text,
    socket_generation bigint NOT NULL DEFAULT 0,
    socket_expires_at timestamptz,
    last_response_id text,
    active_response_id text,
    active_spawn_group_id text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT threads_id_not_blank CHECK (btrim(id) <> ''),
    CONSTRAINT threads_root_thread_not_blank CHECK (btrim(root_thread_id) <> ''),
    CONSTRAINT threads_depth_nonnegative CHECK (depth >= 0),
    CONSTRAINT threads_socket_generation_nonnegative CHECK (socket_generation >= 0),
    CONSTRAINT threads_status_valid CHECK (
        status IN (
            'new',
            'ready',
            'running',
            'reconciling',
            'waiting_tool',
            'waiting_children',
            'completed',
            'failed',
            'incomplete',
            'cancelled',
            'orphaned'
        )
    )
);

CREATE INDEX idx_threads_root_thread_id
    ON threads (root_thread_id, created_at);

CREATE INDEX idx_threads_parent_thread_id
    ON threads (parent_thread_id, created_at)
    WHERE parent_thread_id IS NOT NULL;

CREATE INDEX idx_threads_status_updated_at
    ON threads (status, updated_at DESC);

CREATE INDEX idx_threads_last_response_id
    ON threads (last_response_id)
    WHERE last_response_id IS NOT NULL;

CREATE INDEX idx_threads_active_response_id
    ON threads (active_response_id)
    WHERE active_response_id IS NOT NULL;

CREATE TRIGGER trg_threads_set_updated_at
BEFORE UPDATE ON threads
FOR EACH ROW
EXECUTE FUNCTION set_updated_at();

CREATE TABLE responses (
    id text PRIMARY KEY,
    source_thread_id text NOT NULL REFERENCES threads (id) ON DELETE RESTRICT,
    status text,
    response_json jsonb NOT NULL,
    recorded_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT responses_id_not_blank CHECK (btrim(id) <> '')
);

CREATE INDEX idx_responses_source_thread_id
    ON responses (source_thread_id, recorded_at DESC);

CREATE TABLE thread_response_links (
    thread_id text NOT NULL REFERENCES threads (id) ON DELETE CASCADE,
    response_id text NOT NULL,
    link_kind text NOT NULL DEFAULT 'owned',
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (thread_id, response_id),
    CONSTRAINT thread_response_links_kind_valid CHECK (
        link_kind IN (
            'owned',
            'branch_source',
            'referenced'
        )
    )
);

CREATE INDEX idx_thread_response_links_response_id
    ON thread_response_links (response_id);

CREATE TABLE thread_items (
    thread_id text NOT NULL REFERENCES threads (id) ON DELETE CASCADE,
    seq bigint NOT NULL,
    response_id text,
    item_type text NOT NULL,
    direction text NOT NULL,
    payload_json jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (thread_id, seq),
    CONSTRAINT thread_items_seq_positive CHECK (seq > 0),
    CONSTRAINT thread_items_direction_valid CHECK (
        direction IN (
            'input',
            'output'
        )
    )
);

CREATE INDEX idx_thread_items_thread_created_at
    ON thread_items (thread_id, created_at DESC);

CREATE INDEX idx_thread_items_response_id
    ON thread_items (response_id)
    WHERE response_id IS NOT NULL;

CREATE TABLE thread_events (
    thread_id text NOT NULL REFERENCES threads (id) ON DELETE CASCADE,
    event_seq bigint NOT NULL,
    socket_generation bigint NOT NULL,
    event_type text NOT NULL,
    response_id text,
    payload_json jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (thread_id, event_seq),
    CONSTRAINT thread_events_event_seq_positive CHECK (event_seq > 0),
    CONSTRAINT thread_events_socket_generation_nonnegative CHECK (socket_generation >= 0)
);

CREATE INDEX idx_thread_events_thread_created_at
    ON thread_events (thread_id, created_at DESC);

CREATE INDEX idx_thread_events_response_id
    ON thread_events (response_id)
    WHERE response_id IS NOT NULL;

CREATE INDEX idx_thread_events_event_type
    ON thread_events (event_type);

CREATE TABLE spawn_groups (
    id text PRIMARY KEY,
    parent_thread_id text NOT NULL REFERENCES threads (id) ON DELETE RESTRICT,
    parent_call_id text NOT NULL,
    expected integer NOT NULL,
    completed integer NOT NULL DEFAULT 0,
    failed integer NOT NULL DEFAULT 0,
    cancelled integer NOT NULL DEFAULT 0,
    status text NOT NULL,
    aggregate_submitted_at timestamptz,
    aggregate_cmd_id text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT spawn_groups_id_not_blank CHECK (btrim(id) <> ''),
    CONSTRAINT spawn_groups_expected_nonnegative CHECK (expected >= 0),
    CONSTRAINT spawn_groups_completed_nonnegative CHECK (completed >= 0),
    CONSTRAINT spawn_groups_failed_nonnegative CHECK (failed >= 0),
    CONSTRAINT spawn_groups_cancelled_nonnegative CHECK (cancelled >= 0),
    CONSTRAINT spawn_groups_counts_within_expected CHECK (completed + failed + cancelled <= expected),
    CONSTRAINT spawn_groups_status_valid CHECK (
        status IN (
            'waiting',
            'closed',
            'cancelled'
        )
    )
);

CREATE INDEX idx_spawn_groups_parent_thread_id
    ON spawn_groups (parent_thread_id, created_at);

CREATE TRIGGER trg_spawn_groups_set_updated_at
BEFORE UPDATE ON spawn_groups
FOR EACH ROW
EXECUTE FUNCTION set_updated_at();

CREATE TABLE spawn_group_children (
    spawn_group_id text NOT NULL REFERENCES spawn_groups (id) ON DELETE CASCADE,
    child_thread_id text NOT NULL REFERENCES threads (id) ON DELETE RESTRICT,
    child_index integer,
    status text NOT NULL DEFAULT 'pending',
    child_response_id text,
    assistant_text text,
    result_ref text,
    summary_ref text,
    error_ref text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (spawn_group_id, child_thread_id),
    CONSTRAINT spawn_group_children_index_nonnegative CHECK (child_index IS NULL OR child_index >= 0),
    CONSTRAINT spawn_group_children_status_valid CHECK (
        status IN (
            'pending',
            'completed',
            'failed',
            'cancelled'
        )
    )
);

CREATE UNIQUE INDEX idx_spawn_group_children_group_index
    ON spawn_group_children (spawn_group_id, child_index)
    WHERE child_index IS NOT NULL;

CREATE INDEX idx_spawn_group_children_child_thread_id
    ON spawn_group_children (child_thread_id);

CREATE INDEX idx_spawn_group_children_status
    ON spawn_group_children (status);

CREATE TRIGGER trg_spawn_group_children_set_updated_at
BEFORE UPDATE ON spawn_group_children
FOR EACH ROW
EXECUTE FUNCTION set_updated_at();
