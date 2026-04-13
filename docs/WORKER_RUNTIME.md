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
- durable checkpoint and raw event history writes
- live event fanout

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

## Data Boundaries

- document and image preparation happen outside worker core
- prepared input artifacts are loaded by reference
- large binary payloads stay in blob storage
- recovery checkpoints stay in `THREAD_HISTORY`
