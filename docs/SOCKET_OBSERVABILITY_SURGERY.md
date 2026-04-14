# Socket Observability Surgery

Date of operation: April 14, 2026

## Chief Complaint

We could tell which thread owned a socket.

We could not tell which actual OpenAI websocket was alive right now.

That distinction mattered.

The runtime already had durable ownership coordination:

- `threads.owner_worker_id`
- `threads.socket_generation`
- `threads.socket_expires_at`
- `thread_owners.lease_until`

That is enough to keep the worker honest.

It is not enough to answer observability questions cleanly:

- which OpenAI sockets are connected right now
- when did they connect
- when did they last heartbeat
- when did they die
- did a socket die explicitly or just go stale

Clinical decision:

- keep ownership coordination where it already belongs
- add a separate ephemeral socket registry for observability
- keep the worker as the single owner of those lifecycle writes
- do not teach `internal/openaiws` about Postgres

This was a visibility surgery, not a runtime-model surgery.

## Diagnosis

There were already two useful truths in the system:

1. Ownership truth

- Postgres knew which worker owned a thread
- Postgres knew the current thread `socket_generation`
- the worker renewed that lease every minute

2. Transport truth

- `internal/openaiws.Session` already knew:
- `connected_at`
- `last_read_at`
- `last_write_at`
- connected/disconnected state

The problem was that those truths lived in different places.

Ownership was durable.
Transport liveness was in-memory only.

That meant a real OpenAI socket could be alive, dead, rotated, or replaced without leaving behind a clean monitoring row.

## Procedure Summary

We added a dedicated Postgres table:

- `openai_socket_sessions`

This table is intentionally not part of coordination.

It is an observability registry for physical OpenAI websocket sessions.

Each row records:

- the physical socket id
- thread identity
- root/parent thread placement
- worker id
- thread socket generation
- connected/disconnected state
- `connected_at`
- `last_read_at`
- `last_write_at`
- `last_heartbeat_at`
- `heartbeat_expires_at`
- `disconnected_at`
- `disconnect_reason`
- prune `expires_at`

Important model choice:

- `thread.socket_generation` remains the ownership generation
- `openai_socket_sessions.id` is the physical socket identity

That keeps coordination and observability separate on purpose.

## Incision Plan

### Incision 1: Add a dedicated socket registry table — DONE

Added migration:

- [`db/migrations/000013_openai_socket_sessions.sql`](/Users/detachedhead/explorer/db/migrations/000013_openai_socket_sessions.sql)

Design intent:

- `thread_owners` stays the coordination table
- `openai_socket_sessions` becomes the monitoring table
- disconnected rows remain briefly for inspection, then prune away

### Incision 2: Keep transport dumb and worker-owned — DONE

We deliberately did **not** push database logic into [`internal/openaiws/session.go`](/Users/detachedhead/explorer/internal/openaiws/session.go).

Why:

- `openaiws` should stay transport-only
- the worker already owns thread lifecycle
- the worker is the right place to decide when a socket is born, replaced, disconnected, or orphaned

So the worker now:

- creates the socket row after a successful websocket connect
- updates read/write/heartbeat fields during normal actor work
- marks the socket disconnected on explicit shutdown and known failure paths

### Incision 3: Heartbeat through the lease loop — DONE

The worker lease loop in [`internal/worker/actor.go`](/Users/detachedhead/explorer/internal/worker/actor.go) already renews ownership.

We now piggyback socket liveness on that same loop:

- `last_heartbeat_at`
- `heartbeat_expires_at`

This keeps the monitoring model aligned with the ownership model.

It also avoids inventing a second websocket-only heartbeat system.

That matters because idle websocket pinging is still intentionally conservative in this runtime.

### Incision 4: Mark disconnects explicitly — DONE

The worker now marks socket rows disconnected on the paths that actually matter:

- reconnect replacement
- socket rotation
- explicit idle disconnect
- terminal child resource release
- retry exhaustion cleanup
- missing-checkpoint cleanup
- lease loss
- remote error event teardown
- actor shutdown
- stream receive error

The row keeps:

- `disconnected_at`
- `disconnect_reason`
- prune `expires_at`

That means we now get both:

- live visibility
- recent-dead visibility

without keeping old rows forever.

### Incision 5: Let the service sweep the morgue — DONE

Added worker-side sweep behavior in [`internal/worker/service.go`](/Users/detachedhead/explorer/internal/worker/service.go):

- stale connected rows whose `heartbeat_expires_at` has passed are marked disconnected with `heartbeat_expired`
- disconnected rows past their prune TTL are deleted

Important effect:

- if a worker dies without a clean close path, the socket row still gets corrected
- the registry remains self-healing instead of requiring perfect shutdown discipline

### Incision 6: Keep writes modest — DONE

We did not turn every websocket delta into a Postgres write.

Instead:

- connect creates the row
- `response.create` sends refresh write activity
- non-delta inbound events refresh read activity
- lease renewals refresh heartbeat

That keeps the registry useful without turning it into a write-amplified event sink.

## Post-Op Shape

The runtime model is now:

- `thread_owners`
  - who currently owns the thread
  - which ownership generation is current
  - how long that claim remains live

- `openai_socket_sessions`
  - which physical OpenAI sockets existed
  - which one is currently live
  - when it last heartbeated
  - when and why it disconnected

That is the separation we wanted.

## Worker Doctrine

This surgery followed the standard house doctrine:

- keep the worker dedicated
- keep transport transport-only
- keep coordination and observability separate
- do not let monitoring features rewrite the runtime model

The socket registry is therefore:

- written by the worker
- corrected by the worker sweep
- queryable from Postgres
- not a second source of ownership truth

## Verification

Verification run:

- `go test ./internal/worker`
- `go test ./internal/postgresstore`
- `go test ./internal/openaiws`

Target outcome achieved:

- live OpenAI sockets now have durable rows while connected
- stale live rows self-demote after heartbeat expiry
- disconnected rows stick around briefly for monitoring
- old rows prune away automatically
- the worker remains the sole runtime authority
