import { useEffect, useMemo, useRef, useState } from "react";
import { LuChevronDown, LuFileText, LuFolder, LuImage, LuPaperclip, LuSend } from "react-icons/lu";
import { MODEL_OPTIONS, REASONING_OPTIONS } from "../../constants";
import type { AttachedCollection, AttachedDocument, MessageRole, ReasoningEffort } from "../../types";
import { useThread } from "../../thread";

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

function uniqueDocumentIds(
  standaloneDocs: AttachedDocument[],
  collections: AttachedCollection[],
): Set<number> {
  const ids = new Set<number>();
  for (const doc of standaloneDocs) ids.add(doc.id);
  for (const collection of collections) {
    for (const doc of collection.documents) ids.add(doc.id);
  }
  return ids;
}

export function ThreadPanel() {
  const {
    busy,
    draft,
    messages,
    model,
    attachedDocuments,
    pendingDocuments,
    attachedCollections,
    pendingCollections,
    pendingDocumentQueries,
    pendingImages,
    reasoningEffort,
    sendMessage,
    setPendingDocuments,
    setPendingCollections,
    setDraft,
    setModel,
    setPendingImages,
    setReasoningEffort,
    submitDisabled,
    thinking,
    threadId,
    uploadCount,
    addPendingFiles,
  } = useThread();

  const messagesRef = useRef<HTMLDivElement | null>(null);
  const textareaRef = useRef<HTMLTextAreaElement | null>(null);
  const fileInputRef = useRef<HTMLInputElement | null>(null);
  const [attachmentsOpen, setAttachmentsOpen] = useState(false);
  const [prevThreadId, setPrevThreadId] = useState(threadId);
  if (prevThreadId !== threadId) {
    setPrevThreadId(threadId);
    setAttachmentsOpen(false);
  }

  // Badge count: dedup union across standalone docs (attached + pending) and
  // every collection's member docs (attached + pending). Matches the rule
  // "2 collections of 5 docs + 3 standalones = 13" with dedup.
  const persistedBadgeCount = useMemo(
    () => uniqueDocumentIds(attachedDocuments, attachedCollections).size,
    [attachedDocuments, attachedCollections],
  );
  const totalBadgeCount = useMemo(
    () =>
      uniqueDocumentIds(
        [...attachedDocuments, ...pendingDocuments],
        [...attachedCollections, ...pendingCollections],
      ).size,
    [attachedDocuments, pendingDocuments, attachedCollections, pendingCollections],
  );

  const docQueryCount = pendingDocumentQueries.length;

  useEffect(() => {
    const el = messagesRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [messages, busy, thinking, docQueryCount]);

  useEffect(() => {
    const el = textareaRef.current;
    if (!el) return;
    el.style.height = "0px";
    el.style.height = `${Math.min(el.scrollHeight, 200).toString()}px`;
  }, [draft]);

  useEffect(() => {
    if (!attachmentsOpen) {
      return undefined;
    }

    const handleKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") {
        setAttachmentsOpen(false);
      }
    };

    window.addEventListener("keydown", handleKeyDown);
    return () => {
      window.removeEventListener("keydown", handleKeyDown);
    };
  }, [attachmentsOpen]);

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
          {messages.filter((m) => m.text || (m.images?.length ?? 0) > 0).map((msg) => (
            <article className={bubbleClass(msg.role)} key={msg.id}>
              {msg.text && (
                <p className="m-0 whitespace-pre-wrap">
                  {msg.text}
                  {msg.streaming && <span className="streaming-caret" />}
                </p>
              )}

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

          {docQueryCount > 0 && (
            <article className={bubbleClass("assistant")}>
              <p className="m-0 text-[#888]">
                {docQueryCount === 1
                  ? "Reading 1 document..."
                  : `Reading ${docQueryCount.toString()} documents...`}
              </p>
            </article>
          )}

          {busy && !thinking && docQueryCount === 0 && !messages.some((m) => m.streaming && m.text) && (
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

      {(pendingDocuments.length > 0 || pendingCollections.length > 0) && (
        <div className="flex flex-wrap gap-2 px-4 py-2">
          {pendingDocuments.map((doc) => (
            <div
              className="flex items-center gap-2 border border-[#333] bg-[#252525] px-2 py-1.5"
              key={`doc-${doc.id.toString()}`}
            >
              <div className="flex h-10 w-10 items-center justify-center border border-[#333] bg-[#1f1f1f] text-[#888]">
                <LuFileText className="h-4 w-4" />
              </div>
              <div className="min-w-0">
                <p className="m-0 truncate text-xs text-[#d4d4d4]">
                  {doc.filename.trim() || doc.id}
                </p>
                <p className="m-0 text-[11px] text-[#777]">
                  {doc.page_count > 0
                    ? `${doc.page_count.toString()} pages`
                    : doc.status}
                </p>
              </div>
              <button
                className="text-xs text-[#888] hover:text-[#f44747]"
                onClick={() => {
                  setPendingDocuments((current) =>
                    current.filter((entry) => entry.id !== doc.id),
                  );
                }}
                type="button"
              >
                ✕
              </button>
            </div>
          ))}
          {pendingCollections.map((collection) => (
            <div
              className="flex items-center gap-2 border border-[#333] bg-[#252525] px-2 py-1.5"
              key={`collection-${collection.id}`}
            >
              <div className="flex h-10 w-10 items-center justify-center border border-[#333] bg-[#1f1f1f] text-[#888]">
                <LuFolder className="h-4 w-4" />
              </div>
              <div className="min-w-0">
                <p className="m-0 truncate text-xs text-[#d4d4d4]">
                  {collection.name.trim() || collection.id}
                </p>
                <p className="m-0 text-[11px] text-[#777]">
                  {collection.documents.length === 1
                    ? "1 doc"
                    : `${collection.documents.length.toString()} docs`}
                </p>
              </div>
              <button
                className="text-xs text-[#888] hover:text-[#f44747]"
                onClick={() => {
                  setPendingCollections((current) =>
                    current.filter((entry) => entry.id !== collection.id),
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
            const docs = pendingDocuments.slice();
            const collections = pendingCollections.slice();
            setDraft("");
            setPendingImages([]);
            void sendMessage(text, imgs, docs, collections);
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

              <button
                className={`relative text-[#888] hover:text-[#d4d4d4] disabled:cursor-not-allowed disabled:opacity-40 ${
                  persistedBadgeCount > 0 ? "mr-1" : ""
                }`}
                disabled={totalBadgeCount === 0}
                onClick={() => { setAttachmentsOpen(true); }}
                title="View attached documents"
                type="button"
              >
                <LuPaperclip className="h-4 w-4" />
                {persistedBadgeCount > 0 && (
                  <span className="absolute -top-1.5 -right-2 min-w-4 rounded-full bg-[#007acc] px-1 text-[10px] leading-4 text-white">
                    {persistedBadgeCount.toString()}
                  </span>
                )}
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
              className="flex items-center justify-center bg-[#007acc] p-1.5 text-white transition hover:bg-[#1b8de4] disabled:cursor-not-allowed disabled:opacity-40"
              disabled={submitDisabled}
              title="Send"
              type="submit"
            >
              <LuSend className="h-4 w-4" />
            </button>
          </div>
        </form>
      </div>

      {attachmentsOpen && (
        <div
          className="fixed inset-0 z-50 flex items-center justify-center bg-[rgba(0,0,0,0.55)] px-4"
          onClick={() => { setAttachmentsOpen(false); }}
        >
          <div
            className="flex max-h-[80vh] w-full max-w-xl flex-col border border-[#333] bg-[#1f1f1f] shadow-[0_18px_60px_rgba(0,0,0,0.45)]"
            onClick={(event) => { event.stopPropagation(); }}
          >
            <div className="flex items-center justify-between border-b border-[#333] px-4 py-3">
              <div>
                <p className="m-0 text-sm font-medium text-[#d4d4d4]">Thread attachments</p>
                <p className="m-0 mt-1 text-xs text-[#777]">
                  View the documents and collections already attached to this thread, plus anything queued for the next send.
                </p>
              </div>
              <button
                className="text-xs text-[#888] hover:text-[#d4d4d4]"
                onClick={() => { setAttachmentsOpen(false); }}
                type="button"
              >
                Close
              </button>
            </div>

            <div className="min-h-0 flex-1 overflow-y-auto px-4 py-4">
              <div className="space-y-6">
                <section>
                  <div className="mb-2 flex items-center justify-between">
                    <h3 className="m-0 text-xs font-semibold uppercase tracking-widest text-[#888]">
                      Attached documents
                    </h3>
                    <span className="text-xs text-[#666]">
                      {attachedDocuments.length.toString()}
                    </span>
                  </div>
                  {attachedDocuments.length === 0 ? (
                    <p className="m-0 border border-dashed border-[#333] px-3 py-3 text-sm text-[#666]">
                      No persisted documents are attached to this thread yet.
                    </p>
                  ) : (
                    <div className="space-y-2">
                      {attachedDocuments.map((document) => (
                        <div
                          className="flex items-center gap-3 border border-[#333] bg-[#252525] px-3 py-2"
                          key={document.id}
                        >
                          <div className="flex h-9 w-9 items-center justify-center border border-[#333] bg-[#1b1b1b] text-[#888]">
                            <LuFileText className="h-4 w-4" />
                          </div>
                          <div className="min-w-0 flex-1">
                            <p className="m-0 truncate text-sm text-[#d4d4d4]">
                              {document.filename.trim() || document.id}
                            </p>
                          </div>
                        </div>
                      ))}
                    </div>
                  )}
                </section>

                <section>
                  <div className="mb-2 flex items-center justify-between">
                    <h3 className="m-0 text-xs font-semibold uppercase tracking-widest text-[#888]">
                      Attached collections
                    </h3>
                    <span className="text-xs text-[#666]">
                      {attachedCollections.length.toString()}
                    </span>
                  </div>
                  {attachedCollections.length === 0 ? (
                    <p className="m-0 border border-dashed border-[#333] px-3 py-3 text-sm text-[#666]">
                      No collections are attached to this thread yet.
                    </p>
                  ) : (
                    <div className="space-y-2">
                      {attachedCollections.map((collection) => (
                        <div
                          className="flex items-center gap-3 border border-[#333] bg-[#252525] px-3 py-2"
                          key={collection.id}
                        >
                          <div className="flex h-9 w-9 items-center justify-center border border-[#333] bg-[#1b1b1b] text-[#888]">
                            <LuFolder className="h-4 w-4" />
                          </div>
                          <div className="min-w-0 flex-1">
                            <p className="m-0 truncate text-sm text-[#d4d4d4]">
                              {collection.name.trim() || collection.id}
                            </p>
                            <p className="m-0 text-[11px] text-[#777]">
                              {collection.documents.length === 1
                                ? "1 doc"
                                : `${collection.documents.length.toString()} docs`}
                            </p>
                          </div>
                        </div>
                      ))}
                    </div>
                  )}
                </section>

                <section>
                  <div className="mb-2 flex items-center justify-between">
                    <h3 className="m-0 text-xs font-semibold uppercase tracking-widest text-[#888]">
                      Pending next send
                    </h3>
                    <span className="text-xs text-[#666]">
                      {(pendingDocuments.length + pendingCollections.length).toString()}
                    </span>
                  </div>
                  {pendingDocuments.length === 0 && pendingCollections.length === 0 ? (
                    <p className="m-0 border border-dashed border-[#333] px-3 py-3 text-sm text-[#666]">
                      No new documents or collections are queued right now.
                    </p>
                  ) : (
                    <div className="space-y-2">
                      {pendingDocuments.map((document) => (
                        <div
                          className="flex items-center gap-3 border border-[#333] bg-[#252525] px-3 py-2"
                          key={`doc-${document.id.toString()}`}
                        >
                          <div className="flex h-9 w-9 items-center justify-center border border-[#333] bg-[#1b1b1b] text-[#888]">
                            <LuFileText className="h-4 w-4" />
                          </div>
                          <div className="min-w-0 flex-1">
                            <p className="m-0 truncate text-sm text-[#d4d4d4]">
                              {document.filename.trim() || document.id}
                            </p>
                          </div>
                          <button
                            className="text-xs text-[#888] hover:text-[#f44747]"
                            onClick={() => {
                              setPendingDocuments((current) =>
                                current.filter((entry) => entry.id !== document.id),
                              );
                            }}
                            type="button"
                          >
                            Remove
                          </button>
                        </div>
                      ))}
                      {pendingCollections.map((collection) => (
                        <div
                          className="flex items-center gap-3 border border-[#333] bg-[#252525] px-3 py-2"
                          key={`collection-${collection.id}`}
                        >
                          <div className="flex h-9 w-9 items-center justify-center border border-[#333] bg-[#1b1b1b] text-[#888]">
                            <LuFolder className="h-4 w-4" />
                          </div>
                          <div className="min-w-0 flex-1">
                            <p className="m-0 truncate text-sm text-[#d4d4d4]">
                              {collection.name.trim() || collection.id}
                            </p>
                            <p className="m-0 text-[11px] text-[#777]">
                              {collection.documents.length === 1
                                ? "1 doc"
                                : `${collection.documents.length.toString()} docs`}
                            </p>
                          </div>
                          <button
                            className="text-xs text-[#888] hover:text-[#f44747]"
                            onClick={() => {
                              setPendingCollections((current) =>
                                current.filter((entry) => entry.id !== collection.id),
                              );
                            }}
                            type="button"
                          >
                            Remove
                          </button>
                        </div>
                      ))}
                    </div>
                  )}
                </section>
              </div>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
