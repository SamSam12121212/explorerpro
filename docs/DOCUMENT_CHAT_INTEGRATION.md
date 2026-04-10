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

- Current working state captured on: April 10, 2026

## Core Product Direction

The intended product shape is:

- users upload PDFs through the frontend
- PDFs are processed outside the thread runtime
- chat threads can have documents attached to them
- the model will later use a new document tool to work with attached documents
- the worker should remain focused on OpenAI Responses WebSocket thread execution

This is meant to stay aligned with the broader repo design:

- ingestion and preparation stay outside the worker
- the worker stays OpenAI-native and thread-centric
- document-specific behavior should live in an external service/tool layer

## Important Clarifications From Discussion

### 1. The document is not being sent on the main chain as raw engine state

We explicitly moved away from the idea of treating document payloads as permanent runtime state inside the main thread in a transport-shaped form.

The main thread should not become a giant store of PDF payload material.

### 2. The worker should stay dumb

The worker should continue to be:

- a thread actor
- a Responses WebSocket runtime
- a `function_call` / `function_call_output` continuation engine

The worker should not become:

- a PDF processor
- a document-aware orchestration layer
- a document query engine

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
- the actual document execution happens outside the worker

This is the clean boundary.

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
- a worker that can lower `image_ref` to `input_image` at send time
- a `documenthandler` service shell that is now wired into Docker but does nothing yet

### Current boundary we want to preserve

The correct boundary remains:

- PDF upload and split happen outside the thread runtime
- document manifests and page refs are the stable artifact contract
- document querying should happen in a dedicated service/tool layer
- the worker should just wait in `waiting_tool` and continue when it gets `thread.submit_tool_output`

## Current Implementation Status

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
- the worker remains unaware of all of this

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

## Deliberate Limitations Right Now

These are intentional and should not be mistaken for bugs in the current slice.

### 1. Attached documents are persisted thread metadata, not model-visible inputs yet

Right now document attachments:

- are sent to the API on thread create/resume
- are persisted in backend thread attachment state
- are not included in thread input items
- are not available to the model
- are not lowered into any OpenAI-side document prompt shape yet

They now exist as thread attachment state, not just local composer state.

### 2. The document tool does not exist yet

There is no tool like:

- `query_attached_document`
- `read_attached_document`
- `search_attached_document`

yet.

### 3. There is no general tool-runner service yet

Today the worker only has custom handling for:

- `spawn_subagents`

All other tool calls just leave the thread in `waiting_tool`.

So before documents can work end to end, we need a real execution path that:

- notices the tool call
- executes external logic
- submits a `function_call_output` back to the thread

### 4. The backend thread attachment model is still only a first pass

What still does not exist yet:

- no dedicated attach/detach endpoint for thread documents
- no persisted detach/remove flow for already attached thread documents
- no inheritance rules yet for child threads or warm branches
- no server-side validation yet that a future document tool can only access attached docs

## What We Agreed Not To Do

These points are important because they protect the shape of the system.

### 1. Do not make the worker document-smart

The worker should not:

- read manifests as part of business logic
- choose document pages
- own document retrieval policy
- build document answers itself

### 2. Do not persist base64-heavy document payloads as runtime truth

Any future XML-tag + page-image structure should be generated near the OpenAI request boundary, not stored as the long-term engine representation.

### 3. Do not blur document prep with thread execution

`docsplitter` and future document handling should remain separate from the OpenAI thread runtime.

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

Current owner:

- frontend + API/read model

### Layer 3: Tool execution

- model asks to use an attached document
- external service handles the request
- thread resumes with tool output

Current owner:

- not implemented yet
- likely future owner is `documenthandler`

## Recommended Next Step

The next sensible step is no longer attachment persistence.

It is:

1. design a first document tool schema
2. decide how the model should discover attached documents
3. give `documenthandler` a real command/execution loop
4. validate that the tool can only access documents attached to the thread

The current backend shape is now:

- `thread_documents` stores thread/document relationships
- create/resume requests can attach documents by `attached_document_ids`
- thread reads and thread snapshots expose `attached_documents`
- the worker still does not know document semantics

The next backend shape should be:

- a document tool is added to the chat tool list
- `documenthandler` becomes the executor for that tool
- it validates the requested document is attached
- it loads the manifest and page refs
- it later returns a normal `function_call_output`

## Suggested First Tool Shape

Not implemented yet, but this is the current intended direction:

```json
{
  "type": "function",
  "name": "query_attached_document",
  "description": "Inspect one attached document and return concise evidence-backed findings.",
  "parameters": {
    "type": "object",
    "additionalProperties": false,
    "properties": {
      "document_id": { "type": "string" },
      "task": { "type": "string" },
      "page_numbers": {
        "type": "array",
        "items": { "type": "integer" }
      }
    },
    "required": ["document_id", "task"]
  }
}
```

This is only a planning direction, not a locked contract yet.

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
- documenthandler service shell is real
- backend thread attachment persistence is real
- thread attachment read/hydration is real
- chat-side pending attachment UI is real
- paperclip-based thread attachment viewer is real
- document tool execution is not built yet

What comes next:

- tool contract
- documenthandler execution path
- tool-side attachment validation
- optional dedicated attach/detach management if product wants it
- worker continuation through standard tool output
