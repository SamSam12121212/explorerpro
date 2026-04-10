# Postgres Schema

## Decision

Postgres is the next durable storage layer for this runtime, but it does not replace Redis.

Locked boundary:

- Redis remains the live execution truth for ownership, leases, socket generations, actor preconditions, and hot recovery state.
- Postgres becomes the durable and queryable persistence layer for thread history, responses, spawn barriers, and future product-facing reads.

This keeps the worker/runtime model sharp while finally giving the system a real business-data layer.

## What This Schema Covers

This v1 schema mirrors the runtime objects that already exist in Redis today:

- thread snapshots
- thread-to-response links
- raw responses
- ordered item logs
- ordered event logs
- spawn groups
- spawn group child/result rows

This schema intentionally does not try to design the whole future company database yet.

Not included yet:

- tenants / auth / RBAC
- workspaces and repo manifests
- document package registry
- blob object catalog
- billing / usage accounting

Those can come later once the runtime persistence slice is real.

## Core Design Rules

### 1. Keep Existing IDs

The runtime already uses app-generated IDs such as `thread_...`, `resp_...`, and `sg_...`.

Use `text` primary keys in Postgres for those IDs.

Do not introduce UUID translation tables.

### 2. Preserve Raw OpenAI Shapes

Responses objects, item payloads, and event payloads should be stored as raw `jsonb`.

That keeps Postgres aligned with the OpenAI-native product stance and avoids premature relational shredding of provider-shaped payloads.

### 3. Separate Snapshot Rows From Append-Only Logs

The schema uses two families of tables:

- mutable snapshot tables such as `threads` and `spawn_groups`
- append-only history tables such as `thread_items` and `thread_events`

That matches the current runtime model exactly.

### 4. Use Natural Idempotency Keys

The worker already relies on redelivery and idempotency.

The Postgres schema should cooperate with that:

- `threads.id`
- `responses.id`
- `thread_response_links(thread_id, response_id)`
- `thread_items(thread_id, seq)`
- `thread_events(thread_id, event_seq)`
- `spawn_groups.id`
- `spawn_group_children(spawn_group_id, child_thread_id)`

Those keys make retry-safe upserts straightforward.

### 5. Do Not Make Postgres The Hot Coordination Plane

Even after Postgres lands, Redis should still own:

- thread routing
- lease freshness
- `expected_socket_generation` checks
- active ownership claims
- mailbox safety assumptions

Postgres is for durable state, not live socket arbitration.

## Recommended Write Model

The first implementation should use idempotent dual writes from the existing API/worker paths.

Recommended rule:

1. write Redis state first, exactly as the runtime does now
2. write the matching Postgres row set before ACK
3. if the Postgres write fails, do not ACK
4. let JetStream redelivery replay into idempotent upserts

Why this fits the current system:

- there is already no distributed transaction across NATS and Redis
- the runtime already assumes retries and replay
- immutable log tables can absorb duplicates through natural keys
- snapshot tables can converge through `INSERT ... ON CONFLICT DO UPDATE`

This gives strong practical durability without replacing the current execution model.

## Table Design

### `threads`

One durable row per runtime thread.

Purpose:

- latest durable thread snapshot
- parent/root lineage
- model and instruction context
- latest known response pointers
- mirrored owner/socket fields for inspection

Important note:

- `owner_worker_id`, `socket_generation`, and `socket_expires_at` are persisted here for audit and query convenience
- Redis still remains authoritative for live routing and lease checks

### `responses`

One raw OpenAI response payload per `response_id`.

Purpose:

- durable raw response storage
- direct retrieval by `response_id`
- later analytics or export surfaces

### `thread_response_links`

Maps responses to threads.

Why this exists:

- a thread can emit many responses
- warm-branch children may legitimately reference a parent response lineage
- this is cleaner than forcing every response to belong to exactly one thread row forever

Initial `link_kind` values:

- `owned`
- `branch_source`
- `referenced`

Important note:

- `thread_response_links.response_id` should stay a plain text reference, not a hard foreign key
- that keeps replay and partial-order persistence tolerant when raw response rows land slightly later

### `thread_items`

Durable ordered item log.

Primary key:

- `(thread_id, seq)`

Why:

- matches the current Redis monotonic `seq`
- keeps paging semantics simple
- gives exact replayable history

### `thread_events`

Durable ordered raw event log.

Primary key:

- `(thread_id, event_seq)`

Why:

- mirrors the current Redis event stream
- preserves recovery-relevant wire history
- keeps event ordering explicit per thread

### `spawn_groups`

One barrier row per parent `spawn_subagents` tool call.

Purpose:

- current barrier counts
- aggregate submission guard
- parent linkage

### `spawn_group_children`

One row per child thread in a spawn group.

Purpose:

- child membership
- optional child order
- terminal status and result refs
- parent regroup payload source

This intentionally merges the current Redis child-index set and child-result map into one relational table.

## Deliberate Non-FK Pointers

Some response-related columns are intentionally stored as plain `text` references instead of hard foreign keys:

- `threads.last_response_id`
- `threads.active_response_id`
- `thread_items.response_id`
- `thread_events.response_id`
- `spawn_group_children.child_response_id`

Why:

- append-only rows may be persisted in a slightly different order during retry
- recovery paths may materialize history before a response row lands
- warm-branch flows may reference lineage that originated on another thread

The durable integrity rule should be:

- `responses.id` is canonical for raw response bodies
- `thread_response_links` is canonical for thread-to-response membership
- pointer columns stay flexible enough to tolerate replay and partial-order persistence

## Initial Query Priorities

This schema is designed to make the first useful reads cheap:

- load a thread snapshot by id
- list a root thread's descendants
- page thread items by sequence
- page thread events by sequence
- fetch a raw response by `response_id`
- inspect spawn groups for a parent thread
- inspect child results for a spawn group

## JSONB Strategy

Use `jsonb` for:

- `threads.metadata_json`
- `threads.tools_json`
- `threads.tool_choice_json`
- `responses.response_json`
- `thread_items.payload_json`
- `thread_events.payload_json`

Reasoning:

- raw payload fidelity matters more than aggressive normalization right now
- `jsonb` keeps future filtering open
- Postgres can add GIN indexes later when real query patterns exist

## Partitioning Strategy

Do not partition in the very first migration.

Start simple with normal tables, then partition later if volume demands it.

When partitioning becomes necessary, the first candidates are:

- `thread_items`
- `thread_events`
- `responses`

The likely partition key is time, not thread id.

## Cleanup Strategy

No hard-delete policy is part of this first schema.

Assume retention by default.

Later cleanup can be introduced with explicit archival rules once product requirements are clearer.

## Future Extensions

The next schema layers will probably be:

1. workspace manifests and repo inventory
2. document package registry
3. artifact/blob catalog
4. tenant/project/user scoping
5. usage/accounting tables

Those should build on top of this runtime persistence core, not replace it.
