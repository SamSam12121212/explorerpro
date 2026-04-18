import { QUERY_DOCUMENT_TOOL_NAME, READ_DOCUMENT_PAGE_TOOL_NAME } from "../constants";
import type {
  MessageRole,
  ThreadItemsResponse,
  ThreadMessage,
  ThreadStreamPayload,
  UploadedImage,
} from "../types";

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

export function deriveThinkingFromEvent(event: Record<string, unknown>): boolean | null {
  const type = event.type;
  if (typeof type !== "string") return null;

  if (reasoningEventTypes.has(type)) return true;

  if ((type === "response.output_item.added" || type === "response.output_item.done") && isReasoningItemEvent(event)) {
    return true;
  }

  if (stopThinkingEventTypes.has(type)) return false;

  return null;
}

export function deriveThinkingFromItems(items?: ThreadItemsResponse["items"]): boolean | null {
  let result: boolean | null = null;

  for (const item of items ?? []) {
    if (item.item_type === "reasoning") {
      result = true;
      continue;
    }

    if (item.direction === "output" && item.item_type && item.item_type !== "reasoning") {
      result = false;
    }
  }

  return result;
}

function blobRefToUrl(ref: string): string {
  return "/" + ref.replace("blob://", "");
}

function extractMessageText(item: NonNullable<ThreadItemsResponse["items"]>[number]): string {
  return (item.payload?.content ?? [])
    .filter((c) => c.type === "input_text" || c.type === "output_text")
    .map((c) => c.text?.trim() ?? "")
    .filter(Boolean)
    .join("\n\n");
}

function extractMessageImages(item: NonNullable<ThreadItemsResponse["items"]>[number]): UploadedImage[] {
  return (item.payload?.content ?? []).flatMap((content) => {
    if (content.type !== "image_ref" || !content.image_ref) return [];
    return [{
      image_id: content.image_ref.split("/").at(-2) ?? crypto.randomUUID(),
      image_ref: content.image_ref,
      content_type: content.content_type,
      filename: content.filename,
      preview_url: blobRefToUrl(content.image_ref),
    }];
  });
}

export function buildMessagesFromItems(itemsResponse: ThreadItemsResponse): ThreadMessage[] {
  const messages: ThreadMessage[] = [];

  for (const item of itemsResponse.items ?? []) {
    if (item.item_type !== "message") continue;

    const text = extractMessageText(item);
    const images = extractMessageImages(item);
    if (!text && images.length === 0) continue;

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

function sameImageRefs(left?: UploadedImage[], right?: UploadedImage[]): boolean {
  if ((left?.length ?? 0) !== (right?.length ?? 0)) return false;
  return (left ?? []).every((image, index) => image.image_ref === right?.[index]?.image_ref);
}

function sameMessageContent(left: ThreadMessage, right: ThreadMessage): boolean {
  return left.role === right.role && left.text === right.text && sameImageRefs(left.images, right.images);
}

/**
 * Insert or replace a single message by id.
 *
 * Used for live stream updates (streaming stub → deltas → final `.done` message).
 * The `.done` replacement is what realizes the "completion is truth" contract:
 * it overwrites whatever text the deltas had accumulated with the server's
 * authoritative `output_item.done` payload, and clears the `streaming` flag.
 *
 * Also preserves the optimistic-user-merge behavior from `mergeMessages`: an
 * incoming user message matching an optimistic stub by content replaces the
 * stub in place (server-assigned id wins).
 */
export function upsertMessage(current: ThreadMessage[], incoming: ThreadMessage): ThreadMessage[] {
  const existingIndex = current.findIndex((m) => m.id === incoming.id);
  if (existingIndex >= 0) {
    const next = [...current];
    next[existingIndex] = incoming;
    return next;
  }

  if (incoming.role === "user") {
    const optimisticIndex = current.findIndex(
      (candidate) => candidate.optimistic && candidate.role === "user" && sameMessageContent(candidate, incoming),
    );
    if (optimisticIndex >= 0) {
      const next = [...current];
      next[optimisticIndex] = incoming;
      return next;
    }
  }

  return [...current, incoming];
}

/** Clear `streaming` flags from any messages that still have one set. */
export function finalizeStreamingMessages(current: ThreadMessage[]): ThreadMessage[] {
  if (!current.some((m) => m.streaming)) return current;
  return current.map((m) => (m.streaming ? { ...m, streaming: undefined } : m));
}

export function statusMeansBusy(status?: string): boolean {
  return !["ready", "completed", "failed", "incomplete", "cancelled"].includes(status ?? "");
}

type OutputItemContent = {
  type?: string;
  text?: string;
  image_ref?: string;
  content_type?: string;
  filename?: string;
};

type OutputItem = {
  id?: string;
  type?: string;
  role?: string;
  content?: OutputItemContent[];
};

/**
 * Build an empty streaming assistant message from `response.output_item.added`.
 * Returns null for non-message items (reasoning, function_call, etc).
 */
export function buildStreamingMessageFromOutputItemAdded(event: Record<string, unknown>): ThreadMessage | null {
  const item = event.item as OutputItem | undefined;
  if (item?.type !== "message") return null;
  if (!item.id) return null;

  return {
    id: item.id,
    role: "assistant",
    text: "",
    streaming: true,
  };
}

/**
 * Apply a `response.output_item.added` event.
 *
 * Seeds an empty streaming stub for new assistant message items. If a message
 * with the same id already exists — typically because a text delta beat the
 * `.added` event to the client — the existing message is preserved. The
 * `.added` event carries no text, so upserting would overwrite accumulated
 * deltas with an empty string.
 *
 * Returns the input array unchanged for non-message items, missing ids, or
 * the delta-first race case.
 */
export function applyOutputItemAdded(current: ThreadMessage[], event: Record<string, unknown>): ThreadMessage[] {
  const stub = buildStreamingMessageFromOutputItemAdded(event);
  if (!stub) return current;
  if (current.some((m) => m.id === stub.id)) return current;
  return upsertMessage(current, stub);
}

function extractItemId(event: Record<string, unknown>): string | null {
  const itemId = event.item_id;
  if (typeof itemId === "string" && itemId.length > 0) return itemId;
  const item = event.item as OutputItem | undefined;
  if (item?.id) return item.id;
  return null;
}

/**
 * Apply a `response.output_text.delta` event: append `event.delta` to the
 * streaming message whose id matches `event.item_id`. If no such message
 * exists yet (delta-before-added race), create one on demand.
 *
 * Returns the input array unchanged if the event carries no text delta or
 * identifiable item id.
 */
export function applyOutputTextDelta(current: ThreadMessage[], event: Record<string, unknown>): ThreadMessage[] {
  const delta = typeof event.delta === "string" ? event.delta : "";
  if (!delta) return current;

  const itemId = extractItemId(event);
  if (!itemId) return current;

  const existingIndex = current.findIndex((m) => m.id === itemId);
  if (existingIndex >= 0) {
    const existing = current[existingIndex];
    const next = [...current];
    next[existingIndex] = {
      ...existing,
      text: existing.text + delta,
      streaming: true,
    };
    return next;
  }

  return [
    ...current,
    {
      id: itemId,
      role: "assistant",
      text: delta,
      streaming: true,
    },
  ];
}

interface FunctionCallItem {
  type?: string;
  id?: string;
  call_id?: string;
  name?: string;
}

/**
 * Apply a `response.output_item.added` event for a `query_document`
 * function_call item. Appends the call_id to the pending list so the UI can
 * surface a "Reading N documents..." indicator.
 *
 * Returns the input array unchanged for non-function_call items, calls of
 * other tools (e.g. spawn_threads), missing call_id, or duplicates — so the
 * caller can rely on referential equality to skip a no-op setState.
 */
export function applyDocumentQueryAdded(current: string[], event: Record<string, unknown>): string[] {
  const item = event.item as FunctionCallItem | undefined;
  if (item?.type !== "function_call") return current;
  if (item.name !== QUERY_DOCUMENT_TOOL_NAME) return current;

  const callID = item.call_id ?? item.id;
  if (!callID) return current;
  if (current.includes(callID)) return current;

  return [...current, callID];
}

/**
 * Tracks in-flight read_document_page tool calls for the pending indicator.
 * Same contract as applyDocumentQueryAdded — returns input unchanged when no
 * mutation is needed so callers can rely on referential equality.
 */
export function applyPageReadAdded(current: string[], event: Record<string, unknown>): string[] {
  const item = event.item as FunctionCallItem | undefined;
  if (item?.type !== "function_call") return current;
  if (item.name !== READ_DOCUMENT_PAGE_TOOL_NAME) return current;

  const callID = item.call_id ?? item.id;
  if (!callID) return current;
  if (current.includes(callID)) return current;

  return [...current, callID];
}

export function buildMessageFromOutputItemDone(event: Record<string, unknown>): ThreadMessage | null {
  const item = event.item as OutputItem | undefined;
  if (!item || item.type !== "message") return null;

  const text = (item.content ?? [])
    .filter((c) => c.type === "output_text")
    .map((c) => c.text?.trim() ?? "")
    .filter(Boolean)
    .join("\n\n");

  const images: UploadedImage[] = (item.content ?? []).flatMap((c) => {
    if (c.type !== "image_ref" || !c.image_ref) return [];
    return [{
      image_id: c.image_ref.split("/").at(-2) ?? crypto.randomUUID(),
      image_ref: c.image_ref,
      content_type: c.content_type,
      filename: c.filename,
      preview_url: "/" + c.image_ref.replace("blob://", ""),
    }];
  });

  if (!text && images.length === 0) return null;

  return {
    id: item.id ?? crypto.randomUUID(),
    role: "assistant",
    text,
    images: images.length > 0 ? images : undefined,
  };
}

export function logStreamPayload(payload: ThreadStreamPayload): void {
  console.log(`[ws] ${payload.type}`, payload);
}
