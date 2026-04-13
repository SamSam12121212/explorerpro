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

- live websocket fanout
- thread snapshots for connected clients
- item append notifications
- useful live event deltas

## Worker Write Pattern

For each important transition the worker:

1. updates the Postgres read model
2. appends durable raw history when needed
3. publishes live fanout for connected clients

This keeps read models compact, history exact, and the UI responsive.
