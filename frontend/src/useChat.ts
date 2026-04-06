import { useCallback, useEffect, useEffectEvent, useRef, useState } from "react";
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

function mergeMessages(current: ChatMessage[], incoming: ChatMessage[]) {
  if (incoming.length === 0) {
    return current;
  }

  const merged = [...current];
  const seen = new Set(current.map((message) => message.id));

  for (const message of incoming) {
    if (seen.has(message.id)) {
      continue;
    }
    seen.add(message.id);
    merged.push(message);
  }

  return merged;
}

function statusMeansBusy(status?: string) {
  return !["ready", "completed", "failed", "incomplete", "cancelled"].includes(status ?? "");
}

export function useChat() {
  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [draft, setDraft] = useState("");
  const [busy, setBusy] = useState(false);
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

  function disconnectSocket(targetThreadId: string) {
    void apiPost(`/threads/${targetThreadId}/commands`, {
      kind: "thread.disconnect_socket",
    }).catch(() => undefined);
  }

  const handleThreadSnapshot = useEffectEvent((payload: ThreadResponse) => {
    const nextStatus = payload.thread?.status ?? null;
    const previousStatus = lastThreadStatusRef.current;
    lastThreadStatusRef.current = nextStatus;
    setBusy(statusMeansBusy(nextStatus ?? undefined));
    setModel(payload.thread?.model ?? DEFAULT_MODEL);

    if (
      nextStatus &&
      ["failed", "incomplete", "cancelled"].includes(nextStatus) &&
      previousStatus !== nextStatus
    ) {
      appendMessage("error", `Thread ended with status: ${nextStatus}`);
    }
  });

  const handleThreadStreamMessage = useEffectEvent((payload: ThreadStreamMessage) => {
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
        const nextMessages = buildMessagesFromItems({
          items: payload.items,
          page: payload.page,
        });
        if (nextMessages.length > 0) {
          setMessages((current) => mergeMessages(current, nextMessages));
        }
        break;
      }
      case "thread.events.delta":
      case "thread.heartbeat":
        break;
      default:
        break;
    }
  });

  const disconnectThreadStream = useEffectEvent(() => {
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
  });

  const connectThreadStream = useEffectEvent((nextThreadId: string, afterCursor: string | null) => {
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
        const payload = JSON.parse(event.data) as ThreadStreamMessage;
        console.log("[ws event]", payload.type, payload);
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
  });

  async function loadThread(nextThreadId: string) {
    setBusy(true);
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

    try {
      const inputItems = buildUserInputItems(text, images);
      let currentThreadId = threadId;

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
        setMessages([]);
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
    lastThreadStatusRef.current = null;
  }

  const submitDisabled =
    busy || uploadCount > 0 || (!draft.trim() && pendingImages.length === 0);

  return {
    messages,
    draft,
    setDraft,
    busy,
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
