import { useEffect, useState } from "react";

export type ShellPanel = "left" | "middle" | "right";

export interface PanelVisibilityState {
  left: boolean;
  middle: boolean;
  right: boolean;
}

const PANEL_VISIBILITY_STORAGE_KEY = "explorer.panelVisibility";

const DEFAULT_PANEL_VISIBILITY: PanelVisibilityState = {
  left: true,
  middle: true,
  right: true,
};

function countVisiblePanels(visibility: PanelVisibilityState) {
  let count = 0;
  if (visibility.left) count += 1;
  if (visibility.middle) count += 1;
  if (visibility.right) count += 1;
  return count;
}

function normalizePanelVisibility(
  value: Partial<Record<ShellPanel, unknown>> | null | undefined,
): PanelVisibilityState {
  const normalized: PanelVisibilityState = {
    left: value?.left !== false,
    middle: value?.middle !== false,
    right: value?.right !== false,
  };

  return countVisiblePanels(normalized) > 0
    ? normalized
    : DEFAULT_PANEL_VISIBILITY;
}

function readPersistedPanelVisibility(): PanelVisibilityState {
  if (typeof window === "undefined") {
    return DEFAULT_PANEL_VISIBILITY;
  }

  try {
    const raw = window.localStorage.getItem(PANEL_VISIBILITY_STORAGE_KEY)?.trim() ?? "";
    if (!raw) {
      return DEFAULT_PANEL_VISIBILITY;
    }

    return normalizePanelVisibility(
      JSON.parse(raw) as Partial<Record<ShellPanel, unknown>>,
    );
  } catch {
    return DEFAULT_PANEL_VISIBILITY;
  }
}

function writePersistedPanelVisibility(visibility: PanelVisibilityState) {
  if (typeof window === "undefined") {
    return;
  }

  try {
    window.localStorage.setItem(
      PANEL_VISIBILITY_STORAGE_KEY,
      JSON.stringify(visibility),
    );
  } catch {
    /* ignore persistence failures */
  }
}

export function usePanelVisibilitySettings() {
  const [panelVisibility, setPanelVisibility] = useState<PanelVisibilityState>(
    () => readPersistedPanelVisibility(),
  );

  useEffect(() => {
    writePersistedPanelVisibility(panelVisibility);
  }, [panelVisibility]);

  const togglePanelVisibility = (panel: ShellPanel) => {
    setPanelVisibility((current) => {
      if (current[panel] && countVisiblePanels(current) === 1) {
        return current;
      }

      return {
        ...current,
        [panel]: !current[panel],
      };
    });
  };

  return {
    panelVisibility,
    togglePanelVisibility,
  };
}
