# Messaging

## Command Plane

Durable commands live in JetStream stream `THREAD_CMD`.

Subjects:

- `thread.dispatch.start`
- `thread.dispatch.adopt`
- `thread.dispatch.<kind_suffix>` for unowned or expired threads
- `thread.worker.<worker_id>.cmd.<kind_suffix>` for direct owner routing

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

## Event Plane

### `THREAD_EVENTS`

Transient ws handoff queue. The worker publishes raw OpenAI Responses API events with an additive identity envelope:

```json
{
  "thread_id":        <leaf thread>,
  "root_thread_id":   <user-facing root>,
  "parent_thread_id": <parent, 0 for roots>,
  "type":             "<openai event type>",
  ...original event fields...
}
```

Published events:

- `client.response.create` — the response.create command sent to OpenAI
- raw OpenAI server events for the thread — all non-delta events, plus `.delta` events for root threads

Not published:

- `response.reasoning_text.delta` and `response.reasoning_summary_text.delta` — dropped at the worker (latency)
- any `.delta` event when `meta.ParentThreadID > 0` — child thread delta suppression

Rules:

- worker publishes once, identity tuple already injected
- wsserver fans out to all connected clients
- after fanout, the message is gone

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
