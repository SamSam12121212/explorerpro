# ExplorerPRO

A runtime built on the OpenAI Responses API with [WebSocket mode](https://developers.openai.com/api/docs/guides/websocket-mode). Not considered production-ready and has loads of bugs.

OpenAI-native by design with no provider abstraction layer which has not yet been planned. Azure OpenAI also does not work.

In the current setup, your data is not processed under ZDR settings, so potentially everything is being logged and stored. I am working this way to start as I want to see the different possibilites that open up with `previous_response_id` and warm sockets and do not want to be constrained.

![Screenshot](docs/image.png)

## Aims

- Thread execution with persistent worker-owned WebSockets and response continuity via `previous_response_id`
- Parallel child thread fan-out — parent spawns N children on separate sockets, results regroup through a barrier
- Warm branching — children fork from a parent's accumulated context instead of starting cold
- Sticky worker ownership tracked in Postgres, commands route directly
- Durable command transport via NATS + JetStream with idempotent delivery and replay safety
- Recovery through socket rotation, orphan adoption, and checkpoint-based reconciliation
- Blob-backed image inputs with base64 only at the OpenAI boundary
- Postgres-backed runtime state and durable history

## Stack

Go, NATS + JetStream, Postgres, OpenAI Responses API ([WebSocket mode](https://developers.openai.com/api/docs/guides/websocket-mode))

## Running

```bash
cp .env.example .env
# set OPENAI_API_KEY

docker compose up -d --build
```

API on `localhost:8080`, frontend on `localhost:5173`.

Or run Go services on the host:

```bash
docker compose up -d nats postgres
make run-api
OPENAI_API_KEY=... make run-worker
```
