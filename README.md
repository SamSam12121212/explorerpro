A runtime built on the OpenAI Responses API with [WebSocket mode](https://developers.openai.com/api/docs/guides/websocket-mode), Go, NATS + JetStream and Postgres. Not considered production-ready and has loads of bugs.

No provider abstraction layer which has not yet been planned. Azure OpenAI also does not work.

In the current setup, your data is not processed under ZDR settings, so potentially everything is being logged and stored. I am working this way to start as I want to see the different possibilites that open up with `previous_response_id` and warm sockets and do not want to be constrained.

![Screenshot](docs/image.png)

## Notes 15th April 2026
- Main priority is making the core worker stable and oblivious to what the rest of the application is doing. Its job is to manage the lifecycle of OpenAI websockets, thread operations, and then storing events received either in JetStream or Postgres. Consumers of the events should not really know about the worker either, they should just know what events to get it working, and receiving its output.
- IDs throughout the app have been switched from uuid like strings to integers for now. It makes it easier during early development to review logs, paths, etc. If in the future a requirement for non-db generated ids, we can switch back. But whilst making the core worker stable, its easier to keep ids as integers.
- React Compiler is being used and there are strict linter enforcement rules enabled for it.
- We are staying as close to the Responses API event shape throughout the app frontend, backend, storage as it is easier to reason about. Some AI agents will try and veer you away to an abstracton of it, but if so, refuse.
- Sometimes AI agents will recommend npm packages that have not been maintained for years, so always check that. If it does recommend an old popular project, which is stale, look for an active popular fork, or something else entirely.

## Aims

- Thread execution with persistent worker-owned WebSockets and response continuity via `previous_response_id`
- Parallel child thread fan-out where parent spawns N children on separate sockets, results regroup through a barrier
- Warm branching where children fork from a parent's accumulated context instead of starting cold
- Sticky worker ownership tracked in Postgres, commands route directly
- Durable command transport via NATS + JetStream with idempotent delivery and replay safety
- Postgres-backed runtime state and durable history

## Running

```bash
cp .env.example .env
# set OPENAI_API_KEY

docker compose up -d --build
```
