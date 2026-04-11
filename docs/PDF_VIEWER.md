# PDF Viewer

Embedded PDF viewer in the middle panel, powered by [EmbedPDF](https://www.embedpdf.com/) (MIT license, uses Google Chrome's PDFium engine via WebAssembly).

## Why EmbedPDF

We evaluated three options:

| Library | Status | Notes |
|---|---|---|
| `react-pdf` (wojtekmaj) | Active, MIT | Bare renderer only — no toolbar, search, zoom, thumbnails. You build everything yourself. |
| `@react-pdf-viewer/core` | **Archived Mar 2026** | Was the best full-featured PDF.js wrapper, but the repo is dead. Do not use. |
| `@embedpdf/react-pdf-viewer` | Active, MIT | Drop-in viewer with full UI, or headless mode for custom builds. Uses PDFium (Chrome's engine). |

EmbedPDF was chosen because:
- Free and open source (MIT)
- Full-featured out of the box (zoom, search, thumbnails, page navigation)
- Headless mode available for future bounding box / SVG overlay work
- Actively maintained (v2.3.0+, 3,200+ GitHub stars)
- PDFium rendering matches Chrome's native PDF quality

## Packages

```
@embedpdf/react-pdf-viewer   # drop-in React viewer component
@embedpdf/engines             # rendering engine abstraction (auto-dependency)
@embedpdf/pdfium              # PDFium WASM engine (auto-dependency)
```

## Architecture

### Routing

The viewer route is `/doc/:documentId`. This prefix was chosen to avoid conflicts with the `/documents` Vite proxy rule (which forwards to the backend API).

```
/doc/:documentId  →  MidPanelHost  →  PdfViewerPanel  →  <PDFViewer>
```

The left sidebar's "Documents" tab stays active when on `/doc/*` routes.

### Backend endpoint

`GET /documents/:id/source` — serves the raw PDF binary from blob storage.

Defined in `internal/httpserver/document_api.go` as `handleDocumentSource`. Reads the document's `source_ref` from the database, resolves it via the blob store, and writes the bytes with `Content-Type: application/pdf`.

### Frontend components

**`PdfViewerPanel`** (`frontend/src/components/PdfViewerPanel.tsx`)

Wraps EmbedPDF's `<PDFViewer>` in a container sized via `absolute inset-0` (required because `react-resizable-panels` doesn't propagate percentage heights reliably on initial mount).

Important implementation detail: the `<PDFViewer>` mount node itself must also get explicit full-size dimensions (`className="absolute inset-0 h-full w-full"`). Sizing only an outer wrapper can leave EmbedPDF measuring a `0x0` target on first paint, which results in a blank document area until some later UI interaction triggers a relayout.

The viewer is keyed by `documentId` so it remounts when navigating between `/doc/:documentId` routes. This is necessary because `@embedpdf/react-pdf-viewer` initializes once in a `useEffect([])` and does not react to config changes after mount.

Configuration:
- Dark theme overridden to match Explorer's existing dark surfaces, borders, and text tokens
- All editing categories disabled (annotations, shapes, forms, redaction, stamps, etc.)
- Top-left document menu removed from the main toolbar
- Top-right search and comment buttons removed from the main toolbar
- Insert mode hidden (`mode-insert`) so the overflow menu stays view-only
- View-only mode: zoom, search, thumbnails, page navigation

Toolbar cleanup is applied with a small runtime schema patch in `onReady`, which strips:
- `document-menu-button` and its adjacent `divider-1` from the left toolbar group
- `search-button` and `comment-button` from the right toolbar group

The corresponding categories (`document-menu`, `panel-search`, `panel-comment`) are also disabled so those controls do not flash before the schema patch runs.

Theme colors are supplied via EmbedPDF's `theme.dark` overrides and are mapped to Explorer tokens from `frontend/src/styles.css` (`--explorer-color-app-bg`, `--explorer-color-surface`, `--explorer-color-border`, `--explorer-color-text-primary`, `--explorer-color-accent`, etc.). This avoids the default navy EmbedPDF chrome and keeps the viewer aligned with the rest of the app shell.

**`MidPanelHost`** (`frontend/src/components/MidPanelHost.tsx`)

Reads the `documentId` route param. When present, renders `PdfViewerPanel` instead of the collection view or empty state.

### Click-to-open wiring

Both `DocumentsView` and `CollectionDetailView` navigate to `/doc/:documentId` when a document row is clicked (only for documents with `status: "ready"`).

## Disabled EmbedPDF categories

The viewer runs in read-only mode. These categories are disabled:

```
annotation, annotation-shape, form, redaction,
document-open, document-close, document-protect,
document-export, document-menu, document-print, document-capture,
history, mode-insert, panel-comment, panel-search,
tools, selection
```

This leaves only the "View" tab with zoom controls, page spreads, and navigation.

## Future: bounding box overlays

EmbedPDF supports a headless mode (`@embedpdf/react-pdf-viewer` headless components) that gives full control over page rendering. The plan for bounding boxes:

1. Switch from the drop-in viewer to headless components for the page rendering layer
2. Each page renders inside a container with `position: relative`
3. Layer an `<svg>` element with `position: absolute; inset: 0` over each page
4. Draw `<rect>` elements for bounding boxes, mapping PDF coordinates to pixel coordinates via the page viewport scale
5. On zoom/resize, recalculate SVG positions from the updated viewport

This does not require replacing EmbedPDF — the headless mode uses the same PDFium engine underneath.
