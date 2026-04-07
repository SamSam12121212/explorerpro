# Image Input Contract

## Decision

The engine does not treat pasted images, repo images, or future image sources as different runtime concepts.

They all become the same thing first:

- a blob-backed image artifact
- a strict runtime image reference

This is the hard boundary for v1.

## Why

- Keeps ingestion concerns out of the thread engine
- Makes pasted images and repo images behave identically once ingested
- Preserves the runtime rule that workers own orchestration, not source-specific processing
- Lets us change storage and upload paths later without changing the engine contract
- Keeps OpenAI wire-format concerns at the edge instead of in thread state

## Ownership Boundary

### External Image Adapters

Responsible for:

- receiving an ad hoc pasted image, repo image, or future external image source
- storing the original bytes in blob storage
- determining basic metadata such as content type
- producing a runtime image reference

### Agent Runtime

Responsible for:

- accepting runtime image references
- storing only image references and metadata in thread state
- passing image references through thread execution like any other semantic artifact
- deciding when an image must be sent to OpenAI

### OpenAI Egress Adapter

Responsible for:

- loading image bytes from blob storage when needed
- converting bytes to a base64 data URL if needed for OpenAI input
- constructing Responses-compatible `input_image` content items

## Core Rule

Base64 is transport format, not engine state.

The engine should not persist base64 image payloads in Redis or thread input history as its source of truth.

The source of truth is the blob-backed image artifact.

Base64 should only be materialized at the OpenAI boundary when building a `response.create` payload.

## Blob Layout

Suggested v1 layout:

- `blob-storage/images/<image_id>/source.<ext>`

Optional later sidecars:

- `blob-storage/images/<image_id>/metadata.json`

The runtime should treat these as blob references, not assume local filesystem paths forever.

## Runtime Image Artifact Shape

Required fields:

- `image_id`
- `image_ref`
- `content_type`

Recommended fields:

- `filename`
- `sha256`
- `bytes`
- `width`
- `height`
- `metadata`

Example:

```json
{
  "image_id": "img_123",
  "image_ref": "blob://images/img_123/source.png",
  "content_type": "image/png",
  "filename": "screenshot.png",
  "sha256": "abc123",
  "bytes": 482919,
  "width": 1440,
  "height": 900,
  "metadata": {
    "origin": "clipboard"
  }
}
```

## Runtime Input Shape

The engine-facing input item should be a semantic image reference, not an OpenAI-specific transport payload.

Suggested message content shape:

```json
{
  "type": "message",
  "role": "user",
  "content": [
    {
      "type": "input_text",
      "text": "What is in this image?"
    },
    {
      "type": "image_ref",
      "image_ref": "blob://images/img_123/source.png",
      "content_type": "image/png",
      "filename": "screenshot.png"
    }
  ]
}
```

This keeps the engine contract stable even if the OpenAI-side transport changes later.

## OpenAI Lowering Rule

Right before `response.create`, runtime image references should be lowered into Responses-compatible image items.

For local blob-backed images, the default v1 lowering should be:

- load bytes from `image_ref`
- base64 encode the bytes
- construct a `data:<content_type>;base64,...` URL
- send as `type: "input_image"` with `detail: "auto"`

Example wire shape:

```json
{
  "type": "message",
  "role": "user",
  "content": [
    {
      "type": "input_text",
      "text": "What is in this image?"
    },
    {
      "type": "input_image",
      "image_url": "data:image/png;base64,...",
      "detail": "auto"
    }
  ]
}
```

Future egress variants may instead use:

- a fully qualified URL
- an OpenAI `file_id`

Those are adapter changes, not engine-contract changes.

## Continuation Rule

Image replay depends on response-chain continuity, not socket continuity.

If a thread can continue with a valid `previous_response_id`, earlier image context should be reused without rebuilding and resending the image.

An image should only need to be rebuilt and resent when continuity is broken, for example:

- a brand new thread
- a branch point that did not already include that image
- a recovery path that cannot rely on prior response context
- a failure that invalidates `previous_response_id`

WebSocket reconnection alone should not force image replay if `previous_response_id` continuity still exists.

## Redis Rule

Redis stores:

- image references
- image metadata
- thread execution state

Redis does not store:

- raw image bytes
- base64 data URLs as durable source of truth
- ingestion-specific adapter state unless needed for audit/debug metadata

## Relationship To PDFs

Images are atomic artifacts.

PDFs are not.

PDFs remain under the separate prepared document package contract in [DOCUMENT_MANIFEST.md](/Users/detachedhead/explorer/DOCUMENT_MANIFEST.md):

- manifest-driven
- one PNG per page
- page-aware
- potentially wrapped in page-position-aware prompt structures later

That distinction is intentional.

The image contract should stay simple and atomic.

## Locked-In v1 Position

- pasted images and repo images become the same blob-backed image artifact
- the engine accepts semantic image references, not ingestion-path-specific payloads
- base64 is generated only at OpenAI send time
- `previous_response_id` continuity avoids unnecessary image replay
- PDF handling remains separate from generic image handling

## References

- OpenAI Responses API create: [platform docs](https://platform.openai.com/docs/api-reference/responses/create)
- OpenAI Images and vision guide: [platform docs](https://platform.openai.com/docs/guides/images-vision?api-mode=responses)
- Vendored SDK image types: [response.go](/Users/detachedhead/explorer/openai-go/responses/response.go#L9336)
