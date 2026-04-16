import { LuFileText, LuFolder, LuGitFork, LuMessageSquare } from "react-icons/lu";
import { CollectionsView } from "../mid-panel/views/CollectionsView";
import { DocumentsView } from "../mid-panel/views/DocumentsView";
import { ReposView } from "../mid-panel/views/ReposView";
import { ThreadSidebar } from "./ThreadSidebar";
import { useThread } from "../thread";

export type LeftSidebarTab = "threads" | "collections" | "documents" | "repos";

export function getInitialLeftSidebarTab(pathname: string): LeftSidebarTab {
  if (pathname.startsWith("/collections")) return "collections";
  if (pathname.startsWith("/documents") || pathname.startsWith("/doc/")) return "documents";
  if (pathname.startsWith("/repos")) return "repos";
  return "threads";
}

const tabs: { id: LeftSidebarTab; icon: typeof LuMessageSquare; label: string }[] = [
  { id: "threads", icon: LuMessageSquare, label: "Threads" },
  { id: "collections", icon: LuFolder, label: "Collections" },
  { id: "documents", icon: LuFileText, label: "Documents" },
  { id: "repos", icon: LuGitFork, label: "Repos" },
];

interface LeftSidebarProps {
  activeTab: LeftSidebarTab;
  onTabChange: (tab: LeftSidebarTab) => void;
}

export function LeftSidebar({ activeTab, onTabChange }: LeftSidebarProps) {
  const {
    threads,
    threadId,
    attachedDocuments,
    pendingDocuments,
    loadThread,
    archiveThread,
    deleteThread,
    attachDocument,
  } = useThread();

  const handleSelectThread = (id: number) => {
    if (id === threadId) return;
    void loadThread(id);
  };

  const attachedDocumentIds = [
    ...attachedDocuments.map((d) => d.id),
    ...pendingDocuments.map((d) => d.id),
  ];

  return (
    <div className="flex h-full w-full min-w-0 flex-col bg-[#1e1e1e]">
      <div className="shell-bar justify-center gap-1 px-2">
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
              onClick={() => { onTabChange(tab.id); }}
              title={tab.label}
              type="button"
            >
              <Icon size={18} />
            </button>
          );
        })}
      </div>

      <div className="min-h-0 flex-1">
        {activeTab === "threads" ? (
          <ThreadSidebar
            activeThreadId={threadId}
            onArchiveThread={archiveThread}
            onDeleteThread={deleteThread}
            onSelectThread={handleSelectThread}
            threads={threads}
          />
        ) : activeTab === "collections" ? (
          <CollectionsView />
        ) : activeTab === "documents" ? (
          <DocumentsView
            attachedDocumentIds={attachedDocumentIds}
            onAttachDocument={attachDocument}
          />
        ) : (
          <ReposView />
        )}
      </div>
    </div>
  );
}
