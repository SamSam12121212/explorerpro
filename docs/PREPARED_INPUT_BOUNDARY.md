# Prepared Input Boundary

## Summary

The worker should own execution, not source-specific payload assembly.

Prepared input exists to keep document and other heavy source expansion outside worker core while preserving worker-owned OpenAI sockets.

## Rule

Durable thread state should stay semantic and compact:

- user text
- tool outputs
- attached document IDs
- image refs
- repo refs
- metadata

Large transport-shaped payloads belong in prepared input artifacts referenced by `prepared_input_ref`, not embedded into thread snapshots or durable history.

## Worker Responsibility

After this boundary lands, the worker should only:

- load `prepared_input_ref` when present
- merge prepared input with thread-owned fields such as model, tools, reasoning, and previous response state
- lower any generic blob-backed refs at the OpenAI send boundary
- persist the final recoverable checkpoint and continue the thread

## Why

- keeps worker core focused on thread execution
- keeps document-specific expansion out of actor code
- avoids stuffing large prepared payloads into Postgres rows or JetStream history
- lets new source types reuse one handoff model
