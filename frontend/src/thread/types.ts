import type {
  AttachedDocument,
  ChatMessage,
  HealthState,
  ReasoningEffort,
  UploadedImage,
} from "../types";

export interface ThreadEntry {
  id: number;
  label: string;
  previewText: string;
  updatedAt: string;
}

export type ThreadPhase = "idle" | "loading" | "streaming" | "error";

export interface ThreadState {
  threadId: number | null;
  messages: ChatMessage[];
  phase: ThreadPhase;
  thinking: boolean;
  model: string;
  attachedDocuments: AttachedDocument[];
  pendingDocuments: AttachedDocument[];
  pendingImages: UploadedImage[];
  draft: string;
  reasoningEffort: ReasoningEffort;
  apiStatus: HealthState;
  threads: ThreadEntry[];
  threadsLoading: boolean;
  uploadCount: number;
}

export interface ThreadActions {
  setDraft: (value: string) => void;
  setModel: (value: string) => void;
  setReasoningEffort: (value: ReasoningEffort) => void;
  setPendingDocuments: (updater: AttachedDocument[] | ((current: AttachedDocument[]) => AttachedDocument[])) => void;
  setPendingImages: (updater: UploadedImage[] | ((current: UploadedImage[]) => UploadedImage[])) => void;
  sendMessage: (text: string, images: UploadedImage[], documents: AttachedDocument[]) => Promise<void>;
  loadThread: (threadId: number) => Promise<void>;
  archiveThread: (threadId: number) => Promise<void>;
  deleteThread: (threadId: number) => Promise<void>;
  resetConversation: () => void;
  addPendingFiles: (files: File[]) => Promise<void>;
  attachDocument: (document: AttachedDocument) => void;
  refreshThreadList: () => void;
}

export type ThreadStore = ThreadState & ThreadActions & {
  busy: boolean;
  submitDisabled: boolean;
};
