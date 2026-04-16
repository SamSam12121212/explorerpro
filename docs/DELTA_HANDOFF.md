# Delta Handoff

Date: April 16, 2026

## Contract

OpenAI deltas are not durable product history.

They exist only to move live thread events from the worker to wsserver so active browser tabs can render them immediately.

Current rule:

- worker publishes live thread events to the event relay (`THREAD_EVENTS` JetStream in the relay client)
- wsserver is the only relay consumer
- wsserver broadcasts those events to all connected clients
- once forwarded, the message is gone

No replay. No browser ack. No delta history.

If a page misses events, it catches up via HTTP (`GET /threads/:id` and `GET /threads/:id/items`).

## What Goes on the Wire

### WS event shape

Every message sent to the browser is a raw OpenAI Responses API event with three additive identity fields prepended:

```json
{
  "thread_id":        7,
  "root_thread_id":   3,
  "parent_thread_id": 3,
  "type":             "response.output_item.done",
  ...original OpenAI event fields...
}
```

The worker injects the identity tuple at publish time in `injectThreadFields`.

### `client.response.create`

The `response.create` command sent to OpenAI is also published so connected clients know a response is starting. Same identity envelope.

### `thread.heartbeat`

A transport-level heartbeat with no OpenAI API analog. Carries only `type` and `time`.

### What is dropped

- `response.reasoning_text.delta` and `response.reasoning_summary_text.delta` — dropped at the worker before publish (early `continue` in `streamUntilTerminal`). Not stored in history either.
- All other `.delta` events from child threads — suppressed at publish time when `meta.ParentThreadID > 0`.

## What We Do Not Have

- `thread.snapshot` WS messages — thread state is read via HTTP
- `thread.items.delta` WS messages — items are read via HTTP on connect/reconnect
- per-tab JetStream consumers
- delta replay
- delta persistence in Postgres
- delta persistence in `THREAD_HISTORY`
- browser-confirmed delivery

## Relevant Files

- [internal/openaiws/event.go](/Users/detachedhead/explorer/internal/openaiws/event.go)
- [internal/worker/actor.go](/Users/detachedhead/explorer/internal/worker/actor.go)
- [internal/wsserver/server.go](/Users/detachedhead/explorer/internal/wsserver/server.go)
- [internal/wsserver/hub.go](/Users/detachedhead/explorer/internal/wsserver/hub.go)
- [internal/wsserver/client.go](/Users/detachedhead/explorer/internal/wsserver/client.go)
