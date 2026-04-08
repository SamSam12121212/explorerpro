# ExplorerPRO

An agent runtime built on the OpenAI Responses API with [WebSocket mode](https://developers.openai.com/api/docs/guides/websocket-mode). Not considered production-ready and has loads of bugs.

OpenAI-native by design with no provider abstraction layer yet and not planned atm.

![Screenshot](docs/Screenshot%202026-04-07%20at%2000.15.44.png)

## Aims

- Thread execution with persistent worker-owned WebSockets and response continuity via `previous_response_id`
- Parallel subagent fan-out — parent spawns N children on separate sockets, results regroup through a barrier
- Warm branching — children fork from a parent's accumulated context instead of starting cold
- Sticky worker ownership tracked in Redis, commands route directly
- Durable command transport via NATS + JetStream with idempotent delivery and replay safety
- Recovery through socket rotation, orphan adoption, and checkpoint-based reconciliation
- Blob-backed image inputs with base64 only at the OpenAI boundary
- Postgres read model for durable history, Redis as the hot coordination plane

## Stack

Go, NATS + JetStream, Redis, Postgres, OpenAI Responses API ([WebSocket mode](https://developers.openai.com/api/docs/guides/websocket-mode))

## Running

```bash
cp .env.example .env
# set OPENAI_API_KEY

docker compose up -d --build
```

API on `localhost:8080`, frontend on `localhost:5173`.

Or run Go services on the host:

```bash
docker compose up -d nats redis postgres
make run-api
OPENAI_API_KEY=... make run-worker
```

