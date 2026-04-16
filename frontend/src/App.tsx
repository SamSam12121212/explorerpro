import { useState } from "react";
import {
  LuColumns3,
  LuPanelLeft,
  LuPanelRight,
  LuSquarePen,
} from "react-icons/lu";
import { Group, Panel, Separator, useDefaultLayout } from "react-resizable-panels";
import { IconActionButton } from "./components/IconActionButton";
import { usePanelVisibilitySettings } from "./panelVisibility";
import {
  LeftSidebar,
  getInitialLeftSidebarTab,
  type LeftSidebarTab,
} from "./components/LeftSidebar";
import { MidPanelHost } from "./components/MidPanelHost";
import { ThreadPanel } from "./components/thread/ThreadPanel";
import { ThreadProvider, useThread } from "./thread";

const PANEL_GROUP_ID = "explorer-shell-panels";
const LEFT_PANEL_ID = "explorer-left-panel";
const MIDDLE_PANEL_ID = "explorer-middle-panel";
const RIGHT_PANEL_ID = "explorer-right-panel";
const PANEL_IDS = [LEFT_PANEL_ID, MIDDLE_PANEL_ID, RIGHT_PANEL_ID] as const;
const DEFAULT_PANEL_LAYOUT = {
  [LEFT_PANEL_ID]: 18,
  [MIDDLE_PANEL_ID]: 50,
  [RIGHT_PANEL_ID]: 32,
};
type PanelId = (typeof PANEL_IDS)[number];

function buildDefaultLayout(panelIds: PanelId[]) {
  const total = panelIds.reduce((sum, panelId) => {
    return sum + DEFAULT_PANEL_LAYOUT[panelId];
  }, 0);

  return panelIds.reduce<Record<PanelId, number>>((layout, panelId) => {
    layout[panelId] = Number(
      ((DEFAULT_PANEL_LAYOUT[panelId] / total) * 100).toFixed(3),
    );
    return layout;
  }, {} as Record<PanelId, number>);
}

export default function App() {
  return (
    <ThreadProvider>
      <AppLayout />
    </ThreadProvider>
  );
}

function AppLayout() {
  const { resetConversation } = useThread();
  const { panelVisibility, togglePanelVisibility } = usePanelVisibilitySettings();
  const [leftSidebarTab, setLeftSidebarTab] = useState<LeftSidebarTab>(() =>
    getInitialLeftSidebarTab(window.location.pathname),
  );
  const visiblePanelIds: PanelId[] = [];
  if (panelVisibility.left) visiblePanelIds.push(LEFT_PANEL_ID);
  if (panelVisibility.middle) visiblePanelIds.push(MIDDLE_PANEL_ID);
  if (panelVisibility.right) visiblePanelIds.push(RIGHT_PANEL_ID);
  const { defaultLayout: persistedLayout, onLayoutChanged } = useDefaultLayout({
    id: PANEL_GROUP_ID,
    panelIds: visiblePanelIds,
  });
  const visiblePanels = [];

  if (panelVisibility.left) {
    visiblePanels.push({
      defaultSize: "18%",
      id: LEFT_PANEL_ID,
      minSize: "14px",
      node: <LeftSidebar activeTab={leftSidebarTab} onTabChange={setLeftSidebarTab} />,
    });
  }

  if (panelVisibility.middle) {
    visiblePanels.push({
      defaultSize: "50%",
      id: MIDDLE_PANEL_ID,
      minSize: "20px",
      node: <MidPanelHost />,
    });
  }

  if (panelVisibility.right) {
    visiblePanels.push({
      defaultSize: "32%",
      id: RIGHT_PANEL_ID,
      minSize: "22px",
      node: <ThreadPanel />,
    });
  }

  return (
    <div className="flex h-screen w-screen flex-col overflow-hidden bg-[#1e1e1e]">
      <div className="shell-bar shrink-0 justify-end gap-1 border-b border-[#333] px-3">
        <IconActionButton
          label={panelVisibility.left ? "Hide left panel" : "Show left panel"}
          onClick={() => { togglePanelVisibility("left"); }}
          pressed={panelVisibility.left}
        >
          <LuPanelLeft size={18} />
        </IconActionButton>
        <IconActionButton
          label={panelVisibility.middle ? "Hide middle panel" : "Show middle panel"}
          onClick={() => { togglePanelVisibility("middle"); }}
          pressed={panelVisibility.middle}
        >
          <LuColumns3 size={18} />
        </IconActionButton>
        <IconActionButton
          label={panelVisibility.right ? "Hide right panel" : "Show right panel"}
          onClick={() => { togglePanelVisibility("right"); }}
          pressed={panelVisibility.right}
        >
          <LuPanelRight size={18} />
        </IconActionButton>
        <IconActionButton
          label="New thread"
          onClick={resetConversation}
        >
          <LuSquarePen size={18} />
        </IconActionButton>
      </div>

      <div className="min-h-0 flex-1">
        <Group
          className="h-full w-full min-w-0"
          defaultLayout={persistedLayout ?? buildDefaultLayout(visiblePanelIds)}
          id={PANEL_GROUP_ID}
          onLayoutChanged={onLayoutChanged}
          orientation="horizontal"
        >
          {visiblePanels.map((panel, index) => [
            index > 0 ? (
              <Separator className="resize-handle" key={`${panel.id}-separator`} />
            ) : null,
            <Panel
              className="min-w-0"
              defaultSize={panel.defaultSize}
              id={panel.id}
              key={panel.id}
              minSize={panel.minSize}
            >
              {panel.node}
            </Panel>,
          ])}
        </Group>
      </div>
    </div>
  );
}
