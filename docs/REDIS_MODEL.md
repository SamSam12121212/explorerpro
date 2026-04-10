# Redis Model

## Overview

Redis is the runtime source of truth for:

- Thread ownership
- Actor preconditions
- Response continuity
- Spawn barriers
- Replay and recovery state

The design deliberately separates:

- Hot scalar state
- Ordered append-only logs
- Raw response payloads

## Keyspace

### Thread Metadata

- `thread:{thread_id}:meta`

Recommended storage: Redis hash with JSON-encoded nested fields where needed.

Recommended fields:

- `id`
- `root_thread_id`
- `parent_thread_id`
- `parent_call_id`
- `depth`
- `status`
- `model`
- `instructions_json`
- `metadata_json`
- `owner_worker_id`
- `socket_generation`
- `socket_expires_at`
- `last_response_id`
- `active_response_id`
- `active_spawn_group_id`
- `created_at`
- `updated_at`

### Thread Item Log

- `thread:{thread_id}:items`

Recommended storage: Redis Stream.

Each entry should include:

- `seq`
- `response_id`
- `item_id`
- `item_type`
- `direction`
- `payload_json`
- `created_at`

Notes:

- `direction` is `input` or `output`
- Preserve raw item shape inside `payload_json`
- `seq` is monotonic within the thread

### Thread Event Log

- `thread:{thread_id}:events`

Recommended storage: Redis Stream.

Each entry should include:

- `event_seq`
- `socket_generation`
- `event_type`
- `response_id`
- `payload_json`
- `created_at`

This is the raw WebSocket event log used for debugging and recovery.

### Raw Response Storage

- `response:{response_id}:raw`

Recommended storage: string value containing the raw response JSON.

### Ownership

- `thread_owner:{thread_id}`

Recommended storage: hash.

Fields:

- `worker_id`
- `lease_until`
- `socket_generation`
- `claimed_at`
- `updated_at`

### Worker Ownership Index

- `worker:{worker_id}:threads`

Recommended storage: set of owned thread ids.

### Spawn Group Metadata

- `spawn_group:{spawn_group_id}:meta`

Recommended storage: hash.

Fields:

- `id`
- `parent_thread_id`
- `parent_call_id`
- `expected`
- `completed`
- `failed`
- `cancelled`
- `status`
- `aggregate_submitted_at`
- `aggregate_cmd_id`
- `created_at`
- `updated_at`

### Spawn Group Child Index

- `spawn_group:{spawn_group_id}:children`

Recommended storage: set.

### Spawn Group Child Results

- `spawn_group:{spawn_group_id}:results`

Recommended storage: hash or stream.

Fields per child:

- `child_thread_id`
- `status`
- `child_response_id`
- `result_ref`
- `error_ref`
- `updated_at`

### Dedupe State

- `thread:{thread_id}:processed_cmds`

Recommended storage: set.

Keep a bounded retention policy, for example 7 days.

## Secondary Indexes

Recommended helper indexes:

- `thread_status:{status}` -> set of thread ids
- `thread_root:{root_thread_id}` -> set of descendant thread ids
- `thread_parent:{parent_thread_id}` -> set of child thread ids
- `spawn_group_parent:{parent_thread_id}` -> set of spawn group ids

These are not the source of truth. They are convenience indexes for reconciliation and operations.

## Thread Metadata Example

```json
{
  "id": "thread_parent_001",
  "root_thread_id": "thread_parent_001",
  "parent_thread_id": null,
  "parent_call_id": null,
  "depth": 0,
  "status": "waiting_children",
  "model": "gpt-5.4",
  "instructions_json": "{\"role\":\"system\",\"text\":\"You are the lead agent.\"}",
  "metadata_json": "{\"tenant_id\":\"tenant_123\"}",
  "owner_worker_id": "worker_a",
  "socket_generation": 3,
  "socket_expires_at": "2026-03-13T19:25:00Z",
  "last_response_id": "resp_111",
  "active_response_id": null,
  "active_spawn_group_id": "sg_999",
  "created_at": "2026-03-13T18:25:00Z",
  "updated_at": "2026-03-13T18:29:00Z"
}
```

## Item Log Example

```json
{
  "seq": 42,
  "response_id": "resp_111",
  "item_id": "fc_123",
  "item_type": "function_call",
  "direction": "output",
  "payload_json": "{\"type\":\"function_call\",\"call_id\":\"call_spawn_1\",\"name\":\"spawn_subagents\",\"arguments\":\"{\\\"chunks\\\":20}\"}",
  "created_at": "2026-03-13T18:28:40Z"
}
```

## Atomic Scripts

Ownership and barrier updates should be atomic. Use Lua or Redis functions for the following operations.

### `ClaimThreadOwnership`

Inputs:

- `thread_id`
- `worker_id`
- `lease_until`
- `new_socket_generation`

Behavior:

- Succeeds only if no valid owner exists
- Writes `thread_owner:{thread_id}`
- Updates `thread:{thread_id}:meta.owner_worker_id`
- Updates `thread:{thread_id}:meta.socket_generation`
- Adds thread id to `worker:{worker_id}:threads`

### `RenewThreadLease`

Inputs:

- `thread_id`
- `worker_id`
- `socket_generation`
- `lease_until`

Behavior:

- Succeeds only if current owner and generation match
- Extends lease

### `ReleaseThreadOwnership`

Inputs:

- `thread_id`
- `worker_id`
- `socket_generation`

Behavior:

- Removes the ownership record only if owner and generation match
- Removes the thread id from the worker ownership set

### `ApplyCommandOnce`

Inputs:

- `thread_id`
- `cmd_id`
- `expected_status`
- `expected_last_response_id`

Behavior:

- Fails if the command was already applied
- Fails if thread state preconditions do not match
- Records the command id in the dedupe set

### `RecordChildTerminalState`

Inputs:

- `spawn_group_id`
- `child_thread_id`
- `status`
- `child_response_id`
- `result_ref`
- `error_ref`

Behavior:

- No-ops if this child already has a terminal record
- Persists the child result
- Increments `completed`, `failed`, or `cancelled`
- Marks the barrier closed when all expected children are terminal

### `MarkSpawnGroupAggregateSubmitted`

Inputs:

- `spawn_group_id`
- `cmd_id`
- `submitted_at`

Behavior:

- Succeeds only once per spawn group
- Writes `aggregate_submitted_at`
- Writes `aggregate_cmd_id`
- Prevents duplicate parent resume submission

## Persistence Rules

- Persist every WebSocket event before ACKing any command that depends on it
- Persist terminal response JSON before clearing `active_response_id`
- Update `last_response_id` only after the response is stable in Redis
- Update thread state and ownership in the same logical transaction whenever possible
- Record child completion before deciding whether to resume the parent

## Retention

Suggested v1 retention:

- Thread metadata: retained until explicit cleanup policy is introduced
- Item logs: retained
- Event logs: retained, optionally compacted later
- Raw responses: retained
- Dedupe sets: bounded retention
- Ownership records: no TTL-only ownership model; always use explicit `lease_until`

## Notes on Large Artifacts

Redis should never store large PDFs or full binary artifacts.

Store only:

- Blob references
- Child result references
- Extracted summaries or slices if they are operationally useful
