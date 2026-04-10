# Document Chat Integration

## Purpose

This file is the working handoff note for document support in chat.

It captures:

- what we discussed
- what was clarified
- what is already implemented
- what is intentionally not implemented yet
- the next sensible steps

The goal is that after a full context reset, this file is enough to recover the current plan and avoid re-deciding the same things.

## Date

- Previous working state captured on: April 10, 2026
- Updated: April 10, 2026 (document lineage schema + tool dispatch skeleton)

## Core Product Direction

The intended product shape is:

- users upload PDFs through the frontend
- PDFs are processed outside the thread runtime
- chat threads can have documents attached to them
- the model will later use a new document tool to work with attached documents
- the worker should remain focused on OpenAI Responses WebSocket execution, including worker-owned document sessions for tools

This is meant to stay aligned with the broader repo design:

- ingestion and preparation stay outside the worker
- the worker stays OpenAI-native and thread-centric
- the worker is the only process that ever touches OpenAI
- OpenAI-facing document session execution must therefore live inside the worker boundary
- non-OpenAI document preparation or support behavior can still live in separate services

## Important Clarifications From Discussion

### 1. The document is not being sent on the main chain as raw engine state

We explicitly moved away from the idea of treating document payloads as permanent runtime state inside the main thread in a transport-shaped form.

The main thread should not become a giant store of PDF payload material.

### 2. The worker should stay focused, but it is the only OpenAI boundary

The worker should continue to be:

- a thread actor
- a Responses WebSocket runtime
- a `function_call` / `function_call_output` continuation engine
- the only process that opens Responses sockets or sends OpenAI requests

The worker should not become:

- a PDF processor
- a page renderer
- a document ingestion pipeline
- a generic document preparation service

What this means in practice:

- the worker must own document sockets
- the worker must send document warmup requests with `generate:false`
- the worker must send the actual document question
- any future helper service must stay out of the OpenAI path

### 3. XML-like tags are still valid, but only at the OpenAI adapter boundary

The clarified idea was not "text-wrapped base64".

The intended future wire shape is closer to:

- an `input_text` item for a document/container tag
- page-scoped text markers such as `<pdf_page ...>`
- `input_image` items for page images
- closing tags as separate text items if useful

That can make sense as an OpenAI request-shaping contract.

It should not become the durable source of truth in thread storage.

### 4. Attached documents will later be exposed through a tool

The longer-term direction is:

- a thread knows which document IDs are attached
- the model gets a new document tool
- the tool can only operate on attached documents
- OpenAI-facing document execution happens in a worker-local document executor, not in an external OpenAI-calling service

This is the clean boundary.

### 5. Document execution should use durable response lineage, not permanent sockets

The newest agreed direction is:

- durable state should be OpenAI response IDs
- live sockets are a latency optimization, not the long-term source of truth
- a document may have a hot reusable worker-owned socket for a while, but the system must always be able to reopen and continue from `previous_response_id`

This matters because:

- a single socket still only supports one in-flight response at a time
- parent chat sockets and document sockets therefore need to be separate worker-owned sessions
- parallel document reads therefore still require multiple worker-owned sockets
- socket lifetime is operationally bounded, so we cannot model "document memory" as "one socket forever"

### 6. We want two layers of lineage for documents

The agreed model is now:

- the `documents` row can hold one shared base anchor for that document
- the `thread_documents` relation should hold the thread-local lineage for that document inside one chat thread

That means:

- a brand new chat thread should not inherit another thread's document Q&A history
- but it should be able to reuse a document-level initialization anchor
- thread-local follow-up questions should continue from the thread-local latest response ID, not from the document global base every time

### 7. First use in a new thread may need a two-step bootstrap

If a thread asks a document question and there is no usable prior lineage yet, the agreed bootstrap is:

1. a worker-local document executor warms the document with `generate:false` on a worker-owned document session
2. persist the returned response ID as the shared base anchor on the `documents` row
3. the same worker-local executor then sends the actual question
4. persist the returned response ID as the thread-local latest lineage for that `thread_id + document_id`

If a shared base anchor already exists, then the new thread can skip the warmup step and start from that anchor immediately.

### 8. The main thread should not care about document session mechanics

The parent thread's job is only:

- decide which attached documents to ask
- trigger the question through the normal tool flow
- wait for regrouped results

The parent thread should not care whether the worker-local document executor:

- reused a hot worker-owned socket
- continued from a thread-local latest response ID
- bootstrapped from the shared document base anchor
- rebuilt missing lineage after an OpenAI error
- reopened a fresh socket from persisted lineage

### 9. `documenthandler` is not the OpenAI executor

The `documenthandler` service shell exists in the repo, but after the latest clarification:

- it should not open Responses sockets
- it should not send `generate:false`
- it should not ask the actual document question
- if it gains future responsibilities, they must remain outside the OpenAI boundary

This matters because the worker is the only thing that ever touches OpenAI.

## Architecture Position As Of Now

### What already exists in the repo

The current repo already has:

- PDF upload via the frontend and API
- `docsplitter` as a separate service
- manifest + page PNG generation
- blob-backed document artifacts
- backend thread attachment persistence via a dedicated join table
- thread attachment reads on `GET /threads/{thread_id}` and websocket snapshots
- chat image attachments
- chat document attachment hydration and a paperclip attachment viewer
- worker-side generated instruction decoration for attached-document discovery
- a worker that can lower `image_ref` to `input_image` at send time
- a `documenthandler` service shell that is now wired into Docker but does nothing yet, but it is not the planned owner of OpenAI document execution

### Current boundary we want to preserve

The correct boundary remains:

- PDF upload and split happen outside the thread runtime
- document manifests and page refs are the stable artifact contract
- OpenAI-facing document querying must stay in a worker-local executor/session layer
- non-OpenAI document support work may still happen in dedicated services
- the worker may read thread attachment metadata at send time to shape outbound instructions
- the parent chat thread actor should just wait in `waiting_tool` and continue when it gets `thread.submit_tool_output`

## Current Implementation Status

### 0. Document lineage schema and tool dispatch skeleton now exist

The database schema now supports the lineage model and the worker can dispatch document tool calls.

Implemented behavior:

- `documents` table now has `base_response_id`, `base_model`, `base_initialized_at` columns
- `thread_documents` table now has `latest_response_id`, `initialized_at`, `last_used_at` columns
- all new columns are nullable / have safe defaults so existing data is unaffected
- `docstore.Document` struct reads and writes the new lineage fields
- `threaddocstore` has a `FilterAttached` method for validating document IDs against a thread
- a `query_attached_documents` tool definition is dynamically injected into the outbound tools list when documents are attached to the thread
- the tool accepts `document_ids` (array of strings) and `task` (string)
- no `page_numbers` parameter — each document has all pages loaded; the model mentions specific pages in the task text if needed
- in `streamUntilTerminal`, a `function_call` with name `query_attached_documents` is now recognized and dispatched
- after `response.completed`, if the only pending tool is a document query, the worker validates the requested document IDs are attached, builds a stub `function_call_output`, and self-publishes a `thread.submit_tool_output` command
- the stub output reports validation errors for unattached documents, or returns a `not_yet_implemented` status for valid ones
- the self-published command follows the normal `handleSubmitToolOutput` path, so the thread continues cleanly
- the document query tool is stripped from subagent tools via `filterSubagentTools` to prevent children from querying documents directly
- if both a document query and another unknown tool are pending, the thread falls back to `waiting_tool` for all tools (no partial auto-handling)

Current relevant files:

- `db/migrations/000009_document_lineage.sql`
- `internal/docstore/store.go`
- `internal/threaddocstore/store.go`
- `internal/worker/actor.go`
- `internal/worker/actor_test.go`

### 1. Document upload and split already exist

Already in place:

- upload PDFs from the frontend
- store source PDF in blob storage
- create a document row in Postgres
- publish a split command
- `docsplitter` renders PNG-per-page and writes a manifest

Current relevant files:

- `internal/httpserver/document_api.go`
- `internal/docsplitter/service.go`
- `internal/docsplitter/manifest.go`
- `internal/docstore/store.go`

### 2. `documenthandler` service now exists as infrastructure only

A new service was added in the same monorepo/service style as the rest of the Go backend.

What exists now:

- `cmd/documenthandler/main.go`
- `internal/documenthandler/service.go`
- Dockerfile build/copy support
- `compose.yaml` service entry

What it does right now:

- starts
- connects to NATS/Postgres/blob store
- logs startup
- waits for shutdown

What it does not do yet:

- no subscriptions
- no tool execution
- no document logic
- no chat integration
- it is not the planned owner of OpenAI document execution

This was intentionally only the first infrastructure step.

### 3. Document attachments in the chat UI now exist

The frontend first gained a simple composer-level attachment flow for documents.

Implemented behavior:

- in the Documents section of the sidebar, each document row shows a hover menu
- the menu includes `Attach to chat`
- selected documents appear above the chat input as pending composer attachments
- pending documents can be removed from the composer before send

This mirrors the existing image attachment experience closely enough for the first pass.

Current relevant files:

- `frontend/src/mid-panel/views/DocumentsView.tsx`
- `frontend/src/components/chat/ChatPanel.tsx`
- `frontend/src/components/LeftSidebar.tsx`
- `frontend/src/useChat.ts`
- `frontend/src/App.tsx`
- `frontend/src/types.ts`

### 4. Backend thread attachment persistence now exists

The next backend slice is now implemented.

Implemented behavior:

- `POST /threads` accepts `attached_document_ids`
- `thread.resume` bodies accept `attached_document_ids`
- the API validates referenced document IDs exist before attaching them
- attached document rows are persisted in Postgres through `thread_documents`
- repeated sends with the same document are idempotent through the table primary key
- the worker still remains unaware of document contents and manifests

Current relevant files:

- `db/migrations/000008_thread_documents.sql`
- `internal/threaddocstore/store.go`
- `internal/httpserver/command_api.go`

### 5. Thread attachment reads and chat hydration now exist

Attached documents can now be read back and shown on an existing thread.

Implemented behavior:

- `GET /threads/{thread_id}` returns `attached_documents`
- websocket `thread.snapshot` payloads also include `attached_documents`
- loading an existing thread hydrates persisted attached documents into the frontend
- the chat composer now distinguishes between persisted thread attachments and pending next-send attachments
- the paperclip button opens a modal-style viewer that can show many documents at once
- the viewer currently shows:
  - documents already attached to the thread
  - documents pending for the next send

Current relevant files:

- `internal/httpserver/command_api.go`
- `internal/wsserver/client.go`
- `internal/wsserver/server.go`
- `cmd/wsserver/main.go`
- `frontend/src/components/chat/ChatPanel.tsx`
- `frontend/src/useChat.ts`
- `frontend/src/App.tsx`
- `frontend/src/types.ts`

### 6. Attached document discovery through generated instructions now exists

The model now has a first discovery mechanism for attached documents.

Implemented behavior:

- right before a normal `response.create`, the worker reads the thread's currently attached documents
- the worker appends a generated `<available_documents>` block to the outgoing `instructions`
- this happens for both thread start and later continuation sends
- stored thread metadata keeps the original base instructions unchanged
- if attachments change between turns, the next send gets an updated generated list
- only attachment metadata is exposed here:
  - document `id`
  - document `filename`
- manifests, page refs, and document contents are still not part of this step

Current generated shape:

```text
<available_documents>
<document id="doc_123" name="Report.pdf" />
</available_documents>
```

Current relevant files:

- `internal/worker/actor.go`
- `internal/worker/service.go`
- `internal/worker/actor_test.go`

### 7. The per-document lineage model is now agreed in detail, but not implemented yet

This is the newest planning state and should be treated as the intended next architecture.

Agreed durable model:

- `documents` row stores the shared base anchor for one document
- `thread_documents` stores the thread-local latest lineage for that document inside one parent chat thread

Agreed execution ownership:

- the worker is the only process that ever talks to OpenAI
- a worker-local document executor owns document sockets
- `generate:false` warmups happen in the worker
- actual document questions happen in the worker
- other services may assist with non-OpenAI work only

Planned `documents` row additions:

- `base_response_id`
- `base_model`
- `base_prompt_version`
- `base_initialized_at`
- `base_manifest_ref` or equivalent document-version marker

Planned `thread_documents` additions:

- `latest_response_id`
- `initialized_at`
- `last_used_at`
- optional `status`
- optional `last_error`

Agreed execution behavior:

- first query for a document inside a thread checks `thread_documents.latest_response_id`
- if present, continue from that thread-local lineage
- otherwise check `documents.base_response_id`
- if the base anchor exists, start the thread-local document chain from that anchor
- if the base anchor does not exist, create it first with `generate:false` on a worker-owned document session
- after the actual query completes, save the new response ID back to `thread_documents.latest_response_id`

Agreed rebuild behavior:

- if OpenAI returns a lineage error such as missing previous response state, rebuild
- if the document changes materially, rebuild the shared base anchor
- if the prompt or model contract changes materially, rebuild the shared base anchor
- thread-local lineage should be treated as disposable and rebuildable from the shared base anchor

Agreed socket behavior:

- a hot worker-owned document socket is allowed and useful
- a hot worker-owned document socket is not the durable truth
- sockets should be reopenable from persisted response lineage
- document sockets should be closable on idle timeout, worker pressure, or planned rotation
- the system should assume sockets are temporary but response IDs are durable enough for continuation

## Deliberate Limitations Right Now

These are intentional and should not be mistaken for bugs in the current slice.

### 1. Attached documents are discoverable, but not real document inputs yet

Right now document attachments:

- are sent to the API on thread create/resume
- are persisted in backend thread attachment state
- are not included in thread input items
- are discoverable to the model only through generated outgoing instructions
- are not lowered into any OpenAI-side document prompt shape yet
- are not executable through a document tool yet

They now exist as:

- persistent thread attachment state
- generated instruction-level discovery context

They still do not exist as real document execution context.

### 2. The document tool exists as a stub only

The `query_attached_documents` tool is now:

- defined and dynamically injected into outbound tool lists when documents are attached
- recognized and dispatched in `streamUntilTerminal`
- validated (document IDs must be attached to the thread)
- self-handled via a `thread.submit_tool_output` self-publish

But it is still a stub:

- it returns `not_yet_implemented` for valid document queries
- it does not open a document session or call OpenAI
- it does not read manifests or page images
- it does not persist lineage

### 3. The document lineage schema exists, but no executor logic uses it yet

The columns now exist:

- `base_response_id`, `base_model`, `base_initialized_at` on `documents`
- `latest_response_id`, `initialized_at`, `last_used_at` on `thread_documents`

What still does not exist yet:

- no executor logic yet for:
  - thread-local lineage reuse
  - shared base-anchor reuse
  - `generate:false` bootstrap
  - rebuild-on-error behavior
- no idle/rotation policy yet specifically for hot worker-owned document sockets

### 4. The worker now has a document tool dispatch path, but no general tool runner

The worker now has custom handling for:

- `spawn_subagents` (full implementation)
- `query_attached_documents` (stub, self-publishes tool output)

All other tool calls still leave the thread in `waiting_tool`.

The document query path demonstrates the pattern for worker-local tool execution: detect the tool call, build the output, self-publish `thread.submit_tool_output`. The next step replaces the stub with a real document executor.

### 5. The backend thread attachment model is still only a first pass

What still does not exist yet:

- no dedicated attach/detach endpoint for thread documents
- no persisted detach/remove flow for already attached thread documents
- no inheritance rules yet for child threads or warm branches
- no server-side validation yet that a future document tool can only access attached docs

## What We Agreed Not To Do

These points are important because they protect the shape of the system.

### 1. Do not turn the worker into a document preparation pipeline

The worker should not:

- read raw PDFs as primary execution input
- split PDFs or render pages
- own long-term document artifact generation
- become a generic document preparation service

What is acceptable:

- reading the thread's attached document IDs and filenames at send time
- appending a generated discovery block to outbound instructions
- loading manifest/page refs at the OpenAI boundary when executing a document tool
- opening or reusing worker-owned document sockets
- sending `generate:false` warmups
- continuing a document query from stored response lineage
- keeping a worker-owned document socket warm temporarily if it improves latency
- submitting `function_call_output` back to the parent thread

That is runtime/session ownership, not document preparation.

### 2. Do not persist base64-heavy document payloads as runtime truth

Any future XML-tag + page-image structure should be generated near the OpenAI request boundary, not stored as the long-term engine representation.

### 3. Do not blur document prep with thread execution

`docsplitter` and future non-OpenAI document handling should remain separate from the OpenAI thread runtime.

## Best Current Mental Model

Think of the document path as three layers:

### Layer 1: Preparation

- upload PDF
- split to page PNGs
- write manifest

Current owner:

- `docsplitter`

### Layer 2: Attachment

- user picks a document from the Documents panel
- document appears in the chat composer as pending
- on send, `attached_document_ids` are sent to the API
- the backend stores thread/document relations
- thread reads and snapshots hydrate attached docs back into the frontend
- the paperclip viewer becomes the scalable UI for many attached documents
- right before each normal `response.create`, the worker appends a generated `<available_documents>` block so the model can see which docs exist

Current owner:

- frontend + API/read model + worker send boundary

### Layer 3: Tool execution

- model asks to use an attached document
- worker-local document executor resolves document lineage
- it may reuse a hot worker-owned document socket
- otherwise it continues from a stored thread-local or document-level response ID
- if no usable base anchor exists yet, the worker first warms the document with `generate:false`
- then the worker runs the actual question on a separate document session
- thread resumes with tool output

Current owner:

- not implemented yet
- likely future owner is a worker-local document executor/session manager
- `documenthandler` is not the OpenAI executor for this path

## Recommended Next Step

The next sensible step is to replace the document query stub with a real document executor.

The schema, tool definition, dispatch path, and self-publish mechanism are now all in place. The stub returns `not_yet_implemented` but proves the full round-trip works.

The next increment should:

1. build a worker-local document executor that:
   - loads the document manifest from blob storage
   - opens a worker-owned document session (separate from the parent chat thread socket)
   - checks `thread_documents.latest_response_id` for existing thread-local lineage
   - falls back to `documents.base_response_id` for the shared base anchor
   - creates the shared base anchor with `generate:false` when missing
   - sends the actual question with all page images from the manifest
   - persists the resulting response ID back to `thread_documents.latest_response_id`
   - returns the model's answer as the `function_call_output`
2. replace `buildDocumentQueryStubOutput` with a real call to the executor
3. persist lineage after each successful query
4. add rebuild-on-error handling for stale or missing lineage

The current backend shape is now:

- `documents` has lineage columns (`base_response_id`, `base_model`, `base_initialized_at`) but they are unused
- `thread_documents` has lineage columns (`latest_response_id`, `initialized_at`, `last_used_at`) but they are unused
- the `query_attached_documents` tool is injected when docs are attached
- the worker dispatches document tool calls and self-publishes stub outputs
- the full round-trip from model tool call through dispatch through `thread.submit_tool_output` works

The next backend shape should:

- populate `documents.base_response_id` after the first `generate:false` warmup
- populate `thread_documents.latest_response_id` after each document query
- use the lineage fields to decide warm vs cold starts
- handle OpenAI lineage errors by rebuilding from the base anchor

## Implemented Tool Shape

The tool is now registered and dispatched. The implemented shape is:

```json
{
  "type": "function",
  "name": "query_attached_documents",
  "description": "Query one or more attached documents. Each document has all of its pages already loaded into a separate analysis session. Describe what you need in the task field; mention specific page numbers there if needed.",
  "parameters": {
    "type": "object",
    "additionalProperties": false,
    "properties": {
      "document_ids": {
        "type": "array",
        "items": { "type": "string" },
        "description": "IDs of the attached documents to query."
      },
      "task": {
        "type": "string",
        "description": "What to look for or ask about in the documents."
      }
    },
    "required": ["document_ids", "task"]
  }
}
```

Key design decisions:

- `document_ids` is an array so the model can query multiple documents in one call
- there is no `page_numbers` parameter — each document has all pages loaded; the model mentions specific pages in the task text if needed
- the tool is dynamically injected into the outbound tools list only when documents are attached to the thread
- the tool is stripped from subagent tools to prevent children from querying documents directly

## Planned Document Session Flow

This is the most important newest planning section.

All OpenAI calls below happen inside the worker on worker-owned document sessions. The parent chat thread actor can still sit in `waiting_tool` while the worker process drives the separate document session.

The intended flow for one document query is now:

### Case A: Existing thread-local lineage exists

1. Parent thread asks an attached document question.
2. Worker-local document executor looks up `thread_documents.latest_response_id`.
3. Worker-local document executor continues that document chain from the thread-local latest response ID.
4. Worker-local document executor saves the newly returned response ID back onto `thread_documents`.
5. Worker-local document executor returns a normal tool result to the parent thread.

### Case B: No thread-local lineage, but shared document base anchor exists

1. Parent thread asks an attached document question.
2. Worker-local document executor does not find a thread-local latest response ID.
3. Worker-local document executor finds `documents.base_response_id`.
4. Worker-local document executor starts the new thread-local document chain from that shared base anchor.
5. Worker-local document executor saves the newly returned response ID onto `thread_documents`.
6. Worker-local document executor returns a normal tool result to the parent thread.

### Case C: No thread-local lineage and no shared base anchor yet

1. Parent thread asks an attached document question.
2. Worker-local document executor finds no thread-local latest response ID.
3. Worker-local document executor finds no shared document base anchor.
4. Worker-local document executor sends a document warmup request with `generate:false` on a worker-owned document session.
5. Worker-local document executor saves the returned response ID onto the `documents` row as the shared base anchor.
6. Worker-local document executor then sends the actual question from the parent thread.
7. Worker-local document executor saves the returned response ID onto `thread_documents`.
8. Worker-local document executor returns a normal tool result to the parent thread.

### Case D: Stored lineage is invalid or stale

If OpenAI rejects the stored lineage, or the system decides the base anchor is stale because the document/model/prompt contract changed, then:

1. discard the invalid thread-local lineage
2. rebuild the shared base anchor if needed
3. rebuild the thread-local lineage from the fresh base anchor
4. continue normally

## Socket Policy For Documents

The current intended policy is:

- a document query may run on a hot reusable worker-owned socket if one already exists
- the parent chat thread socket and the document query socket are separate worker-owned sockets
- a single socket still only supports one in-flight response at a time
- a document socket is allowed to stay warm temporarily for low-latency follow-ups
- a document socket is not the durable state boundary
- persisted response IDs are the durable state boundary
- document sockets can be closed and later reopened from stored lineage

The likely closure triggers are:

- idle timeout
- worker pressure / socket budget pressure
- planned rotation before connection lifetime limits
- worker shutdown or ownership loss

So the correct mental model is:

- response IDs are the durable lineage
- worker-owned sockets are a hot cache
- parent/document regroup still happens through the normal barrier/tool-output machinery

## Future OpenAI Request Shape

The current intended direction for the eventual OpenAI-side adapter is something like:

- one text item to establish document context
- one text marker per page or per page group
- one `input_image` item per page image
- optional closing markers

Example idea:

```text
input_text: <pdf name="Report.pdf" id="doc_123" page_count="12">
input_text: <pdf_page number="1">
input_image: data or blob-lowered image
input_text: </pdf_page>
...
input_text: </pdf>
```

Again:

- this is for request shaping
- not for durable engine state

## Current UI State Details

Behavior implemented in the frontend right now:

- hover a document row in Documents
- click 3-dot menu
- choose `Attach to chat`
- selected document appears above the composer as pending
- pending document can be removed before send
- after send, the document is persisted on the thread
- when a thread has attached docs, the paperclip shows an attachment count
- clicking the paperclip opens a viewer for thread attachments and pending attachments

Current behavior after reset/load:

- pending docs clear when the conversation is reset
- pending docs clear when loading another thread
- persisted thread attachments hydrate when loading an existing thread
- local attachment UI clears on reset because no thread is selected anymore

That is acceptable for the current pass because the backend now owns the persistent thread attachment state.

## Validation Already Done

During this work the following checks passed:

- `go test ./...`
- frontend typecheck
- frontend production build
- `documenthandler` Go package build/test
- Docker Compose service registration/build for `documenthandler`

## Short Summary

Where we are now:

- document upload and split are real
- documenthandler service shell is real, but it is not the planned OpenAI executor
- backend thread attachment persistence is real
- thread attachment read/hydration is real
- attached-document discovery via generated instructions is real
- chat-side pending attachment UI is real
- paperclip-based thread attachment viewer is real
- the per-document lineage model is now agreed in detail
- the worker-only OpenAI ownership rule is now explicitly clarified
- the lineage schema is in place (`base_response_id` on documents, `latest_response_id` on thread_documents)
- the `query_attached_documents` tool is defined, injected, dispatched, and validated
- the full tool call round-trip works end to end (model calls tool → worker dispatches → stub output → self-publish → thread continues)
- the stub returns `not_yet_implemented` for valid queries and validation errors for unattached documents

What comes next:

- replace the stub with a real worker-local document executor
- load manifests and page images from blob storage
- open worker-owned document sessions with `generate:false` warmup
- persist lineage after each query
- add rebuild rules for missing/stale lineage
- decide later whether `documenthandler` needs any non-OpenAI role
