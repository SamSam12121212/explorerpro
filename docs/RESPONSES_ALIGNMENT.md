# Responses Alignment

## Purpose

This file is the working handoff note for aligning the app more cleanly to the OpenAI Responses API shape.

It captures:

- what we are trying to preserve
- what the codebase is actually doing today
- what small alignment work has already been completed
- the next small steps that should follow

The goal is that after a context reset, this file is enough to recover the current direction without re-auditing the repo from scratch.

## Date

- Captured: April 11, 2026

## Core Goal

The product direction is still:

- stay native to the OpenAI Responses API
- keep the app's internal data as close as practical to Responses objects
- avoid building a separate shadow schema unless the runtime truly needs it
- keep custom runtime concepts very small and explicit

The custom concepts we do need are still:

- `thread`
- `spawn_group`
- command routing / ownership / recovery

Those are runtime wrappers around Responses execution, not replacements for Responses objects.

## Current Reality

The codebase is already fairly close to the intended model, but the alignment is currently mostly at the JSON/runtime boundary rather than through typed `openai-go` usage.

### What is Responses-shaped today

- inbound API input is normalized into Responses-style input item arrays
- worker continuation uses `previous_response_id`
- raw OpenAI response bodies are stored with minimal transformation
- output items are persisted close to wire shape
- raw websocket events are stored close to wire shape

### What is custom runtime state today

- sticky worker ownership
- socket generation / lease handling
- thread status
- spawn barriers
- recovery / adoption commands

### Important practical note

The vendored `openai-go` repo is in-tree, but the runtime does not currently import it as part of normal execution.

Today it is effectively being used as:

- a reference surface
- a future schema source
- a place to compare against upstream behavior

It is not yet the worker transport layer.

## Current Architecture Boundary

The current clean boundary we want to preserve is:

- API layer: normalize input, create/publish commands, expose read APIs
- worker: own thread actors, build `response.create` payloads, own continuation logic
- `internal/openaiws`: own websocket transport only
- stores: persist raw responses, items, events, and thread state
- wsserver: fan out stored thread updates to the browser

That means the worker should understand Responses request/continuation semantics, while `internal/openaiws` should understand sockets, not schema.

## Problem We Started Fixing

Some Responses-shape knowledge had leaked into `internal/openaiws`.

In particular, `internal/openaiws` had a helper that accepted an arbitrary Go value, marshaled it, decoded it again into a map, and then wrapped it as `response.create`.

That is small, but it is the wrong direction because it makes the transport package partially responsible for request shaping.

If we keep going that way, `internal/openaiws` slowly turns into a copied mini-schema layer derived from `openai-go`.

## First Alignment Step Completed

The first small cleanup step is now done.

### What changed

- `openaiws.ClientEvent` now carries raw JSON payload bytes instead of `map[string]any`
- `openaiws.NewResponseCreateEvent` now accepts raw JSON and validates that it is a JSON object
- `openaiws` now only wraps the payload with `"type":"response.create"`
- worker code now marshals the outbound payload before passing it into `openaiws`
- document-executor code now does the same thing

### Why this is better

- payload shaping now clearly lives in the worker boundary
- `openaiws` is thinner and more transport-only
- we removed one small piece of duplicated schema-ish behavior from the socket layer
- this creates a cleaner seam for future `openai-go` adoption

## Second Alignment Step Completed

The next small cleanup step is now done too.

### What changed

- there is now one canonical worker-side builder for outbound `response.create` payloads in `internal/worker/responsecreate.go`
- `buildResponseCreatePayload` now returns canonical JSON bytes rather than a mutable `map[string]any`
- thread start/continue paths now build outbound payloads through that one file
- recovery replay still extracts stored client payloads, but now runs back through the same worker-side response handling path
- document warmup/query paths now also build their top-level `response.create` payloads through that one file instead of assembling them inline

### Why this is better

- all outbound `response.create` shaping now starts from one worker boundary
- document execution no longer has its own duplicate top-level payload assembly
- the code now has one clear place to improve if we start replacing map-based building with upstream types
- this is a much better seam for selective `openai-go` adoption than spreading typed code through actor and document runtime code

## Third Alignment Step Completed

The next small cleanup step is now done too.

### What changed

- thread-side required include injection and attached-document tool injection now happen at the worker payload-build boundary instead of inside `sendAndStream`
- thread start/continue now use a thread-specific builder path that returns a finalized payload object before send
- recovery replay now finalizes the extracted stored payload before handing it to the send path
- `sendAndStream` now just logs, lowers input payloads for wire format, and sends

### Why this is better

- the send path no longer decodes canonical payload JSON just to mutate it
- the last runtime-only request mutations now happen alongside payload assembly instead of deep in transport-adjacent code
- the thread path is easier to reason about because payload preparation and payload sending are separate responsibilities
- this is a cleaner place to begin swapping builder internals over to upstream types later

## Fourth Alignment Step Completed

The first real `openai-go` adoption is now done.

### What changed

- the app now depends directly on upstream `github.com/openai/openai-go/v3`
- `internal/worker/responsecreate.go` now uses `openai-go` response types at the builder boundary
- the required `include` list is now normalized as `[]responses.ResponseIncludable`
- the attached-documents tool definition is now built from `responses.ToolUnionParam` and `responses.FunctionToolParam`
- the runtime still converts that typed tool definition back into the existing map shape before it flows through the rest of the worker

### Why this is better

- we now have a real upstream dependency in the code path, not just a reference repo on disk
- one of the biggest hand-written Responses-shaped blobs is no longer defined entirely by us
- the adoption point is still tightly scoped to the worker builder boundary
- this proves we can use upstream types without forcing the websocket transport or persistence layers to change

## Fifth Alignment Step Completed

The next typed builder slice is now done too.

### What changed

- `internal/worker/responsecreate.go` now normalizes `reasoning` through `openai-go`'s `shared.ReasoningParam`
- the builder no longer strips reasoning summary fields by mutating a generic map after decode
- both direct request reasoning input and stored thread reasoning state now go through the same typed normalization path

### Why this is better

- one more top-level Responses request field is now shaped by upstream types instead of ad hoc map surgery
- the builder boundary is starting to own normalization rules in a more explicit, typed way
- this keeps reducing custom request-shape code without touching the websocket transport or persistence layers

## Current Rule Going Forward

For now, the intended rule is:

- worker/runtime code builds Responses payloads, ideally through `internal/worker/responsecreate.go`
- thread payloads should be finalized before they enter `sendAndStream`
- use upstream `openai-go` types at the builder boundary where they reduce hand-written schema code
- `openaiws` sends and receives wire messages
- stores persist raw JSON plus minimal runtime metadata
- any future typed adoption from `openai-go` should happen at payload-build or decode boundaries, not inside websocket transport

## What Still Is Not Fully Aligned

We are not yet fully typed against upstream Responses structs.

That means:

- the canonical builder still internally assembles payloads as `map[string]any`
- many decoded items/events are still handled as raw JSON plus small helper structs
- `openai-go` is only the source of truth for a small subset of outbound request fields so far
- `metadata` is still raw at the builder boundary because our warm-branch helper currently injects non-string values such as `branch_index`
- thread execution and document execution still use slightly different in-memory builder outputs before final marshal

This is acceptable for now, but it is the next area to improve.

## Recommended Next Small Steps

The next good small steps are:

1. Decide the metadata normalization rule for branch-specific fields like `branch_index` so we can safely move `metadata` onto upstream string-map types.
2. Type `tool_choice` inside `internal/worker/responsecreate.go`, and then reuse the same typed shape in child-thread filtering.
3. Standardize the builder boundary on one in-memory representation before final marshal, instead of thread flow using finalized objects and document flow using raw JSON bytes.

## Strong Recommendation

Do not try to convert the whole runtime to typed `openai-go` structs in one pass.

The safer direction is:

- first clean up boundaries
- then type outbound payload building
- then type selected decode points where it materially helps
- keep raw JSON persistence the whole time

That keeps the runtime close to Responses shape without tying transport, persistence, and orchestration together too early.

## Relevant Files

- `docs/OPENAI_GO_EXPLORATION.md`
- `docs/OPENAI_WS_RUNTIME.md`
- `docs/ARCHITECTURE.md`
- `docs/API_RUNTIME.md`
- `docs/WORKER_RUNTIME.md`
- `internal/openaiws/event.go`
- `internal/openaiws/session.go`
- `internal/worker/actor.go`
- `internal/worker/docexec.go`
- `internal/worker/responsecreate.go`
- `internal/threadstore/store.go`
- `internal/postgresstore/store.go`
