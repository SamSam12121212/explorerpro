# Image Input Contract

## Decision

All image sources become the same runtime artifact first:

- a blob-backed image asset
- a strict runtime image reference

The engine stores references and metadata, not transport payloads.

## Ownership Boundary

### External Image Adapters

Responsible for:

- accepting pasted, uploaded, or repository-backed images
- storing original bytes in blob storage
- producing the runtime image reference and metadata

### Agent Runtime

Responsible for:

- accepting image references in thread input
- storing only refs and metadata in thread state
- deciding when an image must be sent to OpenAI

### OpenAI Egress Adapter

Responsible for:

- loading bytes from blob storage
- materializing transport format only at send time
- building Responses-compatible `input_image` items

## Core Rule

Base64 is transport format, not runtime truth.

Keep image bytes in blob storage. Keep thread state and thread history focused on semantic refs and recoverable checkpoints.
