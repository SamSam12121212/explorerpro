# openai-go Exploration

## Goal

Evaluate the vendored [`openai-go`](/Users/detachedhead/explorer/openai-go) repository for one specific question:

- Does it currently support the new Responses WebSocket mode strongly enough for our backend to rely on it?

## Snapshot

- Vendored repo path: [`openai-go`](/Users/detachedhead/explorer/openai-go)
- Current checked out branch: `main`
- Current HEAD at time of review: `b6d8afc5fcd43d7c2e4630c32d001ce4d7f78037`
- Latest changelog entry in the vendored repo: `3.27.0 (2026-03-13)`

Evidence:

- [`openai-go/CHANGELOG.md`](/Users/detachedhead/explorer/openai-go/CHANGELOG.md#L3)

## Short Answer

No, not for the transport layer we want.

The vendored SDK clearly supports:

- Responses over normal HTTP
- Responses streaming over SSE
- Realtime REST resources such as client secret creation and call control
- Generated types for many Responses and Realtime objects

The vendored SDK does not currently appear to ship:

- A native client for Responses WebSocket mode
- A WebSocket dialer/connection manager for `/v1/responses`
- A worker-oriented event loop for sending and receiving Responses WebSocket events

## What The Code Actually Shows

### 1. Responses support is HTTP + SSE

The generated `ResponseService` exposes `New`, `NewStreaming`, `Get`, `GetStreaming`, `Delete`, `Cancel`, and `Compact`.

Most importantly, `NewStreaming` is implemented by setting `"stream": true` on a normal request and decoding the returned HTTP response with `packages/ssestream`.

Evidence:

- [`openai-go/responses/response.go`](/Users/detachedhead/explorer/openai-go/responses/response.go#L61)
- [`openai-go/responses/response.go`](/Users/detachedhead/explorer/openai-go/responses/response.go#L79)

Why this matters:

- This is SSE over HTTP, not a persistent bidirectional Responses socket
- It is useful, but it is not the same execution model as WebSocket mode

### 2. The public README documents SSE streaming, not Responses WebSocket mode

The README includes a `Streaming responses` example using `client.Responses.NewStreaming(...)`.

I did not find a README example for:

- opening a Responses WebSocket
- sending `response.create` events over a live socket
- resuming a thread by reusing a warm Responses socket

Evidence:

- [`openai-go/README.md`](/Users/detachedhead/explorer/openai-go/README.md#L163)

### 3. The `Realtime` namespace is not the same thing as Responses WebSocket mode

The top-level client exposes both `Responses` and `Realtime`, which is good, but the `Realtime` service in this SDK is a generated REST surface.

At the service level, it currently wires up:

- `ClientSecrets`
- `Calls`

The `ClientSecretService` is about minting short-lived tokens and session config for Realtime client connections, especially WebRTC-oriented flows.

Evidence:

- [`openai-go/client.go`](/Users/detachedhead/explorer/openai-go/client.go#L51)
- [`openai-go/client.go`](/Users/detachedhead/explorer/openai-go/client.go#L108)
- [`openai-go/realtime/clientsecret.go`](/Users/detachedhead/explorer/openai-go/realtime/clientsecret.go#L39)
- [`openai-go/realtime/clientsecret.go`](/Users/detachedhead/explorer/openai-go/realtime/clientsecret.go#L50)

Why this matters:

- Realtime support in the SDK does not automatically mean there is a Responses WebSocket transport
- For our product, the critical path is the Responses socket, not Realtime client-secret issuance

### 4. There is no obvious WebSocket transport implementation in the repo

The repo search did not turn up a concrete WebSocket client implementation for Responses mode.

In particular, the exploration did not find imports or code paths for:

- `gorilla/websocket`
- `coder/websocket`
- `nhooyr`
- `DialContext`
- `wss://`
- a dedicated Responses WebSocket transport package

That absence is consistent with the rest of the repo: the Responses client is HTTP/SSE-based and the Realtime surface is generated REST.

## Comparison To Official OpenAI Docs

OpenAI's current Responses WebSocket mode guide describes a live socket model for the Responses API, including a persistent connection, socket-bound in-memory state, one in-flight response at a time, and connection rotation at 60 minutes.

Relevant docs:

- [Responses WebSocket mode guide](https://developers.openai.com/api/docs/guides/websocket-mode/)
- [Responses create reference](https://developers.openai.com/api/reference/resources/responses/methods/create)

Two practical implications matter for us:

- `instructions` are not automatically carried over with `previous_response_id`, so we need to manage them at the thread layer
- our worker model depends on an actual Responses socket, not just SSE streaming support

That means the SDK is useful, but it does not remove the need for a native worker-side Responses WebSocket client in our system.

## Recommendation

Keep the vendored SDK, but do not treat it as the execution transport for our workers.

### Use `openai-go` for

- generated request and response types
- normal REST calls where useful
- SSE flows if we ever need them for secondary tools
- keeping pace with OpenAI surface-area changes

### Own the Responses WebSocket transport ourselves

Create a thin internal package, likely something like:

- `internal/openaiws`
- or `internal/openairesponsesws`

That package should own:

- dialing the Responses WebSocket endpoint
- auth headers
- reader loop
- writer loop
- event framing
- response lifecycle tracking
- heartbeat / reconnect policy
- socket generation and ownership guards
- raw event persistence hooks into Redis

### Treat the SDK as a schema source, not a transport abstraction

This is the cleanest split for our architecture:

- `openai-go` gives us generated structs and keeps us close to the official surface
- our worker runtime owns the warm socket lifecycle, which is the real differentiator in this platform

## Why This Is Still A Good Outcome

This is not a problem. It is actually a clean architecture boundary.

If we tried to force our runtime shape around an SDK that does not yet model the Responses socket the way we need, we would end up with an awkward abstraction anyway.

Owning the socket layer ourselves gives us:

- exact control over sticky worker affinity
- exact control over live thread persistence
- exact control over tool-call pause and resume
- exact control over parent and child thread orchestration
- freedom to stay close to Responses WebSocket semantics without waiting on SDK ergonomics

## Suggested Next Step

Do not wire the project to the vendored SDK yet.

Instead:

1. Keep [`openai-go`](/Users/detachedhead/explorer/openai-go) in the repo as a reference and future dependency target.
2. Build `internal/openaiws` as the first-class worker transport.
3. Reuse `openai-go` types selectively once we decide exactly where they help more than they constrain.
4. Re-check upstream later for native Responses WebSocket support before we lock our permanent package boundaries.

## Open Questions To Revisit Later

- Do we want to import the vendored repo via a local `replace` directive, or keep it purely as a reference until our transport package settles?
- Do we want to map raw socket events into `openai-go` event unions immediately, or store raw JSON first and decode second?
- If upstream later adds a Responses WebSocket client, do we wrap it or keep our transport implementation as the stable runtime core?
