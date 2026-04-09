import { useEffect, useState } from "react";
import { Group, Panel, Separator } from "react-resizable-panels";
import { useNavigate, useParams } from "react-router";
import { apiGet } from "./api";
import { LeftSidebar } from "./components/LeftSidebar";
import { MidPanelHost } from "./components/MidPanelHost";
import type { ThreadEntry } from "./components/ThreadSidebar";
import { ChatPanel } from "./components/chat/ChatPanel";
import type { ThreadListResponse } from "./types";
import { useChat } from "./useChat";

export default function App() {
  const { threadId: urlThreadId } = useParams<"threadId">();
  const navigate = useNavigate();

  const {
    messages,
    draft,
    setDraft,
    busy,
    thinking,
    uploadCount,
    threadId,
    pendingImages,
    setPendingImages,
    model,
    setModel,
    reasoningEffort,
    setReasoningEffort,
    sendMessage,
    loadThread,
    addPendingFiles,
    resetConversation,
    submitDisabled,
  } = useChat();

  const [threads, setThreads] = useState<ThreadEntry[]>([]);
  const [threadsLoading, setThreadsLoading] = useState(true);

  useEffect(() => {
    let cancelled = false;

    async function fetchThreads() {
      try {
        const payload = await apiGet<ThreadListResponse>("/threads?limit=100");
        if (cancelled) return;

        setThreads(
          (payload.threads ?? [])
            .filter((thread) => Boolean(thread.id))
            .map((thread) => ({
              id: thread.id ?? "",
              label: thread.label?.trim() ?? "New thread",
              previewText: thread.preview_text?.trim() ?? "",
              updatedAt: thread.updated_at ?? thread.created_at ?? "",
            })),
        );
      } finally {
        if (!cancelled) {
          setThreadsLoading(false);
        }
      }
    }

    void fetchThreads();
    return () => {
      cancelled = true;
    };
  }, [threadId]);

  // Sync URL -> useChat: load thread when URL param changes
  useEffect(() => {
    if (urlThreadId && urlThreadId !== threadId) {
      void loadThread(urlThreadId);
    } else if (!urlThreadId && threadId) {
      resetConversation();
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [urlThreadId]);

  // Sync useChat -> URL: update URL when a new thread is created
  useEffect(() => {
    if (threadId && threadId !== urlThreadId) {
      void navigate(`/thread/${threadId}`, { replace: true });
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [threadId]);

  const handleSelectThread = (id: string) => {
    void navigate(`/thread/${id}`);
  };

  const handleNewChat = () => {
    resetConversation();
    void navigate("/");
  };

  return (
    <div className="h-screen w-screen overflow-hidden bg-[#1e1e1e]">
      <Group className="h-full w-full min-w-0" orientation="horizontal">
        <Panel className="min-w-0" defaultSize={18} minSize={14}>
          <LeftSidebar
            activeThreadId={threadId}
            onNewChat={handleNewChat}
            onSelectThread={handleSelectThread}
            threads={threads}
          />
        </Panel>

        <Separator className="resize-handle" />

        <Panel className="min-w-0" defaultSize={50} minSize={20}>
          <MidPanelHost />
        </Panel>

        <Separator className="resize-handle" />

        <Panel className="min-w-0" defaultSize={32} minSize={22}>
          <ChatPanel
            addPendingFiles={addPendingFiles}
            busy={busy}
            draft={draft}
            messages={messages}
            model={model}
            pendingImages={pendingImages}
            reasoningEffort={reasoningEffort}
            sendMessage={sendMessage}
            setDraft={setDraft}
            setModel={setModel}
            setPendingImages={setPendingImages}
            setReasoningEffort={setReasoningEffort}
            submitDisabled={submitDisabled}
            thinking={thinking}
            threadId={threadId}
            uploadCount={uploadCount}
          />
        </Panel>
      </Group>

      {threadsLoading && (
        <div className="pointer-events-none absolute inset-x-0 bottom-3 flex justify-center">
          <div className="border border-[#333] bg-[#202020] px-3 py-1 text-[0.7rem] text-[#777]">
            Loading threads…
          </div>
        </div>
      )}
    </div>
  );
}
