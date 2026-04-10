# Recovery

## Overview

Recovery is where the sticky-worker WebSocket design becomes real. The system needs to survive:

- Socket expiry
- Socket disconnects
- Worker crashes
- Redelivered commands
- Partial child completion
- Parent threads waiting at barriers while workers change

The v1 rule is simple:

- Redis is the runtime truth
- The thread actor is recreated from Redis
- OpenAI sockets are replaceable
- Thread ownership is not

If ownership is uncertain, no one writes to the thread until ownership is re-established.

## Failure Classes

### Planned Rotation

- The socket is healthy
- The actor rotates before hard expiry
- No ownership change is needed

### Transient Disconnect

- The worker is still alive
- The lease is still valid
- The socket must be recreated

### Worker Death

- The lease expires
- The thread becomes `orphaned`
- A different worker must adopt and reconstruct the actor

### In-Flight Uncertainty

- A response was active when the socket failed
- The worker does not know whether the model reached a terminal response state
- The thread must enter `reconciling`

## Planned Socket Rotation

Recommended v1 approach:

- Schedule rotation several minutes before hard expiry
- Rotate only when the thread is not currently `running`
- If the thread is `running` near expiry, let the response finish if possible
- If the socket expires during `running`, fall into reconciliation

Rotation steps:

1. Receive `thread.rotate_socket`
2. Validate current ownership and `socket_generation`
3. Open a fresh socket
4. Increment `socket_generation`
5. Update `socket_expires_at`
6. Swap the actor's live socket reference
7. Close the old socket

## Transient Disconnect Recovery

When the socket drops but the worker still owns the thread:

1. Stop sending new OpenAI writes
2. Mark thread `reconciling`
3. Open a new socket
4. Rehydrate thread state from Redis
5. If there was no active response, move back to `ready`
6. If there was an active response, continue reconciliation before issuing new work

## In-Flight Reconciliation

This is the hardest case.

When the socket dies while `active_response_id` is non-null:

1. Mark the thread `reconciling`
2. Preserve the current `active_response_id`
3. Freeze new resume commands for that thread
4. Rebuild state from:
   - `thread:{id}:meta`
   - `thread:{id}:items`
   - `thread:{id}:events`
   - `response:{active_response_id}:raw` if already persisted
5. Determine whether the response is already terminal and durable
6. If terminal and durable:
   - Clear `active_response_id`
   - Update `last_response_id`
   - Move to the correct next state
7. If not terminal:
   - Keep the thread blocked in `reconciling`
   - Run the platform's chosen repair procedure

### Repair Procedure

The repair procedure should be explicit, not implicit retry.

Recommended v1 procedure:

1. Treat the last fully persisted response as the safe checkpoint
2. Reconstruct the pending input items that were meant to continue from that checkpoint
3. Resume from the safe checkpoint on the fresh socket
4. Mark the old in-flight attempt as abandoned in local runtime state

Important implication:

- Any recovery from a mid-stream disconnect may replay work from the last stable checkpoint
- Tool handlers must therefore be designed for idempotent external side effects

## Worker Death and Thread Adoption

When a worker dies:

1. Lease renewal stops
2. `thread_owner:{thread_id}.lease_until` expires logically
3. The thread becomes eligible for adoption
4. A new worker claims ownership atomically
5. The new worker receives `thread.adopt`
6. The new worker rebuilds the actor from Redis

Adoption steps:

1. Claim ownership with a higher `socket_generation`
2. Add thread id to `worker:{worker_id}:threads`
3. Open a fresh socket
4. Load thread metadata, item log, event log, and spawn state
5. Decide recovery path based on thread status:
   - `ready` -> reopen and wait
   - `waiting_tool` -> reopen and wait for tool output
   - `waiting_children` -> rebuild barrier and either wait or resume
   - `running` -> move to `reconciling`
   - `reconciling` -> continue reconciliation
6. Persist the recovered state before processing any new command

## Barrier Recovery

Parent threads waiting on children must be recoverable without the original worker.

Rebuild rules:

1. Load `spawn_group:{id}:meta`
2. Load `spawn_group:{id}:children`
3. Load `spawn_group:{id}:results`
4. Recompute whether the barrier is open or closed
5. If closed and `aggregate_submitted_at` is empty:
   - Synthesize an internal `thread.submit_tool_output`
   - Resume the parent
6. If still open:
   - Stay in `waiting_children`
   - Accept more child terminal notifications

## Child Completion Dedupe

Child notifications are naturally redelivery-prone.

The handler must no-op when:

- The child already has a terminal record in the spawn group
- The same `cmd_id` was already applied
- The barrier is already closed and the child state was already counted

## Parent Cancellation

If a parent is cancelled while waiting on children:

1. Mark the parent as cancelling
2. Publish `thread.cancel` for each non-terminal child
3. Record late child completions but do not resume the parent
4. Mark the spawn group `cancelled`
5. Mark the parent `cancelled`

## Child Failure Policy

v1 should not automatically fail the parent because one child failed.

Recommended rule:

- The barrier closes when all children are terminal
- The aggregate `function_call_output` includes both successes and failures
- The parent model decides how to proceed

This keeps the orchestration runtime simple and Responses-native.

## Command Replay Handling

Every actor should assume JetStream can redeliver commands.

Safe handling rules:

- Reject or no-op commands already recorded in `processed_cmds`
- Enforce `expected_status`
- Enforce `expected_last_response_id`
- Ignore commands whose ownership generation is stale
- Never ACK before Redis persistence succeeds

## Operational Signals

Track these from day one:

- Threads by state
- Threads by owner worker
- Socket rotations
- Orphan adoptions
- Reconciliation count
- Average child barrier close time
- Duplicate child completion count
- Command redelivery rate

## Recovery Philosophy

The system should prefer:

- Deterministic ownership
- Explicit recovery states
- Replay from safe checkpoints
- Idempotent command handling

The system should avoid:

- Hidden retries against uncertain state
- Multiple workers writing to one thread
- Queue-based thread affinity assumptions
- Parent orchestration state living only in memory

## Current Slice

The current implementation now covers the first real recovery loop:

- workers periodically sweep active thread states from Redis
- expired or unowned idle threads are re-enqueued as `thread.adopt`
- expired or uncertain in-flight threads are re-enqueued as `thread.reconcile`
- owned idle threads nearing expiry are re-enqueued as `thread.rotate_socket`
- parent threads with closed child barriers can resume from Redis barrier state after adoption
- in-flight reconciliation replays the latest persisted `client.response.create` checkpoint on a fresh socket

This is intentionally checkpoint-based rather than magical. The runtime is now opinionated that the last persisted `client.response.create` is the safe repair point for v1.
