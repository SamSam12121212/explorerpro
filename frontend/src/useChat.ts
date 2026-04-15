import { useEffect, useRef, useState, useSyncExternalStore } from "react";
import { uploadImage } from "./api";
import { chatStore } from "./chatStore";
import type {
  AttachedDocument,
  ReasoningEffort,
  UploadedImage,
} from "./types";

export function useChat() {
  const chatState = useSyncExternalStore(
    chatStore.subscribe,
    chatStore.getSnapshot,
    chatStore.getSnapshot,
  );

  const [draft, setDraft] = useState("");
  const [uploadCount, setUploadCount] = useState(0);
  const [pendingDocuments, setPendingDocuments] = useState<AttachedDocument[]>([]);
  const [pendingImages, setPendingImages] = useState<UploadedImage[]>([]);
  const [reasoningEffort, setReasoningEffort] =
    useState<ReasoningEffort>("medium");
  const previewUrlsRef = useRef(new Set<string>());

  function setModel(value: string) {
    chatStore.setModel(value);
  }

  useEffect(() => {
    const urls = previewUrlsRef.current;
    return () => {
      for (const url of urls) {
        URL.revokeObjectURL(url);
      }
    };
  }, []);

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
        chatStore.reportError(
          error instanceof Error ? error.message : "Image upload failed",
        );
      } finally {
        setUploadCount((count) => count - 1);
      }
    }
  }

  async function loadThread(nextThreadId: number) {
    const loaded = await chatStore.loadThread(nextThreadId);
    if (!loaded) {
      return;
    }

    setPendingDocuments([]);
    setPendingImages([]);
    setDraft("");
  }

  async function sendMessage(text: string, images: UploadedImage[], documents: AttachedDocument[]) {
    const sent = await chatStore.sendMessage({
      text,
      images,
      documents,
      reasoningEffort,
    });

    if (sent) {
      setPendingDocuments([]);
    }
  }

  function resetConversation() {
    chatStore.resetConversation();
    setDraft("");
    setPendingDocuments([]);
    setPendingImages([]);
    setReasoningEffort("medium");
  }

  const submitDisabled =
    chatState.busy || uploadCount > 0 || (!draft.trim() && pendingImages.length === 0);

  return {
    messages: chatState.messages,
    draft,
    setDraft,
    busy: chatState.busy,
    thinking: chatState.thinking,
    uploadCount,
    threadId: chatState.threadId,
    attachedDocuments: chatState.attachedDocuments,
    pendingDocuments,
    setPendingDocuments,
    pendingImages,
    setPendingImages,
    apiStatus: chatState.apiStatus,
    model: chatState.model,
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
