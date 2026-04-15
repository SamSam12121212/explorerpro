import { useLocation, useNavigate } from "react-router";
import { LuFileText, LuFolder, LuGitFork, LuMessageSquare } from "react-icons/lu";
import { CollectionsView } from "../mid-panel/views/CollectionsView";
import { DocumentsView } from "../mid-panel/views/DocumentsView";
import { ReposView } from "../mid-panel/views/ReposView";
import { ThreadSidebar } from "./ThreadSidebar";
import { useThread } from "../thread";

type Tab = "threads" | "collections" | "documents" | "repos";

const tabs: { id: Tab; icon: typeof LuMessageSquare; label: string }[] = [
  { id: "threads", icon: LuMessageSquare, label: "Threads" },
  { id: "collections", icon: LuFolder, label: "Collections" },
  { id: "documents", icon: LuFileText, label: "Documents" },
  { id: "repos", icon: LuGitFork, label: "Repos" },
];

export function LeftSidebar() {
  const {
    threads,
    threadId,
    attachedDocuments,
    pendingDocuments,
    loadThread,
    attachDocument,
  } = useThread();

  const location = useLocation();
  const navigate = useNavigate();
  const activeTab: Tab = location.pathname.startsWith("/collections")
    ? "collections"
    : location.pathname.startsWith("/documents") || location.pathname.startsWith("/doc/")
      ? "documents"
      : location.pathname.startsWith("/repos")
        ? "repos"
        : "threads";

  const handleTabChange = (tab: Tab) => {
    switch (tab) {
      case "collections":
        void navigate("/collections");
        return;
      case "documents":
        void navigate("/documents");
        return;
      case "repos":
        void navigate("/repos");
        return;
      case "threads":
      default:
        void navigate("/");
    }
  };

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
              onClick={() => { handleTabChange(tab.id); }}
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
