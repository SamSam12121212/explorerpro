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
  - validates the requested document IDs are attached
  - resolves the model for each document
  - resolves `previous_response_id` from thread-local lineage, then from a compatible shared base anchor if present
  - if no usable lineage exists, requests a `document_query` prepared input from `documenthandler`
  - creates one child thread per document
  - each child thread starts either from `previous_response_id` + task input, or from `prepared_input_ref`, with `store: true`
  - publishes `thread.start` commands for each child
  - creates a spawn group, sets parent status to `waiting_children`
4. Each child thread is a **normal thread** — it gets claimed by a worker, opens a normal socket, runs `sendAndStream` → `streamUntilTerminal`, persists items and events, publishes `thread.child_completed`
5. Parent thread regroups through the **normal spawn barrier** in `handleChildResult`

The worker does not know it is running a "document query." It just sees a thread that was started with a model plus either a branch point (`previous_response_id`) or a prepared input artifact, then executed through the normal thread runtime.

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

### Incision 1: Make `query_attached_documents` spawn child threads — DONE

Replaced `handlePendingDocumentQuery` (inline `docExec.Execute()`) with `startDocumentQueryGroup`:

- resolves `previous_response_id` for each document from the latest completed document-query child thread, then from the shared base anchor on `documents`
- if no lineage exists, requests a `document_query` prepared input from `documenthandler` that bundles page images + task into one blob
- builds one child thread per document with `store: true`, the resolved model, and `document_id` in metadata
- delegates to the same spawn group + dispatch barrier used by `spawn_subagents`
- parent status → `waiting_children`

New interfaces on the actor: `docActorDocStore`, `docActorPreparedInputClient`, plus a runtime-store lookup for latest completed document child lineage.

### Incision 2: Warmup strategy — SIMPLIFIED

Instead of a separate warmup-then-query two-phase flow, the first query for a document that has no lineage sends pages + task together in one shot via `PrepareKindDocumentQuery`. The response ID from that query becomes the thread-local lineage for follow-up queries. The shared base anchor optimization (cross-thread warmup reuse) can be re-added later.

### Incision 3: Kill `docexec.go` — DONE

Deleted:

- `internal/worker/docexec.go` (18KB)
- `internal/worker/docexec_test.go` (24KB)
- all `documentExec` references in `service.go` and `actor.go`
- the hot session cache, the `documentSession` type, the idle sweep logic
- `streamDocumentResponse`, `extractTextDelta`, `extractResponseFailedError`
- all `docExec` fields on `threadActorConfig`, `threadActor`, `Service`
- the `documentSessionIdleTTL`, `documentSessionMaxTTL`, `documentQueryParallel` constants

### Incision 4: Lineage — DONE

Thread-local document follow-up state now comes directly from normal thread state:

- the worker queries the latest completed document-query child thread for `(parent_thread_id, document_id)`
- follow-up queries branch from that child thread's `last_response_id`
- sticky model reuse comes from that child thread's `model`
- `thread_documents` lineage columns are gone

### Incision 5: Clean up actor.go — DONE

Removed:

- `handlePendingDocumentQuery`
- `executeDocumentQuery`
- `updateDocumentLineageFromChild`
- `closeDocumentSessions` and all 6 call sites
- `docExec` field from `threadActorConfig` and `threadActor`
- the `newDocumentExec()` call in `service.go`
- `CloseIdleSessions` sweep in `recoverThreads`

### Incision 6: Update the docs — DONE

Aligned:

- `docs/DOCUMENT_CHAT_INTEGRATION.md` — rewritten around the child-thread flow
- `docs/WORKER_RUNTIME.md` — aligned with the post-executor worker shape
- `docs/ARCHITECTURE.md` — already compatible with the shared thread runtime model

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

The child thread does not know it is a "document query." It is just a thread that started as a normal thread, either from a branch point or from a prepared input artifact.

## Lineage After Surgery

- A document's **thread-local lineage** is the `last_response_id` of the latest completed document-query child thread for that `(parent_thread_id, document_id)` pair
- Follow-up queries to the same document in the same parent thread branch from that child thread's `last_response_id`
- Sticky model reuse comes from that child thread's stored `model`
- If `documents.base_response_id` already exists and `documents.base_model` matches, a new parent thread can branch from that anchor
- If no usable lineage exists, the first query sends pages + task together via `PrepareKindDocumentQuery`
- The current child-thread flow does **not** rebuild shared base anchors automatically; reintroducing that optimization is future work

## Files Changed

- `internal/worker/docexec.go` — **DELETED** (the entire document executor)
- `internal/worker/docexec_test.go` — **DELETED** (executor tests)
- `internal/worker/actor.go` — new `startDocumentQueryGroup`; removed `handlePendingDocumentQuery`, `executeDocumentQuery`, `updateDocumentLineageFromChild`, `closeDocumentSessions`, `docExec` field
- `internal/worker/service.go` — removed `docExec` creation/wiring and old doc-lineage wiring; kept `docStore` + `PreparedInputs` for the child-thread flow
- `internal/doccmd/command.go` — added `PrepareKindDocumentQuery` constant
- `internal/documenthandler/service.go` — new `prepareDocumentQueryInput` + `buildDocumentQueryInput` for pages+task prepared input
- `db/migrations/000012_document_child_lineage.sql` — dropped `thread_documents` lineage columns and added an index for latest completed document-query child lookup

## What Remains

1. **Shared base anchor optimization**: re-add cross-thread warmup reuse for first queries (skipped for POC simplicity)
