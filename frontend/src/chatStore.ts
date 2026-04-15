import { appStreamManager } from "./stream/appStream";
import {
  apiGet,
  apiPost,
  checkHealthApi,
} from "./api";
import { DEFAULT_INSTRUCTIONS, DEFAULT_MODEL, EXPLORER_TOOLS } from "./constants";
import type {
  AttachedDocument,
  ChatMessage,
  HealthState,
  MessageRole,
  ReasoningEffort,
  ThreadCreateResponse,
  ThreadItemsResponse,
  ThreadResponse,
  ThreadStreamItemsDeltaMessage,
  ThreadStreamPayload,
  ThreadStreamSnapshotMessage,
  UploadedImage,
} from "./types";

type Listener = () => void;

export interface ChatStoreState {
  messages: ChatMessage[];
  busy: boolean;
  thinking: boolean;
  threadId: number | null;
  attachedDocuments: AttachedDocument[];
  apiStatus: HealthState;
  model: string;
}

const initialChatStoreState: ChatStoreState = {
  messages: [],
  busy: false,
  thinking: false,
  threadId: null,
  attachedDocuments: [],
  apiStatus: "checking",
  model: DEFAULT_MODEL,
};

const reasoningEventTypes = new Set([
  "response.reasoning_summary_part.added",
  "response.reasoning_summary_part.done",
  "response.reasoning_summary_text.delta",
  "response.reasoning_summary_text.done",
  "response.reasoning_text.delta",
  "response.reasoning_text.done",
]);

const stopThinkingEventTypes = new Set([
  "error",
  "response.completed",
  "response.failed",
  "response.function_call_arguments.done",
  "response.incomplete",
  "response.output_item.added",
  "response.output_item.done",
  "response.output_text.delta",
  "response.output_text.done",
  "response.refusal.done",
]);

const ACTIVE_THREAD_STORAGE_KEY = "explorer.activeThreadId";

function buildUserInputItems(text: string, images: UploadedImage[]) {
  const content: Record<string, string>[] = [];

  if (text) {
    content.push({ type: "input_text", text });
  }

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

function normalizeAttachedDocuments(documents?: AttachedDocument[]) {
  return (documents ?? []).map((document) => ({
    id: document.id,
    filename: document.filename,
    page_count: document.page_count,
    status: document.status,
  }));
}

function mergeAttachedDocuments(current: AttachedDocument[], incoming: AttachedDocument[]) {
  const merged = [...current];
  const seen = new Set(current.map((document) => document.id));

  for (const document of incoming) {
    if (seen.has(document.id)) {
      continue;
    }
    seen.add(document.id);
    merged.push(document);
  }

  return merged;
}

function extractMessageText(item: NonNullable<ThreadItemsResponse["items"]>[number]) {
  return (item.payload?.content ?? [])
    .filter(
      (content) =>
        content.type === "input_text" || content.type === "output_text",
    )
    .map((content) => content.text?.trim() ?? "")
    .filter(Boolean)
    .join("\n\n");
}

function blobRefToUrl(ref: string): string {
  return "/" + ref.replace("blob://", "");
}

function extractMessageImages(item: NonNullable<ThreadItemsResponse["items"]>[number]): UploadedImage[] {
  return (item.payload?.content ?? []).flatMap((content) => {
    if (content.type !== "image_ref" || !content.image_ref) {
      return [];
    }

    return [{
      image_id: content.image_ref.split("/").at(-2) ?? crypto.randomUUID(),
      image_ref: content.image_ref,
      content_type: content.content_type,
      filename: content.filename,
      preview_url: blobRefToUrl(content.image_ref),
    }];
  });
}

function buildMessagesFromItems(itemsResponse: ThreadItemsResponse): ChatMessage[] {
  const messages: ChatMessage[] = [];

  for (const item of itemsResponse.items ?? []) {
    if (item.item_type !== "message") {
      continue;
    }

    const text = extractMessageText(item);
    const images = extractMessageImages(item);
    if (!text && images.length === 0) {
      continue;
    }

    let role: MessageRole = "assistant";
    if (item.direction === "input" || item.payload?.role === "user") {
      role = "user";
    }

    messages.push({
      id: item.cursor ?? crypto.randomUUID(),
      role,
      text,
      images: images.length > 0 ? images : undefined,
    });
  }

  return messages;
}

function sameImageRefs(left?: UploadedImage[], right?: UploadedImage[]) {
  if ((left?.length ?? 0) !== (right?.length ?? 0)) {
    return false;
  }
  return (left ?? []).every((image, index) => image.image_ref === right?.[index]?.image_ref);
}

function sameMessageContent(left: ChatMessage, right: ChatMessage) {
  return left.role === right.role &&
    left.text === right.text &&
    sameImageRefs(left.images, right.images);
}

function mergeMessages(current: ChatMessage[], incoming: ChatMessage[]) {
  if (incoming.length === 0) {
    return current;
  }

  const merged = [...current];
  const seen = new Set(
    current
      .filter((message) => !message.optimistic)
      .map((message) => message.id),
  );

  for (const message of incoming) {
    if (seen.has(message.id)) {
      continue;
    }
    seen.add(message.id);
    if (message.role === "user") {
      const optimisticIndex = merged.findIndex(
        (candidate) =>
          candidate.optimistic &&
          candidate.role === "user" &&
          sameMessageContent(candidate, message),
      );
      if (optimisticIndex >= 0) {
        merged[optimisticIndex] = message;
        continue;
      }
    }
    merged.push(message);
  }

  return merged;
}

function statusMeansBusy(status?: string) {
  return !["ready", "completed", "failed", "incomplete", "cancelled"].includes(status ?? "");
}

function parseStoredThreadId(raw: string | null | undefined) {
  const normalized = raw?.trim() ?? "";
  if (!normalized) {
    return null;
  }

  const parsed = Number.parseInt(normalized, 10);
  if (!Number.isSafeInteger(parsed) || parsed <= 0) {
    return null;
  }

  return parsed;
}

function readPersistedActiveThreadId() {
  if (typeof window === "undefined") {
    return null;
  }

  try {
    return parseStoredThreadId(window.localStorage.getItem(ACTIVE_THREAD_STORAGE_KEY));
  } catch {
    return null;
  }
}

function writePersistedActiveThreadId(threadId: number | null) {
  if (typeof window === "undefined") {
    return;
  }

  try {
    if (threadId !== null) {
      window.localStorage.setItem(ACTIVE_THREAD_STORAGE_KEY, String(threadId));
    } else {
      window.localStorage.removeItem(ACTIVE_THREAD_STORAGE_KEY);
    }
  } catch {
    /* ignore storage write failures */
  }
}

function isReasoningItemEvent(event: Record<string, unknown>): boolean {
  const item = event.item;
  return typeof item === "object" && item !== null && (item as Record<string, unknown>).type === "reasoning";
}

function deriveThinkingFromEvent(event: Record<string, unknown>): boolean | null {
  const type = event.type;
  if (typeof type !== "string") {
    return null;
  }

  if (reasoningEventTypes.has(type)) {
    return true;
  }

  if ((type === "response.output_item.added" || type === "response.output_item.done") && isReasoningItemEvent(event)) {
    return true;
  }

  if (stopThinkingEventTypes.has(type)) {
    return false;
  }

  return null;
}

function deriveThinkingStateFromItems(items?: ThreadItemsResponse["items"]) {
  let nextThinking: boolean | null = null;

  for (const item of items ?? []) {
    if (item.item_type === "reasoning") {
      nextThinking = true;
      continue;
    }

    if (item.direction === "output" && item.item_type && item.item_type !== "reasoning") {
      nextThinking = false;
    }
  }

  return nextThinking;
}

class ChatStore {
  private state = initialChatStoreState;
  private readonly listeners = new Set<Listener>();
  private initialized = false;
  private lastThreadStatus: string | null = null;

  subscribe = (listener: Listener) => {
    this.listeners.add(listener);
    return () => {
      this.listeners.delete(listener);
    };
  };

  getSnapshot = () => this.state;

  initialize() {
    if (this.initialized) {
      return;
    }

    this.initialized = true;
    appStreamManager.subscribe(this.handleThreadStreamMessage);
    appStreamManager.subscribeStatus(this.handleStreamStatus);
    appStreamManager.connect();
    void this.checkHealth();

    const persistedThreadId = readPersistedActiveThreadId();
    if (persistedThreadId) {
      void this.loadThread(persistedThreadId);
    }
  }

  setModel(model: string) {
    this.setState((current) => ({ ...current, model }));
  }

  reportError(text: string) {
    this.appendMessage("error", text);
  }

  async checkHealth() {
    try {
      const status = await checkHealthApi();
      this.setState((current) => ({ ...current, apiStatus: status }));
    } catch {
      this.setState((current) => ({ ...current, apiStatus: "offline" }));
    }
  }

  async loadThread(nextThreadId: number) {
    this.setState((current) => ({
      ...current,
      busy: true,
      thinking: false,
    }));

    try {
      const [threadInfo, payload] = await Promise.all([
        apiGet<ThreadResponse>(`/threads/${nextThreadId.toString()}`),
        apiGet<ThreadItemsResponse>(
          `/threads/${nextThreadId.toString()}/items?limit=200`,
        ),
      ]);

      const nextStatus = threadInfo.thread?.status ?? null;
      const nextThinking = deriveThinkingStateFromItems(payload.items) ?? false;

      this.lastThreadStatus = nextStatus;
      writePersistedActiveThreadId(nextThreadId);

      this.setState((current) => ({
        ...current,
        threadId: nextThreadId,
        model: threadInfo.thread?.model ?? DEFAULT_MODEL,
        attachedDocuments: normalizeAttachedDocuments(threadInfo.attached_documents),
        messages: buildMessagesFromItems(payload),
        busy: statusMeansBusy(nextStatus ?? undefined),
        thinking: nextThinking,
      }));

      return true;
    } catch (error) {
      this.lastThreadStatus = null;
      this.setState((current) => ({
        ...current,
        messages: [{
          id: crypto.randomUUID(),
          role: "error",
          text: error instanceof Error ? error.message : "Failed to load thread",
        }],
        busy: false,
        thinking: false,
        threadId: null,
        attachedDocuments: [],
      }));
      return false;
    }
  }

  async sendMessage(args: {
    text: string;
    images: UploadedImage[];
    documents: AttachedDocument[];
    reasoningEffort: ReasoningEffort;
  }) {
    const { text, images, documents, reasoningEffort } = args;
    if (!text && images.length === 0) {
      return false;
    }

    this.setState((current) => ({
      ...current,
      busy: true,
      thinking: false,
    }));

    const optimisticMessageId = this.appendOptimisticUserMessage(text, images);

    try {
      const inputItems = buildUserInputItems(text, images);
      const reasoning = { effort: reasoningEffort, summary: "concise" } as const;
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
          attached_document_ids: documents.map((document) => document.id),
          tools: EXPLORER_TOOLS,
          reasoning,
          store: true,
        });

        currentThreadId = created.thread_id ?? null;
        if (!currentThreadId) {
          throw new Error("No thread_id returned");
        }

        writePersistedActiveThreadId(currentThreadId);
        this.setState((current) => ({
          ...current,
          threadId: currentThreadId,
          attachedDocuments: normalizeAttachedDocuments(documents),
        }));
      } else {
        await apiPost(`/threads/${currentThreadId.toString()}/commands`, {
          kind: "thread.resume",
          body: {
            input_items: inputItems,
            reasoning,
            attached_document_ids: documents.map((document) => document.id),
          },
        });

        this.setState((current) => ({
          ...current,
          attachedDocuments: mergeAttachedDocuments(
            current.attachedDocuments,
            normalizeAttachedDocuments(documents),
          ),
        }));
      }
      return true;
    } catch (error) {
      this.removeMessage(optimisticMessageId);
      this.appendMessage(
        "error",
        error instanceof Error ? error.message : "Request failed",
      );
      this.setState((current) => ({
        ...current,
        busy: false,
      }));
      return false;
    } finally {
      void this.checkHealth();
    }
  }

  resetConversation() {
    this.lastThreadStatus = null;
    writePersistedActiveThreadId(null);
    this.setState((current) => ({
      ...initialChatStoreState,
      apiStatus: current.apiStatus,
    }));
  }

  private emit() {
    for (const listener of this.listeners) {
      listener();
    }
  }

  private setState(updater: (current: ChatStoreState) => ChatStoreState) {
    const nextState = updater(this.state);
    if (nextState === this.state) {
      return;
    }
    this.state = nextState;
    this.emit();
  }

  private appendMessage(role: MessageRole, text: string, images?: UploadedImage[]) {
    this.setState((current) => ({
      ...current,
      messages: [
        ...current.messages,
        { id: crypto.randomUUID(), role, text, images },
      ],
    }));
  }

  private appendOptimisticUserMessage(text: string, images: UploadedImage[]) {
    const id = `optimistic-${crypto.randomUUID()}`;
    this.setState((current) => ({
      ...current,
      messages: [
        ...current.messages,
        {
          id,
          role: "user",
          text,
          images: images.length > 0 ? images : undefined,
          optimistic: true,
        },
      ],
    }));
    return id;
  }

  private removeMessage(messageId: string) {
    this.setState((current) => ({
      ...current,
      messages: current.messages.filter((message) => message.id !== messageId),
    }));
  }

  private readonly handleStreamStatus = (status: "online" | "degraded") => {
    this.setState((current) => ({
      ...current,
      apiStatus: status,
    }));
  };

  private readonly handleThreadSnapshot = (payload: ThreadResponse) => {
    const nextStatus = payload.thread?.status ?? null;
    const previousStatus = this.lastThreadStatus;
    const nextBusy = statusMeansBusy(nextStatus ?? undefined);
    this.lastThreadStatus = nextStatus;

    this.setState((current) => ({
      ...current,
      busy: nextBusy,
      thinking: nextBusy ? current.thinking : false,
      model: payload.thread?.model ?? DEFAULT_MODEL,
      attachedDocuments: normalizeAttachedDocuments(payload.attached_documents),
    }));

    if (
      nextStatus &&
      ["failed", "incomplete", "cancelled"].includes(nextStatus) &&
      previousStatus !== nextStatus
    ) {
      this.appendMessage("error", `Thread ended with status: ${nextStatus}`);
    }
  };

  private readonly logThreadStreamMessage = (payload: ThreadStreamPayload) => {
    if (payload.type === "response.output_item.added" || payload.type === "response.output_item.done") {
      console.log(`[ws debug] ${payload.type}`, payload);
    }
  };

  private readonly handleOpenAIEvent = (payload: ThreadStreamPayload) => {
    const nextThinking = deriveThinkingFromEvent(payload as Record<string, unknown>);
    if (nextThinking !== null) {
      this.setState((current) => ({
        ...current,
        thinking: nextThinking,
      }));
    }
  };

  private readonly handleThreadStreamMessage = (payload: ThreadStreamPayload) => {
    this.logThreadStreamMessage(payload);

    if (!this.state.threadId || payload.thread_id !== this.state.threadId) {
      return;
    }

    switch (payload.type) {
      case "thread.snapshot":
        this.handleThreadSnapshot(payload as ThreadStreamSnapshotMessage);
        break;
      case "thread.items.delta": {
        const delta = payload as ThreadStreamItemsDeltaMessage;
        const nextThinking = deriveThinkingStateFromItems(delta.items);
        const nextMessages = buildMessagesFromItems({
          items: delta.items,
          page: delta.page,
        });

        this.setState((current) => ({
          ...current,
          thinking: nextThinking ?? current.thinking,
          messages: nextMessages.length > 0
            ? mergeMessages(current.messages, nextMessages)
            : current.messages,
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
}

export const chatStore = new ChatStore();
