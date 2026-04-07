# OpenAI WebSocket Runtime

## Purpose

[`internal/openaiws`](/Users/detachedhead/explorer/internal/openaiws) is the first code boundary for our Responses WebSocket runtime.

Its job is narrow on purpose:

- own the worker-side Responses socket contract
- keep connection lifecycle logic out of business handlers
- preserve sticky thread-to-socket semantics
- stay lightweight until we choose the concrete websocket transport implementation

This package is not the worker actor and not the orchestration engine. It is the transport edge for one warm OpenAI Responses socket.

## Why We Built This Ourselves

The vendored [`openai-go`](/Users/detachedhead/explorer/openai-go) SDK is useful, but the current source audit in [OPENAI_GO_EXPLORATION.md](/Users/detachedhead/explorer/OPENAI_GO_EXPLORATION.md) shows it does not yet provide the Responses WebSocket client shape we need for our worker model.

So the split is:

- `openai-go` for schemas and supported REST/SSE surfaces
- `internal/openaiws` for our worker-native Responses socket runtime

## Package Shape

Current files:

- [`internal/openaiws/config.go`](/Users/detachedhead/explorer/internal/openaiws/config.go)
- [`internal/openaiws/coder.go`](/Users/detachedhead/explorer/internal/openaiws/coder.go)
- [`internal/openaiws/event.go`](/Users/detachedhead/explorer/internal/openaiws/event.go)
- [`internal/openaiws/transport.go`](/Users/detachedhead/explorer/internal/openaiws/transport.go)
- [`internal/openaiws/session.go`](/Users/detachedhead/explorer/internal/openaiws/session.go)
- [`internal/worker/service.go`](/Users/detachedhead/explorer/internal/worker/service.go)
- [`cmd/worker/main.go`](/Users/detachedhead/explorer/cmd/worker/main.go)

## Responsibilities

### `config.go`

Defines the socket-level OpenAI config:

- API key
- responses socket URL
- organization and project headers
- dial, read, write, and heartbeat timing

### `event.go`

Defines thin wire envelopes for:

- outbound client events such as `response.create`
- inbound server events such as `response.created`, `response.output_text.delta`, and terminal events

This layer intentionally preserves raw JSON so we can store exact event payloads in Redis before we commit to a richer typed decoding strategy.

### `transport.go`

Defines the transport abstraction:

- `Dialer`
- `Conn`
- websocket close codes

This lets us choose the concrete websocket implementation later without rewriting the worker-facing session logic.

### `coder.go`

Provides the first concrete websocket adapter using `github.com/coder/websocket`.

It is responsible for:

- dialing the Responses socket URL
- applying handshake headers
- enforcing a sane websocket read limit above the library default
- mapping the concrete library connection into our internal `Conn` interface

### `session.go`

Defines the session lifecycle skeleton:

- connect
- send
- receive
- close
- socket generation tracking
- basic timestamps for read and write activity

This is the first code object that maps naturally to our planned `thread -> one warm socket` rule.

## What It Does Not Do Yet

- no concrete websocket library is wired in yet
- no ping loop yet
- no reconnect or rotation manager yet
- no Redis persistence hooks yet
- no NATS command integration yet
- no worker actor loop yet
- no typed decoding of all Responses event variants yet

That is intentional. We are defining the seam before we harden the implementation.

Update:

- the first concrete websocket adapter now exists
- the worker bootstrap now exists
- the orchestration layer still does not exist, by design

## Planned Integration

The next wiring path should be:

1. add a concrete websocket adapter under `internal/openaiws/<adapter>.go`
2. give each owned thread actor one `openaiws.Session`
3. persist raw inbound and outbound events to Redis
4. map JetStream commands to `response.create` payload generation
5. add socket rotation and orphan adoption rules from [RECOVERY.md](/Users/detachedhead/explorer/RECOVERY.md)
6. subscribe the worker to JetStream command subjects and start the actor mailbox loop

## Design Rule

Do not let the worker talk directly in terms of an arbitrary websocket library.

The worker should depend on `openaiws.Session` and the wire-event helpers. That keeps the runtime architecture stable even if we swap transport adapters or selectively adopt upstream SDK support later.
