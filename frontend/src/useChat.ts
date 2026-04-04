import { useCallback, useEffect, useRef, useState } from "react";
import { apiGet, apiPost, checkHealthApi, uploadImage } from "./api";
import { DEFAULT_INSTRUCTIONS, DEFAULT_MODEL, EXPLORER_TOOLS } from "./constants";
import type {
  ChatMessage,
  HealthState,
  MessageRole,
  ReasoningEffort,
  ThreadCreateResponse,
  ThreadItemsResponse,
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

function extractAssistantText(itemsResponse: ThreadItemsResponse) {
  const items = itemsResponse.items ?? [];

  for (let index = items.length - 1; index >= 0; index -= 1) {
    const item = items[index];

    if (item.direction !== "output" || item.item_type !== "message") {
      continue;
    }

    const parts = item.payload?.content
      ?.filter((c) => c.type === "output_text" && c.text)
      .map((c) => c.text?.trim() ?? "")
      .filter(Boolean);

    if (parts && parts.length > 0) {
      return parts.join("\n\n");
    }
  }

  return null;
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
      for (const url of urls) {
        URL.revokeObjectURL(url);
      }
    };
  }, []);

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

  async function waitForReady(currentThreadId: string) {
    const terminalStatuses = new Set([
      "ready",
      "completed",
      "failed",
      "incomplete",
      "cancelled",
    ]);
    let status = "running";
    let attempts = 0;
    const maxAttempts = 120;

    while (!terminalStatuses.has(status)) {
      attempts += 1;
      if (attempts > maxAttempts) {
        throw new Error("Timed out waiting for response");
      }

      await new Promise((resolve) =>
        window.setTimeout(resolve, attempts < 5 ? 500 : 1500),
      );
      const payload = await apiGet<ThreadResponse>(
        `/threads/${currentThreadId}`,
      );
      status = payload.thread?.status ?? "unknown";
    }

    if (status !== "ready" && status !== "completed") {
      throw new Error(`Thread ended with status: ${status}`);
    }
  }

  async function fetchNewItems(currentThreadId: string) {
    const query = new URLSearchParams({ limit: "50" });
    if (lastItemCursor) {
      query.set("after", lastItemCursor);
    }

    const payload = await apiGet<ThreadItemsResponse>(
      `/threads/${currentThreadId}/items?${query.toString()}`,
    );
    const cursor =
      payload.page?.last_cursor ?? payload.page?.last_stream_id ?? null;

    if (cursor) {
      setLastItemCursor(cursor);
    }

    return payload;
  }

  function disconnectSocket(targetThreadId: string) {
    void apiPost(`/threads/${targetThreadId}/commands`, {
      kind: "thread.disconnect_socket",
    }).catch(() => undefined);
  }

  async function loadThread(nextThreadId: string) {
    setBusy(true);

    if (threadId && threadId !== nextThreadId) {
      disconnectSocket(threadId);
    }

    try {
      const [threadInfo, payload] = await Promise.all([
        apiGet<ThreadResponse>(`/threads/${nextThreadId}`),
        apiGet<ThreadItemsResponse>(
          `/threads/${nextThreadId}/items?limit=200`,
        ),
      ]);
      setThreadId(nextThreadId);
      setModel(threadInfo.thread?.model ?? DEFAULT_MODEL);
      setLastItemCursor(payload.page?.last_cursor ?? payload.page?.last_stream_id ?? null);
      setMessages(buildMessagesFromItems(payload));
      setPendingImages([]);
      setDraft("");
    } catch (error) {
      setMessages([]);
      appendMessage(
        "error",
        error instanceof Error ? error.message : "Failed to load thread",
      );
    } finally {
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
    appendMessage("user", text, images);

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

        setThreadId(currentThreadId);
        setLastItemCursor(null);
      } else {
        await apiPost(`/threads/${currentThreadId}/commands`, {
          kind: "thread.resume",
          body: { input_items: inputItems },
        });
      }

      await waitForReady(currentThreadId);
      const items = await fetchNewItems(currentThreadId);
      const reply = extractAssistantText(items);

      if (reply) {
        appendMessage("assistant", reply);
      } else {
        appendMessage("error", "No response text found.");
      }
    } catch (error) {
      appendMessage(
        "error",
        error instanceof Error ? error.message : "Request failed",
      );
    } finally {
      setBusy(false);
      void checkHealth();
    }
  }

  function resetConversation() {
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
