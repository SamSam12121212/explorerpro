# Redis Surgery

Date of operation: April 13, 2026

## Chief Complaint

The app had developed a severe and persistent dependency on Redis.

Symptoms included:

- runtime coordination living in one store while durable reads lived somewhere else
- recovery depending on Redis event history
- item and event history duplicated across multiple systems
- compatibility seams hanging around long after the architecture decision had changed
- local/dev/runtime config still pretending Redis was part of the family

Clinical decision:

- remove Redis completely
- do not stage a soft migration
- do not carry fallback logic
- do not preserve old cursor shapes for nostalgia

This was not a taper. This was an amputation with reconstruction.

## Pre-Op Anatomy

Before surgery, the runtime shape was effectively:

- Redis for ownership, hot coordination, processed command dedupe, item streams, event streams, and assorted runtime truth
- Postgres for part of the durable model
- NATS + JetStream for commands and live delivery

That shape had two real problems:

1. It split the truth across systems in a way that made recovery and inspection uglier than they needed to be.
2. It kept dead compatibility branches alive in the code even after we had decided where the system actually wanted to live.

## Procedure Summary

We replaced the old shape with this one:

- Postgres owns durable runtime state and coordination
- JetStream `THREAD_HISTORY` owns durable raw socket history and recoverable `client.response.create` checkpoints
- JetStream `THREAD_EVENTS` owns live browser fanout
- Blob storage owns large source artifacts

No Redis. No fallback. No ceremonial backwards compatibility.

## Incision 1: Move Recovery Checkpoints Out Of Redis

First cut:

- created [`internal/threadhistory/store.go`](/Users/detachedhead/explorer/internal/threadhistory/store.go)
- created [`internal/natsbootstrap/threadhistory.go`](/Users/detachedhead/explorer/internal/natsbootstrap/threadhistory.go)
- bootstrapped JetStream stream `THREAD_HISTORY`
- wrote `client.response.create` checkpoints there
- changed worker recovery to read the latest checkpoint from `THREAD_HISTORY`

Important effect:

- recovery stopped depending on Redis event streams

Relevant files:

- [`internal/worker/actor.go`](/Users/detachedhead/explorer/internal/worker/actor.go)
- [`internal/worker/service.go`](/Users/detachedhead/explorer/internal/worker/service.go)
- [`internal/threadhistory/store.go`](/Users/detachedhead/explorer/internal/threadhistory/store.go)
- [`internal/natsbootstrap/threadhistory.go`](/Users/detachedhead/explorer/internal/natsbootstrap/threadhistory.go)

## Incision 2: Move Raw Event History To JetStream

Second cut:

- moved raw socket event persistence into `THREAD_HISTORY`
- kept live fanout in `THREAD_EVENTS`
- changed `GET /threads/{id}/events` to read from `THREAD_HISTORY`

Important effect:

- Redis stopped being the runtime source for event-history reads
- recovery and event inspection now pointed at the same durable append-only history

Relevant files:

- [`internal/httpserver/command_api.go`](/Users/detachedhead/explorer/internal/httpserver/command_api.go)
- [`internal/httpserver/server.go`](/Users/detachedhead/explorer/internal/httpserver/server.go)
- [`internal/worker/actor.go`](/Users/detachedhead/explorer/internal/worker/actor.go)
- [`internal/threadhistory/store.go`](/Users/detachedhead/explorer/internal/threadhistory/store.go)

## Incision 3: Move Coordination And Runtime Truth To Postgres

This was the big organ transplant.

We added a dedicated migration:

- [`db/migrations/000011_runtime_coordination.sql`](/Users/detachedhead/explorer/db/migrations/000011_runtime_coordination.sql)

New durable coordination tables:

- `thread_owners`
- `thread_processed_commands`

We then expanded [`internal/postgresstore/store.go`](/Users/detachedhead/explorer/internal/postgresstore/store.go) so Postgres owned:

- command dedupe via `CommandProcessed` and `MarkCommandProcessed`
- ownership claim, renew, rotate, load, and release
- listing thread IDs by status for recovery sweeps
- per-thread item sequencing inside Postgres
- spawn child result upsert semantics

Important effect:

- worker ownership and lease state moved out of Redis
- processed command dedupe moved out of Redis
- item sequencing moved out of Redis
- spawn result coordination stopped depending on Redis-era structures

## Incision 4: Remove Redis From Runtime Wiring

Once the data path was moved, we deleted the plumbing.

Removed:

- [`internal/redisclient/client.go`](/Users/detachedhead/explorer/internal/redisclient/client.go)
- [`internal/threadstore/store.go`](/Users/detachedhead/explorer/internal/threadstore/store.go)

Kept:

- [`internal/threadstore/types.go`](/Users/detachedhead/explorer/internal/threadstore/types.go) as the shared types package

Rewired services to Postgres + JetStream only:

- [`internal/worker/service.go`](/Users/detachedhead/explorer/internal/worker/service.go)
- [`internal/httpserver/command_api.go`](/Users/detachedhead/explorer/internal/httpserver/command_api.go)
- [`internal/wsserver/client.go`](/Users/detachedhead/explorer/internal/wsserver/client.go)
- [`internal/wsserver/server.go`](/Users/detachedhead/explorer/internal/wsserver/server.go)
- [`cmd/wsserver/main.go`](/Users/detachedhead/explorer/cmd/wsserver/main.go)
- [`internal/platform/runtime.go`](/Users/detachedhead/explorer/internal/platform/runtime.go)
- [`internal/config/config.go`](/Users/detachedhead/explorer/internal/config/config.go)

Important effect:

- runtime boot no longer initializes Redis
- service code no longer accepts Redis clients
- wsserver reads thread snapshots and item backlog from Postgres
- ownership-aware routing reads owner records from Postgres

## Incision 5: Remove Redis From Environment And Local Dev

We cut Redis out of config and local orchestration too.

Changed:

- [`compose.yaml`](/Users/detachedhead/explorer/compose.yaml)
- [`.env.example`](/Users/detachedhead/explorer/.env.example)
- [`README.md`](/Users/detachedhead/explorer/README.md)

What changed:

- removed the `redis` compose service
- removed `REDIS_URL`
- removed Redis volume wiring
- updated startup instructions to `nats` + `postgres`

Important effect:

- local dev stopped lying about what the app actually needs

## Incision 6: Kill Compatibility Theater

After Redis was gone, some old shapes were still haunting the repo.

### 6a. Remove `stream_id` Cursor Baggage

We removed legacy `stream_id` and `*_stream_id` payload fields and made list cursors numeric-only.

Changed:

- [`internal/httpserver/command_api.go`](/Users/detachedhead/explorer/internal/httpserver/command_api.go)
- [`internal/wsserver/client.go`](/Users/detachedhead/explorer/internal/wsserver/client.go)
- [`internal/worker/actor.go`](/Users/detachedhead/explorer/internal/worker/actor.go)
- [`internal/threadstore/types.go`](/Users/detachedhead/explorer/internal/threadstore/types.go)
- [`internal/threadhistory/store.go`](/Users/detachedhead/explorer/internal/threadhistory/store.go)
- [`frontend/src/types.ts`](/Users/detachedhead/explorer/frontend/src/types.ts)
- [`frontend/src/useChat.ts`](/Users/detachedhead/explorer/frontend/src/useChat.ts)

Important effect:

- one pagination model
- no old stream-shaped cursor compatibility path
- cleaner API payloads

### 6b. Make Child completion Results First-Class Enough To Stop Scraping

Originally, parent threads handled child completion by reaching back into the child thread's item history and scraping the latest assistant message to synthesize `assistant_text`.

That was rude.

We changed the contract so the child publishes its own completion summary directly in the child-result command body.

Changed:

- [`internal/threadcmd/command.go`](/Users/detachedhead/explorer/internal/threadcmd/command.go)
- [`internal/worker/actor.go`](/Users/detachedhead/explorer/internal/worker/actor.go)

What changed:

- added `assistant_text` to `thread.child_completed` / `thread.child_failed`
- child completion publishing now fills that field from the child thread's own latest assistant message
- parent-side `handleChildResult` now trusts the command body instead of scraping child items

Important effect:

- parent completion stopped doing cross-thread read-model archaeology
- child-result handling became more explicit and less magical

## Incision 7: Rewrite The Docs So They Stop Gaslighting Us

A bunch of docs still described the old world.

We rewrote the living architecture docs around the new storage split:

- [`docs/ARCHITECTURE.md`](/Users/detachedhead/explorer/docs/ARCHITECTURE.md)
- [`docs/API_RUNTIME.md`](/Users/detachedhead/explorer/docs/API_RUNTIME.md)
- [`docs/POSTGRES_SCHEMA.md`](/Users/detachedhead/explorer/docs/POSTGRES_SCHEMA.md)
- [`docs/MESSAGING.md`](/Users/detachedhead/explorer/docs/MESSAGING.md)
- [`docs/OPENAI_WS_RUNTIME.md`](/Users/detachedhead/explorer/docs/OPENAI_WS_RUNTIME.md)
- [`docs/PREPARED_INPUT_BOUNDARY.md`](/Users/detachedhead/explorer/docs/PREPARED_INPUT_BOUNDARY.md)
- [`docs/IMAGE_INPUT_CONTRACT.md`](/Users/detachedhead/explorer/docs/IMAGE_INPUT_CONTRACT.md)
- [`docs/DOCUMENT_MANIFEST.md`](/Users/detachedhead/explorer/docs/DOCUMENT_MANIFEST.md)
- [`docs/RECOVERY.md`](/Users/detachedhead/explorer/docs/RECOVERY.md)
- [`docs/THREAD_STREAM_IMPLEMENTATION.md`](/Users/detachedhead/explorer/docs/THREAD_STREAM_IMPLEMENTATION.md)
- [`docs/WORKER_RUNTIME.md`](/Users/detachedhead/explorer/docs/WORKER_RUNTIME.md)
- [`docs/STREAMS_REFACTOR.md`](/Users/detachedhead/explorer/docs/STREAMS_REFACTOR.md)

We also deleted dead Redis-era exploration docs.

Important effect:

- the written architecture now matches the actual runtime

## Final Anatomy

As of this surgery, the runtime shape is:

### Postgres

Owns:

- thread snapshots
- owner leases
- processed command dedupe
- normalized item history
- response raw storage and response-thread links
- spawn groups
- spawn child results
- thread-document links

### JetStream `THREAD_CMD`

Owns:

- durable command transport
- dispatch subjects
- owner-routed worker subjects

### JetStream `THREAD_HISTORY`

Owns:

- durable `client.response.create` checkpoints
- raw socket history for event inspection
- recovery replay input

### JetStream `THREAD_EVENTS`

Owns:

- live websocket fanout
- item delta notifications
- thread snapshots for connected clients
- live event deltas that matter to the UI

### Blob Storage

Owns:

- prepared input artifacts
- document assets
- image bytes
- other large source artifacts

## What We Explicitly Refused To Do

We did not:

- keep a Redis fallback path
- preserve old runtime branches for legacy threads
- keep `stream_id` payloads alive for compatibility
- leave docs describing an architecture we had already abandoned

This was intentional. The app is a proof-of-concept, the builder count is one, and the design direction was already clear.

## Post-Op Checks

Representative checks run during and after surgery included:

- `go test ./internal/worker ./internal/httpserver ./internal/natsbootstrap ./internal/threadhistory`
- `go test ./internal/httpserver ./internal/wsserver ./internal/worker ./internal/platform ./internal/postgresstore ./internal/natsbootstrap ./internal/threadhistory ./cmd/wsserver`
- `go test ./internal/threadstore`
- `go test ./internal/threadcmd ./internal/worker`
- `npm run typecheck` in `frontend`
- `rg -n --hidden --glob '!**/.git/**' 'redis|Redis|REDIS' .`
- `rg -n --hidden --glob '!**/.git/**' 'stream_id|first_stream_id|last_stream_id|\\bStreamID\\b' .`

Those last two coming back empty was the sound of the ventilator unplugging from the wrong patient.

## Pathology Report

Removed from the codebase:

- Redis client bootstrap
- Redis runtime dependency
- Redis compose service
- Redis config/env vars
- Redis-backed store implementation
- Redis-backed event history
- Redis-backed recovery checkpoint reads
- Redis-backed ownership and dedupe
- Redis-era cursor compatibility baggage

Cause of death:

- deliberate surgical intervention

## Remaining Future Work

The Redis work itself is done. Future work is normal product/runtime improvement, not unfinished migration cleanup.

Likely next quality cuts:

- make child completion results richer than `assistant_text` without depending on item scraping
- keep tightening prepared-input and document boundaries
- continue simplifying runtime contracts now that the storage split is honest

The patient survived.

Redis did not.
