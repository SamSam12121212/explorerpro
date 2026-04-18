-- Schema backing for the evidence-locator child thread type.
--
-- threads.one_shot — when true, the worker skips the idle/warm-socket loop
-- after the child's terminal response and releases the socket immediately.
-- Not tied to child_kind: this is a general "one response then close"
-- mode that other callers can reuse later.
--
-- threads_child_shape_valid relaxed to allow child_kind='citation_locator'
-- (document_id pinned, no document_phase).
--
-- spawn_group_children.tool_call_args_json — carries the forced-tool-call
-- arguments a locator child emits on terminal. The main thread never
-- reads this directly; documenthandler parses it in the finalize RPC
-- before building the parent's function_call_output.

ALTER TABLE threads
    ADD COLUMN IF NOT EXISTS one_shot boolean NOT NULL DEFAULT false;

ALTER TABLE threads DROP CONSTRAINT IF EXISTS threads_child_shape_valid;

ALTER TABLE threads ADD CONSTRAINT threads_child_shape_valid CHECK (
    (
        child_kind IS NULL
        AND document_id IS NULL
        AND document_phase IS NULL
    )
    OR (
        child_kind = 'document'
        AND document_id IS NOT NULL
        AND document_phase IN ('warmup', 'query')
    )
    OR (
        child_kind = 'citation_locator'
        AND document_id IS NOT NULL
        AND document_phase IS NULL
    )
);

ALTER TABLE spawn_group_children
    ADD COLUMN IF NOT EXISTS tool_call_args_json jsonb;
