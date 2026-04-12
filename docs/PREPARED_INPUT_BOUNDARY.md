# Prepared Input Boundary

## Summary

This note captures the current direction for removing document-shaped payload assembly from the worker core without giving up worker-owned OpenAI execution.

The intended split is:

1. keep the worker focused on threads, OpenAI WebSockets, coordination, and recovery
2. move source-specific input materialization out of `internal/worker`
3. pass heavy prepared input into the worker by `prepared_input_ref`
4. keep canonical thread state semantic and durable
5. avoid persisting giant base64-heavy payloads as runtime truth

This direction is especially important before the streams refactor grows a new durable home for `client.response.create` checkpoints. We do not want large document warmup payloads to become normal thread history.

## Problem

The current document path has drifted past the thin-worker boundary described in the broader architecture notes.

Today the worker still rightly owns:

- thread actors
- worker ownership and leases
- OpenAI Responses WebSocket sessions
- continuation through `previous_response_id`
- recovery and replay

But the worker also currently owns document-specific payload assembly concerns such as:

- walking document manifests
- loading page images from blob storage
- building page-scoped XML-like wrappers
- base64 encoding page images into `input_image` items
- managing a document-specific execution subsystem inside `internal/worker`

That is too much source-specific knowledge inside worker runtime code.

It also creates a second problem: the materialized document payload can be very large. A long PDF warmup request can produce a `response.create` input that is not a good fit for Redis, JetStream history, or durable replay checkpoints.

## Existing Architecture Direction

The broader docs already point toward a thin worker:

- `ARCHITECTURE.md`: document preparation happens outside the thread runtime
- `DOCUMENT_MANIFEST.md`: the runtime should consume prepared document packages
- `IMAGE_INPUT_CONTRACT.md`: ingestion and source-specific processing stay out of engine state
- `THREAD_STREAM_IMPLEMENTATION.md`: the worker should own the OpenAI connection and thread persistence, not extra delivery or source-specific logic

The document integration notes also preserve one important rule:

- the worker is still the only process allowed to touch OpenAI

So the correct goal is not "move OpenAI calls out of the worker".

The correct goal is:

- move source-specific input materialization out of worker core
- keep OpenAI send/continue/recover inside the worker

## Decision

We will introduce a prepared-input handoff boundary.

### Durable canonical truth

Durable thread state should stay semantic and compact.

Examples:

- user text input
- attached document IDs
- image refs
- repo refs
- tool outputs
- thread metadata

This layer is the engine truth.

It should not contain:

- base64 page images
- expanded document warmup payloads
- large source-specific transport-shaped OpenAI input

### Prepared input artifact

Source-specific materialization will produce a prepared input artifact outside worker core.

That artifact is:

- Responses-compatible `input`
- already expanded for the specific source
- stored outside the command envelope
- referenced by `prepared_input_ref`

This lets the worker remain agnostic to whether the input came from:

- a document
- a repo
- an image
- a future source type

The worker should not need branching like "if document do X" at the payload-materialization boundary.

### Worker responsibility after this change

The worker will:

- receive a command that may contain `prepared_input_ref`
- load the prepared input artifact
- combine that prepared `input` with thread-owned fields such as `model`, `instructions`, `tools`, `tool_choice`, `reasoning`, `store`, and `previous_response_id`
- send the resulting `response.create` over the worker-owned OpenAI socket
- keep owning recovery and continuation semantics

This keeps the worker in charge of runtime behavior without making it responsible for source-specific payload assembly.

## Why The Ref Belongs In The NATS Command

We do not currently need Redis as the handoff lane for prepared input.

The better fit is:

- put the artifact location directly in the durable JetStream command body
- keep the heavy artifact body in blob storage or another durable object store

Why this is cleaner:

- the command already represents one specific execution attempt
- JetStream already gives us durable delivery and redelivery
- the handoff is immutable, not hot coordination state
- Redis would add another mutable runtime surface without solving the size problem

So the planned handoff is:

- command carries `prepared_input_ref`
- artifact body lives outside NATS
- worker resolves the ref at execution time

Important boundary:

- `prepared_input_ref` is an internal command detail
- it is not part of the public frontend or public HTTP API contract
- frontend clients should send semantic inputs such as user text and `attached_document_ids`
- backend command producers may attach `prepared_input_ref` later when a turn requires source-specific prepared input

## Planned Command Direction

This is a planned extension to the command shapes.

### `thread.start`

The start body should support a prepared input reference:

```json
{
  "model": "gpt-5.4",
  "instructions": "optional system instructions",
  "prepared_input_ref": "blob://prepared-inputs/pi_123.json",
  "store": true
}
```

### `thread.resume`

The resume body should support the same idea:

```json
{
  "prepared_input_ref": "blob://prepared-inputs/pi_456.json"
}
```

The intended rule is:

- the heavy source-specific input should not need to ride inline inside the command
- the worker should load it by ref

These examples describe internal command shapes, not public API request bodies.

We may continue to allow inline `initial_input` / `input_items` for simple cases, but prepared refs should be the path for large or source-specific materialized input.

## Prepared Artifact Shape

The artifact should represent prepared `input`, not a full opaque `response.create`.

That keeps thread/runtime fields under worker control.

Suggested shape:

```json
{
  "version": "v1",
  "input": [
    {
      "type": "message",
      "role": "user",
      "content": [
        { "type": "input_text", "text": "..." }
      ]
    }
  ]
}
```

Optional debug metadata may exist, but the worker should not branch on source kind during normal execution.

Examples of optional metadata:

- `source_kind`
- `created_at`
- `expires_at`
- `source_hash`

Those fields are for tracing and garbage collection, not runtime behavior.

## Recovery And Cleanup Rules

This boundary only works if replay stays safe.

### Command replay

If a worker crashes before ACK, JetStream may redeliver the command.

Therefore:

- the object behind `prepared_input_ref` must survive command redelivery
- it cannot be deleted immediately after first read

### In-flight replay

The worker currently recovers from the last persisted `client.response.create` checkpoint.

That means the replay path must also stay small and ref-friendly.

Planned direction:

- do not persist giant materialized prepared input blobs as long-term thread history
- persist a replay-safe checkpoint that can refer back to `prepared_input_ref` or another compact replay descriptor

The durable checkpoint should be enough to replay safely without turning event history into a store of huge base64 payloads.

### Cleanup

Prepared input artifacts should be wiped only after:

- the command is safely past the redelivery window, and
- the runtime has a replay-safe checkpoint that no longer depends on the heavy expanded body

The exact cleanup mechanism can be decided later, but the rule is simple:

- cleanup must happen after replay safety, not before it

## What Changes First

The first extraction target is the document warmup path.

In particular, the current logic that:

- walks the document manifest
- loads page blobs
- builds `<pdf>` / `<pdf_page>` wrappers
- base64 encodes page images into `input_image`

should move out of worker core and become a prepared-input producer.

This is the part of the current document path that most clearly violates the desired worker boundary.

## What Can Stay In The Worker For Now

This note does not require moving every document concern out immediately.

The worker may still keep, at least initially:

- OpenAI socket ownership
- `previous_response_id` continuation logic
- document-query session reuse if we still want that latency optimization
- tool-call continuation through `function_call_output`

The first move is narrower:

- remove source-specific payload materialization from worker core

That gives us a cleaner boundary without forcing a full rewrite of document lineage or session ownership in one step.

## Phase Plan

### Phase 1: Define the prepared-input contract

- add `prepared_input_ref` to the planned command model
- define the prepared artifact format
- define lifetime and cleanup rules

### Phase 2: Build a prepared-input producer for documents

- take the current document warmup payload assembly out of `internal/worker`
- produce a prepared input artifact from document state and manifest data
- store that artifact in blob storage

### Phase 3: Teach the worker to consume prepared input by ref

- worker loads prepared input by `prepared_input_ref`
- worker places that `input` into the final `response.create`
- worker keeps runtime-owned fields under its control

### Phase 4: Update replay/checkpoint behavior

- make sure recovery does not depend on giant inlined payloads
- keep checkpoints compact and replay-safe

### Phase 5: Remove source-specific document payload assembly from worker core

- delete or shrink the current worker-side document materialization path
- keep only the runtime responsibilities that truly belong to thread execution

### Phase 6: Reuse the same boundary for other source types

- repo-derived prepared input
- image-derived prepared input
- future source adapters

The point of `prepared_input_ref` is not only to fix documents. It is to establish one clean worker boundary for any source-specific input path.

## Non-Goals

This note is not proposing:

- moving OpenAI calls out of the worker
- turning Redis into a staging store for large prepared payloads
- making the worker consume a fully opaque prebuilt `response.create`

The worker should still own thread/runtime fields and OpenAI execution.

The prepared-input boundary is specifically about moving source-specific `input` materialization out of worker core.

## Progress

- 2026-04-12: Task 1 completed. Added the first `internal/preparedinput` foundation with artifact validation plus blob-backed read/write helpers. This creates the initial contract for storing prepared `input` outside worker core before we touch command or worker behavior.
- 2026-04-12: Task 2 completed. Extended the internal command model with `prepared_input_ref` for `thread.start` and `thread.resume`, and made the worker reject that field explicitly until prepared-input consumption is implemented. This keeps the schema moving forward without silently ignoring new command bodies.
- 2026-04-12: Task 3 completed. The worker now resolves `prepared_input_ref` at send time, strips it from the OpenAI wire payload, and keeps the persisted `client.response.create` checkpoint compact by storing the ref instead of the expanded prepared input. Recovery replay now re-materializes prepared input from the stored ref. Resume-command normalization also now accepts `prepared_input_ref` without inline `input_items`.
- 2026-04-12: Task 4 completed. Tightened the public API boundary so `prepared_input_ref` remains internal-only. The public HTTP resume endpoint now rejects it explicitly, and this note now states that frontend clients should only send semantic thread input such as text and `attached_document_ids`.

Files touched:

- `docs/PREPARED_INPUT_BOUNDARY.md`
- `internal/preparedinput/preparedinput.go`
- `internal/preparedinput/preparedinput_test.go`
- `internal/agentcmd/command.go`
- `internal/agentcmd/command_test.go`
- `internal/worker/actor.go`
- `internal/httpserver/command_api.go`
- `internal/httpserver/command_api_test.go`
- `internal/worker/actor_test.go`
- `internal/worker/actor_recovery_harness_test.go`
