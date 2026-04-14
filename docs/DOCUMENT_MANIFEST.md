# Document Manifest Contract

## Decision

The thread runtime does not ingest raw PDFs and does not render pages.

That work happens in the document pipeline. The runtime consumes prepared document assets by reference.

## Boundary

### Document Pipeline

Responsible for:

- locating or receiving the source document
- rendering page assets
- writing the manifest
- storing assets in blob storage

### Thread Runtime

Responsible for:

- storing attached document links in Postgres
- reading manifest metadata when needed
- assigning page work to child threads
- combining child results back into the parent flow

## Storage Rule

- Large page assets stay in blob storage.
- Thread state keeps document IDs, manifest refs, and execution state.
- Durable thread history should keep semantic checkpoints, not page bytes.

## Manifest Shape

Required fields:

- `version`
- `document_id`
- `created_at`
- `page_count`
- `pages`

Required per-page fields:

- `page_number`
- `image_ref`
