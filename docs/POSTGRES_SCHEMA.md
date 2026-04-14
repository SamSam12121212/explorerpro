# Postgres Runtime Schema

## Role

Postgres is the durable runtime store for the thread engine.

It owns:

- thread snapshots
- owner leases and socket generations
- ephemeral OpenAI socket observability rows
- processed command dedupe
- normalized item history
- raw response storage and response-thread links
- spawn groups, child membership, and child terminal results
- thread-document links

## Core Tables

- `threads`
- `thread_owners`
- `openai_socket_sessions`
- `thread_processed_commands`
- `thread_items`
- `responses`
- `thread_responses`
- `spawn_groups`
- `spawn_group_children`
- `spawn_child_results`
- `thread_documents`

## Write Rules

- The API creates the root thread row before publishing `thread.start`.
- The worker is the single writer for thread mutation on a given thread.
- Ownership updates happen in Postgres and carry the current `socket_generation`.
- OpenAI socket session rows are worker-owned observability state, not coordination state.
- Live socket rows heartbeat through the worker lease loop and age out after disconnect.
- Item append assigns the next per-thread sequence in Postgres.
- Command dedupe records `(thread_id, cmd_id)` in Postgres after durable handling.
- Spawn child completion writes the child result and returns the full group result set from one transaction.

## Read Rules

- Thread snapshots, items, responses, spawn groups, child IDs, and child results read directly from Postgres.
- The websocket server uses the same Postgres read model for its initial snapshot and item catch-up.
- Socket monitoring reads should come from `openai_socket_sessions`, not from `thread_owners`.
- Historical raw socket events stay in JetStream because they are naturally append-only and sequence-oriented.

## Operational Intent

- Keep runtime coordination queryable.
- Keep worker ownership durable across restarts.
- Keep live OpenAI socket visibility queryable without overloading the ownership tables.
- Keep item history easy to page.
- Keep Postgres focused on compact state and read models, not large binary payloads.
