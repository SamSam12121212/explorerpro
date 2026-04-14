# Log Surgery

Date of operation: April 14, 2026

## Chief Complaint

The worker logs had started lying by abbreviation.

After document queries were moved into the normal thread system, the runtime shape was correct, but the logs still made it feel like document work was some strange sidecar creature:

- child thread ids collapsed into misleading shapes like `thread_doc_thread`
- child start command ids collapsed into the same visible prefix
- parent and child relationships were mostly implicit
- regroup/resume reasons were not visible at the moment they mattered
- the parent's second `response.create` looked too much like a new user send
- document queries were structurally normal threads, but the logs did not let you read them that way

Clinical decision:

- stop shortening thread ids
- stop shortening command ids
- log the thread graph explicitly
- log causes, not prompts
- make document children and subagent children read like the same runtime
- do not change execution behavior just to satisfy logging

This was an observability surgery, not a behavior surgery. The point was not to make the worker emit more noise. The point was to make the existing thread runtime legible.

## Pre-Op Anatomy

Before surgery, a real document-child flow looked something like this in logs:

- parent thread started
- model emitted `query_attached_documents`
- parent spawned children
- child threads were dispatched
- children ran
- parent resumed

That sounds fine in theory. In practice, two things went wrong:

1. Identity collapsed.

- `thread_id` was shortened from the front
- stable document child thread ids all start with `thread_doc_thread_...`
- that meant multiple different child threads rendered as the same visible token
- `cmd_id` shortening caused the same collapse for child `thread.start` commands

2. Cause disappeared.

- logs did not say whether a `response.create` came from initial start, user resume, tool output, child-barrier regroup, or checkpoint recovery
- logs did not say whether document lineage came from thread-local child history, shared document base anchor, or warmup
- logs did not carry enough thread-tree context to reconstruct the run with confidence from logs alone

The runtime was already threads all the way down.

The logs just refused to admit it.

## Procedure Summary

We aligned worker logs to the actual thread model:

- full `thread_id`
- full `cmd_id`
- explicit `root_thread_id`
- explicit `parent_thread_id`
- explicit `parent_call_id`
- explicit `depth`
- explicit `spawn_group_id`
- explicit start/resume/send triggers
- explicit child-barrier lifecycle
- explicit document lineage source

Important effect:

- grepping one root thread id now gives a coherent story
- child threads read as normal threads instead of pseudo-types
- parent regroup is visible as a barrier close and resume, not as mysterious extra model work

## Incision Plan

### Incision 1: Stop abbreviating the truth — DONE

Changed [`internal/logutil/handler.go`](/Users/detachedhead/explorer/internal/logutil/handler.go):

- removed front-shortening for `thread_id`
- removed front-shortening for `cmd_id`
- kept the bare-message / bare-cmd rendering style

Important effect:

- child thread ids stay distinct
- child start command ids stay distinct
- the logger no longer turns a normal thread id into a misleading pseudo-category

### Incision 2: Make thread graph context first-class — DONE

Added shared log attribute enrichment in [`internal/worker/actor.go`](/Users/detachedhead/explorer/internal/worker/actor.go):

- `root_thread_id`
- `parent_thread_id`
- `parent_call_id`
- `depth`
- `spawn_group_id`

Important effect:

- every key lifecycle line can now be placed directly in the thread tree
- document children and subagent children use the same structural vocabulary

### Incision 3: Log execution cause explicitly — DONE

Enriched lifecycle logs in [`internal/worker/actor.go`](/Users/detachedhead/explorer/internal/worker/actor.go):

- `starting thread` now includes `start_kind` and `input_kind`
- `resuming thread` now includes `resume_reason`
- `sending response.create to openai` now includes `trigger` and `input_kind`

Current causes logged:

- `trigger=start`
- `trigger=child_barrier`
- `trigger=checkpoint_recovery`
- `resume_reason=user_input`
- `resume_reason=tool_output`
- `resume_reason=child_barrier`
- `resume_reason=child_barrier_recovery`

Important effect:

- the parent's post-child regroup send is now visibly different from a user-originated resume
- recovery replay is distinguishable from live work

### Incision 4: Make child barriers readable — DONE

Added explicit barrier lifecycle logs in [`internal/worker/actor.go`](/Users/detachedhead/explorer/internal/worker/actor.go):

- `opened child barrier`
- `child barrier updated`
- `child barrier still waiting after recovery`
- `resuming thread` when the barrier closes

Logged counts:

- `completed_children`
- `failed_children`
- `cancelled_children`
- `expected_children`

Important effect:

- parent regroup stopped being implicit
- you can now watch the barrier open, progress, close, and resume in order

### Incision 5: Surface document lineage source — DONE

Document child spawn logs now distinguish:

- `lineage_source=thread_local`
- `lineage_source=document_base`
- `lineage_source=warmup`

Warmup handoff logs also emit the bootstrap/query child relationship.

Important effect:

- it is now obvious whether a document query reused a shared base anchor or had to bootstrap

### Incision 6: Remove duplicate graph attrs — DONE

The shared thread-graph helper now fills in missing context instead of blindly appending it.

Changed [`internal/worker/actor.go`](/Users/detachedhead/explorer/internal/worker/actor.go):

- added duplicate-key detection for graph attrs
- stopped emitting duplicate `spawn_group_id` and similar keys on lines that already set them explicitly

Important effect:

- parent lifecycle lines stay readable instead of repeating the same field

### Incision 7: Lock the contract with tests — DONE

Updated tests:

- [`internal/logutil/handler_test.go`](/Users/detachedhead/explorer/internal/logutil/handler_test.go)
- [`internal/worker/actor_test.go`](/Users/detachedhead/explorer/internal/worker/actor_test.go)

Coverage added for:

- explicit full `thread_id`
- explicit full `cmd_id`
- response-create input classification
- thread-graph attr enrichment
- duplicate-key avoidance in thread-graph attr enrichment

Verification run:

- `go test ./internal/logutil`
- `go test ./internal/worker`

## Post-Op Anatomy

After surgery, a document query run reads like this:

1. root thread `thread.start`
2. initial `response.create` with `trigger=start`
3. model emits `query_attached_documents`
4. parent logs one `spawning child thread` per document
5. parent logs `opened child barrier`
6. each child logs its own normal thread lifecycle with:
- full child `thread_id`
- `parent_thread_id`
- `parent_call_id`
- `depth=1`
- shared `spawn_group_id`
7. each child publishes terminal completion
8. parent logs `child barrier updated` for each child result
9. parent logs `resuming thread resume_reason=child_barrier`
10. parent sends a second `response.create` with `trigger=child_barrier` and `input_kind=function_call_output`
11. parent reaches `ready`

That is the actual runtime shape.

The logs now say so out loud.

## What This Gives Us

- one grep target: `root_thread_id`
- one structural vocabulary for root threads, children, document queries, and subagents
- clear distinction between initial send, resume, regroup, and recovery
- immediate visibility into whether document queries reused base anchors or required warmup
- less need to jump to Postgres just to understand a run

Most importantly:

- the worker no longer makes normal child threads look like a special runtime

## What We Explicitly Did Not Do

- log user prompt text
- log assistant answer text
- log raw tool arguments on every path
- introduce a document-specific logging subsystem
- change the execution model

This stayed inside the normal thread runtime on purpose.

## Files Changed

- [`internal/logutil/handler.go`](/Users/detachedhead/explorer/internal/logutil/handler.go) — removed id shortening; kept explicit thread ids
- [`internal/logutil/handler_test.go`](/Users/detachedhead/explorer/internal/logutil/handler_test.go) — updated formatter expectations
- [`internal/worker/actor.go`](/Users/detachedhead/explorer/internal/worker/actor.go) — added thread-graph attr enrichment, trigger/cause logging, barrier logging, lineage-source logging, duplicate-key protection
- [`internal/worker/actor_test.go`](/Users/detachedhead/explorer/internal/worker/actor_test.go) — added tests for input-kind classification and graph attr behavior

## What Remains

1. If we later want even tighter troubleshooting loops, we can add more explicit classification for `output item received` when the item is a known `function_call`.
2. We may eventually want the same graph/cause treatment in adjacent services if they emit worker-adjacent runtime logs.
3. We should keep resisting the temptation to log prompts and answers by default; item history already owns payload inspection.
