# Worker Runtime

## Scope

The worker owns thread execution.

That includes:

- command consumption from `AGENT_CMD`
- thread actor lifecycle
- ownership claim and lease renewal in Postgres
- OpenAI socket management
- normalized item persistence
- raw response persistence
- spawn-group coordination
- document runtime-context augmentation and document-query child dispatch
- durable checkpoint and raw event history writes
- live event handoff to wsserver

## Command Handling

A thread actor processes one command at a time.

Typical flow:

1. load or claim the thread
2. validate expected status and socket generation
3. mutate Postgres state
4. write history and live events
5. ack the command

## Socket Rules

- one active socket per owned thread
- one in-flight response per thread
- socket generation increments on rotation or adoption
- the owner worker is the only process allowed to write to the socket

## Document Queries

- attached documents are discovered from `thread_documents` before `response.create`
- runtime context appends `<available_documents>` and injects `query_attached_documents`
- when the model calls that tool, the actor spawns one child thread per requested document
- follow-up queries branch from the latest completed document-query child lineage; first-touch queries reuse a compatible shared base anchor or run a warmup child before the real query child
- document queries do not use a separate executor runtime

## Data Boundaries

- document and image preparation happen outside worker core
- prepared input artifacts are loaded by reference
- large binary payloads stay in blob storage
- recovery checkpoints stay in `THREAD_HISTORY`
- response deltas do not stay in `THREAD_HISTORY`
- response deltas do not go to Postgres
- live UI events go through `THREAD_EVENTS` and disappear after wsserver ack
