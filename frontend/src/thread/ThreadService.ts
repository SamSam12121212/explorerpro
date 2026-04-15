import { appStreamManager } from "../stream/appStream";
import { apiGet, apiPost, checkHealthApi, uploadImage } from "../api";
import { DEFAULT_INSTRUCTIONS, DEFAULT_MODEL, EXPLORER_TOOLS } from "../constants";
import type {
  AttachedDocument,
  MessageRole,
  ThreadCreateResponse,
  ThreadItemsResponse,
  ThreadListResponse,
  ThreadResponse,
  ThreadStreamItemsDeltaMessage,
  ThreadStreamPayload,
  ThreadStreamSnapshotMessage,
  UploadedImage,
} from "../types";
import type { ThreadEntry, ThreadState } from "./types";
import {
  buildMessagesFromItems,
  deriveThinkingFromEvent,
  deriveThinkingFromItems,
  logStreamPayload,
  mergeMessages,
  statusMeansBusy,
} from "./eventInterpreter";

type Listener = () => void;

const ACTIVE_THREAD_STORAGE_KEY = "explorer.activeThreadId";

function normalizeAttachedDocuments(documents?: AttachedDocument[]): AttachedDocument[] {
  return (documents ?? []).map((d) => ({
    id: d.id,
    filename: d.filename,
    page_count: d.page_count,
    status: d.status,
  }));
}

function mergeAttachedDocuments(current: AttachedDocument[], incoming: AttachedDocument[]): AttachedDocument[] {
  const merged = [...current];
  const seen = new Set(current.map((d) => d.id));
  for (const doc of incoming) {
    if (!seen.has(doc.id)) {
      seen.add(doc.id);
      merged.push(doc);
    }
  }
  return merged;
}

function buildUserInputItems(text: string, images: UploadedImage[]) {
  const content: Record<string, string>[] = [];
  if (text) content.push({ type: "input_text", text });
  for (const image of images) {
    content.push({
      type: "image_ref",
      image_ref: image.image_ref,
      content_type: image.content_type ?? "",
      filename: image.filename ?? "",
    });
  }
  return [{ type: "message", role: "user", content }];
}

function readPersistedActiveThreadId(): number | null {
  if (typeof window === "undefined") return null;
  try {
    const raw = window.localStorage.getItem(ACTIVE_THREAD_STORAGE_KEY)?.trim() ?? "";
    if (!raw) return null;
    const parsed = Number.parseInt(raw, 10);
    return Number.isSafeInteger(parsed) && parsed > 0 ? parsed : null;
  } catch {
    return null;
  }
}

function writePersistedActiveThreadId(threadId: number | null) {
  if (typeof window === "undefined") return;
  try {
    if (threadId !== null) {
      window.localStorage.setItem(ACTIVE_THREAD_STORAGE_KEY, String(threadId));
    } else {
      window.localStorage.removeItem(ACTIVE_THREAD_STORAGE_KEY);
    }
  } catch { /* ignore */ }
}

const initialState: ThreadState = {
  threadId: null,
  messages: [],
  phase: "idle",
  thinking: false,
  model: DEFAULT_MODEL,
  attachedDocuments: [],
  pendingDocuments: [],
  pendingImages: [],
  draft: "",
  reasoningEffort: "medium",
  apiStatus: "checking",
  threads: [],
  threadsLoading: true,
  uploadCount: 0,
};

export class ThreadService {
  private state: ThreadState = initialState;
  private readonly listeners = new Set<Listener>();
  private initialized = false;
  private lastThreadStatus: string | null = null;
  private readonly previewUrls = new Set<string>();

  subscribe = (listener: Listener): (() => void) => {
    this.listeners.add(listener);
    return () => { this.listeners.delete(listener); };
  };

  getSnapshot = (): ThreadState => this.state;

  initialize() {
    if (this.initialized) return;
    this.initialized = true;

    appStreamManager.subscribe(this.handleStreamMessage);
    appStreamManager.subscribeStatus(this.handleStreamStatus);
    appStreamManager.connect();
    void this.checkHealth();

    const persistedThreadId = readPersistedActiveThreadId();
    if (persistedThreadId) void this.loadThread(persistedThreadId);

    void this.fetchThreadList();
  }

  dispose() {
    for (const url of this.previewUrls) {
      URL.revokeObjectURL(url);
    }
    this.previewUrls.clear();
  }

  // --- State setters exposed as actions ---

  setDraft = (value: string) => {
    this.setState((s) => ({ ...s, draft: value }));
  };

  setModel = (value: string) => {
    this.setState((s) => ({ ...s, model: value }));
  };

  setReasoningEffort = (value: ThreadState["reasoningEffort"]) => {
    this.setState((s) => ({ ...s, reasoningEffort: value }));
  };

  setPendingDocuments = (updater: AttachedDocument[] | ((current: AttachedDocument[]) => AttachedDocument[])) => {
    this.setState((s) => ({
      ...s,
      pendingDocuments: typeof updater === "function" ? updater(s.pendingDocuments) : updater,
    }));
  };

  setPendingImages = (updater: UploadedImage[] | ((current: UploadedImage[]) => UploadedImage[])) => {
    this.setState((s) => ({
      ...s,
      pendingImages: typeof updater === "function" ? updater(s.pendingImages) : updater,
    }));
  };

  attachDocument = (document: AttachedDocument) => {
    this.setState((s) => {
      if (s.attachedDocuments.some((d) => d.id === document.id)) return s;
      if (s.pendingDocuments.some((d) => d.id === document.id)) return s;
      return { ...s, pendingDocuments: [...s.pendingDocuments, document] };
    });
  };

  // --- Async actions ---

  addPendingFiles = async (files: File[]) => {
    for (const file of files) {
      this.setState((s) => ({ ...s, uploadCount: s.uploadCount + 1 }));
      try {
        const uploaded = await uploadImage(file);
        const previewUrl = URL.createObjectURL(file);
        this.previewUrls.add(previewUrl);
        this.setState((s) => ({
          ...s,
          pendingImages: [...s.pendingImages, { ...uploaded, preview_url: previewUrl }],
        }));
      } catch (error) {
        this.appendMessage("error", error instanceof Error ? error.message : "Image upload failed");
      } finally {
        this.setState((s) => ({ ...s, uploadCount: s.uploadCount - 1 }));
      }
    }
  };

  loadThread = async (nextThreadId: number) => {
    if (nextThreadId === this.state.threadId) return;

    this.setState((s) => ({ ...s, phase: "loading", thinking: false }));

    try {
      const [threadInfo, payload] = await Promise.all([
        apiGet<ThreadResponse>(`/threads/${nextThreadId.toString()}`),
        apiGet<ThreadItemsResponse>(`/threads/${nextThreadId.toString()}/items?limit=200`),
      ]);

      const nextStatus = threadInfo.thread?.status ?? null;
      const nextThinking = deriveThinkingFromItems(payload.items) ?? false;
      this.lastThreadStatus = nextStatus;
      writePersistedActiveThreadId(nextThreadId);

      this.setState((s) => ({
        ...s,
        threadId: nextThreadId,
        model: threadInfo.thread?.model ?? DEFAULT_MODEL,
        attachedDocuments: normalizeAttachedDocuments(threadInfo.attached_documents),
        messages: buildMessagesFromItems(payload),
        phase: statusMeansBusy(nextStatus ?? undefined) ? "streaming" : "idle",
        thinking: nextThinking,
        pendingDocuments: [],
        pendingImages: [],
        draft: "",
      }));
    } catch (error) {
      this.lastThreadStatus = null;
      this.setState((s) => ({
        ...s,
        messages: [{ id: crypto.randomUUID(), role: "error", text: error instanceof Error ? error.message : "Failed to load thread" }],
        phase: "error",
        thinking: false,
        threadId: null,
        attachedDocuments: [],
      }));
    }
  };

  sendMessage = async (text: string, images: UploadedImage[], documents: AttachedDocument[]) => {
    if (!text && images.length === 0) return;

    this.setState((s) => ({ ...s, phase: "streaming", thinking: false }));
    const optimisticId = this.appendOptimisticUserMessage(text, images);

    try {
      const inputItems = buildUserInputItems(text, images);
      const reasoning = { effort: this.state.reasoningEffort, summary: "concise" } as const;
      const terminalStatuses = ["failed", "incomplete", "cancelled"];
      let currentThreadId = this.state.threadId;
      if (currentThreadId && terminalStatuses.includes(this.lastThreadStatus ?? "")) {
        currentThreadId = null;
      }

      if (!currentThreadId) {
        const created = await apiPost<ThreadCreateResponse>("/threads", {
          model: this.state.model,
          instructions: DEFAULT_INSTRUCTIONS,
          input: inputItems,
          attached_document_ids: documents.map((d) => d.id),
          tools: EXPLORER_TOOLS,
          reasoning,
          store: true,
        });

        currentThreadId = created.thread_id ?? null;
        if (!currentThreadId) throw new Error("No thread_id returned");

        writePersistedActiveThreadId(currentThreadId);
        this.setState((s) => ({
          ...s,
          threadId: currentThreadId,
          attachedDocuments: normalizeAttachedDocuments(documents),
          pendingDocuments: [],
        }));
      } else {
        await apiPost(`/threads/${currentThreadId.toString()}/commands`, {
          kind: "thread.resume",
          body: {
            input_items: inputItems,
            reasoning,
            attached_document_ids: documents.map((d) => d.id),
          },
        });

        this.setState((s) => ({
          ...s,
          attachedDocuments: mergeAttachedDocuments(s.attachedDocuments, normalizeAttachedDocuments(documents)),
          pendingDocuments: [],
        }));
      }

      void this.fetchThreadList();
    } catch (error) {
      this.removeMessage(optimisticId);
      this.appendMessage("error", error instanceof Error ? error.message : "Request failed");
      this.setState((s) => ({ ...s, phase: "idle" }));
    } finally {
      void this.checkHealth();
    }
  };

  resetConversation = () => {
    this.lastThreadStatus = null;
    writePersistedActiveThreadId(null);
    this.setState((s) => ({
      ...initialState,
      apiStatus: s.apiStatus,
      threads: s.threads,
      threadsLoading: s.threadsLoading,
    }));
  };

  refreshThreadList = () => {
    void this.fetchThreadList();
  };

  // --- Private helpers ---

  private emit() {
    for (const listener of this.listeners) listener();
  }

  private setState(updater: (current: ThreadState) => ThreadState) {
    const next = updater(this.state);
    if (next === this.state) return;
    this.state = next;
    this.emit();
  }

  private appendMessage(role: MessageRole, text: string, images?: UploadedImage[]) {
    this.setState((s) => ({
      ...s,
      messages: [...s.messages, { id: crypto.randomUUID(), role, text, images }],
    }));
  }

  private appendOptimisticUserMessage(text: string, images: UploadedImage[]): string {
    const id = `optimistic-${crypto.randomUUID()}`;
    this.setState((s) => ({
      ...s,
      messages: [...s.messages, { id, role: "user", text, images: images.length > 0 ? images : undefined, optimistic: true }],
    }));
    return id;
  }

  private removeMessage(messageId: string) {
    this.setState((s) => ({
      ...s,
      messages: s.messages.filter((m) => m.id !== messageId),
    }));
  }

  private async checkHealth() {
    try {
      const status = await checkHealthApi();
      this.setState((s) => ({ ...s, apiStatus: status }));
    } catch {
      this.setState((s) => ({ ...s, apiStatus: "offline" }));
    }
  }

  private async fetchThreadList() {
    try {
      const payload = await apiGet<ThreadListResponse>("/threads?limit=100");
      const threads: ThreadEntry[] = (payload.threads ?? [])
        .filter((t) => Boolean(t.id))
        .map((t) => ({
          id: t.id ?? 0,
          label: t.label?.trim() ?? "New thread",
          previewText: t.preview_text?.trim() ?? "",
          updatedAt: t.updated_at ?? t.created_at ?? "",
        }));
      this.setState((s) => ({ ...s, threads, threadsLoading: false }));
    } catch {
      this.setState((s) => ({ ...s, threadsLoading: false }));
    }
  }

  // --- Stream handlers ---

  private readonly handleStreamStatus = (status: "online" | "degraded") => {
    this.setState((s) => ({ ...s, apiStatus: status }));
  };

  private readonly handleStreamMessage = (payload: ThreadStreamPayload) => {
    logStreamPayload(payload);

    if (!this.state.threadId || payload.thread_id !== this.state.threadId) return;

    switch (payload.type) {
      case "thread.snapshot":
        this.handleSnapshot(payload as ThreadStreamSnapshotMessage);
        break;
      case "thread.items.delta": {
        const delta = payload as ThreadStreamItemsDeltaMessage;
        const nextThinking = deriveThinkingFromItems(delta.items);
        const nextMessages = buildMessagesFromItems({ items: delta.items, page: delta.page });

        this.setState((s) => ({
          ...s,
          thinking: nextThinking ?? s.thinking,
          messages: nextMessages.length > 0 ? mergeMessages(s.messages, nextMessages) : s.messages,
        }));
        break;
      }
      case "thread.heartbeat":
        break;
      default:
        this.handleOpenAIEvent(payload);
        break;
    }
  };

  private handleSnapshot(payload: ThreadStreamSnapshotMessage) {
    const nextStatus = payload.thread?.status ?? null;
    const previousStatus = this.lastThreadStatus;
    const nextBusy = statusMeansBusy(nextStatus ?? undefined);
    this.lastThreadStatus = nextStatus;

    this.setState((s) => ({
      ...s,
      phase: nextBusy ? "streaming" : "idle",
      thinking: nextBusy ? s.thinking : false,
      model: payload.thread?.model ?? DEFAULT_MODEL,
      attachedDocuments: normalizeAttachedDocuments(payload.attached_documents),
    }));

    if (nextStatus && ["failed", "incomplete", "cancelled"].includes(nextStatus) && previousStatus !== nextStatus) {
      this.appendMessage("error", `Thread ended with status: ${nextStatus}`);
    }

    if (!nextBusy) void this.fetchThreadList();
  }

  private handleOpenAIEvent(payload: ThreadStreamPayload) {
    const nextThinking = deriveThinkingFromEvent(payload as Record<string, unknown>);
    if (nextThinking !== null) {
      this.setState((s) => ({ ...s, thinking: nextThinking }));
    }
  }
}
