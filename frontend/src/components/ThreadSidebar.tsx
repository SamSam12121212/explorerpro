import type { ThreadEntry } from "../thread";

interface ThreadSidebarProps {
  threads: ThreadEntry[];
  activeThreadId: number | null;
  onSelectThread: (id: number) => void;
}

export function ThreadSidebar({
  threads,
  activeThreadId,
  onSelectThread,
}: ThreadSidebarProps) {
  return (
    <div className="flex h-full w-full min-w-0 flex-col bg-[#1e1e1e]">
      <div className="px-3 py-0.5" />

      <div className="min-h-0 flex-1 overflow-y-auto">
        {threads.map((thread) => {
          const isActive = thread.id === activeThreadId;
          return (
            <button
              className={`flex w-full flex-col gap-0.5 border-b border-[#2a2a2a] px-3 py-2.5 text-left transition ${
                isActive
                  ? "bg-[#2a2a2a] text-white shadow-[inset_2px_0_0_#007acc]"
                  : "bg-transparent text-[#b2b2b2] hover:bg-[#252525] hover:text-[#d4d4d4]"
              }`}
              key={thread.id}
              onClick={() => { onSelectThread(thread.id); }}
              type="button"
            >
              <span className="truncate text-sm font-medium">{thread.label}</span>
              <span className="truncate text-[0.78rem] text-[#777]">
                {thread.previewText || "No messages yet"}
              </span>
            </button>
          );
        })}

        {threads.length === 0 && (
          <div className="px-3 py-6 text-center text-xs text-[#666]">
            No threads yet. Send a message to start one.
          </div>
        )}
      </div>
    </div>
  );
}
