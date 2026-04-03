# PDF to PNG Exploration

## Purpose

Capture the initial investigation into a service that takes a PDF and splits it into per-page PNG files, with an emphasis on rendering accuracy and a preference for Go.

This is an exploratory handoff note only. No implementation work was done as part of this pass.

## Executive Summary

- The repo is already shaped for this kind of feature: Go workers, local blob storage for large artifacts, and worker-side pause/resume around tool calls.
- The cleanest fit is a Go-owned PDF render job that writes PNGs into blob storage and returns a manifest/blob reference back into the thread.
- If rendering accuracy is the priority, the best starting point is not a pure-Go renderer. The strongest near-term option is Go orchestration around a mature renderer such as Poppler via `pdftocairo` or `pdftoppm`.
- On this machine, both `pdftocairo` and `pdftoppm` are already installed locally at Poppler `26.03.0`.

## Repo Context

The most relevant local docs and code were:

- `README.md`
- `ARCHITECTURE.md`
- `WORKER_RUNTIME.md`
- `LOCAL_DEV.md`
- `RECOVERY.md`
- `REDIS_MODEL.md`
- `internal/blobstore/local.go`
- `internal/platform/runtime.go`
- `internal/worker/actor.go`

Key repo-grounded findings:

- The project is a Go, worker-centric backend built around OpenAI Responses WebSocket mode.
- Large artifacts are expected to live in blob storage, not Redis.
- Local development already treats `./blob-storage` as the artifact root.
- The suggested local blob layout already includes `blob-storage/pdfs/`, `blob-storage/child-results/`, and `blob-storage/tmp/`.
- The worker already pauses on `function_call` output and resumes via `thread.submit_tool_output`, which is the right seam for integrating a PDF render step later.
- There is not yet a general-purpose tool execution subsystem. The current code mainly special-cases `spawn_subagents` and otherwise lands model tool calls in `waiting_tool`.
- Recovery is replay-oriented, which means any eventual PDF render step should be idempotent.

Practical implication:

- Do not store page PNGs in Redis.
- Store PNGs and any render manifest in blob storage.
- Keep only references, metadata, and job state in Redis.

## Local Environment Findings

Observed locally during this exploration:

- `pdftocairo` is installed
- `pdftoppm` is installed
- both report Poppler version `26.03.0`
- `mutool` is not installed
- `gs` is not installed
- `magick` is not installed
- no `.pdf` files were present in the workspace at the time of investigation

Relevant local command capabilities:

- `pdftocairo` supports direct PNG output, DPI control, crop box usage, antialias settings, transparency, and ICC profile output.
- `pdftoppm` supports PNG output, DPI control, crop box usage, annotation hiding, thin-line mode, and separate font/vector antialias toggles.

## Recommendation

Recommendation: use Go for orchestration, storage, manifests, idempotency, and worker integration, but let a mature PDF renderer do the actual page rasterization.

Best first path:

1. Build the service boundary in Go.
2. Use Poppler from Go via `exec.CommandContext`.
3. Start with a bake-off between `pdftocairo` and `pdftoppm` on representative PDFs.
4. Default to whichever one wins the corpus on fidelity and operational simplicity.

Why this is the recommended path:

- It matches the current repo architecture.
- It minimizes implementation risk for an accuracy-sensitive feature.
- It avoids betting early on a pure-Go PDF rendering path, which is the weak part of the Go ecosystem compared with orchestration and systems code.
- It keeps the future option open to swap render backends behind the same Go package boundary.

## Renderer Options

### Option A: Poppler CLI from Go

Status:

- Strongest near-term recommendation.

Pros:

- Mature renderer.
- Already installed locally in this environment.
- Easy to wrap from Go.
- Clear CLI surface for DPI, cropping, antialiasing, transparency, and output naming.
- Keeps the complex rendering engine outside the Go codebase while preserving a Go-first service.

Cons:

- Introduces an external runtime dependency.
- Production environments would need Poppler installed and versioned deliberately.
- Requires careful subprocess and timeout handling.

Notes:

- `pdftocairo` looks attractive for direct PNG generation and exposes image-quality controls such as `-antialias`, `-transp`, and `-icc`.
- `pdftoppm` exposes useful knobs such as `-hide-annotations`, `-thinlinemode`, `-aa`, and `-aaVector`.
- Which one is "more accurate" is document-dependent. This needs a corpus-based bake-off rather than a theoretical choice.

### Option B: `go-pdfium`

Status:

- Viable secondary option if a tighter Go-native integration is more important than the simplicity of Poppler CLI.

Pros:

- Official repo documents page rendering through bitmap output.
- Supports multiple backends with the same interface.
- WebAssembly mode avoids CGO and external PDFium installation.

Cons:

- Bigger operational and memory decision than just calling Poppler.
- Native modes still require PDFium installation and CGO.
- WebAssembly mode is documented as slower than native mode.

Notes:

- This is the most credible Go-first alternative if the team wants to avoid shelling out long-term.
- It is still not as operationally boring as Go + Poppler CLI.

### Option C: `go-fitz` / MuPDF

Status:

- Plausible, but less attractive than Poppler as the first implementation path.

Pros:

- Official package docs show direct page-to-image support, including PNG output helpers.
- Familiar wrapper shape from Go.

Cons:

- MuPDF binding introduces native dependency complexity.
- Package notes say bundled libraries are built without CJK fonts unless an external library is used.
- Package notes also say concurrent image or text extraction on the same document is not supported.

Notes:

- This could be fine for a controlled deployment, but it is not the lowest-risk handoff recommendation for a first accurate renderer.

### Option D: UniPDF

Status:

- Technically viable, but a commercial/product decision.

Pros:

- Official docs include PDF-to-image rendering examples.

Cons:

- Uses a license/API-key model.
- Adds commercial/vendor considerations immediately.

Notes:

- Worth considering only if the team is open to a licensed dependency and wants vendor support.

### Option E: `pdfcpu`

Status:

- Not recommended for this problem.

Reasoning:

- Based on the official repo/docs surface reviewed here, `pdfcpu` is clearly strong on PDF processing and manipulation workflows.
- I did not find an obvious page-raster rendering path in the official repo/docs reviewed during this pass.

This is an inference from the official project surface, not a claim that rendering is impossible in every form.

## Suggested Service Shape

The feature should probably look like a Go package or service boundary, not an ad hoc script.

Suggested shape:

- Input PDF stored under blob storage, for example:
  - `blob-storage/pdfs/<pdf_id>.pdf`
- Output directory stored under blob storage, for example:
  - `blob-storage/renders/<job_id>/page-0001.png`
  - `blob-storage/renders/<job_id>/page-0002.png`
  - `blob-storage/renders/<job_id>/manifest.json`

Suggested manifest contents:

- source PDF reference
- render backend used
- backend version if known
- render parameters such as DPI, crop mode, transparency, antialias choices
- page count
- per-page filename
- per-page width and height
- checksums
- started and completed timestamps

Suggested worker contract later:

- model emits a tool/function call for PDF rendering
- worker or tool runner executes the render job
- result comes back as one `function_call_output`
- output contains blob refs plus manifest metadata

Suggested idempotency rule:

- derive a stable job key from:
  - source PDF checksum
  - renderer backend
  - renderer version if available
  - render parameters
- if a complete manifest already exists for the same key, return the existing result instead of re-rendering

This matters because the repo's recovery model may replay work from the last safe checkpoint.

## Accuracy Considerations

For an "accurate process", the next person should explicitly test against a representative corpus:

- text-heavy PDFs
- scanned/image-only PDFs
- mixed vector + bitmap PDFs
- PDFs with transparency
- PDFs with annotations
- PDFs with unusual fonts
- PDFs with non-Latin or CJK fonts
- rotated pages
- very large pages and posters
- encrypted PDFs if those matter for the product

Comparison criteria:

- visual fidelity
- font rendering correctness
- transparency behavior
- annotation behavior
- predictable page dimensions
- performance and memory
- operational simplicity

## What I Would Do Next

If continuing this investigation, the next step should be a bake-off, not implementation:

1. Gather a small PDF corpus that reflects real documents.
2. Run the same files through `pdftocairo` and `pdftoppm` at the same DPI.
3. Compare output quality and note any rendering differences.
4. Decide whether Poppler CLI is good enough.
5. Only if Poppler is not good enough, evaluate `go-pdfium` as the next Go-first option.

## Sources

Repo files reviewed:

- `README.md`
- `ARCHITECTURE.md`
- `WORKER_RUNTIME.md`
- `LOCAL_DEV.md`
- `RECOVERY.md`
- `REDIS_MODEL.md`
- `internal/blobstore/local.go`
- `internal/platform/runtime.go`
- `internal/worker/actor.go`

External primary sources reviewed:

- `go-fitz`: https://pkg.go.dev/github.com/gen2brain/go-fitz
- `go-pdfium`: https://github.com/klippa-app/go-pdfium
- `pdfcpu`: https://github.com/pdfcpu/pdfcpu
- UniPDF PDF-to-image guide: https://docs.unidoc.io/docs/unipdf/guides/conversion/pdf-to-image/
- `pdftocairo` man page: https://manpages.debian.org/testing/poppler-utils/pdftocairo.1.en.html
