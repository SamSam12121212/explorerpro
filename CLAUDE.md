# CLAUDE.md

Working agreement and project context for Claude sessions on this repo. Read this first.

## What this project is

**Explorer** — a runtime built on the OpenAI Responses API (WebSocket mode), Go + NATS + JetStream + Postgres backend, React + Vite frontend with the React Compiler. Core goal is an AI-native document workspace where the model cites with **pixel-perfect bounding boxes** on PDFs — the "evidence chain". See `README.md` for the long version.

## Today's mission

**Evidence chain closed.** The citation system landed as one epic PR on branch `claude/evidence-locator-alpha`. The `locate_citation` fuzzy-quote design from the earlier draft of this block was replaced with a cleaner shape after the design conversation — two-phase PrepareInput into documenthandler, evidence-locator spawned as a one-shot child thread (new socket behaviour), forced structured output via `tool_choice: emit_bboxes`, citations persisted in DB and referenced by integer id.

The shape the model now works with:
1. **`store_citation(document_id, pages, instruction, include_images)`** — root-thread-only tool. `pages` is 1 or 2 consecutive entries (N or [N, N+1]); `instruction` is natural language including the model's authoritative quoted text as an anchor; `include_images` is false by default, true only for noisy scans.
2. **Two-phase flow**: worker sees the call, fires `PrepareKindStoreCitationSpawn` → documenthandler loads OCR for the pages and builds the locator child's first-turn input. Locator is a new child thread (ChildKind=`citation_locator`, OneShot=true — a new flag on `threads.one_shot` that releases the socket immediately after the single response instead of warm-idling). Locator runs with `gpt-5.4-mini` / medium reasoning, forced through `emit_bboxes` tool call. Barrier closes when all spawned locators return; worker fires `PrepareKindStoreCitationFinalize` per child with `ToolCallArgsJSON`, documenthandler validates indices against OCR, persists `citations` + `citation_bboxes`, returns `{"citation_id": N}` as function_call_output for the parent's turn 2.
3. **Main thread never sees OCR text or bbox numbers.** Only `{citation_id}` comes back, and the model embeds it as `[display text][citation_id]` in its reply. Frontend preprocessor swaps that for a `CitationChip`; click navigates to `/doc/{id}?page={first}&citation={id}`; painter reads bboxes from thread context (fetched from `GET /threads/:id/citations`), no URL coordinates.
4. **Structural hallucination gate** preserved through every layer: tool args validate pages (1-2 consecutive), locator indices validate in-range against OCR lines, emit_bboxes JSON schema is strict. The model physically cannot emit a bbox that isn't a real OCR line.
5. **Per-turn cap** of 20 parallel `store_citation` calls (separate from query_document's 50). Each is its own OpenAI session so the cap is a real cost control.

The `?cite=page,x,y,w,h` URL format and `MOCK_CITATIONS` test fixture are gone. `?page=N` stays as the jump mechanism; `?citation={id}` selects the highlight set.

**Next after this lands:** end-to-end smoke test on the cat-allergy doc + the EMA bipolar guideline (has the 4.7.1.10 continuation case and the 4.7.1.8 single-page case). Watch for locator hallucinating line indices, cost per citation, and OCR coverage on the pharma doc (we haven't OCR'd it yet on the droplet). If quality is tight, add 2-3 few-shot examples to `doccmd.CitationLocatorInstructions`.

## Working style

We move fast. Sam ships PRs all day; pacing matches.

- **Tone:** casual, direct, slang-friendly. "King", "bro", "lock in", "vibes" are normal. Match the energy without forcing it. Light emojis welcome — heavy emoji walls are not.
- **Tight responses.** No preamble, no "I'll now do X" filler. Answer first, justify briefly if needed. For exploratory questions, 2-3 sentences with a recommendation and the key tradeoff.
- **Push back on bad ideas.** Sam wants pushback, not yes-manning. If a suggestion is overkill or wrong (e.g. H200 for OCR - what was he thinking? 🤠), say so with the tradeoff.
- **One change at a time.** Each PR has a tight scope. Don't bundle unrelated fixes.
- **Show the work.** When debugging, dump enough of the failure that Sam can verify the diagnosis. Don't summarize away the receipts.
- **Save memory as you learn.** Project memories for non-derivable facts (deployment IPs, in-flight initiatives, "why we chose X"). Future sessions shouldn't relearn from scratch.
- **TodoWrite for multi-step work.** 3+ discrete steps → set them up, mark completion as you go.
- **Confirm before destructive actions.** SSH `shutdown`, `git push --force`, `docker rm -v`, etc. — name what's about to happen.

## Project conventions

- **Commits:** short imperative title (no period), emoji optional. Multi-paragraph body explaining the *why*. Example title: `Paint citation bounding boxes from ?cite= URL params 🖍️`.
- **PR bodies follow a recognisable shape:** `## The vibes _emoji indicating overall coversation energy_`, `## What changed`, `## What's NOT in here`, `## 🤖 To Cursor bugbot, with love 💌`, `## Test plan`. The bugbot brief pre-empts findings by explaining anything deliberate-looking that bugbot would otherwise flag. Saves a review cycle.
- **Branch names:** auto-generated `claude/<adjective>-<surname>-<hash>` — keep the convention.
- **Coordinate system:** all pixel coords throughout the app are **manifest pixels, top-left origin**. No PDF points at the boundary, no normalization, no y-flip. Applies to OCR output, `?cite=` URLs, manifest, and the painter. **Do not** introduce conversion layers — the cost gets paid in off-by-one bugs. 
- **Responses API event shape stays raw** through frontend, backend, and storage. AI agents will suggest abstractions; refuse them. Easier to reason about, cheaper to debug.
- **IDs are integers**, not UUIDs. Easier log/path scanning during early dev.
- **Worker is oblivious.** The core worker manages OpenAI websockets + thread ops + event persistence. It does not know about consumers; consumers subscribe to events. Keep that boundary clean.
- **npm staleness.** AI agents recommend packages unmaintained for years — always check. If popular but stale, look for an active fork.

## Recent marathon (2026-04-16 → 2026-04-18, 26 PRs merged)

**Foundations (#1-5)** — Responses API event stream correctness
- #1: Don't pass child thread deltas to wsserver
- #2: Guard root thread state against child terminal events; fix `injectThreadFields` trailing comma
- #3: Lift left sidebar tab state to `AppLayout` (fix doc clearing on tab switch)
- #4: Render `output_text` deltas live, use `output_item.done` as truth; preserve accumulated deltas when `output_item.added` arrives late
- #5: Drop tool-call argument deltas at worker; rename `query_attached_documents` → `query_document`; align `function_call_arguments` naming

**Document tooling (#6-10)**
- #6: Surface in-flight `query_document` tool calls in thread UI
- #7: Attach document collections to threads (with batched `ListAttached` fetch)
- #8: Simplify thread sidebar items to title only
- #9: Align PDF toolbar with app shell styling
- #10: Delete documents feature

**The Credit Fire (#11-13)** — collection-attached docs failed query validation, retried forever, burned OpenAI credits
- #11: Accept collection-attached documents in query validation
- #12: Drop document-query commands with unattached docs as precondition-failure (kills the retry storm)
- #13: Make command stream ephemeral, cap recovery reconciles per worker

**Polish + the citation arc (#14-20)**
- #14: Updated `docs/image.png` to the cat collection screenshot 🐈 — the Ohio State cat allergy guide that became the smoke-test fixture
- #15: Render assistant markdown on final responses (deltas stay plain text)
- #16: Emit page citations as internal document links — `[page N](/doc/{id}?page=N)`. **Bugbot caught two real bugs**: wrong route (`/documents/` vs actual `/doc/:documentId`) and `//evil.com` bypass of the internal-link check
- #17: Jump to cited page when opening a PDF via `?page=N` (initial attempt)
- #18: Fix "Scroll state not found" by gating jump on `isScrollReady` (scroll-plugin registration vs per-doc layout-ready race)
- #19: The actual fix — `?page=N` jumps never landed in #17/#18 because passing `src` to `PDFViewer` made embedpdf auto-generate its own document ID; switched to `documentManager.initialDocuments` with explicit ID
- #20: Paint citation bounding boxes from `?cite=page,x,y,w,h` URL params via embedpdf's annotation plugin (`autoCommit:false`, `locked:All` — ephemeral overlay, never flushed to PDF). Bugbot caught `parseFloat("1.5")` sailing past `page >= 1`; fixed with `Number.isInteger`

**OCR pipeline (2026-04-17 overnight → morning, #21-24, all merged)**
- **#21:** PaddleOCR FastAPI service on a DO GPU droplet (RTX 6000 Ada, 48GB VRAM). Three endpoints (`/ocr`, `/ocr/visualize`, `/structure`), bearer auth, GPU passthrough. Smoke-tested end-to-end (88 lines on the cat doc, 0.97-1.00 confidences, ~1s warm). Compose now binds `0.0.0.0:8000` behind a DO Cloud Firewall locked to the home IP — no SSH tunnel in the dev loop
- **#22:** README refresh — replaced dated `Notes` with `Rough edges`, turned `Aims` into `What it does`, added an `Evidence chain` walkthrough
- **#23:** Split backend Docker builds per service + `.dockerignore` — each Go service gets its own Dockerfile, build context shrinks from the whole repo to just what each binary needs
- **#24:** `dococr` worker — subscribes to `doc.split.done` on the `DOC_OCR` JetStream, POSTs each page PNG to PaddleOCR `/ocr`, persists per-page OCR JSON, stamps `pages[i].ocr_ref` onto the manifest, publishes `doc.ocr.done`. Three rounds of bugbot: (1) Nak on publish failure instead of silent-ack, (2) publish before `UpdateStatus(ready)` so Nak'd redeliveries don't regress status, (3) `mergeExistingOCRRefs` preserves ocr_refs on redelivery manifest rewrite when page SHA matches (pdftocairo is deterministic → same PNG bytes = same OCR, no reason to redo the GPU work). Covered by `merge_test.go`

**Page-into-main-thread primitive (2026-04-18, #25-26, all merged)**
- **#25:** CLAUDE.md refresh — rolled OCR-pipeline work into history, pointed next mission at evidence-chain closure via `query_document` bbox enrichment. (Mission since pivoted — see "Today's mission" above.)
- **#26:** `read_document_page` tool, root threads only. The reciprocal of `query_document` — where query delegates reasoning to a child thread with the whole doc baked in, read_document_page pulls a single specific page directly into the main thread's context as an image inside a `function_call_output` (list-form `output` array). Three prep commits before the feature: shared `<pdf>`/`<pdf_page>` envelope helper with width/height attributes, blob-ref lowering extended to walk `function_call_output.output` arrays (so the NATS wire stays blob-ref-only until the OpenAI-bound edge), then the tool itself. **New pattern:** post-terminal sync-tool resume in `sendAndStream` — the function is now a loop that reloads `meta`, fires `PrepareInput` RPCs on pending calls, and issues a follow-up `response.create` with the outputs as input. Stack stays flat, no recursion. Per-call failures surface as error-shaped `function_call_output` strings so one bad call doesn't kill the turn. One bugbot round caught three real bugs: `read_document_page` missing from `isInternalRuntimeToolName` (would leak into child threads if ever stored in `ToolsJSON`), reused `cmdID` across loop iterations meant `Nats-Msg-Id` dedup silently dropped follow-up history entries (fixed with a `-turn-N` suffix for iterations > 0), and the terminal bail-out didn't gate on `response.completed` (would execute pending tools on failed responses). End-to-end smoke test working — model reads a page, answers with real content and correct details from it.

## Where things live

- `cmd/` — Go entry points (worker, docsplitter, dococr, app, etc.)
- `internal/` — Go internals (`docsplitter`, `dococr`, `blobstore`, `doccmd`, `ocrcmd`, `docstore`, `natsbootstrap`, `docprompt`, ...). `ocrcmd` owns the `DOC_OCR` JetStream + `doc.split.done` / `doc.ocr.done` subjects
- `frontend/` — React + Vite + React Compiler + react-router + embedpdf
- `services/paddleocr/` — Dockerfile + FastAPI app for the OCR microservice. Deployed separately on the DO GPU droplet — **not** in the main `compose.yaml`
- `db/` — Postgres migrations
- `compose.yaml` — main app stack (NATS + Postgres + Go services + frontend)
- `docs/` — README screenshot lives here

## Watch-outs

- **Docker mounts the main repo, not worktrees.** Frontend dev server binds `/Users/detachedhead/explorer/frontend` directly. Worktree edits won't HMR. Edit in the main checkout when iterating on hot frontend paths, or restart the container bound to the worktree.
- **OpenAI billing surface.** Anything that retries on tool-call validation should fail-precondition first; only retry on transient signals. The Credit Fire is the cautionary tale (#11-13).
- **PaddleOCR cold start ~60-90s** (CUDA autotune on first inference per container lifetime). Warm calls ~1s. Test pipelines should expect the one-time hit.
- **GPU droplet costs $1.57/hr.** Power off via `ssh paddle-ocr 'shutdown -h now'` between sessions. **Verify in DO billing whether GPU droplets keep billing while off** — historically yes (reserved hardware); destroy + snapshot is the only true-zero option.
- **Bundled vLLM container** on the droplet has `restart=unless-stopped`. Run `docker update --restart=no vllm` before any reboot or it resurrects and contends for the GPU.
- **Cursor bugbot reviews every PR.** Address its findings in a follow-up commit on the same branch; pre-empt deliberate-looking choices in the bugbot brief section of the PR body.
- **JetStream redelivery is not hypothetical.** Any handler that writes blob state + updates Postgres + publishes a follow-up event has to survive being replayed. Three ordering/idempotency bugs landed in #24 that all looked fine on the happy path. Before shipping a new consumer: write out the Nak-at-each-step cases and check that redelivery converges, not clobbers.
- **Responses API `function_call_output.output` accepts list-form content.** Not just a string — the union is `string | []{input_text | input_image | input_file}`. The Go SDK types (`github.com/openai/openai-go/v3/responses`) are authoritative. Sub-agent summaries have gotten this wrong; don't trust a paraphrase, check the SDK source in `~/go/pkg/mod/github.com/openai/openai-go/`.
- **Post-terminal sync-tool resume is a new actor pattern (#26).** `sendAndStream` is a loop, not a one-shot. When `streamUntilTerminal` returns pending sync-tool calls (currently only `read_document_page`), the loop executes them via `PrepareInput` RPCs and issues a follow-up `response.create`. Follow-up turns must use a unique `eventID` (we append `-turn-N` from turn 1 onward) or JetStream dedup silently drops the history checkpoint entries. If you add a new sync-tool kind, reuse this pattern — don't invent a new one.
- **`paddle-ocr` SSH alias** lives in `~/.ssh/config` (uses `~/.ssh/droplet`). Service config + bearer token live at `/root/paddleocr/` on the droplet. See `~/.claude/projects/-Users-detachedhead-explorer/memory/reference_paddle_ocr_droplet.md` for full details.
