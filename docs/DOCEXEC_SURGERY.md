# Document Executor Surgery

Date of operation: April 13, 2026

## Chief Complaint

The worker had grown a second brain.

`internal/worker/docexec.go` is a parallel runtime inside the worker. It has its own session pool, its own streaming loop, its own lineage tracking, its own retry/rebuild logic, its own connection lifecycle management, and its own in-memory state cache.

It does all of this to avoid treating document queries as threads.

Symptoms:

- document queries have no Postgres ownership, no lease, no socket generation tracking
- document queries have no item history — the worker cannot tell you what a document session actually said
- document queries have no event publishing — the UI cannot see a document query in progress
- document queries have no durable checkpoints — if the worker dies mid-query, the work is lost
- document queries have no recovery path — they are fire-and-forget from a durability perspective
- the hot session cache is a private in-memory map that duplicates what thread ownership already does
- the lineage tracking in `thread_documents` is a private state model that duplicates what `last_response_id` on a thread already does
- the document executor has its own `streamDocumentResponse` that is a stripped-down copy of `streamUntilTerminal`
- the worker now has two completely different execution paths depending on whether it is running a "thread" or a "document query"

Clinical decision:

- kill the document executor
- make document queries into real child threads
- do not carry the old execution path
- do not keep the hot session cache
- do not keep the private lineage model
- let threads be threads

This is the same kind of surgery as the Redis removal. The architecture direction is already decided. The old shape is a parallel system that replicates primitives the thread model already owns. Keeping it alive means maintaining two runtimes forever.

## Pre-Op Anatomy

Before surgery, the document query flow is:

1. Model calls `query_attached_documents` during `streamUntilTerminal`
2. Actor detects the tool call and stores it as `pendingDocQuery`
3. On `response.completed`, actor calls `handlePendingDocumentQuery`
4. `handlePendingDocumentQuery` calls `a.docExec.Execute()` **inline**
5. The document executor:
  - looks up a hot `documentSession` from an in-memory map keyed by `(threadID, documentID)`
  - resolves lineage from `thread_documents` table, then `documents.base_response_id`, then warmup
  - opens its own OpenAI WebSocket session (same transport, separate socket)
  - sends `response.create` with `previous_response_id` and the task
  - streams the response through a private `streamDocumentResponse` loop
  - persists the response ID to `thread_documents` and the hot session cache
  - returns the answer text as a string
6. Actor wraps the answer as `function_call_output` JSON
7. Actor self-publishes `thread.submit_tool_output` back to its own command subject
8. The command flows through `handleSubmitToolOutput` and continues the parent thread normally

Problems with this shape:

- steps 4-5 are an entire parallel runtime that bypasses thread ownership, persistence, events, history, and recovery
- the hot session cache is private state that cannot be inspected, recovered, or transferred
- the lineage model in `thread_documents` is a private schema that duplicates `ThreadMeta.LastResponseID`
- parallel fan-out across documents creates multiple concurrent OpenAI sockets that are invisible to the ownership model
- if the worker crashes during step 5, the work is silently lost
- document query results have no item history — you cannot inspect them after the fact

## Post-Op Anatomy

After surgery, the document query flow will be:

1. Model calls `query_attached_documents` during `streamUntilTerminal`
2. Actor detects the tool call — same as today
3. On `response.completed`, actor treats it **exactly like `spawn_subagents`**:
  - resolves the `previous_response_id` for each document (from the latest completed child thread for that document, or from the shared base anchor, or triggers a warmup)
  - creates one child thread per document
  - each child thread starts with `previous_response_id`, the task as input, `store: true`
  - publishes `thread.start` commands for each child
  - creates a spawn group, sets parent status to `waiting_children`
4. Each child thread is a **normal thread** — it gets claimed by a worker, opens a normal socket, runs `sendAndStream` → `streamUntilTerminal`, persists items and events, publishes `thread.child_completed`
5. Parent thread regroups through the **normal spawn barrier** in `handleChildResult`

The worker does not know it is running a "document query." It just sees a thread that was started in warm branch mode from a `previous_response_id`, with some input text, a model, and instructions.

## What This Gives Us

Everything the thread model already provides, which the document executor currently lacks:

- Postgres ownership and lease management
- full item history (inspectable via `GET /threads/{id}/items`)
- event publishing to `THREAD_EVENTS` (live UI could show document query progress)
- durable `THREAD_HISTORY` checkpoints (document queries become recoverable)
- recovery and adoption (worker death mid-query is survivable)
- socket rotation sweeps
- observability through existing thread APIs
- spawn barrier coordination (already built, already tested)

## Incision Plan

### Incision 1: Make `query_attached_documents` spawn child threads

Change `handlePendingDocumentQuery` in `actor.go` to:

- resolve the `previous_response_id` for each requested document
- build a child thread spec per document (warm branch mode)
- delegate to `startSpawnGroup` — the same path used by `spawn_subagents`
- set parent status to `waiting_children`

The child thread `thread.start` body should carry:

- `previous_response_id` from the document lineage
- the task text as `initial_input`
- `store: true`
- the resolved document model
- `prepared_input_ref` if warmup is needed

This incision replaces the inline `a.docExec.Execute()` call with a normal spawn.

### Incision 2: Move document warmup to `documenthandler` prepared input

The warmup step (sending all page images with `generate: false`) should happen **before** the child thread is spawned, not inside the child thread.

Flow:

1. Actor resolves lineage for a document
2. If no usable lineage exists, actor requests a warmup prepared input from `documenthandler`
3. `documenthandler` returns a `prepared_input_ref` that contains the page images
4. Actor creates the child thread with that `prepared_input_ref` and `generate: false`
5. The child thread runs the warmup as a normal `thread.start` with `generate: false`
6. After the warmup child completes, its `last_response_id` becomes the shared base anchor

Alternatively, the warmup can be a separate child thread that completes before the query child is spawned. The simpler option is to let the query child thread carry the `prepared_input_ref` and `generate: false` and then re-query from its own `last_response_id`. But this means the child thread does two sends. The cleanest design is probably: warmup child (if needed) → query child (from warmup child's `last_response_id` or existing lineage).

The exact warmup strategy can be decided during implementation. The important rule is: **the worker actor does not open document sockets itself.**

### Incision 3: Kill `docexec.go`

Delete:

- `internal/worker/docexec.go`
- `internal/worker/docexec_test.go`
- all `documentExec` references in `service.go` and `actor.go`
- the hot session cache, the `documentSession` type, the idle sweep logic
- `streamDocumentResponse`, `extractTextDelta`, `extractResponseFailedError`
- all `docExec` fields on `threadActorConfig`, `threadActor`, `Service`

### Incision 4: Simplify the lineage model

The `thread_documents` lineage columns (`latest_response_id`, `latest_model`, `initialized_at`, `last_used_at`) were built for the document executor's private lineage tracking.

After this surgery, document lineage is just thread lineage:

- the `last_response_id` of the most recent completed document-query child thread for a given `(parent_thread_id, document_id)` is the lineage
- the shared base anchor on `documents` (`base_response_id`, `base_model`) stays — it is still useful for cross-thread warmup reuse
- `thread_documents` shrinks back to being a join table (thread ↔ document attachment links)

The lineage query becomes: "find the latest completed child thread for this parent thread and this document, and use its `last_response_id`."

This may mean adding a `document_id` column to `threads` (or to `thread_documents` alongside a `child_thread_id`), so we can look up "the latest child thread that was a document query for document X in parent thread Y." The exact schema can be decided during implementation.

### Incision 5: Clean up actor.go

After the executor is gone, remove from `actor.go`:

- `executeDocumentQuery`
- `handlePendingDocumentQuery` (replaced by spawn logic)
- `closeDocumentSessions` calls throughout the actor lifecycle
- the `docExec` nil-check stub fallback

The document tool detection in `streamUntilTerminal` stays, but the handling changes from "execute inline and self-publish tool output" to "spawn child threads and set status to waiting_children."

### Incision 6: Update the docs

Rewrite:

- `docs/DOCUMENT_CHAT_INTEGRATION.md` — the document query flow section
- `docs/WORKER_RUNTIME.md` — remove document executor references
- `docs/ARCHITECTURE.md` — if needed

Delete any dead exploration docs that describe the old executor model.

## What We Explicitly Will Not Do

- keep the hot session cache as a latency optimization
- keep `docexec.go` as a fallback path
- keep the private `streamDocumentResponse` loop
- keep the `thread_documents` lineage columns for the old executor model
- keep the `documentSession` type or the in-memory session map
- stage a migration — old document queries are not recoverable anyway

## Thread Model After Surgery

The thread hierarchy naturally handles document queries:

- **Root thread**: the chat thread the user is talking to
- **Parent thread**: the chat thread (or a subagent, if subagents gain document access later)
- **Child thread**: the document query thread (structurally identical to a subagent child)

The child thread does not know it is a "document query." It is just a thread that started from a `previous_response_id` with some input.

## Lineage After Surgery

- A document's **shared base anchor** (`documents.base_response_id`) is the `last_response_id` of the most recent warmup child thread for that document
- A document's **thread-local lineage** is the `last_response_id` of the most recent completed document-query child thread for that `(parent_thread_id, document_id)` pair
- Follow-up queries to the same document in the same parent thread spawn a new child thread from the previous child's `last_response_id`
- New parent threads reuse the shared base anchor if the model matches

## Relevant Files (Pre-Op)

- `[internal/worker/docexec.go](/Users/detachedhead/explorer/internal/worker/docexec.go)` — the entire document executor (dies)
- `[internal/worker/docexec_test.go](/Users/detachedhead/explorer/internal/worker/docexec_test.go)` — executor tests (dies)
- `[internal/worker/actor.go](/Users/detachedhead/explorer/internal/worker/actor.go)` — `handlePendingDocumentQuery`, `executeDocumentQuery`, `closeDocumentSessions`
- `[internal/worker/service.go](/Users/detachedhead/explorer/internal/worker/service.go)` — `docExec` creation and wiring
- `[internal/threaddocstore/store.go](/Users/detachedhead/explorer/internal/threaddocstore/store.go)` — lineage columns
- `[db/migrations/000009_document_lineage.sql](/Users/detachedhead/explorer/db/migrations/000009_document_lineage.sql)` — lineage migration

## Minimal First Task

The smallest useful first cut is Incision 1: change `handlePendingDocumentQuery` to spawn child threads through `startSpawnGroup` instead of calling `a.docExec.Execute()` inline.

That single change moves document execution from the private executor into the normal thread model. Everything else follows.