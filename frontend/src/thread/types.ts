import type {
  AttachedCollection,
  AttachedDocument,
  ThreadMessage,
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
  messages: ThreadMessage[];
  phase: ThreadPhase;
  thinking: boolean;
  model: string;
  attachedDocuments: AttachedDocument[];
  pendingDocuments: AttachedDocument[];
  attachedCollections: AttachedCollection[];
  pendingCollections: AttachedCollection[];
  pendingImages: UploadedImage[];
  draft: string;
  reasoningEffort: ReasoningEffort;
  apiStatus: HealthState;
  threads: ThreadEntry[];
  threadsLoading: boolean;
  uploadCount: number;
  // call_ids of `query_document` function calls observed in the current turn.
  // Populated as `response.output_item.added` events arrive; cleared when the
  // worker resumes (next `response.created`) or on terminal failure.
  pendingDocumentQueries: string[];
  // call_ids of in-flight `read_document_page` tool calls. Same lifecycle as
  // pendingDocumentQueries.
  pendingPageReads: string[];
}

export interface ThreadActions {
  setDraft: (value: string) => void;
  setModel: (value: string) => void;
  setReasoningEffort: (value: ReasoningEffort) => void;
  setPendingDocuments: (updater: AttachedDocument[] | ((current: AttachedDocument[]) => AttachedDocument[])) => void;
  setPendingCollections: (updater: AttachedCollection[] | ((current: AttachedCollection[]) => AttachedCollection[])) => void;
  setPendingImages: (updater: UploadedImage[] | ((current: UploadedImage[]) => UploadedImage[])) => void;
  sendMessage: (
    text: string,
    images: UploadedImage[],
    documents: AttachedDocument[],
    collections: AttachedCollection[],
  ) => Promise<void>;
  loadThread: (threadId: number) => Promise<void>;
  archiveThread: (threadId: number) => Promise<void>;
  deleteThread: (threadId: number) => Promise<void>;
  resetConversation: () => void;
  addPendingFiles: (files: File[]) => Promise<void>;
  attachDocument: (document: AttachedDocument) => void;
  attachCollection: (collection: AttachedCollection) => void;
  refreshThreadList: () => void;
}

export type ThreadStore = ThreadState & ThreadActions & {
  busy: boolean;
  submitDisabled: boolean;
};
