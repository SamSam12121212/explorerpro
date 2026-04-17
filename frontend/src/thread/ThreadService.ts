import { appStreamManager } from "../stream/appStream";
import { apiDelete, apiGet, apiPost, checkHealthApi, uploadImage } from "../api";
import { DEFAULT_INSTRUCTIONS, DEFAULT_MODEL, EXPLORER_TOOLS } from "../constants";
import type {
  AttachedCollection,
  AttachedDocument,
  MessageRole,
  ThreadCreateResponse,
  ThreadItemsResponse,
  ThreadListResponse,
  ThreadResponse,
  ThreadStreamPayload,
  UploadedImage,
} from "../types";
import type { ThreadEntry, ThreadState } from "./types";
import {
  applyDocumentQueryAdded,
  applyOutputItemAdded,
  applyOutputTextDelta,
  buildMessageFromOutputItemDone,
  buildMessagesFromItems,
  deriveThinkingFromEvent,
  deriveThinkingFromItems,
  finalizeStreamingMessages,
  logStreamPayload,
  statusMeansBusy,
  upsertMessage,
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

function normalizeAttachedCollections(collections?: AttachedCollection[]): AttachedCollection[] {
  return (collections ?? []).map((c) => ({
    id: c.id,
    name: c.name,
    documents: normalizeAttachedDocuments(c.documents),
  }));
}

function mergeAttachedCollections(
  current: AttachedCollection[],
  incoming: AttachedCollection[],
): AttachedCollection[] {
  const merged = [...current];
  const byId = new Map(current.map((c, i) => [c.id, i] as const));
  for (const collection of incoming) {
    const existingIdx = byId.get(collection.id);
    if (existingIdx === undefined) {
      byId.set(collection.id, merged.length);
      merged.push(collection);
    } else {
      // Refresh the cached members from the latest response.
      merged[existingIdx] = collection;
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
  attachedCollections: [],
  pendingCollections: [],
  pendingImages: [],
  draft: "",
  reasoningEffort: "medium",
  apiStatus: "checking",
  threads: [],
  threadsLoading: true,
  uploadCount: 0,
  pendingDocumentQueries: [],
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

  setPendingCollections = (updater: AttachedCollection[] | ((current: AttachedCollection[]) => AttachedCollection[])) => {
    this.setState((s) => ({
      ...s,
      pendingCollections: typeof updater === "function" ? updater(s.pendingCollections) : updater,
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

  attachCollection = (collection: AttachedCollection) => {
    this.setState((s) => {
      if (s.attachedCollections.some((c) => c.id === collection.id)) return s;
      if (s.pendingCollections.some((c) => c.id === collection.id)) return s;
      return { ...s, pendingCollections: [...s.pendingCollections, collection] };
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
        attachedCollections: normalizeAttachedCollections(threadInfo.attached_collections),
        messages: buildMessagesFromItems(payload),
        phase: statusMeansBusy(nextStatus ?? undefined) ? "streaming" : "idle",
        thinking: nextThinking,
        pendingDocuments: [],
        pendingCollections: [],
        pendingImages: [],
        pendingDocumentQueries: [],
        draft: "",
      }));
    } catch (error) {
      this.lastThreadStatus = null;
      writePersistedActiveThreadId(null);
      const message = error instanceof Error ? error.message : "Failed to load thread";
      if (message.toLowerCase().includes("not found")) {
        this.setState((s) => ({
          ...initialState,
          apiStatus: s.apiStatus,
          threads: s.threads,
          threadsLoading: s.threadsLoading,
        }));
        return;
      }
      this.setState((s) => ({
        ...s,
        messages: [{ id: crypto.randomUUID(), role: "error", text: message }],
        phase: "error",
        thinking: false,
        threadId: null,
        attachedDocuments: [],
        attachedCollections: [],
        pendingDocumentQueries: [],
      }));
    }
  };

  archiveThread = async (threadId: number) => {
    await apiPost(`/threads/${threadId.toString()}/archive`, {});
    if (threadId === this.state.threadId) {
      this.resetConversation();
    }
    this.setState((s) => ({
      ...s,
      threads: s.threads.filter((thread) => thread.id !== threadId),
    }));
    void this.fetchThreadList();
  };

  deleteThread = async (threadId: number) => {
    await apiDelete(`/threads/${threadId.toString()}`);
    if (threadId === this.state.threadId) {
      this.resetConversation();
    }
    this.setState((s) => ({
      ...s,
      threads: s.threads.filter((thread) => thread.id !== threadId),
    }));
    void this.fetchThreadList();
  };

  sendMessage = async (
    text: string,
    images: UploadedImage[],
    documents: AttachedDocument[],
    collections: AttachedCollection[],
  ) => {
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
          attached_collection_ids: collections.map((c) => c.id),
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
          attachedCollections: normalizeAttachedCollections(collections),
          pendingDocuments: [],
          pendingCollections: [],
        }));
      } else {
        await apiPost(`/threads/${currentThreadId.toString()}/commands`, {
          kind: "thread.resume",
          body: {
            input_items: inputItems,
            reasoning,
            attached_document_ids: documents.map((d) => d.id),
            attached_collection_ids: collections.map((c) => c.id),
          },
        });

        this.setState((s) => ({
          ...s,
          attachedDocuments: mergeAttachedDocuments(s.attachedDocuments, normalizeAttachedDocuments(documents)),
          attachedCollections: mergeAttachedCollections(
            s.attachedCollections,
            normalizeAttachedCollections(collections),
          ),
          pendingDocuments: [],
          pendingCollections: [],
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

    if (!this.state.threadId) return;

    // Route by root_thread_id when present; fall back to thread_id for events
    // that predate the identity tuple (e.g. heartbeat has neither).
    const rootId = payload.root_thread_id ?? payload.thread_id;
    if (rootId !== this.state.threadId) return;

    if (payload.type === "thread.heartbeat") return;

    // Child-thread events carry parent_thread_id > 0. They must not mutate
    // root thread state (phase, lastThreadStatus, messages). Silently drop
    // until the aggregate child-indicator feature is wired up.
    if (payload.parent_thread_id) return;

    this.handleOpenAIEvent(payload);
  };

  private handleOpenAIEvent(payload: ThreadStreamPayload) {
    const event = payload as Record<string, unknown>;

    // Thinking indicator from reasoning / output events.
    //
    // Many event types (including every output_text.delta) produce the same
    // `thinking` value we already hold. Return the current state reference
    // unchanged in that case so `setState` can bail — otherwise every delta
    // would trigger a spurious listener emit and re-render.
    const nextThinking = deriveThinkingFromEvent(event);
    if (nextThinking !== null) {
      this.setState((s) => (s.thinking === nextThinking ? s : { ...s, thinking: nextThinking }));
    }

    // A new turn is starting (model resumed after spawn group, or a fresh
    // user-driven response). Clear the per-turn `query_document` indicator
    // set: any in-flight calls from the previous turn have been resolved
    // (their `function_call_output`s are inputs to this new response).
    if (payload.type === "response.created") {
      this.setState((s) => (
        s.pendingDocumentQueries.length === 0
          ? s
          : { ...s, pendingDocumentQueries: [] }
      ));
    }

    // New output item → seed a streaming stub for assistant messages. Deltas
    // will fill in the text; `.done` replaces with the authoritative payload.
    // applyOutputItemAdded preserves any text already accumulated by an
    // earlier delta (delta-before-added race).
    //
    // Same event also drives the `query_document` indicator: function_call
    // items with name "query_document" get their call_id appended.
    if (payload.type === "response.output_item.added") {
      this.setState((s) => {
        const nextMessages = applyOutputItemAdded(s.messages, event);
        const nextDocQueries = applyDocumentQueryAdded(s.pendingDocumentQueries, event);
        if (nextMessages === s.messages && nextDocQueries === s.pendingDocumentQueries) return s;
        return { ...s, messages: nextMessages, pendingDocumentQueries: nextDocQueries };
      });
    }

    // Text delta → append to the streaming message keyed by item_id.
    // applyOutputTextDelta returns the same array reference for no-op events
    // (empty delta / missing item_id); bail in that case to avoid an emit.
    if (payload.type === "response.output_text.delta") {
      this.setState((s) => {
        const nextMessages = applyOutputTextDelta(s.messages, event);
        return nextMessages === s.messages ? s : { ...s, messages: nextMessages };
      });
    }

    // Completed output items → replace the streaming stub with the
    // authoritative message (this is the "completion is truth" step).
    if (payload.type === "response.output_item.done") {
      const msg = buildMessageFromOutputItemDone(event);
      if (msg) {
        this.setState((s) => ({
          ...s,
          messages: upsertMessage(s.messages, msg),
        }));
      }
    }

    // Terminal response events → update phase and trigger list refresh.
    if (
      payload.type === "response.completed" ||
      payload.type === "response.failed" ||
      payload.type === "response.incomplete"
    ) {
      const terminalStatus =
        payload.type === "response.completed" ? "completed" :
        payload.type === "response.failed" ? "failed" : "incomplete";

      const previousStatus = this.lastThreadStatus;
      this.lastThreadStatus = terminalStatus;

      // Only clear `pendingDocumentQueries` on terminal *failure*: a normal
      // `response.completed` for a turn that emitted query_document calls is
      // immediately followed by spawn-group execution (no events) and then
      // `response.created` for the worker's follow-up turn — which is what
      // clears the indicator. Clearing on `.completed` would hide it during
      // the very window we want to surface.
      const clearDocQueries = terminalStatus !== "completed";

      this.setState((s) => ({
        ...s,
        phase: "idle",
        thinking: false,
        messages: finalizeStreamingMessages(s.messages),
        pendingDocumentQueries: clearDocQueries ? [] : s.pendingDocumentQueries,
      }));

      if (
        terminalStatus !== "completed" &&
        previousStatus !== terminalStatus
      ) {
        this.appendMessage("error", `Thread ended with status: ${terminalStatus}`);
      }

      void this.fetchThreadList();
    }
  }
}
