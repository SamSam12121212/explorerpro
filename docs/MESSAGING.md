# Messaging

## Command Plane

Durable commands live in JetStream stream `AGENT_CMD`.

Subjects:

- `agent.dispatch.thread.start`
- `agent.dispatch.thread.adopt`
- `agent.dispatch.thread.<kind>` for unowned or expired threads
- `agent.worker.<worker_id>.cmd.<kind>` for direct owner routing

Each command carries:

- `cmd_id`
- `thread_id`
- `root_thread_id`
- `kind`
- optional expected status, last response, and socket generation guards

## Routing Rule

1. Load the owner row from Postgres.
2. If the lease is live, publish directly to the owner worker subject.
3. Otherwise publish to the dispatch subject.

## Event Planes

### `THREAD_EVENTS`

Transient ws handoff queue for:

- `thread.snapshot`
- `thread.item.appended`
- `client.response.create`
- raw OpenAI events needed by the live UI, including deltas

Rules:

- worker publishes once
- wsserver is the only consumer
- wsserver fans out to connected sockets
- after wsserver ack, the message is gone

### `THREAD_HISTORY`

Durable append-only history for:

- the latest recoverable `client.response.create` checkpoint
- non-delta raw socket events in sequence order
- recovery inspection and event-history APIs

## Worker Model

- Dispatch consumers compete for unowned work.
- Each worker also has a private durable consumer for direct-routed commands.
- A thread actor processes commands sequentially and is the only socket writer for that thread.
- Child-thread completion is published back into the same command plane, not through a side channel.
