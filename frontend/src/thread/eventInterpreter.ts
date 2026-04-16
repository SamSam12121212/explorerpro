import type {
  ChatMessage,
  MessageRole,
  ThreadItemsResponse,
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

export function buildMessagesFromItems(itemsResponse: ThreadItemsResponse): ChatMessage[] {
  const messages: ChatMessage[] = [];

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

function sameMessageContent(left: ChatMessage, right: ChatMessage): boolean {
  return left.role === right.role && left.text === right.text && sameImageRefs(left.images, right.images);
}

export function mergeMessages(current: ChatMessage[], incoming: ChatMessage[]): ChatMessage[] {
  if (incoming.length === 0) return current;

  const merged = [...current];
  const seen = new Set(
    current.filter((m) => !m.optimistic).map((m) => m.id),
  );

  for (const message of incoming) {
    if (seen.has(message.id)) continue;
    seen.add(message.id);

    if (message.role === "user") {
      const optimisticIndex = merged.findIndex(
        (candidate) => candidate.optimistic && candidate.role === "user" && sameMessageContent(candidate, message),
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

export function buildMessageFromOutputItemDone(event: Record<string, unknown>): ChatMessage | null {
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
