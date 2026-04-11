import {
  PDFViewer,
  type PluginRegistry,
  type ThemeConfig,
  type ToolbarItem,
  type UISchema,
} from "@embedpdf/react-pdf-viewer";

const MAIN_TOOLBAR_ID = "main-toolbar";
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

function buildToolbarPatch(schema: UISchema): Partial<UISchema> | null {
  const mainToolbar = schema.toolbars[MAIN_TOOLBAR_ID];
  if (!mainToolbar) {
    return null;
  }

  let changed = false;
  const nextItems = mainToolbar.items.map((item) => {
    if (item.type !== "group") {
      return item;
    }

    const hiddenItemIds = GROUP_ITEM_IDS_TO_HIDE.get(item.id);
    if (!hiddenItemIds) {
      return item;
    }

    const filteredItems = item.items.filter((child) => !hiddenItemIds.has(child.id));
    if (filteredItems.length === item.items.length) {
      return item;
    }

    changed = true;
    return { ...item, items: filteredItems } satisfies ToolbarItem;
  });

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

function handleViewerReady(registry: PluginRegistry) {
  const ui = registry.getPlugin("ui")?.provides?.();
  if (!ui) {
    return;
  }

  const patch = buildToolbarPatch(ui.getSchema());
  if (patch) {
    ui.mergeSchema(patch);
  }
}

interface PdfViewerPanelProps {
  documentId: string;
}

export function PdfViewerPanel({ documentId }: PdfViewerPanelProps) {
  return (
    <div className="relative h-full w-full min-h-0 overflow-hidden bg-[#1e1e1e]">
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
        onReady={handleViewerReady}
      />
    </div>
  );
}
