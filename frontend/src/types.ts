export type MessageRole = "user" | "assistant" | "status" | "error";
export type HealthState = "checking" | "online" | "degraded" | "offline";
/** Matches OpenAI Responses API / openai-go `shared.ReasoningEffort`. */
export type ReasoningEffort =
  | "none"
  | "minimal"
  | "low"
  | "medium"
  | "high"
  | "xhigh";

export interface ModelOption {
  value: string;
  label: string;
}

export interface UploadedImage {
  image_id: string;
  image_ref: string;
  content_type?: string;
  filename?: string;
  bytes?: number;
  preview_url: string;
}

export interface ChatMessage {
  id: string;
  role: MessageRole;
  text: string;
  images?: UploadedImage[];
  optimistic?: boolean;
}

export interface HealthResponse {
  status?: string;
}

export interface ThreadResponse {
  thread?: {
    id?: string;
    status?: string;
    model?: string;
  };
  attached_documents?: AttachedDocument[];
}

export interface ThreadCreateResponse {
  thread_id?: string;
}

export interface ThreadListResponse {
  threads?: {
    id?: string;
    label?: string;
    preview_text?: string;
    model?: string;
    created_at?: string;
    updated_at?: string;
  }[];
}

export interface RepoEntry {
  id: string;
  url: string;
  ref: string;
  name: string;
  status: string;
  error?: string;
  commit_sha?: string;
  created_at: string;
  updated_at: string;
}

export interface RepoListResponse {
  repos: RepoEntry[];
  count: number;
}

export interface RepoAddResponse {
  repo: RepoEntry;
  cmd_id: string;
}

export interface DocumentEntry {
  id: string;
  filename: string;
  source_ref: string;
  status: string;
  error?: string;
  manifest_ref?: string;
  page_count: number;
  dpi: number;
  created_at: string;
  updated_at: string;
}

export interface AttachedDocument {
  id: string;
  filename: string;
  page_count: number;
  status: string;
}

export interface DocumentListResponse {
  documents: DocumentEntry[];
  count: number;
}

export interface DocumentUploadResponse {
  document: DocumentEntry;
  cmd_id: string;
}

export interface CollectionEntry {
  id: string;
  name: string;
  document_count: number;
  created_at: string;
  updated_at: string;
}

export interface CollectionListResponse {
  collections: CollectionEntry[];
  count: number;
}

export interface CollectionCreateResponse {
  collection: CollectionEntry;
}

export interface CollectionDetailResponse {
  collection: CollectionEntry;
  documents: DocumentEntry[];
}

export interface ThreadItemsResponse {
  items?: {
    cursor?: string;
    direction?: string;
    item_type?: string;
    created_at?: string;
    payload?: {
      role?: string;
      content?: {
        type?: string;
        text?: string;
        image_ref?: string;
        content_type?: string;
        filename?: string;
      }[];
    };
  }[];
  page?: {
    first_cursor?: string;
    last_cursor?: string;
    last_stream_id?: string;
  };
}

export interface ThreadStreamSnapshotMessage {
  type: "thread.snapshot";
  thread_id: string;
  thread?: ThreadResponse["thread"];
  attached_documents?: AttachedDocument[];
}

export interface ThreadStreamItemsDeltaMessage {
  type: "thread.items.delta";
  thread_id: string;
  items?: ThreadItemsResponse["items"];
  page?: ThreadItemsResponse["page"];
}

export interface ThreadStreamEventsDeltaMessage {
  type: "thread.events.delta";
  thread_id: string;
  events?: Record<string, unknown>[];
}

export interface ThreadStreamHeartbeatMessage {
  type: "thread.heartbeat";
  thread_id: string;
  time?: string;
}

export type ThreadStreamMessage =
  | ThreadStreamSnapshotMessage
  | ThreadStreamItemsDeltaMessage
  | ThreadStreamEventsDeltaMessage
  | ThreadStreamHeartbeatMessage;
