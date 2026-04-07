# Messaging

## Overview

NATS + JetStream is the backend control plane.

Use JetStream for:

- Thread start
- Thread resume
- Tool output submission
- Child completion notifications
- Cancellation
- Socket rotation
- Thread adoption and reconciliation

Use plain Core NATS later for:

- Heartbeats
- Internal observability fanout
- UI-facing streaming if needed

v1 planning focuses only on the durable command plane.

## Subject Layout

### Dispatcher Subjects

- `agent.dispatch.thread.start`
- `agent.dispatch.thread.adopt`

Use dispatcher subjects only when a thread has no current owner or must be reassigned.

### Worker Command Subjects

- `agent.worker.<worker_id>.cmd.thread.start`
- `agent.worker.<worker_id>.cmd.thread.resume`
- `agent.worker.<worker_id>.cmd.thread.submit_tool_output`
- `agent.worker.<worker_id>.cmd.thread.child_completed`
- `agent.worker.<worker_id>.cmd.thread.child_failed`
- `agent.worker.<worker_id>.cmd.thread.cancel`
- `agent.worker.<worker_id>.cmd.thread.rotate_socket`
- `agent.worker.<worker_id>.cmd.thread.reconcile`
- `agent.worker.<worker_id>.cmd.thread.adopt`

### Recommended Stream

- Stream name: `AGENT_CMD`
- Stream subjects:
  - `agent.dispatch.>`
  - `agent.worker.*.cmd.>`

## Routing Rules

### New Threads

The API service creates the thread record, then publishes:

- `agent.dispatch.thread.start`

A placement component or any worker listening for unowned work may claim the thread and re-publish to the selected worker subject.

### Existing Threads

The API service reads `thread_owner:{thread_id}` from Redis.

- If the owner lease is valid, publish directly to `agent.worker.<worker_id>.cmd.<kind>`
- If the owner is missing or stale, publish `agent.dispatch.thread.adopt`

### Child Notifications

Child notifications never go through random load balancing.

- Resolve the parent owner in Redis
- Publish directly to `agent.worker.<parent_worker_id>.cmd.thread.child_completed`
- Or `agent.worker.<parent_worker_id>.cmd.thread.child_failed`

## Command Envelope

Every JetStream command should use one stable envelope:

```json
{
  "cmd_id": "cmd_01HV...",
  "kind": "thread.resume",
  "thread_id": "thread_123",
  "root_thread_id": "thread_root_123",
  "causation_id": "resp_abc",
  "correlation_id": "req_789",
  "expected_status": "waiting_tool",
  "expected_last_response_id": "resp_prev_456",
  "expected_socket_generation": 7,
  "attempt": 1,
  "created_at": "2026-03-13T18:30:00Z",
  "body": {}
}
```

Field meanings:

- `cmd_id`
  - Global unique id for dedupe
- `kind`
  - The command type handled by the actor
- `thread_id`
  - The target thread
- `root_thread_id`
  - Stable lineage root for tracing
- `causation_id`
  - The event or response that caused this command
- `correlation_id`
  - Request or trace id spanning multiple commands
- `expected_status`
  - Actor precondition for safe application
- `expected_last_response_id`
  - Optional guard to prevent stale resumes
- `expected_socket_generation`
  - Ownership-generation guard to prevent stale workers from applying commands
- `attempt`
  - Redelivery attempt counter
- `body`
  - Kind-specific payload

## Command Types

### `thread.start`

Used for brand new threads and freshly created child threads.

```json
{
  "initial_input": [
    {
      "type": "message",
      "role": "user",
      "content": [{ "type": "input_text", "text": "Analyze this document." }]
    }
  ],
  "model": "gpt-5.4",
  "instructions": "System prompt here.",
  "metadata": {
    "origin": "api"
  },
  "store": true
}
```

Planned warm-branch extension:

```json
{
  "initial_input": [
    {
      "type": "message",
      "role": "user",
      "content": [{ "type": "input_text", "text": "Investigate billing flows only." }]
    }
  ],
  "model": "gpt-5.4",
  "instructions": "You are branch 2. Focus only on billing evidence.",
  "metadata": {
    "origin": "parent_spawn",
    "spawn_mode": "warm_branch"
  },
  "store": true,
  "previous_response_id": "resp_parent_plan_123"
}
```

Notes:

- `warm_branch` uses `previous_response_id` to clone lineage, not sockets
- `store=true` is required for warm branching
- the parent branch point must already be stably persisted before children are started

### `thread.resume`

Used to continue a ready thread with new user input or internal input items.

```json
{
  "input_items": [
    {
      "type": "message",
      "role": "user",
      "content": [{ "type": "input_text", "text": "Continue." }]
    }
  ]
}
```

### `thread.submit_tool_output`

Used when a normal tool result returns to the owner worker.

```json
{
  "call_id": "call_123",
  "output_item": {
    "type": "function_call_output",
    "call_id": "call_123",
    "output": {
      "status": "ok"
    }
  }
}
```

### `thread.child_completed`

Used when a child reaches a terminal success state.

```json
{
  "spawn_group_id": "sg_123",
  "child_thread_id": "thread_child_1",
  "child_response_id": "resp_child_1",
  "result_ref": "blob://child-results/1.json",
  "summary_ref": "blob://child-results/1-summary.json"
}
```

### `thread.child_failed`

Used when a child fails or is cancelled.

```json
{
  "spawn_group_id": "sg_123",
  "child_thread_id": "thread_child_2",
  "child_response_id": "resp_child_2",
  "status": "failed",
  "error_ref": "blob://child-results/2-error.json"
}
```

### `thread.cancel`

Used to cancel a root or child thread.

```json
{
  "reason": "user_requested_cancel",
  "cascade": true
}
```

### `thread.rotate_socket`

Used to proactively rotate a socket before hard expiry.

```json
{
  "reason": "pre_expiry_rotation",
  "scheduled_at": "2026-03-13T19:20:00Z"
}
```

### `thread.reconcile`

Used when the actor has uncertain in-flight state after socket loss or partial persistence.

```json
{
  "reason": "socket_disconnect_during_running",
  "active_response_id": "resp_live_123",
  "socket_generation": 7
}
```

### `thread.adopt`

Used when a thread is orphaned and a new worker must recover it.

```json
{
  "previous_worker_id": "worker_old",
  "required_generation": 7
}
```

## Delivery Rules

- Every command must be durable
- Every command handler must be idempotent
- The actor must persist state before ACKing the JetStream message
- The actor must reject stale commands whose preconditions fail
- Commands for one thread must be executed serially by the thread actor

## Idempotency Rules

Use all of the following:

- `cmd_id` stored in `thread:{thread_id}:processed_cmds`
- JetStream message de-duplication via `Nats-Msg-Id`
- Guard checks using `expected_status`
- Guard checks using `expected_last_response_id`
- Guard checks using `expected_socket_generation`

A command is a no-op if:

- `cmd_id` was already applied
- The thread is already terminal
- The command refers to a stale ownership generation
- The `spawn_group` already recorded that child result

## Ownership and Routing Algorithm

### Claim

1. Find candidate worker
2. Execute Redis ownership-claim script
3. On success, publish to `agent.worker.<worker_id>.cmd.thread.start`
4. On failure, retry lookup or dispatch

### Direct Resume

1. Read owner record from Redis
2. Validate lease freshness
3. Publish directly to the owner subject
4. If lease is stale, publish `agent.dispatch.thread.adopt`

### Adoption

1. A worker claims the orphaned thread via Redis
2. The worker receives `thread.adopt`
3. The worker reconstructs actor state from Redis
4. The worker opens a fresh socket and resumes the thread's recovery path

## Mailbox Discipline

The thread actor mailbox is the real sequencing guarantee.

Do not rely on:

- Queue group ordering
- Publish timing
- External tool completion order

Do rely on:

- Single owner per thread
- Single actor mailbox per thread
- Durable command redelivery
- Redis precondition checks

## Example Sequence

1. Parent emits `spawn_subagents`
2. Parent worker writes the tool call item to Redis
3. Parent worker creates `spawn_group`
4. Parent worker publishes 20 `thread.start` commands for child threads
5. Child workers run
6. Child workers publish `thread.child_completed` back to the parent owner
7. Parent owner closes the barrier
8. Parent owner publishes an internal `thread.submit_tool_output` to itself
9. Parent resumes on the same thread actor
