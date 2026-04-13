# Runtime Storage Refactor

The runtime storage split is now complete.

Current model:

- Postgres owns durable runtime state and coordination.
- `THREAD_HISTORY` owns durable raw socket history and recoverable `client.response.create` checkpoints.
- `THREAD_EVENTS` owns live browser fanout.
- Blob storage owns large source artifacts.

Follow-on work should optimize or simplify within that model rather than reintroducing mixed storage paths.
