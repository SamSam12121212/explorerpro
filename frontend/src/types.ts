export type MessageRole = "user" | "assistant" | "status" | "error";
export type HealthState = "checking" | "online" | "degraded" | "offline";
/** Matches OpenAI Responses API / openai-go `shared.ReasoningEffort`. */
export type ReasoningEffort =
  | "none"
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
    id?: number;
    status?: string;
    model?: string;
  };
  attached_documents?: AttachedDocument[];
}

export interface ThreadCreateResponse {
  thread_id?: number;
}

export interface ThreadListResponse {
  threads?: {
    id?: number;
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
  id: number;
  filename: string;
  source_ref: string;
  status: string;
  error?: string;
  manifest_ref?: string;
  page_count: number;
  dpi: number;
  query_model: string;
  query_reasoning: string;
  base_response_id?: string;
  base_model?: string;
  base_reasoning?: string;
  base_initialized_at?: string;
  created_at: string;
  updated_at: string;
}

export interface AttachedDocument {
  id: number;
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

export interface DocumentResponse {
  document: DocumentEntry;
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
  };
}

export interface ThreadStreamHeartbeatMessage {
  type: "thread.heartbeat";
  time?: string;
}

export interface ThreadStreamPayload {
  type: string;
  thread_id: number;
  root_thread_id?: number;
  parent_thread_id?: number;
  [key: string]: unknown;
}

export type ThreadStreamMessage = ThreadStreamHeartbeatMessage;
