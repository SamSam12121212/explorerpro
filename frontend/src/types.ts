export type MessageRole = "user" | "assistant" | "status" | "error";
export type HealthState = "checking" | "online" | "degraded" | "offline";
export type ReasoningEffort = "low" | "medium" | "high";

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
    created_at?: string;
    updated_at?: string;
  }[];
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
      }[];
    };
  }[];
  page?: {
    first_cursor?: string;
    last_cursor?: string;
    last_stream_id?: string;
  };
}
