import { useEffect, useState } from "react";
import { LuArchive, LuEllipsis, LuTrash2 } from "react-icons/lu";
import type { ThreadEntry } from "../thread";

interface ThreadSidebarProps {
  threads: ThreadEntry[];
  activeThreadId: number | null;
  onSelectThread: (id: number) => void;
  onArchiveThread: (id: number) => Promise<void>;
  onDeleteThread: (id: number) => Promise<void>;
}

export function ThreadSidebar({
  threads,
  activeThreadId,
  onSelectThread,
  onArchiveThread,
  onDeleteThread,
}: ThreadSidebarProps) {
  const [openMenuThreadId, setOpenMenuThreadId] = useState<number | null>(null);
  const [actionThreadId, setActionThreadId] = useState<number | null>(null);

  useEffect(() => {
    if (!openMenuThreadId) {
      return undefined;
    }

    const handlePointerDown = (event: MouseEvent) => {
      const target = event.target;
      if (!(target instanceof Element)) {
        setOpenMenuThreadId(null);
        return;
      }
      if (target.closest("[data-thread-menu]")) {
        return;
      }
      setOpenMenuThreadId(null);
    };

    window.addEventListener("mousedown", handlePointerDown);
    return () => {
      window.removeEventListener("mousedown", handlePointerDown);
    };
  }, [openMenuThreadId]);

  const runThreadAction = async (threadId: number, action: "archive" | "delete") => {
    setActionThreadId(threadId);
    try {
      if (action === "archive") {
        await onArchiveThread(threadId);
      } else {
        const confirmed = window.confirm(
          "Delete this thread permanently? This removes the thread and all child thread data.",
        );
        if (!confirmed) {
          return;
        }
        await onDeleteThread(threadId);
      }
      setOpenMenuThreadId(null);
    } catch (error) {
      window.alert(error instanceof Error ? error.message : "Thread action failed");
    } finally {
      setActionThreadId((current) => (current === threadId ? null : current));
    }
  };

  return (
    <div className="flex h-full w-full min-w-0 flex-col bg-[#1e1e1e]">
      <div className="px-3 py-0.5" />

      <div className="min-h-0 flex-1 overflow-y-auto">
        {threads.map((thread) => {
          const isActive = thread.id === activeThreadId;
          const menuOpen = openMenuThreadId === thread.id;
          const actionBusy = actionThreadId === thread.id;

          return (
            <div
              className={`group flex items-start border-b border-[#2a2a2a] transition ${
                isActive
                  ? "bg-[#2a2a2a] shadow-[inset_2px_0_0_#007acc]"
                  : "bg-transparent hover:bg-[#252525]"
              }`}
              key={thread.id}
            >
              <button
                className={`min-w-0 flex-1 px-3 py-2.5 text-left transition ${
                  isActive
                    ? "text-white"
                    : "text-[#b2b2b2] group-hover:text-[#d4d4d4]"
                }`}
                onClick={() => {
                  setOpenMenuThreadId(null);
                  onSelectThread(thread.id);
                }}
                type="button"
              >
                <span className="block truncate text-sm font-medium">{thread.label}</span>
                <span className="block truncate text-[0.78rem] text-[#777]">
                  {thread.previewText || "No messages yet"}
                </span>
              </button>

              <div
                className="relative flex items-start pr-2 pt-2"
                data-thread-menu={thread.id}
              >
                <button
                  className={`flex h-7 w-7 items-center justify-center border border-[#333] bg-[#252525] text-[#888] transition hover:border-[#444] hover:text-[#d4d4d4] disabled:cursor-not-allowed disabled:opacity-40 ${
                    menuOpen
                      ? "opacity-100"
                      : "opacity-0 group-hover:opacity-100 focus:opacity-100"
                  }`}
                  disabled={actionBusy}
                  onClick={() => {
                    setOpenMenuThreadId((current) =>
                      current === thread.id ? null : thread.id,
                    );
                  }}
                  title="Thread actions"
                  type="button"
                >
                  <LuEllipsis className="h-4 w-4" />
                </button>

                {menuOpen && (
                  <div className="absolute top-8 right-0 z-10 min-w-40 border border-[#333] bg-[#202020] p-1 shadow-[0_8px_24px_rgba(0,0,0,0.35)]">
                    <button
                      className="flex w-full items-center gap-2 px-2 py-1.5 text-left text-xs text-[#d4d4d4] transition hover:bg-[#2a2a2a] disabled:cursor-not-allowed disabled:opacity-40"
                      disabled={actionBusy}
                      onClick={() => { void runThreadAction(thread.id, "archive"); }}
                      type="button"
                    >
                      <LuArchive className="h-3.5 w-3.5" />
                      <span>Archive</span>
                    </button>

                    <button
                      className="flex w-full items-center gap-2 px-2 py-1.5 text-left text-xs text-red-300 transition hover:bg-[#2a2a2a] disabled:cursor-not-allowed disabled:opacity-40"
                      disabled={actionBusy}
                      onClick={() => { void runThreadAction(thread.id, "delete"); }}
                      type="button"
                    >
                      <LuTrash2 className="h-3.5 w-3.5" />
                      <span>Delete thread</span>
                    </button>
                  </div>
                )}
              </div>
            </div>
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
