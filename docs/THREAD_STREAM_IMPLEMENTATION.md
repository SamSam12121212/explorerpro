# Thread Stream Implementation

## Storage Split

The runtime uses three storage shapes because each one matches a different workload.

### Postgres

Owns:

- thread snapshots
- ownership leases
- processed command dedupe
- item history
- raw responses and response-thread links
- spawn groups and child terminal results

### `THREAD_HISTORY`

Owns:

- durable `client.response.create` checkpoints
- raw socket event history in order
- event-history API reads
- recovery replay context

### `THREAD_EVENTS`

Owns:

- live websocket fanout to connected browser clients
- raw OpenAI Responses API events with additive identity tuple

Not in `THREAD_EVENTS`:

- thread state snapshots (served via HTTP `GET /threads/:id`)
- item catch-up batches (served via HTTP `GET /threads/:id/items`)
- reasoning deltas (dropped at the worker)
- child thread deltas (suppressed at the worker)

## Worker Write Pattern

For each important transition the worker:

1. updates the Postgres read model
2. appends durable raw history when needed
3. publishes live raw events to connected clients

This keeps read models compact, history exact, and the UI responsive.

## WS Message Shape

Every live message on the WS is a native OpenAI Responses API event, prefixed with the identity tuple:

```json
{
  "thread_id":        7,
  "root_thread_id":   3,
  "parent_thread_id": 3,
  "type":             "response.output_item.done",
  ...
}
```

The sole exception is `thread.heartbeat`, a transport-level ping.

## Frontend Catch-up

The browser uses HTTP for all catch-up and initial state:

- `GET /threads/:id` — thread metadata (status, model, owner, spawn groups, attached documents)
- `GET /threads/:id/items` — full item history

WS is for inflight events only. On reconnect the frontend re-fetches via HTTP.
