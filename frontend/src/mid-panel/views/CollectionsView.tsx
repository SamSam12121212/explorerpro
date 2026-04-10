import { useCallback, useEffect, useRef, useState } from "react";
import { useNavigate, useParams } from "react-router";
import { apiGet, apiPost } from "../../api";
import { COLLECTIONS_CHANGED_EVENT } from "../../constants";
import type {
  CollectionCreateResponse,
  CollectionEntry,
  CollectionListResponse,
} from "../../types";

function documentCountLabel(count: number) {
  return `${count.toString()} doc${count === 1 ? "" : "s"}`;
}

export function CollectionsView() {
  const { collectionId } = useParams<"collectionId">();
  const navigate = useNavigate();
  const [collections, setCollections] = useState<CollectionEntry[]>([]);
  const [draftName, setDraftName] = useState("");
  const [showComposer, setShowComposer] = useState(false);
  const [creating, setCreating] = useState(false);
  const [error, setError] = useState("");
  const inputRef = useRef<HTMLInputElement>(null);

  const fetchCollections = useCallback(async () => {
    try {
      const data = await apiGet<CollectionListResponse>("/api/collections");
      setCollections(data.collections);
    } catch {
      /* swallow fetch errors */
    }
  }, []);

  useEffect(() => {
    void fetchCollections();
  }, [fetchCollections]);

  useEffect(() => {
    const handleCollectionsChanged = () => {
      void fetchCollections();
    };

    window.addEventListener(COLLECTIONS_CHANGED_EVENT, handleCollectionsChanged);
    return () => {
      window.removeEventListener(COLLECTIONS_CHANGED_EVENT, handleCollectionsChanged);
    };
  }, [fetchCollections]);

  useEffect(() => {
    if (showComposer) {
      inputRef.current?.focus();
    }
  }, [showComposer]);

  const handleCreate = async () => {
    const name = draftName.trim();
    if (!name || creating) return;

    setCreating(true);
    setError("");
    try {
      const data = await apiPost<CollectionCreateResponse>("/api/collections", { name });
      setDraftName("");
      setShowComposer(false);
      window.dispatchEvent(new Event(COLLECTIONS_CHANGED_EVENT));
      await fetchCollections();
      void navigate(`/collections/${data.collection.id}`);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to create collection");
    } finally {
      setCreating(false);
    }
  };

  return (
    <div className="flex h-full w-full min-w-0 flex-col bg-[#1e1e1e]">
      <div className="flex items-center justify-between border-b border-[#333] px-3 py-2">
        <span className="text-xs font-semibold uppercase tracking-widest text-[#888]">
          Collections
        </span>
        <button
          className="flex h-7 w-7 items-center justify-center border border-[#333] bg-[#2a2a2a] text-sm text-[#b2b2b2] transition hover:bg-[#333] hover:text-white disabled:cursor-not-allowed disabled:opacity-40"
          disabled={creating}
          onClick={() => {
            setError("");
            setShowComposer((current) => !current);
          }}
          title="New collection"
          type="button"
        >
          +
        </button>
      </div>

      {showComposer && (
        <div className="border-b border-[#2a2a2a] px-3 py-3">
          <form
            className="flex gap-2"
            onSubmit={(event) => {
              event.preventDefault();
              void handleCreate();
            }}
          >
            <input
              className="min-w-0 flex-1 border border-[#333] bg-[#252525] px-3 py-1.5 text-sm text-[#d4d4d4] outline-none placeholder:text-[#555] focus:border-[#007acc]"
              disabled={creating}
              maxLength={120}
              onChange={(event) => {
                setDraftName(event.target.value);
              }}
              placeholder="Collection name"
              ref={inputRef}
              type="text"
              value={draftName}
            />
            <button
              className="bg-[#007acc] px-3 py-1.5 text-xs font-semibold text-white transition hover:bg-[#1b8de4] disabled:cursor-not-allowed disabled:opacity-40"
              disabled={creating || !draftName.trim()}
              type="submit"
            >
              {creating ? "Creating…" : "Create"}
            </button>
          </form>
          {error && (
            <p className="mt-1.5 text-xs text-red-400">{error}</p>
          )}
        </div>
      )}

      <div className="min-h-0 flex-1 overflow-y-auto">
        {collections.length === 0 ? (
          <div className="px-3 py-6 text-center text-xs text-[#666]">
            No collections yet. Create one to start organizing documents.
          </div>
        ) : (
          collections.map((collection) => {
            const isActive = collection.id === collectionId;
            return (
              <button
                className={`flex w-full items-center justify-between gap-2 border-b border-[#2a2a2a] px-3 py-3 text-left transition ${
                  isActive
                    ? "bg-[#2a2a2a] text-white shadow-[inset_2px_0_0_#007acc]"
                    : "text-[#b2b2b2] hover:bg-[#252525]"
                }`}
                key={collection.id}
                onClick={() => {
                  void navigate(`/collections/${collection.id}`);
                }}
                type="button"
              >
                <span className={`truncate text-sm font-medium ${isActive ? "text-white" : "text-[#d4d4d4]"}`}>
                  {collection.name}
                </span>
                <span className="shrink-0 text-[0.68rem] uppercase tracking-wide text-[#666]">
                  {documentCountLabel(collection.document_count)}
                </span>
              </button>
            );
          })
        )}
      </div>

      {!showComposer && error && (
        <div className="border-t border-[#2a2a2a] px-3 py-2">
          <p className="text-xs text-red-400">{error}</p>
        </div>
      )}
    </div>
  );
}
