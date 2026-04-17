# CLAUDE.md

Working agreement and project context for Claude sessions on this repo. Read this first.

## What this project is

**Explorer** — a runtime built on the OpenAI Responses API (WebSocket mode), Go + NATS + JetStream + Postgres backend, React + Vite frontend with the React Compiler. Core goal is an AI-native document workspace where the model cites with **pixel-perfect bounding boxes** on PDFs — the "evidence chain". See `README.md` for the long version.

## Today's mission

**Get PaddleOCR fully functional and integrated into the pipeline.** The OCR microservice is deployed and validated (PR #21, still pending merge). The painter ships in #20. Closing the loop means:

1. **`dococr` Go worker** — subscribes to a post-split NATS subject, POSTs each page PNG to PaddleOCR `/ocr`, persists per-page OCR JSON in blob storage, extends the manifest with `pages[i].ocr_ref`.
2. **Manifest extension** in `internal/docsplitter/manifest.go` — add the OCR ref field, bump `Version`.
3. **`query_document` enrichment** — return bbox(es) for the cited region alongside the answer text.
4. **Model prompt update** — tell the model to emit `?cite=page,x,y,w,h` URLs using the bboxes the tool returns.
5. **End-to-end smoke test** — model answers a question over a real doc, citation link lands on the exact pixel region in the viewer. Loop closed.

When in doubt today, ask: "does this move us closer to citation #5?" If no, defer it.

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

## Yesterday's marathon (2026-04-16/17, ~16h, 21 merged + 1 pending)

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

**This morning (still open)**
- **#21:** PaddleOCR FastAPI service on a DO GPU droplet (RTX 6000 Ada, 48GB VRAM). Three endpoints (`/ocr`, `/ocr/visualize`, `/structure`), bearer auth, GPU passthrough, `127.0.0.1:8000` bind. Smoke-tested end-to-end (88 lines on the cat doc, 0.97-1.00 confidences, ~1s warm)
- **#22:** README refresh — replaced dated `Notes` with `Rough edges`, turned `Aims` into `What it does`, added an `Evidence chain` walkthrough

## Where things live

- `cmd/` — Go entry points (worker, docsplitter, app, etc.)
- `internal/` — Go internals (`docsplitter`, `blobstore`, `doccmd`, `docstore`, `natsbootstrap`, `docprompt`, ...)
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
- **`paddle-ocr` SSH alias** lives in `~/.ssh/config` (uses `~/.ssh/droplet`). Service config + bearer token live at `/root/paddleocr/` on the droplet. See `~/.claude/projects/-Users-detachedhead-explorer/memory/reference_paddle_ocr_droplet.md` for full details.
