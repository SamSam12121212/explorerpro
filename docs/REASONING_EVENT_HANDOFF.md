# Reasoning Event Handoff

## Status

This document captures the current state of the frontend reasoning-indicator bug as of `2026-04-15`.

The app currently has a temporary event-history polling workaround in the frontend. That workaround is not desired and should likely be removed or replaced.

## Short Version

- The worker does emit `response.output_item.added.reasoning` for runs that reason.
- The global app socket path sometimes misses that live event even when the page is active, there is one client, and there is no intentional disconnect.
- When that happens, the UI may only see the persisted reasoning item later, after `response.output_item.done.reasoning`, which makes `Thinking...` appear late or not at all.
- A temporary frontend catch-up loop was added that polls `GET /threads/{id}/events`. This is what causes the repeated `events?limit=200&after=...` requests in DevTools.
- The user does not want any polling.

## What Is Proven

### 1. The worker emits the reasoning-start event

Example worker log:

```text
2026-04-15 01:34:54 "received openai event" thread_id=7 event_type=response.output_item.added.reasoning event_sequence_number=2
```

This also reproduced on other threads, including fresh threads created during investigation.

### 2. wsserver does not intentionally batch live events

Relevant code:

- `internal/worker/service.go`
- `internal/wsserver/client.go`

The worker publishes each raw event individually to `THREAD_EVENTS`.

`wsserver` wraps each raw event into a single-event `thread.events.delta` frame:

```go
return c.writeJSONLocked(map[string]any{
  "type":      "thread.events.delta",
  "thread_id": threadID,
  "events":    []any{decoded},
})
```

So this is not a deliberate server batching issue.

### 3. The browser-path websocket can miss early live events

Using the exact browser URL:

```text
ws://localhost:5173/stream/connect
```

and then posting a reasoning-heavy follow-up like:

```text
Give me 10 egg recipes grouped by difficulty. For each, include when it is worth making, a rough cook time, and one mistake to avoid. Keep it compact but thoughtful.
```

I observed runs where:

- `thread.snapshot` arrived
- `thread.items.delta` for the input message arrived
- no early `thread.events.delta` reasoning-start frame arrived
- later, `thread.items.delta` for the reasoning item appeared
- then text deltas appeared

Meanwhile, worker logs for the same thread showed the live reasoning-start event existed upstream.

### 4. The current polling is from the temporary catch-up code

This is in:

- `frontend/src/chatStore.ts`

The current implementation:

- loads `/threads/{id}/events?limit=200` on thread load
- starts a poll loop while `busy === true`
- keeps requesting `/threads/{id}/events?limit=200&after=<cursor>` every `250ms`

This is the source of the repeated requests seen in DevTools.

## Current Likely Root Cause

The most likely issue is a gap in the global app-socket delivery model.

Important context:

- The old thread-scoped socket path had a gap-closing catch-up pass in `internal/wsserver/client.go`.
- The new global socket path does not have equivalent replay/catch-up for live events.
- `THREAD_EVENTS` is currently configured as a work queue stream in `internal/natsbootstrap/threadevents.go`, so once wsserver consumes and acks an event, that event is not available for replay from that stream.

That means:

- if the global socket/client misses an early live event
- there is no built-in replay path on the websocket itself
- the only durable recovery path is the separate thread history API

## Why The Reasoning Indicator Looks Late

The worker only persists the reasoning item on `response.output_item.done.reasoning`, not on `added`.

So:

1. live reasoning-start is missed
2. no frontend `thinking=true` yet
3. reasoning finishes
4. durable reasoning item appears
5. frontend may infer `thinking=true` from that item
6. assistant message starts immediately after, so the UI can flip again almost instantly

This creates the “late thinking” effect.

## Files Touched During Investigation

### Transport / store refactor

- `frontend/src/stream/appStream.ts`
- `frontend/src/chatStore.ts`
- `frontend/src/useChat.ts`
- `frontend/src/main.tsx`
- `frontend/src/types.ts`

### App-wide socket backend

- `internal/wsserver/server.go`
- `internal/wsserver/hub.go`
- `internal/wsserver/client.go`

## Current Temporary Workaround

The current frontend state includes a polling-based event-history catch-up in:

- `frontend/src/chatStore.ts`

Specifically:

- `loadThread()` now fetches `/threads/{id}/events`
- `startEventCatchup()` starts a busy-time poll loop
- `pollEventHistory()` keeps polling every `250ms`

This was added only as a stopgap to prove the missed-live-event theory.

It is not the desired final design.

## Recommended Next Direction

Do not keep polling.

The better fix should be one of these:

### Option A. Add non-polling replay/catch-up to the global socket path

Best architectural fit.

Possible shapes:

- on global websocket connect, replay a bounded recent event window for relevant threads
- or add an explicit subscribe/catch-up message over the socket for the active thread
- or maintain replayable event retention for the app socket path instead of relying on the work-queue-only `THREAD_EVENTS` stream

This keeps the app-level socket model intact and avoids HTTP polling.

### Option B. Add a one-shot event catch-up only at known transition points

Less ideal, but much better than constant polling.

For example:

- one fetch right after `sendMessage()`
- one fetch right after `loadThread()`
- maybe one delayed follow-up fetch a short time later

This still uses HTTP, but avoids continuous polling.

### Option C. Revisit global wsserver registration / replay semantics

The global client path currently skips the thread-scoped initial snapshot/items/catch-up flow.
That may be too thin for reliable first-event delivery.

See:

- `internal/wsserver/client.go`

## Important Constraints

- Keep OpenAI / backend event shape intact.
- Do not invent frontend-only semantic event types.
- Avoid polling.
- Stay compatible with the current prototype stance in `README.md`.

## Repro Commands Used

### Direct websocket probe against browser path

```bash
node <<'EOF'
const threadId = 9;
const ws = new WebSocket('ws://localhost:5173/stream/connect');
ws.onmessage = (ev) => {
  const payload = JSON.parse(ev.data.toString());
  if (payload.thread_id !== threadId) return;
  console.log(payload);
};
EOF
```

### Reasoning-heavy resume

```bash
curl 'http://localhost:5173/threads/9/commands' \
  -H 'content-type: application/json' \
  --data-raw '{"kind":"thread.resume","body":{"input_items":[{"type":"message","role":"user","content":[{"type":"input_text","text":"Give me 10 egg recipes grouped by difficulty. For each, include when it is worth making, a rough cook time, and one mistake to avoid. Keep it compact but thoughtful."}]}],"reasoning":{"effort":"xhigh","summary":"concise"},"attached_document_ids":[]}}'
```

## Useful Code References

- `frontend/src/chatStore.ts`
- `frontend/src/stream/appStream.ts`
- `internal/wsserver/client.go`
- `internal/wsserver/hub.go`
- `internal/worker/service.go`
- `internal/worker/actor.go`
- `internal/httpserver/command_api.go`
- `internal/threadhistory/store.go`
- `internal/natsbootstrap/threadevents.go`

## Suggested First Step When Resuming

Before changing anything else:

1. Decide whether to remove the current polling catch-up immediately or keep it only long enough to compare behavior.
2. Reproduce once on a fresh thread with a reasoning-heavy prompt.
3. Focus on giving the global socket path a non-polling replay/catch-up mechanism.

