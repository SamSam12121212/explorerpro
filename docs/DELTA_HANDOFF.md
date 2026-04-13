# Delta Handoff

Date: April 13, 2026

## Contract

OpenAI deltas are not durable product history.

They exist only to move live thread events from the worker to wsserver so active browser tabs can render them immediately.

Current rule:

- worker publishes live thread events to JetStream `THREAD_EVENTS`
- wsserver is the only JetStream consumer
- wsserver broadcasts those events to connected sockets for the thread
- wsserver acks the JetStream message after fanout
- once acked, the message is gone

No replay. No browser ack. No delta history.

If a page misses deltas, it catches up later from snapshot/item reads or polling.

## What Goes Where

### `THREAD_EVENTS`

Transient live handoff queue for:

- `client.response.create`
- `thread.snapshot`
- `thread.item.appended`
- raw OpenAI socket events needed by the live UI, including deltas

### `THREAD_HISTORY`

Durable history for:

- recoverable `client.response.create` checkpoints
- non-delta raw socket events
- event-history inspection

### Postgres

Durable read model and coordination.

Not delta storage.

## What Changed

- `THREAD_EVENTS` moved to `WorkQueuePolicy`
- wsserver stopped creating one JetStream subscription per browser connection
- wsserver now owns a single consumer and fans out in-process
- worker live-event publish stopped being best-effort
- deltas stopped being written into `THREAD_HISTORY`

## What We Explicitly Do Not Keep

- per-tab JetStream consumers
- delta replay
- delta persistence in Postgres
- delta persistence in `THREAD_HISTORY`
- browser-confirmed delivery

## Relevant Files

- [internal/natsbootstrap/threadevents.go](/Users/detachedhead/explorer/internal/natsbootstrap/threadevents.go)
- [internal/threadevents/threadevents.go](/Users/detachedhead/explorer/internal/threadevents/threadevents.go)
- [internal/worker/service.go](/Users/detachedhead/explorer/internal/worker/service.go)
- [internal/worker/actor.go](/Users/detachedhead/explorer/internal/worker/actor.go)
- [internal/wsserver/server.go](/Users/detachedhead/explorer/internal/wsserver/server.go)
- [internal/wsserver/hub.go](/Users/detachedhead/explorer/internal/wsserver/hub.go)
- [internal/wsserver/client.go](/Users/detachedhead/explorer/internal/wsserver/client.go)
