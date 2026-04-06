export type MessageRole = "user" | "assistant" | "status" | "error";
export type HealthState = "checking" | "online" | "degraded" | "offline";
export type ReasoningEffort = "low" | "medium" | "high";

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
