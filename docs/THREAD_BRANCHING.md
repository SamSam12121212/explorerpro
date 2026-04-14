# Thread Branching

## Status

This is a design lock-in for the next execution primitive.

It is not implemented yet.

## Decision

The runtime will support two child thread spawn modes:

- `cold_spawn`
- `warm_branch`

`warm_branch` is the important new primitive.

In `warm_branch`, child threads do not start from an empty context. They start from a stable parent response lineage using `previous_response_id`, then add task-specific input on top.

This is the right fit for:

- multi-repo GitHub analysis
- architecture tracing
- impact analysis
- decomposition of one large reasoning context into many focused branches

## Hard Rules

- `store=true` is required for warm-branch execution
- every branch child gets its own worker-owned socket
- warm branches copy conversation lineage, not sockets
- the parent response used for branching must already be stably persisted
- child branch instructions must be re-sent explicitly
- branch completion still returns through the normal `spawn_group` barrier

## Why `store=true`

Warm branching depends on `previous_response_id` being safely reusable across separate child threads and sockets.

That means the parent branch point must not rely only on one live socket's short-lived in-memory cache.

`store=true` is therefore the default and required branch mode.

## Concept

Parent flow:

1. Parent thread runs normally.
2. Parent produces a plan or task list for child threads.
3. Parent spawns N child threads in `warm_branch` mode.
4. Each child starts from the same parent `previous_response_id`.
5. Each child gets a different task.
6. Children run independently.
7. Results are aggregated back into the parent as one `function_call_output`.

The important distinction is:

- the parent does not clone its socket
- the runtime clones branch lineage

## Planned Thread Metadata

Warm-branch children should record:

- `branch_mode`
- `branch_parent_thread_id`
- `branch_parent_response_id`
- `branch_index`

Suggested values:

- `branch_mode = warm_branch`
- `branch_parent_thread_id = parent.id`
- `branch_parent_response_id = last stable parent response id used for branching`

These fields may live as first-class thread fields later or inside metadata first.

## Planned `thread.start` Shape

Warm-branch children can still use `thread.start`, but the body needs branch-aware fields:

```json
{
  "initial_input": [
    {
      "type": "message",
      "role": "user",
      "content": [
        {
          "type": "input_text",
          "text": "Investigate authentication flows across repo_api and repo_auth."
        }
      ]
    }
  ],
  "model": "gpt-5.4",
  "instructions": "You are branch 3. Focus only on authentication paths and evidence.",
  "metadata": {
    "spawn_mode": "warm_branch",
    "workspace_manifest_ref": "blob://workspaces/ws_123/manifest.json"
  },
  "store": true,
  "previous_response_id": "resp_parent_plan_123"
}
```

The runtime should treat this as:

- continue from parent branch point
- add only the branch-specific task input
- keep the child on its own thread, socket, and ownership lease

## Planned `spawn_threads` Additions

The parent tool call should be able to request the spawn mode explicitly.

Suggested top-level fields:

- `spawn_mode`
- `branch_previous_response_id`
- `inherit_instructions`
- `inherit_tools`
- `children`

Suggested child-level fields:

- `task`
- `repo_ids`
- `path_prefixes`
- `file_refs`
- `page_numbers`
- `metadata`

Example:

```json
{
  "spawn_mode": "warm_branch",
  "branch_previous_response_id": "resp_parent_plan_123",
  "inherit_instructions": true,
  "inherit_tools": true,
  "children": [
    {
      "task": "Trace authentication flows across repo_api and repo_auth.",
      "repo_ids": ["repo_api", "repo_auth"]
    },
    {
      "task": "Trace billing flows across repo_api and repo_billing.",
      "repo_ids": ["repo_api", "repo_billing"]
    }
  ]
}
```

## Git Engine Fit

This is especially powerful for the Git engine.

Example:

1. Parent thread reads a workspace manifest spanning many repos.
2. Parent creates a task list:
   - auth
   - billing
   - infra
   - shared libraries
   - API edge contracts
3. Runtime warm-branches five child threads from the same parent understanding.
4. Each child explores one concern using the same inherited codebase context.
5. Parent recomposes the five results into one answer.

This is stronger than single-repo threads because the branch point can already contain cross-repo understanding before the split.

## Barrier Behavior

Warm-branch children still use the same regroup path as normal child threads:

- child finishes
- child result is persisted
- parent barrier counts advance
- aggregate `function_call_output` is submitted once
- parent continues on its own chain

There is no separate regroup mechanism for warm branches.

## Important Caveats

- one socket still supports only one in-flight response
- warm branching is parallelism through many child sockets, not multiplexing
- prompt-caching or cached-input cost benefits should be treated as an optimization, not a correctness guarantee
- branch points should always use a stable, already-persisted parent response id

## Locked-In v1 Position

- Warm branching is a first-class runtime primitive.
- It uses `previous_response_id`, not socket cloning.
- It requires `store=true`.
- It is especially important for the multi-repo Git engine.
- It rejoins through the same `spawn_group` barrier system as standard child threads.
