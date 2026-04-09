import { useCallback, useEffect, useRef, useState } from "react";
import { apiGet, apiPost, buildStreamWebSocketUrl, checkHealthApi, uploadImage } from "./api";
import { DEFAULT_INSTRUCTIONS, DEFAULT_MODEL, EXPLORER_TOOLS } from "./constants";
import type {
  ChatMessage,
  HealthState,
  MessageRole,
  ReasoningEffort,
  ThreadCreateResponse,
  ThreadItemsResponse,
  ThreadStreamMessage,
  ThreadResponse,
  UploadedImage,
} from "./types";

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
  return (item.payload?.content ?? []).flatMap((c) => {
    if (c.type !== "image_ref" || !c.image_ref) return [];
    return [{
      image_id: c.image_ref.split("/").at(-2) ?? crypto.randomUUID(),
      image_ref: c.image_ref,
      content_type: c.content_type,
      filename: c.filename,
      preview_url: blobRefToUrl(c.image_ref),
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

function isReasoningItemEvent(event: Record<string, unknown>): boolean {
  const item = event.item;
  return typeof item === "object" && item !== null && (item as Record<string, unknown>).type === "reasoning";
}

function deriveThinkingState(events?: Record<string, unknown>[]) {
  let nextThinking: boolean | null = null;

  for (const event of events ?? []) {
    const type = event.type;
    if (typeof type !== "string") {
      continue;
    }

    if (reasoningEventTypes.has(type)) {
      nextThinking = true;
      continue;
    }

    if ((type === "response.output_item.added" || type === "response.output_item.done") && isReasoningItemEvent(event)) {
      nextThinking = true;
      continue;
    }

    if (stopThinkingEventTypes.has(type)) {
      nextThinking = false;
    }
  }

  return nextThinking;
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

export function useChat() {
  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [draft, setDraft] = useState("");
  const [busy, setBusy] = useState(false);
  const [thinking, setThinking] = useState(false);
  const [uploadCount, setUploadCount] = useState(0);
  const [threadId, setThreadId] = useState<string | null>(null);
  const [lastItemCursor, setLastItemCursor] = useState<string | null>(null);
  const [pendingImages, setPendingImages] = useState<UploadedImage[]>([]);
  const [apiStatus, setApiStatus] = useState<HealthState>("checking");
  const [model, setModel] = useState(DEFAULT_MODEL);
  const [reasoningEffort, setReasoningEffort] =
    useState<ReasoningEffort>("medium");
  const previewUrlsRef = useRef(new Set<string>());
  const threadStreamRef = useRef<WebSocket | null>(null);
  const closingThreadStreamRef = useRef<WebSocket | null>(null);
  const reconnectTimerRef = useRef<number | null>(null);
  const reconnectDelayRef = useRef(1000);
  const activeThreadIdRef = useRef<string | null>(null);
  const lastItemCursorRef = useRef<string | null>(null);
  const lastThreadStatusRef = useRef<string | null>(null);
  const debugLoggedCreateRef = useRef(false);
  const debugLoggedReasoningItemCountRef = useRef(0);
  const debugLoggedOutputDeltaCountRef = useRef(0);
  const debugLoggedOutputItemCountRef = useRef(0);
  const debugReasoningSeenRef = useRef(false);

  function resetDebugStreamLogging() {
    debugLoggedCreateRef.current = false;
    debugLoggedReasoningItemCountRef.current = 0;
    debugLoggedOutputDeltaCountRef.current = 0;
    debugLoggedOutputItemCountRef.current = 0;
    debugReasoningSeenRef.current = false;
  }

  const checkHealth = useCallback(async () => {
    try {
      const status = await checkHealthApi();
      setApiStatus(status);
    } catch {
      setApiStatus("offline");
    }
  }, []);

  useEffect(() => {
    void checkHealth();
  }, [checkHealth]);

  useEffect(() => {
    const urls = previewUrlsRef.current;
    return () => {
      if (reconnectTimerRef.current !== null) {
        window.clearTimeout(reconnectTimerRef.current);
      }
      if (threadStreamRef.current) {
        threadStreamRef.current.close();
      }
      for (const url of urls) {
        URL.revokeObjectURL(url);
      }
    };
  }, []);

  useEffect(() => {
    activeThreadIdRef.current = threadId;
  }, [threadId]);

  useEffect(() => {
    lastItemCursorRef.current = lastItemCursor;
  }, [lastItemCursor]);

  function appendMessage(
    role: MessageRole,
    text: string,
    images?: UploadedImage[],
  ) {
    setMessages((current) => [
      ...current,
      { id: crypto.randomUUID(), role, text, images },
    ]);
  }

  function appendOptimisticUserMessage(text: string, images: UploadedImage[]) {
    const id = `optimistic-${crypto.randomUUID()}`;
    setMessages((current) => [
      ...current,
      {
        id,
        role: "user",
        text,
        images: images.length > 0 ? images : undefined,
        optimistic: true,
      },
    ]);
    return id;
  }

  function removeMessage(messageId: string) {
    setMessages((current) => current.filter((message) => message.id !== messageId));
  }

  function disconnectSocket(targetThreadId: string) {
    void apiPost(`/threads/${targetThreadId}/commands`, {
      kind: "thread.disconnect_socket",
    }).catch(() => undefined);
  }

  const handleThreadSnapshot = useCallback((payload: ThreadResponse) => {
    const nextStatus = payload.thread?.status ?? null;
    const previousStatus = lastThreadStatusRef.current;
    lastThreadStatusRef.current = nextStatus;
    setBusy(statusMeansBusy(nextStatus ?? undefined));
    if (!statusMeansBusy(nextStatus ?? undefined)) {
      setThinking(false);
    }
    setModel(payload.thread?.model ?? DEFAULT_MODEL);

    if (
      nextStatus &&
      ["failed", "incomplete", "cancelled"].includes(nextStatus) &&
      previousStatus !== nextStatus
    ) {
      appendMessage("error", `Thread ended with status: ${nextStatus}`);
    }
  }, []);

  const logThreadStreamMessage = useCallback((payload: ThreadStreamMessage) => {
    switch (payload.type) {
      case "thread.events.delta":
        for (const event of payload.events ?? []) {
          const type = typeof event.type === "string" ? event.type : "unknown";

          if (type === "response.output_item.added" || type === "response.output_item.done") {
            console.log(`[ws debug] ${type}`, event);
          }
        }
        break;
      default:
        break;
    }
  }, []);

  const handleThreadStreamMessage = useCallback((payload: ThreadStreamMessage) => {
    if (!activeThreadIdRef.current || payload.thread_id !== activeThreadIdRef.current) {
      return;
    }

    switch (payload.type) {
      case "thread.snapshot":
        handleThreadSnapshot({ thread: payload.thread });
        break;
      case "thread.items.delta": {
        const cursor =
          payload.page?.last_cursor ?? payload.page?.last_stream_id ?? null;
        if (cursor) {
          setLastItemCursor(cursor);
        }
        const nextThinking = deriveThinkingStateFromItems(payload.items);
        if (nextThinking !== null) {
          setThinking(nextThinking);
        }
        const nextMessages = buildMessagesFromItems({
          items: payload.items,
          page: payload.page,
        });
        if (nextMessages.length > 0) {
          setMessages((current) => mergeMessages(current, nextMessages));
        }
        break;
      }
      case "thread.events.delta": {
        const nextThinking = deriveThinkingState(payload.events);
        if (nextThinking !== null) {
          setThinking(nextThinking);
        }
        break;
      }
      case "thread.heartbeat":
        break;
      default:
        break;
    }
  }, [handleThreadSnapshot]);

  const disconnectThreadStream = useCallback(() => {
    if (reconnectTimerRef.current !== null) {
      window.clearTimeout(reconnectTimerRef.current);
      reconnectTimerRef.current = null;
    }
    reconnectDelayRef.current = 1000;

    if (!threadStreamRef.current) {
      return;
    }

    const socket = threadStreamRef.current;
    threadStreamRef.current = null;
    closingThreadStreamRef.current = socket;
    socket.close();
  }, []);

  const connectThreadStream = useCallback((nextThreadId: string, afterCursor: string | null) => {
    disconnectThreadStream();

    const query = new URLSearchParams();
    if (afterCursor) {
      query.set("after_item", afterCursor);
    }

    const path = `/threads/${nextThreadId}/connect${query.toString() ? `?${query.toString()}` : ""}`;
    const socket = new WebSocket(buildStreamWebSocketUrl(path));
    threadStreamRef.current = socket;

    socket.onopen = () => {
      reconnectDelayRef.current = 1000;
      setApiStatus("online");
    };

    socket.onmessage = (event) => {
      try {
        const payload = JSON.parse(event.data as string) as ThreadStreamMessage;
        logThreadStreamMessage(payload);
        handleThreadStreamMessage(payload);
      } catch {
        /* swallow invalid websocket payloads */
      }
    };

    socket.onerror = () => {
      setApiStatus("degraded");
    };

    socket.onclose = () => {
      if (closingThreadStreamRef.current === socket) {
        closingThreadStreamRef.current = null;
        return;
      }

      if (threadStreamRef.current === socket) {
        threadStreamRef.current = null;
      }

      if (activeThreadIdRef.current !== nextThreadId) {
        return;
      }

      const retryDelay = reconnectDelayRef.current;
      reconnectDelayRef.current = Math.min(retryDelay * 2, 10000);

      reconnectTimerRef.current = window.setTimeout(() => {
        reconnectTimerRef.current = null;
        if (activeThreadIdRef.current === nextThreadId) {
          connectThreadStream(nextThreadId, lastItemCursorRef.current);
        }
      }, retryDelay);
    };
  }, [disconnectThreadStream, handleThreadStreamMessage]);

  async function loadThread(nextThreadId: string) {
    setBusy(true);
    setThinking(false);
    resetDebugStreamLogging();
    activeThreadIdRef.current = nextThreadId;

    if (threadId && threadId !== nextThreadId) {
      disconnectSocket(threadId);
    }
    disconnectThreadStream();

    try {
      const [threadInfo, payload] = await Promise.all([
        apiGet<ThreadResponse>(`/threads/${nextThreadId}`),
        apiGet<ThreadItemsResponse>(
          `/threads/${nextThreadId}/items?limit=200`,
        ),
      ]);
      const cursor = payload.page?.last_cursor ?? payload.page?.last_stream_id ?? null;
      setThreadId(nextThreadId);
      setModel(threadInfo.thread?.model ?? DEFAULT_MODEL);
      setLastItemCursor(cursor);
      lastThreadStatusRef.current = threadInfo.thread?.status ?? null;
      setMessages(buildMessagesFromItems(payload));
      setPendingImages([]);
      setDraft("");
      setBusy(statusMeansBusy(threadInfo.thread?.status));
      connectThreadStream(nextThreadId, cursor);
    } catch (error) {
      setMessages([]);
      appendMessage(
        "error",
        error instanceof Error ? error.message : "Failed to load thread",
      );
      setBusy(false);
      setThinking(false);
    }
  }

  async function addPendingFiles(files: File[]) {
    for (const file of files) {
      setUploadCount((count) => count + 1);

      try {
        const uploaded = await uploadImage(file);
        const previewUrl = URL.createObjectURL(file);
        previewUrlsRef.current.add(previewUrl);

        setPendingImages((current) => [
          ...current,
          { ...uploaded, preview_url: previewUrl },
        ]);
      } catch (error) {
        appendMessage(
          "error",
          error instanceof Error ? error.message : "Image upload failed",
        );
      } finally {
        setUploadCount((count) => count - 1);
      }
    }
  }

  async function sendMessage(text: string, images: UploadedImage[]) {
    if (!text && images.length === 0) return;

    setBusy(true);
    setThinking(false);
    resetDebugStreamLogging();
    const optimisticMessageId = appendOptimisticUserMessage(text, images);

    try {
      const inputItems = buildUserInputItems(text, images);
      const terminalStatuses = ["failed", "incomplete", "cancelled"];
      let currentThreadId = threadId;
      if (currentThreadId && terminalStatuses.includes(lastThreadStatusRef.current ?? "")) {
        currentThreadId = null;
      }

      if (!currentThreadId) {
        const created = await apiPost<ThreadCreateResponse>("/threads", {
          model,
          instructions: DEFAULT_INSTRUCTIONS,
          input: inputItems,
          tools: EXPLORER_TOOLS,
          reasoning: { effort: reasoningEffort, summary: "concise" },
          store: true,
        });

        currentThreadId = created.thread_id ?? null;
        if (!currentThreadId) throw new Error("No thread_id returned");

        activeThreadIdRef.current = currentThreadId;
        setThreadId(currentThreadId);
        setLastItemCursor(null);
        connectThreadStream(currentThreadId, null);
      } else {
        if (!threadStreamRef.current) {
          connectThreadStream(currentThreadId, lastItemCursorRef.current);
        }
        await apiPost(`/threads/${currentThreadId}/commands`, {
          kind: "thread.resume",
          body: { input_items: inputItems },
        });
      }
    } catch (error) {
      removeMessage(optimisticMessageId);
      setBusy(false);
      appendMessage(
        "error",
        error instanceof Error ? error.message : "Request failed",
      );
    } finally {
      void checkHealth();
    }
  }

  function resetConversation() {
    activeThreadIdRef.current = null;
    disconnectThreadStream();
    if (threadId) {
      disconnectSocket(threadId);
    }
    setMessages([]);
    setDraft("");
    setThreadId(null);
    setLastItemCursor(null);
    setPendingImages([]);
    setModel(DEFAULT_MODEL);
    setReasoningEffort("medium");
    setThinking(false);
    resetDebugStreamLogging();
    lastThreadStatusRef.current = null;
  }

  const submitDisabled =
    busy || uploadCount > 0 || (!draft.trim() && pendingImages.length === 0);

  return {
    messages,
    draft,
    setDraft,
    busy,
    thinking,
    uploadCount,
    threadId,
    pendingImages,
    setPendingImages,
    apiStatus,
    model,
    setModel,
    reasoningEffort,
    setReasoningEffort,
    sendMessage,
    loadThread,
    addPendingFiles,
    resetConversation,
    submitDisabled,
  };
}
