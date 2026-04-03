import { useEffect, useRef, useState } from "react";
import type { Dispatch, SetStateAction } from "react";
import { Group, Panel, Separator } from "react-resizable-panels";
import { apiGet } from "./api";
import { REASONING_OPTIONS } from "./constants";
import type {
  ChatMessage,
  MessageRole,
  ReasoningEffort,
  ThreadListResponse,
  UploadedImage,
} from "./types";
import { useChat } from "./useChat";

interface ThreadEntry {
  id: string;
  label: string;
  previewText: string;
  updatedAt: string;
}

interface SidebarProps {
  threads: ThreadEntry[];
  activeThreadId: string | null;
  onSelectThread: (id: string) => void;
  onNewChat: () => void;
}

interface ChatPanelProps {
  busy: boolean;
  draft: string;
  messages: ChatMessage[];
  pendingImages: UploadedImage[];
  reasoningEffort: ReasoningEffort;
  sendMessage: (text: string, images: UploadedImage[]) => Promise<void>;
  setDraft: (value: string) => void;
  setPendingImages: Dispatch<SetStateAction<UploadedImage[]>>;
  setReasoningEffort: (value: ReasoningEffort) => void;
  submitDisabled: boolean;
  threadId: string | null;
  uploadCount: number;
  addPendingFiles: (files: File[]) => Promise<void>;
  resetConversation: () => void;
}

function formatBytes(bytes?: number) {
  if (!bytes || bytes <= 0) return "";
  if (bytes < 1024) return `${bytes.toString()} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

function bubbleClass(role: MessageRole) {
  const base = "max-w-[92%] px-4 py-3 text-sm leading-relaxed";
  switch (role) {
    case "user":
      return `${base} self-end bg-[#2a2a2a] text-[#d4d4d4]`;
    case "assistant":
      return `${base} self-start bg-transparent text-[#d4d4d4]`;
    case "error":
      return `${base} self-center bg-[#3b1111] text-[#fca5a5]`;
    default:
      return `${base} self-start bg-transparent text-[#d4d4d4]`;
  }
}

function Sidebar({
  threads,
  activeThreadId,
  onSelectThread,
  onNewChat,
}: SidebarProps) {
  return (
    <div className="flex h-full w-full min-w-0 flex-col bg-[#181818]">
      <div className="flex items-center justify-between border-b border-[#333] px-3 py-2">
        <span className="text-xs font-semibold uppercase tracking-widest text-[#888]">
          Threads
        </span>
        <button
          className="flex h-7 w-7 items-center justify-center border border-[#333] bg-[#2a2a2a] text-sm text-[#b2b2b2] transition hover:bg-[#333] hover:text-white"
          onClick={onNewChat}
          title="New thread"
          type="button"
        >
          +
        </button>
      </div>

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
              onClick={() => onSelectThread(thread.id)}
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

function ChatPanel({
  busy,
  draft,
  messages,
  pendingImages,
  reasoningEffort,
  sendMessage,
  setDraft,
  setPendingImages,
  setReasoningEffort,
  submitDisabled,
  threadId,
  uploadCount,
  addPendingFiles,
  resetConversation,
}: ChatPanelProps) {
  const messagesRef = useRef<HTMLDivElement | null>(null);
  const textareaRef = useRef<HTMLTextAreaElement | null>(null);
  const fileInputRef = useRef<HTMLInputElement | null>(null);

  useEffect(() => {
    const el = messagesRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [messages, busy]);

  useEffect(() => {
    const el = textareaRef.current;
    if (!el) return;
    el.style.height = "0px";
    el.style.height = `${Math.min(el.scrollHeight, 200).toString()}px`;
  }, [draft]);

  return (
    <div className="flex h-full w-full min-w-0 flex-col bg-[#1e1e1e]">
      {/* Header */}
      <div className="flex flex-wrap items-center justify-between gap-3 border-b border-[#333] px-4 py-2">
        <div className="flex min-w-0 flex-wrap items-center gap-3">
          <span className="truncate text-sm font-medium text-[#d4d4d4]">
            {threadId ? `Thread ${threadId.slice(0, 14)}…` : "New thread"}
          </span>

          <div className="flex flex-wrap items-center gap-1">
            <span className="text-[0.68rem] uppercase tracking-wide text-[#666]">
              Reasoning
            </span>
            {REASONING_OPTIONS.map((opt) => (
              <button
                className={`px-2 py-0.5 text-[0.72rem] font-medium transition ${
                  reasoningEffort === opt.value
                    ? "bg-[#2a2a2a] text-white"
                    : "text-[#888] hover:text-[#d4d4d4]"
                }`}
                key={opt.value}
                onClick={() => setReasoningEffort(opt.value)}
                type="button"
              >
                {opt.label}
              </button>
            ))}
          </div>
        </div>

        <button
          className="border border-[#333] bg-[#2a2a2a] px-3 py-1 text-xs text-[#b2b2b2] transition hover:bg-[#333] hover:text-white"
          onClick={resetConversation}
          type="button"
        >
          New chat
        </button>
      </div>

      {/* Messages */}
      <div
        className="min-h-0 flex-1 overflow-y-auto px-4 py-4"
        ref={messagesRef}
      >
        {messages.length === 0 && (
          <div className="flex h-full items-center justify-center">
            <p className="max-w-xs text-center text-sm text-[#555]">
              Send a message to start a thread.
            </p>
          </div>
        )}

        <div className="flex flex-col gap-3">
          {messages.map((msg) => (
            <article className={bubbleClass(msg.role)} key={msg.id}>
              {msg.text && <p className="m-0 whitespace-pre-wrap">{msg.text}</p>}

              {msg.images && msg.images.length > 0 && (
                <div className="mt-2 flex flex-wrap gap-2">
                  {msg.images.map((img) => (
                    <img
                      alt={img.filename ?? "Attached"}
                      className="block max-h-40 max-w-40 border border-[#333] object-cover"
                      key={img.image_id}
                      src={img.preview_url}
                    />
                  ))}
                </div>
              )}
            </article>
          ))}

          {busy && (
            <div className="self-start px-4 py-3">
              <div className="inline-flex gap-1">
                <span className="typing-dot h-1.5 w-1.5 rounded-full bg-[#888]" />
                <span className="typing-dot h-1.5 w-1.5 rounded-full bg-[#888] [animation-delay:150ms]" />
                <span className="typing-dot h-1.5 w-1.5 rounded-full bg-[#888] [animation-delay:300ms]" />
              </div>
            </div>
          )}
        </div>
      </div>

      {/* Pending images */}
      {pendingImages.length > 0 && (
        <div className="flex flex-wrap gap-2 border-t border-[#2a2a2a] px-4 py-2">
          {pendingImages.map((img) => (
            <div
              className="flex items-center gap-2 border border-[#333] bg-[#252525] px-2 py-1.5"
              key={img.image_id}
            >
              <img
                alt={img.filename ?? "Pending"}
                className="h-10 w-10 border border-[#333] object-cover"
                src={img.preview_url}
              />
              <div className="min-w-0">
                <p className="m-0 truncate text-xs text-[#d4d4d4]">
                  {img.filename ?? img.image_id}
                </p>
                <p className="m-0 text-[0.68rem] text-[#666]">
                  {formatBytes(img.bytes)}
                </p>
              </div>
              <button
                className="text-xs text-[#888] hover:text-[#f44747]"
                onClick={() =>
                  setPendingImages((cur) =>
                    cur.filter((e) => e.image_id !== img.image_id),
                  )
                }
                type="button"
              >
                ✕
              </button>
            </div>
          ))}
        </div>
      )}

      {/* Composer */}
      <div className="border-t border-[#333] px-4 py-3">
        <form
          className="flex flex-col gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            const text = draft.trim();
            if ((!text && pendingImages.length === 0) || busy || uploadCount > 0)
              return;
            const imgs = pendingImages.slice();
            setDraft("");
            setPendingImages([]);
            void sendMessage(text, imgs);
          }}
        >
          <input
            accept="image/*"
            hidden
            multiple
            onChange={(e) => {
              const files = Array.from(e.target.files ?? []);
              e.target.value = "";
              void addPendingFiles(files);
            }}
            ref={fileInputRef}
            type="file"
          />

          <textarea
            className="min-h-[2.5rem] max-h-52 w-full resize-none border border-[#333] bg-[#252525] px-3 py-2 text-sm text-[#d4d4d4] outline-none placeholder:text-[#555] focus:border-[#007acc]"
            onChange={(e) => setDraft(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter" && !e.shiftKey) {
                e.preventDefault();
                e.currentTarget.form?.requestSubmit();
              }
            }}
            onPaste={(e) => {
              const files = Array.from(e.clipboardData.items)
                .filter(
                  (it) => it.kind === "file" && it.type.startsWith("image/"),
                )
                .map((it) => it.getAsFile())
                .filter((f): f is File => f instanceof File);
              if (files.length === 0) return;
              e.preventDefault();
              void addPendingFiles(files);
            }}
            placeholder="Send a message…"
            ref={textareaRef}
            rows={1}
            value={draft}
          />

          <div className="flex items-center justify-between">
            <button
              className="text-xs text-[#888] transition hover:text-[#d4d4d4] disabled:opacity-40"
              disabled={busy || uploadCount > 0}
              onClick={() => fileInputRef.current?.click()}
              type="button"
            >
              + Image
            </button>

            <button
              className="bg-[#007acc] px-4 py-1.5 text-xs font-semibold text-white transition hover:bg-[#1b8de4] disabled:cursor-not-allowed disabled:opacity-40"
              disabled={submitDisabled}
              type="submit"
            >
              Send
            </button>
          </div>
        </form>
      </div>
    </div>
  );
}

export default function App() {
  const {
    messages,
    draft,
    setDraft,
    busy,
    uploadCount,
    threadId,
    pendingImages,
    setPendingImages,
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
  const lastLoadedThreadIdRef = useRef<string | null>(null);

  useEffect(() => {
    let cancelled = false;

    async function fetchThreads() {
      try {
        const payload = await apiGet<ThreadListResponse>("/threads?limit=100");
        if (cancelled) return;

        setThreads(
          (payload.threads ?? [])
            .filter((thread) => Boolean(thread?.id))
            .map((thread) => ({
              id: thread.id ?? "",
              label: thread.label?.trim() || "New thread",
              previewText: thread.preview_text?.trim() || "",
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

  useEffect(() => {
    if (!threadId || threadId === lastLoadedThreadIdRef.current) {
      return;
    }
    if (!threads.some((thread) => thread.id === threadId)) {
      return;
    }
    if (messages.length > 0) {
      return;
    }

    lastLoadedThreadIdRef.current = threadId;
    void loadThread(threadId);
  }, [loadThread, messages.length, threadId, threads]);

  return (
    <div className="h-screen w-screen overflow-hidden bg-[#181818]">
      <Group className="h-full w-full min-w-0" orientation="horizontal">
        <Panel className="min-w-0" defaultSize={22} minSize={18}>
          <Sidebar
            activeThreadId={threadId}
            onNewChat={resetConversation}
            onSelectThread={(id) => {
              lastLoadedThreadIdRef.current = id;
              void loadThread(id);
            }}
            threads={threads}
          />
        </Panel>

        <Separator className="resize-handle" />

        <Panel className="min-w-0" defaultSize={78} minSize={40}>
          <ChatPanel
            addPendingFiles={addPendingFiles}
            busy={busy}
            draft={draft}
            messages={messages}
            pendingImages={pendingImages}
            reasoningEffort={reasoningEffort}
            resetConversation={resetConversation}
            sendMessage={sendMessage}
            setDraft={setDraft}
            setPendingImages={setPendingImages}
            setReasoningEffort={setReasoningEffort}
            submitDisabled={submitDisabled}
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
