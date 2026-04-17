import { useCallback, useEffect, useRef, useState } from "react";
import { LuEllipsis, LuFolder } from "react-icons/lu";
import { useNavigate, useParams } from "react-router";
import { apiGet, apiPost } from "../../api";
import { COLLECTIONS_CHANGED_EVENT } from "../../constants";
import type {
  AttachedCollection,
  CollectionCreateResponse,
  CollectionDetailResponse,
  CollectionEntry,
  CollectionListResponse,
  DocumentEntry,
} from "../../types";

function documentCountLabel(count: number) {
  return `${count.toString()} doc${count === 1 ? "" : "s"}`;
}

function toAttachedCollection(
  collection: CollectionEntry,
  documents: DocumentEntry[],
): AttachedCollection {
  return {
    id: collection.id,
    name: collection.name,
    documents: documents.map((d) => ({
      id: d.id,
      filename: d.filename,
      page_count: d.page_count,
      status: d.status,
    })),
  };
}

interface CollectionsViewProps {
  attachedCollectionIds: string[];
  onAttachCollection: (collection: AttachedCollection) => void;
}

export function CollectionsView({
  attachedCollectionIds,
  onAttachCollection,
}: CollectionsViewProps) {
  const { collectionId } = useParams<"collectionId">();
  const navigate = useNavigate();
  const [collections, setCollections] = useState<CollectionEntry[]>([]);
  const [draftName, setDraftName] = useState("");
  const [showComposer, setShowComposer] = useState(false);
  const [creating, setCreating] = useState(false);
  const [error, setError] = useState("");
  const [openMenuCollectionId, setOpenMenuCollectionId] = useState<string | null>(null);
  const [attachingCollectionId, setAttachingCollectionId] = useState<string | null>(null);
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

  useEffect(() => {
    if (!openMenuCollectionId) {
      return undefined;
    }

    const handlePointerDown = (event: MouseEvent) => {
      const target = event.target;
      if (!(target instanceof Element)) {
        setOpenMenuCollectionId(null);
        return;
      }

      if (target.closest("[data-collection-menu]")) {
        return;
      }

      setOpenMenuCollectionId(null);
    };

    window.addEventListener("mousedown", handlePointerDown);
    return () => {
      window.removeEventListener("mousedown", handlePointerDown);
    };
  }, [openMenuCollectionId]);

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

  const handleAttach = async (collection: CollectionEntry) => {
    if (attachingCollectionId) return;
    setAttachingCollectionId(collection.id);
    setError("");
    try {
      const data = await apiGet<CollectionDetailResponse>(
        `/api/collections/${encodeURIComponent(collection.id)}`,
      );
      onAttachCollection(toAttachedCollection(data.collection, data.documents));
      setOpenMenuCollectionId(null);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to attach collection");
    } finally {
      setAttachingCollectionId(null);
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
            const isAttached = attachedCollectionIds.includes(collection.id);
            const isAttaching = attachingCollectionId === collection.id;
            return (
              <div
                className={`group relative border-b border-[#2a2a2a] transition cursor-pointer ${
                  isActive
                    ? "bg-[#2a2a2a] shadow-[inset_2px_0_0_#007acc]"
                    : "hover:bg-[#252525]"
                }`}
                key={collection.id}
                onClick={() => {
                  void navigate(`/collections/${collection.id}`);
                }}
              >
                <div className="flex items-center justify-between gap-2 px-3 py-3 pr-10">
                  <span
                    className={`truncate text-sm font-medium ${
                      isActive ? "text-white" : "text-[#d4d4d4]"
                    }`}
                  >
                    {collection.name}
                  </span>
                  <span className="shrink-0 text-[0.68rem] uppercase tracking-wide text-[#666]">
                    {documentCountLabel(collection.document_count)}
                  </span>
                </div>

                <div
                  className="absolute top-2 right-2"
                  data-collection-menu={collection.id}
                  onClick={(event) => {
                    event.stopPropagation();
                  }}
                >
                  <button
                    className={`flex h-7 w-7 items-center justify-center border border-[#333] bg-[#252525] text-[#888] transition hover:border-[#444] hover:text-[#d4d4d4] ${
                      openMenuCollectionId === collection.id
                        ? "opacity-100"
                        : "opacity-0 group-hover:opacity-100 focus:opacity-100"
                    }`}
                    onClick={() => {
                      setOpenMenuCollectionId((current) =>
                        current === collection.id ? null : collection.id,
                      );
                    }}
                    title="Collection actions"
                    type="button"
                  >
                    <LuEllipsis className="h-4 w-4" />
                  </button>

                  {openMenuCollectionId === collection.id && (
                    <div className="absolute top-8 right-0 z-10 min-w-40 border border-[#333] bg-[#202020] p-1 shadow-[0_8px_24px_rgba(0,0,0,0.35)]">
                      <button
                        className={`flex w-full items-center gap-2 px-2 py-1.5 text-left text-xs transition ${
                          isAttached
                            ? "cursor-default text-[#666]"
                            : "text-[#d4d4d4] hover:bg-[#2a2a2a]"
                        }`}
                        disabled={isAttached || isAttaching}
                        onClick={() => {
                          void handleAttach(collection);
                        }}
                        type="button"
                      >
                        <LuFolder className="h-3.5 w-3.5" />
                        <span>
                          {isAttached
                            ? "Attached"
                            : isAttaching
                              ? "Attaching…"
                              : "Attach to thread"}
                        </span>
                      </button>
                    </div>
                  )}
                </div>
              </div>
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
