import { useState } from "react";
import { LuGitFork, LuMessageSquare } from "react-icons/lu";
import { ReposView } from "../mid-panel/views/ReposView";
import { ThreadSidebar, type ThreadEntry } from "./ThreadSidebar";

type Tab = "threads" | "repos";

interface LeftSidebarProps {
  threads: ThreadEntry[];
  activeThreadId: string | null;
  onSelectThread: (id: string) => void;
  onNewChat: () => void;
}

const tabs: { id: Tab; icon: typeof LuMessageSquare; label: string }[] = [
  { id: "threads", icon: LuMessageSquare, label: "Threads" },
  { id: "repos", icon: LuGitFork, label: "Repos" },
];

export function LeftSidebar({
  threads,
  activeThreadId,
  onSelectThread,
  onNewChat,
}: LeftSidebarProps) {
  const [activeTab, setActiveTab] = useState<Tab>("threads");

  return (
    <div className="flex h-full w-full min-w-0 flex-col bg-[#1e1e1e]">
      {/* Icon toolbar */}
      <div className="flex items-center justify-center gap-1 border-b border-[#333] px-2 py-1.5">
        {tabs.map((tab) => {
          const Icon = tab.icon;
          const isActive = activeTab === tab.id;
          return (
            <button
              className={`flex h-7 w-7 items-center justify-center transition ${
                isActive
                  ? "text-white"
                  : "text-[#666] hover:text-[#b2b2b2]"
              }`}
              key={tab.id}
              onClick={() => { setActiveTab(tab.id); }}
              title={tab.label}
              type="button"
            >
              <Icon size={18} />
            </button>
          );
        })}
      </div>

      {/* View content */}
      <div className="min-h-0 flex-1">
        {activeTab === "threads" ? (
          <ThreadSidebar
            activeThreadId={activeThreadId}
            onNewChat={onNewChat}
            onSelectThread={onSelectThread}
            threads={threads}
          />
        ) : (
          <ReposView />
        )}
      </div>
    </div>
  );
}
