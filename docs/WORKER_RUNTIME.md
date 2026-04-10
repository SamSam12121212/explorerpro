# Worker Runtime

## Current Slice

The worker now has the first executable backend backbone:

- JetStream stream bootstrap for `AGENT_CMD`
- dispatch subscription on `agent.dispatch.thread.*`
- direct worker subscription on `agent.worker.<worker_id>.cmd.>`
- one actor mailbox per thread
- Redis-backed thread metadata, ownership, item log, event log, and response raw storage
- OpenAI Responses WebSocket session creation and command execution for the first happy paths
- background lease renewal for owned threads
- idle socket heartbeat maintenance for non-running threads
- first parent-side child-result fan-in for spawn groups
- spawn-group creation and child thread fan-out
- periodic orphan/adoption recovery sweep
- checkpoint-based `thread.reconcile` replay for uncertain in-flight responses
- planned idle-socket rotation before hard expiry

Main implementation files:

- [`cmd/worker/main.go`](/Users/detachedhead/explorer/cmd/worker/main.go)
- [`internal/worker/service.go`](/Users/detachedhead/explorer/internal/worker/service.go)
- [`internal/worker/actor.go`](/Users/detachedhead/explorer/internal/worker/actor.go)
- [`internal/threadstore/store.go`](/Users/detachedhead/explorer/internal/threadstore/store.go)
- [`internal/agentcmd/command.go`](/Users/detachedhead/explorer/internal/agentcmd/command.go)
- [`internal/natsbootstrap/agentcmd.go`](/Users/detachedhead/explorer/internal/natsbootstrap/agentcmd.go)

## Commands Implemented

The actor currently handles:

- `thread.start`
- `thread.resume`
- `thread.submit_tool_output`
- `thread.child_completed`
- `thread.child_failed`
- `thread.adopt`
- `thread.rotate_socket`
- `thread.reconcile`
- `thread.cancel`

For `thread.start` and `thread.resume`, the worker currently:

1. ensures the thread exists in Redis
2. claims sticky ownership
3. opens or reuses a Responses WebSocket
4. stores raw input items in Redis
5. sends a `response.create` event
6. stores raw inbound OpenAI events and raw response payloads
7. updates thread status and `last_response_id`

For `thread.submit_tool_output`, the worker currently:

1. loads the thread in `waiting_tool`
2. reclaims or confirms ownership
3. appends the `function_call_output` item to the thread input log
4. sends a new `response.create` with `previous_response_id`
5. continues the thread on the same actor and socket

For `thread.child_completed` and `thread.child_failed`, the worker currently:

1. loads the parent thread and its active spawn group
2. records the child terminal result in Redis
3. recomputes barrier counts
4. if the barrier is still open, keeps the parent in `waiting_children`
5. if the barrier closes, synthesizes one aggregate `function_call_output`
6. resumes the parent thread through the normal Responses continuation path

For `spawn_subagents`, the worker currently:

1. detects the `function_call` output item during normal Responses streaming
2. decodes the child thread plan from the tool arguments
3. creates a `spawn_group` in Redis
4. creates child thread records with parent linkage and `depth=1`
5. publishes one `thread.start` command per child to JetStream dispatch
6. moves the parent into `waiting_children`

For `thread.adopt` and `thread.reconcile`, the worker currently:

1. claims ownership for adoptable or expired threads
2. recreates the thread actor on a fresh socket
3. restores idle threads back to `ready`, `waiting_tool`, or `waiting_children`
4. rebuilds parent barrier state from Redis when a parent was waiting on children
5. replays the latest persisted `client.response.create` event when a thread was mid-flight and uncertain

For `thread.rotate_socket`, the worker currently:

1. only rotates threads in idle-safe states
2. opens a fresh socket before changing ownership generation
3. increments `socket_generation` atomically in Redis on the same worker
4. swaps the live session reference
5. restarts lease renewal and idle maintenance on the new socket

## Important Current Behavior

- Commands for one thread are serialized through a single actor mailbox.
- Ownership is persisted in Redis before command ACK.
- Ownership is renewed in the background while the actor is alive.
- Idle owned sockets are heartbeat-maintained while the thread is `ready`, `waiting_tool`, or `waiting_children`.
- Raw OpenAI wire events are stored before higher-level orchestration decisions.
- `response.output_item.done` items are persisted to the thread item log.
- A completed response with a `function_call` output currently lands the thread in `waiting_tool`.
- Parent barrier closure resumes through the same `function_call_output` continuation mechanism as any other tool result.
- Child threads are created by the parent worker, not by the API tier.
- Child terminal state is published back to the parent owner through the same command plane.
- Workers now run a periodic recovery sweep over active thread states.
- Recovery prefers barrier reconstruction first, then request replay from the last persisted `client.response.create` checkpoint.
- Workers now schedule proactive rotation for owned idle threads approaching `socket_expires_at`.
- The worker runtime does not extract PDFs or render page assets. Prepared document manifests and page refs are external inputs.

## Honest Gaps

This slice is deliberately real but still narrow.

Not done yet:

- no richer retry policy around failed reconciliations yet
- no metrics/ops counters around orphan adoptions and replay count yet
- no explicit handling yet for a socket expiring mid-rotation attempt

One especially important note:

- the worker now owns both sides of the subagent barrier: child creation and parent barrier closure. The API still stays intentionally thin and command-oriented.

## Immediate Next Step

The next implementation slice should be:

1. richer retry / dead-letter handling for failed reconciliations
2. metrics and operational counters for adoption / replay / rotation
3. explicit mid-rotation failure handling
4. `thread.submit_tool_output` batching / multi-tool turns if needed
