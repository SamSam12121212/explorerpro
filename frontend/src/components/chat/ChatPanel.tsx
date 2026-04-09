import { useEffect, useRef } from "react";
import type { Dispatch, SetStateAction } from "react";
import { LuChevronDown, LuImage } from "react-icons/lu";
import { MODEL_OPTIONS, REASONING_OPTIONS } from "../../constants";
import type {
  ChatMessage,
  MessageRole,
  ReasoningEffort,
  UploadedImage,
} from "../../types";

interface ChatPanelProps {
  busy: boolean;
  draft: string;
  messages: ChatMessage[];
  model: string;
  pendingImages: UploadedImage[];
  reasoningEffort: ReasoningEffort;
  sendMessage: (text: string, images: UploadedImage[]) => Promise<void>;
  setDraft: (value: string) => void;
  setModel: (value: string) => void;
  setPendingImages: Dispatch<SetStateAction<UploadedImage[]>>;
  setReasoningEffort: (value: ReasoningEffort) => void;
  submitDisabled: boolean;
  thinking: boolean;
  threadId: string | null;
  uploadCount: number;
  addPendingFiles: (files: File[]) => Promise<void>;
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

export function ChatPanel({
  busy,
  draft,
  messages,
  model,
  pendingImages,
  reasoningEffort,
  sendMessage,
  setDraft,
  setModel,
  setPendingImages,
  setReasoningEffort,
  submitDisabled,
  thinking,
  threadId,
  uploadCount,
  addPendingFiles,
}: ChatPanelProps) {
  const messagesRef = useRef<HTMLDivElement | null>(null);
  const textareaRef = useRef<HTMLTextAreaElement | null>(null);
  const fileInputRef = useRef<HTMLInputElement | null>(null);

  useEffect(() => {
    const el = messagesRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [messages, busy, thinking]);

  useEffect(() => {
    const el = textareaRef.current;
    if (!el) return;
    el.style.height = "0px";
    el.style.height = `${Math.min(el.scrollHeight, 200).toString()}px`;
  }, [draft]);

  return (
    <div className="flex h-full w-full min-w-0 flex-col bg-[#1e1e1e]">
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

          {thinking && (
            <article className={bubbleClass("assistant")}>
              <p className="m-0 text-[#888]">Thinking...</p>
            </article>
          )}

          {busy && !thinking && (
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

      {pendingImages.length > 0 && (
        <div className="flex flex-wrap gap-2 px-4 py-2">
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
              </div>
              <button
                className="text-xs text-[#888] hover:text-[#f44747]"
                onClick={() => {
                  setPendingImages((cur) =>
                    cur.filter((e) => e.image_id !== img.image_id),
                  );
                }}
                type="button"
              >
                ✕
              </button>
            </div>
          ))}
        </div>
      )}

      <div className="px-4 py-3">
        <form
          className="flex flex-col gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            const text = draft.trim();
            if ((!text && pendingImages.length === 0) || busy || uploadCount > 0) {
              return;
            }
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
            onChange={(e) => { setDraft(e.target.value); }}
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

          <div className="flex items-center justify-between gap-2">
            <div className="flex min-w-0 flex-1 flex-wrap items-center gap-x-3 gap-y-2">
              <button
                className="text-[#888] hover:text-[#d4d4d4] disabled:opacity-40"
                disabled={busy || uploadCount > 0}
                onClick={() => fileInputRef.current?.click()}
                title="Attach image"
                type="button"
              >
                <LuImage className="h-4 w-4" />
              </button>

              <label
                className={`relative inline-flex shrink-0 items-center gap-0.5 text-xs text-[#888] ${
                  threadId
                    ? "cursor-not-allowed hover:text-[#888]"
                    : "cursor-pointer hover:text-[#d4d4d4]"
                }`}
              >
                <span>{MODEL_OPTIONS.find((o) => o.value === model)?.label ?? model}</span>
                <LuChevronDown className="h-3 w-3" />
                <select
                  className="absolute inset-0 appearance-none opacity-0 disabled:cursor-not-allowed"
                  disabled={!!threadId}
                  onChange={(e) => { setModel(e.target.value); }}
                  value={model}
                >
                  {MODEL_OPTIONS.map((opt) => (
                    <option key={opt.value} value={opt.value}>
                      {opt.label}
                    </option>
                  ))}
                </select>
              </label>

              <label className="relative inline-flex shrink-0 cursor-pointer items-center gap-0.5 text-xs text-[#888] hover:text-[#d4d4d4]">
                <span style={thinking ? { animation: "reasoning-pulse 1.5s ease-in-out infinite" } : undefined}>{REASONING_OPTIONS.find((o) => o.value === reasoningEffort)?.label ?? reasoningEffort}</span>
                <LuChevronDown className="h-3 w-3" />
                <select
                  className="absolute inset-0 cursor-pointer appearance-none opacity-0"
                  onChange={(e) => { setReasoningEffort(e.target.value as ReasoningEffort); }}
                  value={reasoningEffort}
                >
                  {REASONING_OPTIONS.map((opt) => (
                    <option key={opt.value} value={opt.value}>
                      {opt.label}
                    </option>
                  ))}
                </select>
              </label>
            </div>

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
