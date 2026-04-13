# OpenAI WebSocket Runtime

## Shape

The runtime stays close to the Responses WebSocket protocol.

Workers own:

- socket connect and reconnect
- `response.create`
- tool-output continuation
- response lifecycle tracking
- raw event capture
- recovery from the last durable checkpoint

## Persistence Split

- Postgres stores durable thread state, normalized items, and raw responses.
- `THREAD_HISTORY` stores the latest `client.response.create` checkpoint plus raw socket history.
- `THREAD_EVENTS` carries live fanout to websocket clients.

## Event Handling Rule

- Keep exact raw JSON for socket history.
- Persist semantic read models separately.
- Treat `.delta` events as live transport noise unless a higher layer needs them.
- Keep `client.response.create` durable in history so recovery can replay the last send shape.

## Recovery Rule

When a worker adopts or reconciles a thread it should:

1. load the thread snapshot from Postgres
2. load the latest `client.response.create` checkpoint from `THREAD_HISTORY`
3. inspect the persisted response state
4. decide whether to resume, reconcile, rotate, or fail the thread

The worker stays responsible for OpenAI interaction; document and image preparation happen elsewhere.
