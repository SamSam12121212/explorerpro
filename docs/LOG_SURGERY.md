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
- make document children and spawned children read like the same runtime
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
- explicit function-call semantics on output items
- explicit command-envelope context at API and worker boundaries
- explicit child-response correlation on barrier updates

Important effect:

- grepping one root thread id now gives a coherent story
- child threads read as normal threads instead of pseudo-types
- parent regroup is visible as a barrier close and resume, not as mysterious extra model work
- known tool calls now say what they are without making you inspect payload storage

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
- document children and spawned children use the same structural vocabulary

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
- [`internal/threadcmd/command_test.go`](/Users/detachedhead/explorer/internal/threadcmd/command_test.go)

Coverage added for:

- explicit full `thread_id`
- explicit full `cmd_id`
- response-create input classification
- thread-graph attr enrichment
- duplicate-key avoidance in thread-graph attr enrichment
- function-call semantic logging
- command-envelope logging attrs

Verification run:

- `go test ./internal/logutil`
- `go test ./internal/threadcmd`
- `go test ./internal/worker`
- `go test ./internal/httpserver`

### Incision 8: Teach output-item logs what tool call they saw — DONE

Changed [`internal/worker/actor.go`](/Users/detachedhead/explorer/internal/worker/actor.go):

- `received openai event` for `response.output_item.added/done.function_call` now logs:
- `call_id`
- `call_name`
- `call_kind`
- known structural hints such as `document_count`, `child_count`, and `spawn_mode`
- `output item received` now carries the same semantic hints for function calls

Important effect:

- `function_call` stopped being a vague noun
- known runtime calls such as `query_attached_documents` and `spawn_threads` are recognizable at a glance
- we still do not log raw arguments or prompt text

### Incision 9: Carry thread-graph context onto raw OpenAI event lines — DONE

Changed [`internal/worker/actor.go`](/Users/detachedhead/explorer/internal/worker/actor.go):

- `received openai event` logs now carry:
- `response_id`
- `root_thread_id`
- `parent_thread_id`
- `parent_call_id`
- `depth`
- `spawn_group_id` when active

This applies to:

- regular non-delta events
- output-item events
- flushed delta summaries

Important effect:

- grepping `root_thread_id` now catches the raw event trail instead of only the high-level lifecycle lines
- the low-level event stream stays attached to the same thread tree as the lifecycle logs

### Incision 10: Make command-envelope logs structurally honest — DONE

Changed:

- [`internal/threadcmd/command.go`](/Users/detachedhead/explorer/internal/threadcmd/command.go)
- [`internal/httpserver/command_api.go`](/Users/detachedhead/explorer/internal/httpserver/command_api.go)
- [`internal/worker/service.go`](/Users/detachedhead/explorer/internal/worker/service.go)

Added shared command-log attr rendering for:

- `root_thread_id`
- `causation_id`
- `correlation_id`
- `input_kind` where the command body makes that knowable
- `model` and `has_previous_response_id` for `thread.start`
- `spawn_group_id`, `child_thread_id`, `child_status`, and `child_response_id` for child-result commands

Important effect:

- API publish logs and worker dispatch logs now speak the same structural language
- child-result commands can be read without decoding their JSON body by hand
- command routing stopped erasing thread ancestry and regroup context

### Incision 11: Correlate parent barrier updates back to child responses — DONE

Changed [`internal/worker/actor.go`](/Users/detachedhead/explorer/internal/worker/actor.go):

- `child barrier updated` now includes `child_response_id`

Important effect:

- parent barrier progress lines can now be correlated directly with child terminal lines and stored responses

### Incision 12: Close the command lifecycle seam — DONE

Changed [`internal/worker/actor.go`](/Users/detachedhead/explorer/internal/worker/actor.go):

- `processing command` now carries thread-graph context
- `command completed` now carries thread-graph context

Those lines now stay inside the same structural envelope as the rest of the run:

- `root_thread_id`
- `parent_thread_id`
- `parent_call_id`
- `depth`
- `spawn_group_id` when active

Important effect:

- the command path now reads as one continuous chain:
- request publish
- worker dispatch
- actor processing
- actor completion
- grepping `root_thread_id` no longer loses the actor-internal command lifecycle lines

### Incision 13: Normalize warmup handoff spawn logs — DONE

Changed [`internal/worker/actor.go`](/Users/detachedhead/explorer/internal/worker/actor.go):

- the post-warmup `spawning child thread` line now includes the same document-facing fields as the initial document-query spawn:
- `document_name`
- `phase=query`
- `has_previous_response_id=true`

Important effect:

- warmup handoff no longer looks like a different class of spawn
- document query children read the same way whether they came from `document_base` directly or from a warmup bootstrap

## Post-Op Anatomy

After surgery, a document query run reads like this:

1. root thread `thread.start`
2. initial `response.create` with `trigger=start`
3. model emits `query_attached_documents` and the output-item log says so with `call_id`, `call_name`, `call_kind=document_query`, and `document_count`
4. parent logs one `spawning child thread` per document
5. parent logs `opened child barrier`
6. each child logs its own normal thread lifecycle with:
- full child `thread_id`
- `parent_thread_id`
- `parent_call_id`
- `depth=1`
- shared `spawn_group_id`
7. if a document needs warmup first, the warmup completion logs a second `spawning child thread` with the same document fields as any other query child, plus `bootstrap_child_thread_id`
8. each terminal child publishes completion
9. parent logs `child barrier updated` for each child result, including `child_response_id`
10. parent logs `resuming thread resume_reason=child_barrier`
11. parent sends a second `response.create` with `trigger=child_barrier` and `input_kind=function_call_output`
12. parent reaches `ready`

That is the actual runtime shape.

The logs now say so out loud.

## What This Gives Us

- one grep target: `root_thread_id`
- one structural vocabulary for root threads, spawned children, and document queries
- clear distinction between initial send, resume, regroup, and recovery
- immediate visibility into whether document queries reused base anchors or required warmup
- direct visibility into which known function call fired without opening payload tables
- command publish/dispatch logs that preserve thread ancestry and child-result structure
- actor processing/completion logs that stay inside the same grep path
- warmup-bootstrap document queries that no longer change shape halfway through the trace
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
- [`internal/threadcmd/command.go`](/Users/detachedhead/explorer/internal/threadcmd/command.go) — added shared command-log attrs and shared input-kind classification
- [`internal/threadcmd/command_test.go`](/Users/detachedhead/explorer/internal/threadcmd/command_test.go) — added tests for command-log attrs
- [`internal/httpserver/command_api.go`](/Users/detachedhead/explorer/internal/httpserver/command_api.go) — enriched API-side request/publish logs with shared command context
- [`internal/worker/service.go`](/Users/detachedhead/explorer/internal/worker/service.go) — enriched dispatch logs with shared command context
- [`internal/worker/actor.go`](/Users/detachedhead/explorer/internal/worker/actor.go) — added thread-graph attr enrichment, trigger/cause logging, barrier logging, lineage-source logging, duplicate-key protection, raw-event graph attrs, function-call semantic attrs, child-response correlation, command lifecycle graph attrs, and warmup-handoff spawn normalization
- [`internal/worker/actor_test.go`](/Users/detachedhead/explorer/internal/worker/actor_test.go) — added tests for input-kind classification, graph attr behavior, delta-event graph attrs, function-call semantic logging, command lifecycle graph attrs, and warmup-handoff logging

## What Remains

1. If we later want even tighter troubleshooting loops, we can consider logging command-route outcome details like `routed_direct` and `owner_worker_id` alongside publish logs where that context exists.
2. We may eventually want symmetric outbound worker-command publish logs for child terminal notifications and recovery commands, not just inbound dispatch logs.
3. We should keep resisting the temptation to log prompts, raw tool arguments, and assistant answers by default; item history and stored payloads already own payload inspection.
