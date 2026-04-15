import { Group, Panel, Separator } from "react-resizable-panels";
import { LeftSidebar } from "./components/LeftSidebar";
import { MidPanelHost } from "./components/MidPanelHost";
import { ChatPanel } from "./components/chat/ChatPanel";
import { ThreadProvider } from "./thread";

export default function App() {
  return (
    <ThreadProvider>
      <AppLayout />
    </ThreadProvider>
  );
}

function AppLayout() {
  return (
    <div className="h-screen w-screen overflow-hidden bg-[#1e1e1e]">
      <Group className="h-full w-full min-w-0" orientation="horizontal">
        <Panel className="min-w-0" defaultSize={18} minSize={14}>
          <LeftSidebar />
        </Panel>

        <Separator className="resize-handle" />

        <Panel className="min-w-0" defaultSize={50} minSize={20}>
          <MidPanelHost />
        </Panel>

        <Separator className="resize-handle" />

        <Panel className="min-w-0" defaultSize={32} minSize={22}>
          <ChatPanel />
        </Panel>
      </Group>
    </div>
  );
}
