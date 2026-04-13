# Document Chat Integration

Updated: April 13, 2026

## Purpose

This file is the current handoff note for attached-document support in chat after the document-executor removal.

It is meant to answer three questions quickly:

- what the product shape is now
- what the runtime actually does now
- what the next real engineering stages are now

## Current Product Shape

- users upload PDFs through the frontend
- `docsplitter` turns PDFs into manifests plus page images
- chat threads can attach document IDs
- attached documents are advertised to the model at send time
- the model can call `query_attached_documents`
- each requested document query runs as a normal child thread
- the worker is still the only process that talks to OpenAI

## Current Boundaries

### `docsplitter`

Owns source preparation:

- read uploaded PDF
- render page PNGs
- write manifest + page image refs

It does not participate in thread execution.

### `documenthandler`

Owns document-adjacent runtime preparation:

- build runtime-context augmentation for attached documents
- inject the `query_attached_documents` tool definition
- build prepared input artifacts for first-touch document queries

It does not open OpenAI sockets and does not execute document queries.

### worker

Owns OpenAI execution:

- build and send `response.create`
- stream socket events
- detect `query_attached_documents`
- spawn one child thread per document
- regroup child results
- update thread-local document lineage

### storage

- Postgres stores threads, spawn groups, child results, document metadata, and `thread_documents`
- blob storage stores source PDFs, manifests, page images, and prepared input artifacts
- JetStream stores commands, live events, and durable thread history

## Current Runtime Flow

### 1. Attachments exist as thread state

Attached documents are persisted in `thread_documents`.

That relation currently serves two roles:

- attachment membership for a thread
- thread-local document lineage for that `(thread_id, document_id)` pair

### 2. Runtime context is applied before send

Before a normal `response.create`, the worker loads attached documents and augments the outgoing payload.

Preferred path:

- call `documenthandler` via `doc.runtime_context`
- append an `<available_documents>` block to `instructions`
- inject `query_attached_documents` into `tools`

Fallback path:

- if the runtime-context RPC fails, the worker performs the same augmentation locally

### 3. The model chooses the tool

If the model emits a `function_call` named `query_attached_documents`, the actor captures it during `streamUntilTerminal`.

The tool shape is:

```json
{
  "type": "function",
  "name": "query_attached_documents",
  "parameters": {
    "type": "object",
    "additionalProperties": false,
    "properties": {
      "document_ids": {
        "type": "array",
        "items": { "type": "string" }
      },
      "task": {
        "type": "string"
      }
    },
    "required": ["document_ids", "task"]
  }
}
```

### 4. The parent thread spawns document-query children

On `response.completed`, the parent thread does not execute the document query inline.

Instead it:

- validates that every requested document is attached to the thread
- chooses a model per document
- resolves `previous_response_id` from thread-local lineage first
- falls back to a compatible shared base anchor if one already exists on the document row
- if no usable lineage exists, asks `documenthandler` for a `PrepareKindDocumentQuery` artifact that bundles document pages plus the task
- creates one child thread per document
- moves the parent thread to `waiting_children`

Each child thread starts in one of two ways:

- `previous_response_id` + `initial_input` containing the task
- `prepared_input_ref` when the document has no usable prior lineage

### 5. Child threads are normal threads

Document-query children use the same runtime as every other thread:

- normal ownership claim
- normal OpenAI socket
- normal `sendAndStream` / `streamUntilTerminal`
- normal item persistence
- normal `THREAD_HISTORY` checkpoints
- normal `THREAD_EVENTS` fanout
- normal recovery and adoption

There is no separate document executor runtime anymore.

### 6. Regroup uses the normal spawn barrier

When a child finishes, the parent handles it through the same child-result path used for subagents.

That path:

- stores the child result in the spawn group
- aggregates the final tool output when all children are done
- resumes the parent thread normally

## Current Lineage Semantics

### Thread-local lineage is now derived from completed child threads

Today, follow-up document queries inside one parent thread use:

- the latest completed document-query child thread for that `(parent_thread_id, document_id)` pair
- that child thread's `last_response_id`
- that child thread's `model`

`thread_documents` is back to attachment membership only.

### Shared base anchor is only an optional read path right now

The current actor will branch from:

- `documents.base_response_id`
- only if `documents.base_model` matches the chosen model

But the current child-thread document-query flow does not create or rebuild new shared base anchors.

That means:

- the read path exists
- the schema and API surface exist
- the automatic write path is not currently part of the live query flow

### First-touch bootstrap is now one-shot

If there is no usable `previous_response_id`, the worker does not do a separate warmup-then-query dance.

Instead it asks `documenthandler` for a `document_query` prepared input artifact that contains:

- the document wrapper text
- page markers
- page image refs
- the user task

The child thread then starts directly from that prepared input.

`PrepareKindWarmup` still exists as a helper shape, but it is not the path used by the current document-query flow.

## Current Model Semantics

- `documents.query_model` is the default model for a new document-query branch
- if a thread already has document lineage, that thread keeps using the latest completed document child's model
- changing a document default model does not migrate existing thread-local chains
- clearing a document base anchor does not clear thread-local lineage that already exists

## Current UI / API State

Already implemented:

- PDF upload
- document split + manifest generation
- attach documents to chat from the Documents view
- persist `attached_document_ids` on thread create/resume
- hydrate attached documents on thread load and websocket snapshot
- inject document availability into runtime context
- dispatch `query_attached_documents`
- show per-document query model and base-anchor metadata in the PDF viewer details drawer
- `PATCH /documents/:id` support for updating `query_model` and clearing stored base-anchor fields

Still true:

- documents are not injected into the parent thread as raw page payloads
- the parent thread only sees attachment metadata plus tool availability
- subagent tools filter out `query_attached_documents`
- the composer still does not support a document-only send without text or image input

## Relevant Files

- `internal/docsplitter/service.go`
- `internal/documenthandler/service.go`
- `internal/doccmd/command.go`
- `internal/worker/actor.go`
- `internal/worker/service.go`
- `internal/threaddocstore/store.go`
- `internal/docstore/store.go`
- `internal/httpserver/command_api.go`
- `internal/httpserver/document_api.go`
- `frontend/src/useChat.ts`
- `frontend/src/components/PdfViewerPanel.tsx`

## What We Explicitly Are Not Doing

- no worker-local document executor runtime
- no private document session cache as durable truth
- no separate document streaming loop
- no raw document payloads persisted as part of the parent thread state
- no OpenAI calls from `documenthandler`
- no inheritance of one thread's document Q&A history into another thread by default

## Next Stages

### Stage 1: Shared base-anchor optimization

If cross-thread first-query latency matters, reintroduce a real shared base-anchor write path.

That would mean:

- a deliberate policy for creating `documents.base_response_id`
- versioning / invalidation rules for model and prompt changes
- explicit document-change invalidation rules

### Stage 2: Product/runtime hardening

After the model is stable:

- live validation with real PDFs and real OpenAI traffic
- observability around document-query fan-out and failure modes
- attachment-management UX beyond attach-on-send

## Short Summary

The important current mental model is:

- attachments live on the thread
- runtime context advertises them to the model
- document queries run as child threads
- child threads are normal threads, not a special executor path
- thread-local lineage is derived from the latest completed document child thread
- shared base-anchor reuse is only partial today
- the next real implementation stage is shared base-anchor optimization
