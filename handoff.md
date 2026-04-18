# handoff

Snapshot of the evidence-chain citation work as of 2026-04-19. PR #29 is merged; picking up in a fresh session.

## Sam flagged an open issue

Bugbot has gone quiet but Sam says there is still an issue to handle. Specifics not captured in the prior conversation. First thing to do in the next session: ask Sam to describe it, reproduce, fix.

## What shipped in #29

Full evidence-chain citation system, backend + frontend.

Main thread tool: `store_citation(document_id, pages[1..2 consecutive], instruction, include_images)`. Root-thread gated, 20-per-turn cap.

Flow per citation:
1. Worker collects store_citation calls from the response stream.
2. At response.completed, `startCitationLocatorGroup` fans out N one-shot locator children (1:1 with parent call ids). Each child: `child_kind="citation_locator"`, `one_shot=true`, model `gpt-5.4-mini` medium reasoning, forced `tool_choice: {type: function, name: emit_bboxes}`.
3. Pre-spawn, `PrepareKindStoreCitationSpawn` into documenthandler loads OCR for the pages and builds the child's initial input (instruction + indexed OCR lines with bbox coords + optional page images).
4. Child emits forced tool call with `{pages: [{page, line_indices}]}`. Socket disconnects immediately after terminal response (new `threads.one_shot` behaviour).
5. Barrier close: worker fires `PrepareKindStoreCitationFinalize` per child carrying `ToolCallArgsJSON`. documenthandler validates indices against OCR, persists via `citationstore`, returns `{"citation_id": N}` as `function_call_output` for the parent's turn 2.
6. Main thread composes reply with `[display text][citation_id]` markers.
7. Frontend `MarkdownContent` preprocesses `[text][id]` to a `citation:N` href, `a` renderer swaps for `CitationChip`. Click navigates to `/doc/{docId}?page={first}&citation={id}`. `PdfViewerPanel` reads bboxes from thread-scoped state (fetched from `GET /threads/:id/citations`) and paints via the embedpdf annotation plugin.

Structural gates:
- `doccmd.ValidateCitationPages` at tool-arg decode AND worker pre-spawn AND documenthandler boundaries
- `emit_bboxes` JSON schema is strict
- Locator line indices validated in range against OCR before persist
- Main thread never sees OCR text or bbox numbers, only citation_ids

`?cite=page,x,y,w,h` URL format is fully removed. `?page=N` stays as the jump mechanism. `?citation={id}` is new, resolves via thread state.

## Known follow-ups (not the issue Sam flagged)

These are flagged in their own commit bodies as out-of-scope for #29.

1. **Redelivery orphan-spawn for citation locator children.** `buildCitationLocatorChildStartCommand` calls `ReserveThreadID` fresh each time. On NAK redelivery of the parent command, round 4's fix preserves the first attempt's `ParentCallID` bindings so barrier close finds the right results, but we still reserve new thread ids and dispatch start commands for them. Second-attempt children run as orphans and each burns an OpenAI session. Real fix: `LoadReusableCitationLocatorChild`-style lookup keyed on `(parent_thread_id, parent_call_id)` so thread id reservation is idempotent. Probably needs a new unique index on `threads (parent_thread_id, parent_call_id) WHERE child_kind='citation_locator'`.

2. **Mixed tool kinds in one response goes to WaitingTool.** If the model emits `store_citation` + `read_document_page` (or any other combo) in a single turn, round 5's fix sets `waitingTool=true` and the thread sits in WaitingTool until the app submits tool outputs. Nothing auto-submits for this case so the thread hangs. Real fix: parallel dispatch plus synthesize error-shaped `function_call_output` items for non-winning kinds so every `function_call` in `previous_response_id` has a matching output in the follow-up input.

## What hasn't been tested

End-to-end against real OpenAI sessions. Feature has never actually run. Plan was a ~20-PDF smoke test; Sam was evaluating topic areas for demo videos:
- **Bipolar rapid cycling / mixed states** (lean: strongest, EMA PDF is an existing fixture, personal-stake narration)
- **GLP-1 agonists beyond diabetes** (strong mass-appeal alt, rich literature)
- **Quant trading research** (strong topic, public-corpus constrained)

PaddleOCR droplet does not yet have the EMA bipolar PDF OCR'd. `ssh paddle-ocr` to reach it. Start from `/root/paddleocr/` for service config.

## 8 bugbot rounds, for context

Two would have shipped the feature inert:
- **Round 2** (984f33a): `LoadOrCreateDocumentQuerySpawnGroup` hardcoded `group_kind='document_query'` in the INSERT and fallback SELECT, silently overwriting whatever the caller passed. Citations persisted with wrong kind, aggregator dispatch took generic path instead of `aggregateCitationLocatorOutputs`. Renamed to `LoadOrCreateSpawnGroup`, kind now comes from meta.
- **Round 3** (23a9292): citation round bindings need `ChildThreadID` which is only known post-spawn, so `ParentCallID` was set after `LoadOrCreateSpawnGroup`. `CreateSpawnGroup` uses `ON CONFLICT DO NOTHING` so the persisted row kept its empty value. `decodeCitationRoundCalls("")` returned nil at barrier close and every citation errored. Fix: `SaveSpawnGroup` before `CreateSpawnGroup`.

Rest were edge cases and polish:
- Round 1: citation guard in page-reads return path + dead scaffolding
- Round 4: guard `ParentCallID` overwrite on redelivery (don't overwrite if row already has bindings)
- Round 5: mixed tool kinds + markdown numeric ref-link regex collision
- Round 6: actually send bbox coords to the locator (prompt claimed we did, code didn't)
- Round 7: share `ValidateCitationPages` across doccmd and worker
- Round 8: nil bboxes slice defense (pre-populate map, frontend `?? []`)

## Key files for orientation

Backend:
- `internal/doccmd/command.go` - tool defs (`store_citation`, `emit_bboxes`), PrepareKinds, `ValidateCitationPages`, `CitationLocatorInstructions`
- `internal/documenthandler/citation.go` - `prepareStoreCitationSpawnInput` + `prepareStoreCitationFinalizeInput`
- `internal/worker/citation_locator.go` - `startCitationLocatorGroup`, `aggregateCitationLocatorOutputs`, `buildCitationLocatorChildStartCommand`
- `internal/worker/actor.go` - `streamUntilTerminal` mix handling, `OneShot` disconnect path, tool-call-args capture in `publishChildInvocationResult`
- `internal/citationstore/store.go` - CRUD
- `internal/httpserver/command_api.go` - `GET /threads/:id/citations` at `handleListCitations`
- `db/migrations/000022_citations.sql` - citations + citation_bboxes tables
- `db/migrations/000023_citation_locator_child_kind.sql` - `threads.one_shot`, relaxed child_shape constraint, `spawn_group_children.tool_call_args_json`

Frontend:
- `frontend/src/components/thread/CitationChip.tsx` - chip
- `frontend/src/components/thread/MarkdownContent.tsx` - `[text][id]` preprocessor, numeric ref-definition skip
- `frontend/src/components/PdfViewerPanel.tsx` - painter reads bboxes via `?citation={id}` from thread context
- `frontend/src/thread/ThreadService.ts` - `fetchCitations` on thread load + terminal events
- `frontend/src/constants.ts` - `DEFAULT_INSTRUCTIONS` rewrite

## Branch + CI state

- `main` has PR #29 merged at `1f138a3`.
- Go test suite green on the merged tree. Vitest green.
- No untracked or staged changes at handoff time.
