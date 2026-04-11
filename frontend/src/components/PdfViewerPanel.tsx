import { useEffect, useState } from "react";
import {
  PDFViewer,
  type PluginRegistry,
  type ThemeConfig,
  type ToolbarItem,
  type UIPlugin,
  type UISchema,
} from "@embedpdf/react-pdf-viewer";
import type { CommandsPlugin } from "@embedpdf/plugin-commands";
import { LuRotateCcw, LuX } from "react-icons/lu";
import { apiGet, apiPatch } from "../api";
import { DEFAULT_MODEL, MODEL_OPTIONS } from "../constants";
import type { DocumentEntry, DocumentResponse } from "../types";

const MAIN_TOOLBAR_ID = "main-toolbar";
const DETAILS_COMMAND_ID = "explorer-toggle-document-details";
const DETAILS_BUTTON_ID = "explorer-document-details-button";
const DETAILS_DIVIDER_ID = "explorer-document-details-divider";
const GROUP_ITEM_IDS_TO_HIDE = new Map([
  ["left-group", new Set(["document-menu-button", "divider-1"])],
  ["right-group", new Set(["search-button", "comment-button"])],
]);
const PDF_VIEWER_THEME: ThemeConfig = {
  preference: "dark",
  dark: {
    background: {
      app: "var(--explorer-color-app-bg)",
      surface: "var(--explorer-color-surface)",
      surfaceAlt: "var(--explorer-color-surface-alt)",
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

const DETAILS_BUTTON: ToolbarItem = {
  type: "command-button",
  id: DETAILS_BUTTON_ID,
  commandId: DETAILS_COMMAND_ID,
  variant: "text",
};

const DETAILS_DIVIDER: ToolbarItem = {
  type: "divider",
  id: DETAILS_DIVIDER_ID,
  orientation: "vertical",
};

function buildToolbarPatch(schema: UISchema): Partial<UISchema> | null {
  const mainToolbar = Object.hasOwn(schema.toolbars, MAIN_TOOLBAR_ID)
    ? schema.toolbars[MAIN_TOOLBAR_ID]
    : null;
  if (mainToolbar === null) {
    return null;
  }

  let changed = false;
  const nextItems: ToolbarItem[] = [];
  for (const item of mainToolbar.items) {
    if (item.type !== "group") {
      nextItems.push(item);
      continue;
    }

    const hiddenItemIds = GROUP_ITEM_IDS_TO_HIDE.get(item.id);
    const filteredItems = hiddenItemIds
      ? item.items.filter((child) => !hiddenItemIds.has(child.id))
      : item.items;

    let nextGroupItems = filteredItems;
    if (item.id === "right-group") {
      const hasDetailsButton = filteredItems.some((child) => child.id === DETAILS_BUTTON_ID);
      if (!hasDetailsButton) {
        nextGroupItems = [...filteredItems, DETAILS_DIVIDER, DETAILS_BUTTON];
      }
    }

    if (nextGroupItems.length !== item.items.length) {
      changed = true;
    } else {
      for (let index = 0; index < nextGroupItems.length; index += 1) {
        if (nextGroupItems[index]?.id !== item.items[index]?.id) {
          changed = true;
          break;
        }
      }
    }

    nextItems.push({ ...item, items: nextGroupItems } satisfies ToolbarItem);
  }

  if (!changed) {
    return null;
  }

  return {
    toolbars: {
      [MAIN_TOOLBAR_ID]: {
        ...mainToolbar,
        items: nextItems,
      },
    },
  };
}

function handleViewerReady(registry: PluginRegistry, onToggleDetails: () => void) {
  const uiPlugin = registry.getPlugin<UIPlugin>("ui");
  if (uiPlugin !== null) {
    const ui = uiPlugin.provides();
    const patch = buildToolbarPatch(ui.getSchema());
    if (patch) {
      ui.mergeSchema(patch);
    }
  }

  const commandsPlugin = registry.getPlugin<CommandsPlugin>("commands");
  if (commandsPlugin !== null) {
    commandsPlugin.provides().registerCommand({
      id: DETAILS_COMMAND_ID,
      label: "Details",
      action: onToggleDetails,
    });
  }
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
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const [statusMessage, setStatusMessage] = useState("");

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
      } catch (err) {
        if (cancelled) {
          return;
        }
        setError(err instanceof Error ? err.message : "Failed to load document details");
        setDocument(null);
        setSelectedModel(DEFAULT_MODEL);
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
  const modelChanged = currentModel !== selectedModel;
  const baseModelMismatch =
    Boolean(document?.base_response_id) &&
    Boolean(document?.base_model) &&
    document?.base_model !== selectedModel;

  async function saveDocumentSettings(clearBaseResponse: boolean) {
    setSaving(true);
    setError("");
    setStatusMessage("");
    try {
      const response = await apiPatch<DocumentResponse>(`/documents/${documentId}`, {
        query_model: selectedModel,
        clear_base_response: clearBaseResponse,
      });
      setDocument(response.document);
      setSelectedModel(response.document.query_model);
      setStatusMessage(
        clearBaseResponse ? "Base response cleared." : "Document model updated.",
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
            src: `/documents/${documentId}/source`,
            theme: PDF_VIEWER_THEME,
            disabledCategories: [
              "annotation",
              "annotation-shape",
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
          onReady={(registry) => {
            handleViewerReady(registry, () => {
              setDetailsOpen((current) => !current);
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
                    Query Model
                  </p>
                  <label className="mt-3 block">
                    <span className="mb-2 block text-xs text-[#8a8a8a]">
                      New document threads start with this model. Threads that already have
                      document lineage keep using the model they started with.
                    </span>
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
                  <button
                    className="mt-4 inline-flex items-center justify-center text-xs font-semibold uppercase tracking-[0.16em] text-[#67b7ff] transition hover:text-[#8fc9ff] disabled:cursor-not-allowed disabled:opacity-40"
                    disabled={saving || !modelChanged}
                    onClick={() => {
                      void saveDocumentSettings(false);
                    }}
                    type="button"
                  >
                    {saving ? "Saving…" : "Save Model"}
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
                        Initialized At
                      </p>
                      <p className="mt-1 text-[#d8d8d8]">
                        {formatTimestamp(document.base_initialized_at)}
                      </p>
                    </div>
                  </div>

                  {baseModelMismatch && (
                    <p className="mt-3 text-xs leading-5 text-[#d3b171]">
                      The saved shared base response was created with {document.base_model}. New
                      threads will rebuild a fresh base response for {selectedModel}, while
                      existing thread-local lineages stay on their current model.
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
