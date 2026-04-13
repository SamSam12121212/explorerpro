# Recovery

## Durable Sources

Recovery uses two durable sources:

- Postgres for thread state, ownership, items, responses, and spawn barriers
- `THREAD_HISTORY` for the latest `client.response.create` checkpoint and raw socket history

## Thread Recovery Flow

1. Find threads whose status requires adoption or reconciliation.
2. Claim or rotate ownership in Postgres.
3. Load the current thread snapshot from Postgres.
4. Load the latest recoverable `client.response.create` payload from `THREAD_HISTORY`.
5. Inspect persisted response state and spawn state.
6. Resume, reconcile, rotate the socket, or fail the thread based on that durable picture.

## Rules

- Do not acknowledge a command before its durable effects are written.
- Ownership changes must carry the expected socket generation.
- Recovery should rebuild from durable state, not from in-memory assumptions.
- Raw socket history is for exact replay context; normalized thread state stays in Postgres.
