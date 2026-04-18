-- Allow 'citation_locator' as a spawn_groups.group_kind.
--
-- Migration 000023 relaxed threads_child_shape_valid to accept
-- child_kind='citation_locator', but missed the sibling check on
-- spawn_groups.group_kind. Result: when the main thread emits
-- store_citation calls, startCitationLocatorGroup -> LoadOrCreateSpawnGroup
-- fails at the first INSERT with "violates check constraint
-- spawn_groups_kind_valid", the child_completed command that was driving
-- the turn NAKs, and redelivery drops on the waiting_children precondition
-- (status was still 'running' because the status flip happens after the
-- spawn call returns). Thread hangs until manual cancel.

ALTER TABLE spawn_groups DROP CONSTRAINT IF EXISTS spawn_groups_kind_valid;

ALTER TABLE spawn_groups ADD CONSTRAINT spawn_groups_kind_valid CHECK (
    group_kind IN (
        'document_query',
        'thread_spawn',
        'citation_locator'
    )
);
