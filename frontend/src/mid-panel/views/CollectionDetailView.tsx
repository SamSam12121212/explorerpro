import { useCallback, useEffect, useState } from "react";
import { useNavigate } from "react-router";
import { apiGet, apiPost } from "../../api";
import { COLLECTIONS_CHANGED_EVENT } from "../../constants";
import type {
  CollectionDetailResponse,
  DocumentEntry,
  DocumentListResponse,
} from "../../types";

function documentTitle(document: DocumentEntry) {
  const filename = document.filename.trim();
  return filename || document.id.toString();
}

/** Title for collection list rows — never show raw document id. */
function documentListTitle(document: DocumentEntry) {
  const filename = document.filename.trim();
  return filename || "Untitled document";
}

function documentMeta(document: DocumentEntry) {
  const parts: string[] = [document.status];
  if (document.page_count > 0) {
    parts.push(`${document.page_count.toString()} pages`);
  }
  return parts.join(" · ");
}

interface CollectionDetailViewProps {
  collectionId: string;
}

export function CollectionDetailView({ collectionId }: CollectionDetailViewProps) {
  const navigate = useNavigate();
  const [detail, setDetail] = useState<CollectionDetailResponse | null>(null);
  const [documents, setDocuments] = useState<DocumentEntry[]>([]);
  const [selectedDocumentId, setSelectedDocumentId] = useState<number | null>(null);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");

  const fetchCollection = useCallback(async () => {
    const data = await apiGet<CollectionDetailResponse>(`/api/collections/${collectionId}`);
    setDetail(data);
  }, [collectionId]);

  const fetchDocuments = useCallback(async () => {
    const data = await apiGet<DocumentListResponse>("/documents");
    setDocuments(data.documents);
  }, []);

  const load = useCallback(async () => {
    setLoading(true);
    setError("");
    try {
      await Promise.all([fetchCollection(), fetchDocuments()]);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to load collection");
    } finally {
      setLoading(false);
    }
  }, [fetchCollection, fetchDocuments]);

  useEffect(() => {
    void load();
  }, [load]);

  const availableDocuments = documents.filter((document) => {
    return !detail?.documents.some((entry) => entry.id === document.id);
  });

  useEffect(() => {
    if (availableDocuments.length === 0) {
      if (selectedDocumentId !== null) {
        setSelectedDocumentId(null);
      }
      return;
    }

    const selectedStillAvailable = availableDocuments.some((document) => {
      return document.id === selectedDocumentId;
    });
    if (!selectedStillAvailable) {
      setSelectedDocumentId(availableDocuments[0]?.id ?? null);
    }
  }, [availableDocuments, selectedDocumentId]);

  const handleAddDocument = async () => {
    if (selectedDocumentId === null || saving) return;

    setSaving(true);
    setError("");
    try {
      const data = await apiPost<CollectionDetailResponse>(`/api/collections/${collectionId}/documents`, {
        document_id: selectedDocumentId,
      });
      setDetail(data);
      window.dispatchEvent(new Event(COLLECTIONS_CHANGED_EVENT));
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to add document");
    } finally {
      setSaving(false);
    }
  };

  if (loading) {
    return (
      <div className="flex h-full w-full items-center justify-center bg-[#1e1e1e] text-sm text-[#666]">
        Loading collection…
      </div>
    );
  }

  if (!detail) {
    return (
      <div className="flex h-full w-full items-center justify-center bg-[#1e1e1e] px-8 text-center text-sm text-red-400">
        {error || "Collection not found."}
      </div>
    );
  }

  return (
    <div className="flex h-full w-full min-w-0 flex-col bg-[#1e1e1e]">
      <div className="border-b border-[#333] px-5 py-4">
        <div className="flex items-end justify-between gap-4">
          <div className="min-w-0">
            <p className="text-[0.68rem] font-semibold uppercase tracking-[0.24em] text-[#666]">
              Collection
            </p>
            <h2 className="mt-1 truncate text-xl font-semibold text-[#f2f2f2]">
              {detail.collection.name}
            </h2>
          </div>
          <div className="shrink-0 text-right">
            <p className="text-[0.68rem] uppercase tracking-[0.2em] text-[#666]">
              Documents
            </p>
            <p className="mt-1 text-sm text-[#d4d4d4]">
              {detail.collection.document_count.toString()}
            </p>
          </div>
        </div>
      </div>

      <div className="border-b border-[#2a2a2a] px-5 py-4">
        <div className="flex items-end gap-3">
          <label className="min-w-0 flex-1">
            <span className="mb-2 block text-[0.68rem] font-semibold uppercase tracking-[0.2em] text-[#666]">
              Add Existing Document
            </span>
            <select
              className="w-full border border-[#333] bg-[#252525] px-3 py-2 text-sm text-[#d4d4d4] outline-none focus:border-[#007acc] disabled:cursor-not-allowed disabled:opacity-50"
              disabled={saving || availableDocuments.length === 0}
              onChange={(event) => {
                const value = Number(event.target.value);
                setSelectedDocumentId(Number.isFinite(value) ? value : null);
              }}
              value={selectedDocumentId?.toString() ?? ""}
            >
              {availableDocuments.length === 0 ? (
                <option value="">No available documents</option>
              ) : (
                availableDocuments.map((document) => (
                  <option key={document.id} value={document.id.toString()}>
                    {documentTitle(document)}
                  </option>
                ))
              )}
            </select>
          </label>
          <button
            className="bg-[#007acc] px-4 py-2 text-xs font-semibold uppercase tracking-wide text-white transition hover:bg-[#1b8de4] disabled:cursor-not-allowed disabled:opacity-40"
            disabled={saving || selectedDocumentId === null}
            onClick={() => {
              void handleAddDocument();
            }}
            type="button"
          >
            {saving ? "Adding…" : "Add"}
          </button>
        </div>
        {error && (
          <p className="mt-2 text-xs text-red-400">{error}</p>
        )}
      </div>

      <div className="min-h-0 flex-1 overflow-y-auto">
        {detail.documents.length === 0 ? (
          <div className="flex h-full items-center justify-center px-8 text-center text-sm text-[#666]">
            No documents in this collection yet. Add one from the dropdown above.
          </div>
        ) : (
          detail.documents.map((document) => (
            <div
              className="cursor-pointer border-b border-[#2a2a2a] px-5 py-4 transition hover:bg-[#252525]"
              key={document.id}
              onClick={() => {
                if (document.status === "ready") {
                  void navigate(`/doc/${document.id}`);
                }
              }}
            >
              <div className="min-w-0">
                <p className="truncate text-sm font-medium text-[#e6e6e6]">
                  {documentListTitle(document)}
                </p>
                <p className="mt-1 truncate text-[0.78rem] text-[#777]">
                  {documentMeta(document)}
                </p>
              </div>
            </div>
          ))
        )}
      </div>
    </div>
  );
}
