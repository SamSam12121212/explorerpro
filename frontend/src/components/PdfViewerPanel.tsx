import { useEffect, useMemo, useRef, useState } from "react";
import { useSearchParams } from "react-router";
import {
  PDFViewer,
  PdfAnnotationSubtype,
  ZoomMode,
  type Command,
  type EmbedPdfContainer,
  type IconsConfig,
  type PluginRegistry,
  type ThemeConfig,
  type ToolbarItem,
  type UIPlugin,
  type UISchema,
} from "@embedpdf/react-pdf-viewer";
import {
  LockModeType,
  type AnnotationCapability,
  type AnnotationPlugin,
} from "@embedpdf/plugin-annotation";
import { PdfAnnotationBorderStyle } from "@embedpdf/models";
import type { CommandsPlugin } from "@embedpdf/plugin-commands";
import type { ScrollCapability, ScrollPlugin } from "@embedpdf/plugin-scroll";
import { LuRotateCcw, LuX } from "react-icons/lu";
import { apiGet, apiPatch } from "../api";
import { DEFAULT_MODEL, DEFAULT_REASONING, MODEL_OPTIONS, REASONING_OPTIONS } from "../constants";
import type { DocumentEntry, DocumentResponse, ReasoningEffort } from "../types";

const MAIN_TOOLBAR_ID = "main-toolbar";
const SIDEBAR_PANEL_ID = "sidebar-panel";
const PAGE_SETTINGS_MENU_ID = "page-settings-menu";
const ZOOM_MENU_ID = "zoom-menu";
const SIDEBAR_BUTTON_ID = "sidebar-button";
const PAGE_SETTINGS_BUTTON_ID = "page-settings-button";
const ZOOM_MENU_BUTTON_ID = "zoom-menu-button";
const SIDEBAR_COMMAND_ID = "explorer:toggle-pdf-sidebar";
const PAGE_SETTINGS_COMMAND_ID = "explorer:toggle-pdf-page-settings";
const ZOOM_MENU_COMMAND_ID = "explorer:toggle-pdf-zoom-menu";
const DETAILS_COMMAND_ID = "explorer-toggle-document-details";
const DETAILS_BUTTON_ID = "explorer-document-details-button";
const SIDEBAR_ICON_ID = "explorer-pdf-sidebar-icon";
const PAGE_SETTINGS_ICON_ID = "explorer-pdf-page-settings-icon";
const ZOOM_ICON_ID = "explorer-pdf-zoom-icon";
const DETAILS_ICON_ID = "explorer-pdf-details-icon";

const PDF_VIEWER_ICONS: IconsConfig = {
  [SIDEBAR_ICON_ID]: {
    viewBox: "-1 -1 26 26",
    paths: [
      { d: "M5 3h14a2 2 0 0 1 2 2v14a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2z", stroke: "primary" },
      { d: "M9 3v18", stroke: "primary" },
    ],
    strokeLinecap: "round",
    strokeLinejoin: "round",
    strokeWidth: 2,
  },
  [PAGE_SETTINGS_ICON_ID]: {
    viewBox: "-1 -1 26 26",
    paths: [
      { d: "M15 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V7Z", stroke: "primary" },
      { d: "M14 2v4a2 2 0 0 0 2 2h4", stroke: "primary" },
      { d: "M8 12h8", stroke: "primary" },
      { d: "M10 11v2", stroke: "primary" },
      { d: "M8 17h8", stroke: "primary" },
      { d: "M14 16v2", stroke: "primary" },
    ],
    strokeLinecap: "round",
    strokeLinejoin: "round",
    strokeWidth: 2,
  },
  [ZOOM_ICON_ID]: {
    viewBox: "-1 -1 26 26",
    paths: [
      { d: "M11 19a8 8 0 1 0 0-16a8 8 0 0 0 0 16z", stroke: "primary" },
      { d: "M21 21l-4.35-4.35", stroke: "primary" },
      { d: "M11 8v6", stroke: "primary" },
      { d: "M8 11h6", stroke: "primary" },
    ],
    strokeLinecap: "round",
    strokeLinejoin: "round",
    strokeWidth: 2,
  },
  [DETAILS_ICON_ID]: {
    viewBox: "-1 -1 26 26",
    paths: [
      { d: "M5 3h14a2 2 0 0 1 2 2v14a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2z", stroke: "primary" },
      { d: "M15 3v18", stroke: "primary" },
    ],
    strokeLinecap: "round",
    strokeLinejoin: "round",
    strokeWidth: 2,
  },
};

const MAIN_TOOLBAR_ITEMS: ToolbarItem[] = [
  {
    type: "group",
    id: "explorer-pdf-toolbar-left",
    alignment: "start",
    gap: 2,
    items: [
      {
        type: "command-button",
        id: SIDEBAR_BUTTON_ID,
        commandId: SIDEBAR_COMMAND_ID,
        variant: "icon",
        categories: ["panel", "panel-sidebar"],
      },
      {
        type: "command-button",
        id: PAGE_SETTINGS_BUTTON_ID,
        commandId: PAGE_SETTINGS_COMMAND_ID,
        variant: "icon",
        categories: ["page", "page-settings"],
      },
      {
        type: "command-button",
        id: ZOOM_MENU_BUTTON_ID,
        commandId: ZOOM_MENU_COMMAND_ID,
        variant: "icon",
        categories: ["zoom", "zoom-menu"],
      },
    ],
  },
  {
    type: "spacer",
    id: "explorer-pdf-toolbar-spacer",
    flex: true,
  },
  {
    type: "group",
    id: "explorer-pdf-toolbar-right",
    alignment: "end",
    gap: 2,
    items: [
      {
        type: "command-button",
        id: DETAILS_BUTTON_ID,
        commandId: DETAILS_COMMAND_ID,
        variant: "icon",
      },
    ],
  },
];

const PDF_TOOLBAR_ICON_PROPS = {
  primaryColor: "var(--explorer-color-text-disabled)",
};

const PDF_VIEWER_THEME: ThemeConfig = {
  preference: "dark",
  dark: {
    background: {
      app: "var(--explorer-color-app-bg)",
      surface: "var(--explorer-color-app-bg)",
      surfaceAlt: "var(--explorer-color-app-bg)",
      elevated: "var(--explorer-color-elevated)",
      overlay: "var(--explorer-color-overlay)",
      input: "var(--explorer-color-input)",
    },
    foreground: {
      primary: "var(--explorer-color-text-primary)",
      secondary: "var(--explorer-color-text-secondary)",
      muted: "var(--explorer-color-text-muted)",
      disabled: "var(--explorer-color-text-disabled)",
      onAccent: "#ffffff",
    },
    border: {
      default: "var(--explorer-color-border)",
      subtle: "var(--explorer-color-border-subtle)",
      strong: "var(--explorer-color-border-strong)",
    },
    accent: {
      primary: "var(--explorer-color-accent)",
      primaryHover: "var(--explorer-color-accent-hover)",
      primaryActive: "var(--explorer-color-accent-active)",
      primaryLight: "var(--explorer-color-accent-soft)",
      primaryForeground: "#ffffff",
    },
    interactive: {
      hover: "var(--explorer-color-hover)",
      active: "var(--explorer-color-active)",
      selected: "var(--explorer-color-selected)",
      focus: "var(--explorer-color-accent-hover)",
      focusRing: "var(--explorer-color-accent-soft)",
    },
    state: {
      error: "var(--explorer-color-error)",
      errorLight: "color-mix(in srgb, var(--explorer-color-error) 18%, transparent)",
      warning: "var(--explorer-color-warning)",
      warningLight: "color-mix(in srgb, var(--explorer-color-warning) 18%, transparent)",
      success: "var(--explorer-color-success)",
      successLight: "color-mix(in srgb, var(--explorer-color-success) 18%, transparent)",
      info: "var(--explorer-color-accent)",
      infoLight: "var(--explorer-color-accent-soft)",
    },
    scrollbar: {
      track: "transparent",
      thumb: "rgba(136, 136, 136, 0.4)",
      thumbHover: "rgba(136, 136, 136, 0.6)",
    },
    tooltip: {
      background: "var(--explorer-color-elevated)",
      foreground: "var(--explorer-color-text-primary)",
    },
  },
};

function buildToolbarPatch(schema: UISchema): Partial<UISchema> | null {
  const mainToolbar = Object.hasOwn(schema.toolbars, MAIN_TOOLBAR_ID)
    ? schema.toolbars[MAIN_TOOLBAR_ID]
    : null;
  if (mainToolbar === null) {
    return null;
  }

  return {
    toolbars: {
      [MAIN_TOOLBAR_ID]: {
        ...mainToolbar,
        responsive: { breakpoints: {} },
        items: MAIN_TOOLBAR_ITEMS,
      },
    },
  };
}

function buildToolbarCommands(onToggleDetails: () => void): Command[] {
  return [
    {
      id: SIDEBAR_COMMAND_ID,
      label: "Sidebar",
      icon: SIDEBAR_ICON_ID,
      iconProps: PDF_TOOLBAR_ICON_PROPS,
      categories: ["panel", "panel-sidebar"],
      action: ({ registry, documentId }) => {
        const ui = registry.getPlugin<UIPlugin>("ui")?.provides();
        ui?.forDocument(documentId).toggleSidebar("left", "main", SIDEBAR_PANEL_ID);
      },
    },
    {
      id: PAGE_SETTINGS_COMMAND_ID,
      label: "Page settings",
      icon: PAGE_SETTINGS_ICON_ID,
      iconProps: PDF_TOOLBAR_ICON_PROPS,
      categories: ["page", "page-settings"],
      action: ({ registry, documentId }) => {
        const ui = registry.getPlugin<UIPlugin>("ui")?.provides();
        ui?.forDocument(documentId).toggleMenu(
          PAGE_SETTINGS_MENU_ID,
          PAGE_SETTINGS_COMMAND_ID,
          PAGE_SETTINGS_BUTTON_ID,
        );
      },
    },
    {
      id: ZOOM_MENU_COMMAND_ID,
      label: "Zoom",
      icon: ZOOM_ICON_ID,
      iconProps: PDF_TOOLBAR_ICON_PROPS,
      categories: ["zoom", "zoom-menu"],
      action: ({ registry, documentId }) => {
        const ui = registry.getPlugin<UIPlugin>("ui")?.provides();
        ui?.forDocument(documentId).toggleMenu(
          ZOOM_MENU_ID,
          ZOOM_MENU_COMMAND_ID,
          ZOOM_MENU_BUTTON_ID,
        );
      },
    },
    {
      id: DETAILS_COMMAND_ID,
      label: "Details",
      icon: DETAILS_ICON_ID,
      iconProps: PDF_TOOLBAR_ICON_PROPS,
      action: onToggleDetails,
    },
  ];
}

const PDF_VIEWER_STYLE_OVERRIDES = `
  [data-epdf-i="${MAIN_TOOLBAR_ID}"] {
    border-bottom-width: 0 !important;
  }
  [data-epdf-i="${MAIN_TOOLBAR_ID}"] svg[role="img"] {
    width: 18px !important;
    height: 18px !important;
  }
`;

function handleContainerInit(container: EmbedPdfContainer) {
  const shadowRoot = container.shadowRoot;
  if (shadowRoot === null) {
    return;
  }

  const style = window.document.createElement("style");
  style.textContent = PDF_VIEWER_STYLE_OVERRIDES;
  shadowRoot.appendChild(style);
}

function handleViewerReady(registry: PluginRegistry, onToggleDetails: () => void) {
  const commandsPlugin = registry.getPlugin<CommandsPlugin>("commands");
  if (commandsPlugin !== null) {
    const commands = commandsPlugin.provides();
    for (const command of buildToolbarCommands(onToggleDetails)) {
      commands.registerCommand(command);
    }
  }

  const uiPlugin = registry.getPlugin<UIPlugin>("ui");
  if (uiPlugin !== null) {
    const ui = uiPlugin.provides();
    const patch = buildToolbarPatch(ui.getSchema());
    if (patch) {
      ui.mergeSchema(patch);
    }
  }
}

// Parse `?page=N` into a 1-based page number, or null if absent / malformed.
// Upper bound is clamped later against the document's totalPages (known only
// after the scroll plugin's layout is ready).
function parseTargetPage(value: string | null): number | null {
  if (value === null) return null;
  const parsed = Number.parseInt(value, 10);
  if (!Number.isFinite(parsed) || parsed < 1) return null;
  return parsed;
}

// A citation is a bounding box on a specific page, supplied in manifest pixel
// coordinates (top-left origin). The format matches what OCR tools like
// PaddleOCR emit directly — page-indexed, integer pixels. Conversion to
// embedpdf's PDF-point page-space happens at render time using the document's
// DPI (both share a top-left origin, so no y-flip).
export interface Citation {
  page: number;
  x: number;
  y: number;
  width: number;
  height: number;
}

// Repeated `?cite=page,x,y,w,h` params. Malformed entries are dropped
// silently so one bad cite doesn't poison the whole highlight set.
function parseCitations(values: string[]): Citation[] {
  const out: Citation[] = [];
  for (const raw of values) {
    const parts = raw.split(",");
    if (parts.length !== 5) continue;
    const nums = parts.map((p) => Number.parseFloat(p));
    if (nums.some((n) => !Number.isFinite(n))) continue;
    const [page, x, y, width, height] = nums;
    if (page < 1 || width <= 0 || height <= 0) continue;
    if (x < 0 || y < 0) continue;
    out.push({ page, x, y, width, height });
  }
  return out;
}

// Stand-in citations until a real producer wires this up. Toggle with
// `?mock=1` on the URL — lets us visually verify the overlay pipeline
// without constructing cite params by hand.
const MOCK_CITATIONS: Citation[] = [
  { page: 1, x: 200, y: 300, width: 900, height: 80 },
  { page: 1, x: 200, y: 500, width: 700, height: 60 },
  { page: 2, x: 150, y: 400, width: 1000, height: 120 },
];

// Manifest pixels → PDF points (embedpdf's page-space unit). Both are
// top-left-origin so no y-flip; it's a single uniform scale factor.
function pixelsToPoints(pixels: number, dpi: number): number {
  return (pixels * 72) / dpi;
}

const CITATION_COLOR = "#ffcc00";
const CITATION_OPACITY = 0.35;

// Jump to the target page if (a) the scroll capability is ready and (b) the
// target page falls within the document's bounds. `behavior: "instant"` avoids
// the smooth-scroll animation — we want link clicks to land immediately.
function jumpToTargetPage(
  scrollCap: ScrollCapability,
  documentId: string,
  target: number | null,
): void {
  if (target === null) return;
  const scope = scrollCap.forDocument(documentId);
  const totalPages = scope.getTotalPages();
  if (totalPages < 1 || target > totalPages) return;
  scope.scrollToPage({ pageNumber: target, behavior: "instant" });
}

function formatTimestamp(value?: string) {
  if (!value) {
    return "Not initialized yet";
  }

  const parsed = new Date(value);
  if (Number.isNaN(parsed.getTime())) {
    return value;
  }

  return parsed.toLocaleString();
}

interface PdfViewerPanelProps {
  documentId: string;
}

export function PdfViewerPanel({ documentId }: PdfViewerPanelProps) {
  const [detailsOpen, setDetailsOpen] = useState(false);
  const [document, setDocument] = useState<DocumentEntry | null>(null);
  const [selectedModel, setSelectedModel] = useState(DEFAULT_MODEL);
  const [selectedReasoning, setSelectedReasoning] = useState<ReasoningEffort>(DEFAULT_REASONING as ReasoningEffort);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const [statusMessage, setStatusMessage] = useState("");

  // `?page=N` on the URL lands the viewer on that page. The scroll plugin
  // builds its per-document state during the load → layout-ready sequence,
  // so calling forDocument(id).getTotalPages() before that fires throws
  // "Scroll state not found". We therefore gate the jump on an explicit
  // "ready for this doc" flag that `onLayoutReady` flips below.
  //
  // One useEffect handles both cases via its deps:
  //   - Different-doc navigation: documentId changes → PDFViewer remounts
  //     (key={documentId}), reset effect clears the flag, new onLayoutReady
  //     flips it back, effect fires and jumps.
  //   - Same-doc navigation: pageParam changes while flag is still true
  //     (doc stayed loaded), effect fires and jumps.
  const [searchParams] = useSearchParams();
  const pageParam = searchParams.get("page");
  const mockCitations = searchParams.get("mock") === "1";
  const citeParams = searchParams.getAll("cite");
  const scrollCapRef = useRef<ScrollCapability | null>(null);
  const annotationCapRef = useRef<AnnotationCapability | null>(null);
  const [isScrollReady, setIsScrollReady] = useState(false);

  // Parsing depends only on the raw param strings, not array identity — memo
  // so the draw effect below isn't re-running on every render.
  const citations = useMemo<Citation[]>(() => {
    if (mockCitations) return MOCK_CITATIONS;
    return parseCitations(citeParams);
    // Join keeps the effect's reactivity without citing the unstable array.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [mockCitations, citeParams.join("|")]);

  useEffect(() => {
    // PDFViewer's key={documentId} remounts the viewer on doc change and
    // its plugin registry is torn down — any captured capability becomes
    // stale. Clear the flag so the jump effect waits for the fresh
    // onLayoutReady before touching the new registry.
    setIsScrollReady(false);
    scrollCapRef.current = null;
    annotationCapRef.current = null;
  }, [documentId]);

  useEffect(() => {
    if (!isScrollReady) return;
    const scrollCap = scrollCapRef.current;
    if (scrollCap === null) return;
    jumpToTargetPage(scrollCap, documentId, parseTargetPage(pageParam));
  }, [pageParam, documentId, isScrollReady]);

  // Paint citation rects as ephemeral Square annotations. We gate on:
  //   - isScrollReady (per-doc layout is live)
  //   - annotationCapRef (registry captured in onReady)
  //   - document loaded AND matching the current route (otherwise we could
  //     draw using a previous doc's DPI during a doc-switch race)
  // Cleanup purges what this run created — `autoCommit: false` keeps them
  // out of the PDF file, and `purgeAnnotation` drops state without
  // round-tripping to PDFium. The viewer remount on docId change also
  // tears everything down for free; this cleanup covers same-doc cite
  // param changes.
  const docDpi = document?.dpi ?? 0;
  const docMatchesRoute = document !== null && document.id.toString() === documentId;
  useEffect(() => {
    if (!isScrollReady) return;
    if (!docMatchesRoute) return;
    if (docDpi <= 0) return;
    const annotationCap = annotationCapRef.current;
    if (annotationCap === null) return;
    if (citations.length === 0) return;

    const scope = annotationCap.forDocument(documentId);
    const created: { pageIndex: number; id: string }[] = [];
    for (const cite of citations) {
      const id = crypto.randomUUID();
      const pageIndex = cite.page - 1;
      const rect = {
        origin: {
          x: pixelsToPoints(cite.x, docDpi),
          y: pixelsToPoints(cite.y, docDpi),
        },
        size: {
          width: pixelsToPoints(cite.width, docDpi),
          height: pixelsToPoints(cite.height, docDpi),
        },
      };
      scope.createAnnotation(pageIndex, {
        id,
        type: PdfAnnotationSubtype.SQUARE,
        pageIndex,
        rect,
        flags: ["readOnly", "locked", "print"],
        color: CITATION_COLOR,
        opacity: CITATION_OPACITY,
        strokeWidth: 0,
        strokeColor: CITATION_COLOR,
        strokeStyle: PdfAnnotationBorderStyle.SOLID,
      });
      created.push({ pageIndex, id });
    }

    return () => {
      const cap = annotationCapRef.current;
      if (cap === null) return;
      const cleanupScope = cap.forDocument(documentId);
      for (const entry of created) {
        cleanupScope.purgeAnnotation(entry.pageIndex, entry.id);
      }
    };
  }, [citations, documentId, isScrollReady, docMatchesRoute, docDpi]);

  useEffect(() => {
    let cancelled = false;

    async function loadDocument() {
      setLoading(true);
      setError("");
      setStatusMessage("");
      try {
        const response = await apiGet<DocumentResponse>(`/documents/${documentId}`);
        if (cancelled) {
          return;
        }
        setDocument(response.document);
        setSelectedModel(response.document.query_model || DEFAULT_MODEL);
        setSelectedReasoning((response.document.query_reasoning || DEFAULT_REASONING) as ReasoningEffort);
      } catch (err) {
        if (cancelled) {
          return;
        }
        setError(err instanceof Error ? err.message : "Failed to load document details");
        setDocument(null);
        setSelectedModel(DEFAULT_MODEL);
        setSelectedReasoning(DEFAULT_REASONING as ReasoningEffort);
      } finally {
        if (!cancelled) {
          setLoading(false);
        }
      }
    }

    void loadDocument();
    return () => {
      cancelled = true;
    };
  }, [documentId]);

  const hasBaseResponse = Boolean(document?.base_response_id);
  const currentModel = document?.query_model ?? DEFAULT_MODEL;
  const currentReasoning = (document?.query_reasoning ?? DEFAULT_REASONING) as ReasoningEffort;
  const settingsChanged = currentModel !== selectedModel || currentReasoning !== selectedReasoning;
  const baseModelMismatch =
    Boolean(document?.base_response_id) &&
    Boolean(document?.base_model) &&
    document?.base_model !== selectedModel;
  const baseReasoningMismatch =
    Boolean(document?.base_response_id) &&
    Boolean(document?.base_reasoning) &&
    document?.base_reasoning !== selectedReasoning;

  async function saveDocumentSettings(clearBaseResponse: boolean) {
    setSaving(true);
    setError("");
    setStatusMessage("");
    try {
      const response = await apiPatch<DocumentResponse>(`/documents/${documentId}`, {
        query_model: selectedModel,
        query_reasoning: selectedReasoning,
        clear_base_response: clearBaseResponse,
      });
      setDocument(response.document);
      setSelectedModel(response.document.query_model);
      setSelectedReasoning((response.document.query_reasoning || DEFAULT_REASONING) as ReasoningEffort);
      setStatusMessage(
        clearBaseResponse ? "Base response cleared." : "Document settings updated.",
      );
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to update document");
    } finally {
      setSaving(false);
    }
  }

  return (
    <div className="flex h-full w-full min-h-0 bg-[#1e1e1e]">
      <div className="relative min-h-0 flex-1 overflow-hidden bg-[#1e1e1e]">
        <PDFViewer
          key={documentId}
          className="absolute inset-0 h-full w-full"
          config={{
            documentManager: {
              initialDocuments: [{ documentId, url: `/documents/${documentId}/source` }],
            },
            icons: PDF_VIEWER_ICONS,
            theme: PDF_VIEWER_THEME,
            zoom: {
              defaultZoomLevel: ZoomMode.FitWidth,
            },
            // Ephemeral programmatic rects only — annotations live in Redux
            // state, never flushed to the PDF file. `locked: All` makes them
            // non-interactive (clicks pass through, no drag/resize handles).
            annotations: {
              autoCommit: false,
              locked: { type: LockModeType.All },
            },
            disabledCategories: [
              "form",
              "redaction",
              "document-open",
              "document-close",
              "document-protect",
              "document-export",
              "document-menu",
              "document-print",
              "document-capture",
              "history",
              "mode-insert",
              "panel-comment",
              "panel-search",
              "tools",
              "selection",
            ],
          }}
          onInit={handleContainerInit}
          onReady={(registry) => {
            handleViewerReady(registry, () => {
              setDetailsOpen((current) => !current);
            });

            const annotationPlugin = registry.getPlugin<AnnotationPlugin>("annotation");
            if (annotationPlugin !== null) {
              annotationCapRef.current = annotationPlugin.provides();
            }

            const scrollPlugin = registry.getPlugin<ScrollPlugin>("scroll");
            if (scrollPlugin === null) return;
            const scrollCap = scrollPlugin.provides();

            // Don't capture the scroll capability until layout is ready for
            // THIS document. The scroll plugin only populates its per-doc
            // state during the load → layout-ready sequence, so reading
            // anything via forDocument(id) before this fires throws.
            scrollCap.onLayoutReady((event) => {
              if (!event.isInitial) return;
              if (event.documentId !== documentId) return;
              scrollCapRef.current = scrollCap;
              setIsScrollReady(true);
            });
          }}
        />
      </div>

      {detailsOpen && (
        <aside className="flex h-full w-[360px] min-w-[320px] max-w-[42vw] flex-col border-l border-[#333] bg-[#181818]">
          <div className="flex items-start justify-between gap-3 border-b border-[#2a2a2a] px-4 py-4">
            <div className="min-w-0">
              <p className="text-[0.68rem] font-semibold uppercase tracking-[0.2em] text-[#666]">
                Document Details
              </p>
              <h2 className="mt-1 truncate text-sm font-semibold text-[#f2f2f2]">
                {document?.filename.trim() ?? documentId}
              </h2>
            </div>
            <button
              className="flex h-8 w-8 items-center justify-center text-[#7f7f7f] transition hover:text-[#f2f2f2]"
              onClick={() => {
                setDetailsOpen(false);
              }}
              title="Close details"
              type="button"
            >
              <LuX className="h-4 w-4" />
            </button>
          </div>

          <div className="min-h-0 flex-1 overflow-y-auto px-4 py-4">
            {loading && (
              <p className="text-sm text-[#777]">
                Loading document details…
              </p>
            )}

            {!loading && document && (
              <div className="space-y-8">
                <section>
                  <p className="text-[0.68rem] font-semibold uppercase tracking-[0.18em] text-[#666]">
                    Summary
                  </p>
                  <dl className="mt-3 space-y-2 text-sm">
                    <div className="flex items-center justify-between gap-4">
                      <dt className="text-[#777]">Status</dt>
                      <dd className="text-[#d8d8d8]">{document.status}</dd>
                    </div>
                    <div className="flex items-center justify-between gap-4">
                      <dt className="text-[#777]">Pages</dt>
                      <dd className="text-[#d8d8d8]">{document.page_count.toString()}</dd>
                    </div>
                    <div className="flex items-center justify-between gap-4">
                      <dt className="text-[#777]">Updated</dt>
                      <dd className="text-right text-[#d8d8d8]">
                        {formatTimestamp(document.updated_at)}
                      </dd>
                    </div>
                  </dl>
                </section>

                <section className="border-t border-[#242424] pt-6">
                  <p className="text-[0.68rem] font-semibold uppercase tracking-[0.18em] text-[#666]">
                    Query Settings
                  </p>
                  <span className="mt-2 block text-xs text-[#8a8a8a]">
                    New document threads start with these settings. Threads that already have
                    document lineage keep using the settings they started with.
                  </span>
                  <label className="mt-3 block">
                    <span className="mb-1 block text-xs text-[#8a8a8a]">Model</span>
                    <select
                      className="w-full border-b border-[#333] bg-transparent px-0 py-2 text-sm text-[#e6e6e6] outline-none transition focus:border-[#007acc]"
                      disabled={saving}
                      onChange={(event) => {
                        setSelectedModel(event.target.value);
                        setStatusMessage("");
                      }}
                      value={selectedModel}
                    >
                      {MODEL_OPTIONS.map((option) => (
                        <option key={option.value} value={option.value}>
                          {option.label}
                        </option>
                    ))}
                    </select>
                  </label>
                  <label className="mt-3 block">
                    <span className="mb-1 block text-xs text-[#8a8a8a]">Reasoning</span>
                    <select
                      className="w-full border-b border-[#333] bg-transparent px-0 py-2 text-sm text-[#e6e6e6] outline-none transition focus:border-[#007acc]"
                      disabled={saving}
                      onChange={(event) => {
                        setSelectedReasoning(event.target.value as ReasoningEffort);
                        setStatusMessage("");
                      }}
                      value={selectedReasoning}
                    >
                      {REASONING_OPTIONS.map((option) => (
                        <option key={option.value} value={option.value}>
                          {option.label}
                        </option>
                      ))}
                    </select>
                  </label>
                  <button
                    className="mt-4 inline-flex items-center justify-center text-xs font-semibold uppercase tracking-[0.16em] text-[#67b7ff] transition hover:text-[#8fc9ff] disabled:cursor-not-allowed disabled:opacity-40"
                    disabled={saving || !settingsChanged}
                    onClick={() => {
                      void saveDocumentSettings(false);
                    }}
                    type="button"
                  >
                    {saving ? "Saving…" : "Save Settings"}
                  </button>
                </section>

                <section className="border-t border-[#242424] pt-6">
                  <div className="flex items-center justify-between gap-3">
                    <div>
                      <p className="text-[0.68rem] font-semibold uppercase tracking-[0.18em] text-[#666]">
                        Base Response
                      </p>
                      <p className="mt-2 text-sm text-[#d8d8d8]">
                        {hasBaseResponse ? "Initialized" : "Not initialized"}
                      </p>
                    </div>
                    <button
                      className="inline-flex items-center gap-2 text-xs font-semibold uppercase tracking-[0.14em] text-[#f1b2b2] transition hover:text-[#ffd0d0] disabled:cursor-not-allowed disabled:opacity-40"
                      disabled={saving || !hasBaseResponse}
                      onClick={() => {
                        void saveDocumentSettings(true);
                      }}
                      type="button"
                    >
                      <LuRotateCcw className="h-3.5 w-3.5" />
                      <span>Clear Base</span>
                    </button>
                  </div>

                  <div className="mt-4 space-y-4 text-sm">
                    <div>
                      <p className="text-xs uppercase tracking-[0.14em] text-[#666]">
                        Response ID
                      </p>
                      <p className="mt-1 break-all font-mono text-[0.78rem] text-[#d8d8d8]">
                        {document.base_response_id ?? "None"}
                      </p>
                    </div>
                    <div>
                      <p className="text-xs uppercase tracking-[0.14em] text-[#666]">
                        Base Model
                      </p>
                      <p className="mt-1 text-[#d8d8d8]">
                        {document.base_model ?? "Not initialized"}
                      </p>
                    </div>
                    <div>
                      <p className="text-xs uppercase tracking-[0.14em] text-[#666]">
                        Base Reasoning
                      </p>
                      <p className="mt-1 text-[#d8d8d8]">
                        {document.base_reasoning ?? "Not initialized"}
                      </p>
                    </div>
                    <div>
                      <p className="text-xs uppercase tracking-[0.14em] text-[#666]">
                        Initialized At
                      </p>
                      <p className="mt-1 text-[#d8d8d8]">
                        {formatTimestamp(document.base_initialized_at)}
                      </p>
                    </div>
                  </div>

                  {(baseModelMismatch || baseReasoningMismatch) && (
                    <p className="mt-3 text-xs leading-5 text-[#d3b171]">
                      The saved shared base response was created with{" "}
                      {[document.base_model, document.base_reasoning].filter(Boolean).join(" / ")}. New
                      threads will rebuild a fresh base response for{" "}
                      {[selectedModel, selectedReasoning].join(" / ")}, while
                      existing thread-local lineages stay on their current settings.
                    </p>
                  )}
                  <p className="mt-3 text-xs leading-5 text-[#8a8a8a]">
                    Clearing the base response only removes the shared document anchor. Existing
                    thread-local lineages are preserved.
                  </p>
                </section>
              </div>
            )}

            {!loading && !document && !error && (
              <p className="text-sm text-[#777]">
                Document details are unavailable.
              </p>
            )}
          </div>

          {(error || statusMessage) && (
            <div className="border-t border-[#2a2a2a] px-4 py-3">
              {error && (
                <p className="text-xs text-red-400">{error}</p>
              )}
              {!error && statusMessage && (
                <p className="text-xs text-emerald-400">{statusMessage}</p>
              )}
            </div>
          )}
        </aside>
      )}
    </div>
  );
}
