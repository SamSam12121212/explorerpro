# Document Manifest Contract

## Decision

The agent runtime does not ingest raw PDFs and does not trigger PDF extraction or page rendering.

That work happens outside the thread engine.

The runtime consumes a prepared document package that already contains:

- a manifest
- one PNG per page

This is a hard boundary for v1.

## Why

- Keeps the thread engine focused on orchestration, continuation, and recovery
- Prevents document processing from polluting worker/socket logic
- Makes the document pipeline independently replaceable
- Fits the current runtime model where workers should only own thread execution concerns
- Works cleanly both on a local Mac and inside a Linux container

## Ownership Boundary

### External Document Pipeline

Responsible for:

- receiving or locating the source PDF
- splitting the PDF into one PNG per page
- writing all artifacts into blob storage
- writing the document manifest

### Agent Runtime

Responsible for:

- accepting a document manifest reference
- loading page metadata from the manifest
- assigning page ranges or page sets to child threads
- storing only references and execution state in Redis
- synthesizing final answers from child results

## Blob Layout

Suggested v1 layout:

- `blob-storage/documents/<document_id>/manifest.json`
- `blob-storage/documents/<document_id>/pages/page-0001.png`
- `blob-storage/documents/<document_id>/pages/page-0002.png`

The runtime should treat these as blob references, not assume direct local paths forever.

For local development, repo-relative paths are fine.

Packaging rule:

- an `N` page PDF becomes `N + 1` files
- one PNG per page
- one manifest JSON

For example:

- a 10-page PDF becomes 11 files
- 10 PNG files
- 1 manifest JSON

Nothing else is part of the runtime contract in v1.

## Manifest Shape

The runtime-facing contract should stay simple and explicit.

Required top-level fields:

- `version`
- `document_id`
- `created_at`
- `page_count`
- `pages`

Recommended top-level fields:

- `assets_root_ref`
- `render_backend`
- `render_backend_version`
- `render_params`
- `metadata`

Required per-page fields:

- `page_number`
- `image_ref`

Recommended per-page fields:

- `width`
- `height`
- `rotation`
- `sha256`
- `content_type`
- `metadata`

Example:

```json
{
  "version": "v1",
  "document_id": "doc_123",
  "created_at": "2026-03-13T22:00:00Z",
  "page_count": 3,
  "assets_root_ref": "blob://documents/doc_123/",
  "render_backend": "poppler/pdftocairo",
  "render_backend_version": "26.03.0",
  "render_params": {
    "format": "png",
    "dpi": 200
  },
  "metadata": {
    "tenant": "local-dev",
    "title": "Quarterly Report"
  },
  "pages": [
    {
      "page_number": 1,
      "image_ref": "blob://documents/doc_123/pages/page-0001.png",
      "width": 1654,
      "height": 2339,
      "rotation": 0,
      "content_type": "image/png",
      "sha256": "abc123"
    },
    {
      "page_number": 2,
      "image_ref": "blob://documents/doc_123/pages/page-0002.png",
      "width": 1654,
      "height": 2339,
      "rotation": 0,
      "content_type": "image/png",
      "sha256": "def456"
    },
    {
      "page_number": 3,
      "image_ref": "blob://documents/doc_123/pages/page-0003.png",
      "width": 1654,
      "height": 2339,
      "rotation": 0,
      "content_type": "image/png",
      "sha256": "ghi789"
    }
  ]
}
```

## Runtime Input Shape

The runtime should accept a manifest reference, not a raw PDF payload.

Example thread metadata shape:

```json
{
  "document_id": "doc_123",
  "document_manifest_ref": "blob://documents/doc_123/manifest.json"
}
```

The worker can load the manifest at execution time or through a document-aware tool layer later, but the thread actor should never be responsible for page rendering.

## Child Assignment Contract

Subagent fan-out should pass page scope explicitly.

Recommended child payload fields:

- `document_id`
- `document_manifest_ref`
- `page_numbers`
- `page_start`
- `page_end`
- `page_refs`
- `task`

Example child work unit:

```json
{
  "document_id": "doc_123",
  "document_manifest_ref": "blob://documents/doc_123/manifest.json",
  "page_start": 1,
  "page_end": 10,
  "page_numbers": [1, 2, 3, 4, 5, 6, 7, 8, 9, 10],
  "page_refs": [
    "blob://documents/doc_123/pages/page-0001.png",
    "blob://documents/doc_123/pages/page-0002.png"
  ],
  "task": "Inspect these pages for material findings and return concise evidence-backed notes."
}
```

The runtime may later optimize this shape, but explicit page assignment is the correct v1 contract.

## Redis Rule

Redis stores:

- document references
- manifest references
- execution metadata
- child result references

Redis does not store:

- page PNG binaries
- raw PDF bytes
- extracted page text sidecars

## Container Rule

The contract must remain portable between:

- local development on macOS
- Linux container execution

That means:

- no runtime dependence on Mac-specific paths
- no hard-coded host filesystem assumptions outside `BLOB_STORAGE_DIR`
- manifest and page assets should be referenced in a storage-agnostic way

## Locked-In v1 Position

- The engine is not a PDF extraction engine.
- The engine is not a renderer.
- The engine consumes prepared document packages.
- V1 document packages are exactly one manifest plus one PNG per page.
- PNG-per-page is the current expected page asset format.
- No OCR or extracted text sidecars are part of the runtime contract yet.
- A separate Go service may eventually prepare those packages, but it is outside the thread runtime.
