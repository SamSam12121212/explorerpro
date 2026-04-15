import { use, useMemo, useSyncExternalStore } from "react";
import { ThreadContext } from "./ThreadContext";
import type { ThreadStore } from "./types";

export function useThread(): ThreadStore {
  const service = use(ThreadContext);
  if (!service) throw new Error("useThread must be used within a ThreadProvider");

  const state = useSyncExternalStore(service.subscribe, service.getSnapshot, service.getSnapshot);

  return useMemo(() => {
    const busy = state.phase === "loading" || state.phase === "streaming";
    const submitDisabled = busy || state.uploadCount > 0 || (!state.draft.trim() && state.pendingImages.length === 0);

    return {
      ...state,
      busy,
      submitDisabled,
      setDraft: service.setDraft,
      setModel: service.setModel,
      setReasoningEffort: service.setReasoningEffort,
      setPendingDocuments: service.setPendingDocuments,
      setPendingImages: service.setPendingImages,
      sendMessage: service.sendMessage,
      loadThread: service.loadThread,
      resetConversation: service.resetConversation,
      addPendingFiles: service.addPendingFiles,
      attachDocument: service.attachDocument,
      refreshThreadList: service.refreshThreadList,
    };
  }, [state, service]);
}
