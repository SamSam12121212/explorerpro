import { useCallback, useEffect, useRef, useState } from "react";
import { LuEllipsis, LuFileText } from "react-icons/lu";
import { apiGet, uploadDocument } from "../../api";
import type {
  AttachedDocument,
  DocumentEntry,
  DocumentListResponse,
} from "../../types";

function documentTitle(document: DocumentEntry) {
  const filename = document.filename.trim();
  return filename || document.id;
}

function toAttachedDocument(document: DocumentEntry): AttachedDocument {
  return {
    id: document.id,
    filename: document.filename,
    page_count: document.page_count,
    status: document.status,
  };
}

interface DocumentsViewProps {
  attachedDocumentIds: string[];
  onAttachDocument: (document: AttachedDocument) => void;
}

export function DocumentsView({
  attachedDocumentIds,
  onAttachDocument,
}: DocumentsViewProps) {
  const [documents, setDocuments] = useState<DocumentEntry[]>([]);
  const [uploading, setUploading] = useState(false);
  const [error, setError] = useState("");
  const [openMenuDocumentId, setOpenMenuDocumentId] = useState<string | null>(null);
  const inputRef = useRef<HTMLInputElement>(null);

  const fetchDocuments = useCallback(async () => {
    try {
      const data = await apiGet<DocumentListResponse>("/documents");
      setDocuments(data.documents);
    } catch {
      /* swallow fetch errors */
    }
  }, []);

  useEffect(() => {
    void fetchDocuments();
  }, [fetchDocuments]);

  useEffect(() => {
    const hasActiveProcessing = documents.some((document) => {
      const status = document.status.trim().toLowerCase();
      return status === "pending" || status === "splitting";
    });
    if (!hasActiveProcessing) {
      return undefined;
    }

    const timer = window.setTimeout(() => {
      void fetchDocuments();
    }, 2500);
    return () => {
      window.clearTimeout(timer);
    };
  }, [documents, fetchDocuments]);

  useEffect(() => {
    if (!openMenuDocumentId) {
      return undefined;
    }

    const handlePointerDown = (event: MouseEvent) => {
      const target = event.target;
      if (!(target instanceof Element)) {
        setOpenMenuDocumentId(null);
        return;
      }

      if (target.closest("[data-document-menu]")) {
        return;
      }

      setOpenMenuDocumentId(null);
    };

    window.addEventListener("mousedown", handlePointerDown);
    return () => {
      window.removeEventListener("mousedown", handlePointerDown);
    };
  }, [openMenuDocumentId]);

  const handleUploadClick = () => {
    inputRef.current?.click();
  };

  const handleFileChange = async (event: React.ChangeEvent<HTMLInputElement>) => {
    const file = event.target.files?.[0];
    event.target.value = "";
    if (!file || uploading) return;

    setUploading(true);
    setError("");
    try {
      await uploadDocument(file);
      await fetchDocuments();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to upload document");
    } finally {
      setUploading(false);
    }
  };

  return (
    <div className="flex h-full w-full min-w-0 flex-col bg-[#1e1e1e]">
      <div className="flex items-center justify-between border-b border-[#333] px-3 py-2">
        <span className="text-xs font-semibold uppercase tracking-widest text-[#888]">
          Documents
        </span>
        <button
          className="flex h-7 w-7 items-center justify-center border border-[#333] bg-[#2a2a2a] text-sm text-[#b2b2b2] transition hover:bg-[#333] hover:text-white disabled:cursor-not-allowed disabled:opacity-40"
          disabled={uploading}
          onClick={handleUploadClick}
          title="Upload document"
          type="button"
        >
          {uploading ? "…" : "+"}
        </button>
        <input
          accept="application/pdf,.pdf"
          className="hidden"
          onChange={(event) => void handleFileChange(event)}
          ref={inputRef}
          type="file"
        />
      </div>

      <div className="min-h-0 flex-1 overflow-y-auto">
        {documents.length === 0 && (
          <div className="px-3 py-6 text-center text-xs text-[#666]">
            No documents yet. Upload a PDF to add one.
          </div>
        )}

        {documents.map((document) => (
          <div
            className="group relative border-b border-[#2a2a2a] px-3 py-3 transition hover:bg-[#252525]"
            key={document.id}
          >
            <div className="pr-10">
              <span className="block truncate text-sm font-medium text-[#d4d4d4]">
                {documentTitle(document)}
              </span>
              <span className="mt-1 block text-xs text-[#666]">
                {document.page_count > 0
                  ? `${document.page_count.toString()} pages`
                  : document.status}
              </span>
            </div>

            <div
              className="absolute top-2 right-2"
              data-document-menu={document.id}
            >
              <button
                className={`flex h-7 w-7 items-center justify-center border border-[#333] bg-[#252525] text-[#888] transition hover:border-[#444] hover:text-[#d4d4d4] ${
                  openMenuDocumentId === document.id
                    ? "opacity-100"
                    : "opacity-0 group-hover:opacity-100 focus:opacity-100"
                }`}
                onClick={() => {
                  setOpenMenuDocumentId((current) =>
                    current === document.id ? null : document.id,
                  );
                }}
                title="Document actions"
                type="button"
              >
                <LuEllipsis className="h-4 w-4" />
              </button>

              {openMenuDocumentId === document.id && (
                <div className="absolute top-8 right-0 z-10 min-w-40 border border-[#333] bg-[#202020] p-1 shadow-[0_8px_24px_rgba(0,0,0,0.35)]">
                  <button
                    className={`flex w-full items-center gap-2 px-2 py-1.5 text-left text-xs transition ${
                      attachedDocumentIds.includes(document.id)
                        ? "cursor-default text-[#666]"
                        : "text-[#d4d4d4] hover:bg-[#2a2a2a]"
                    }`}
                    disabled={attachedDocumentIds.includes(document.id)}
                    onClick={() => {
                      onAttachDocument(toAttachedDocument(document));
                      setOpenMenuDocumentId(null);
                    }}
                    type="button"
                  >
                    <LuFileText className="h-3.5 w-3.5" />
                    <span>
                      {attachedDocumentIds.includes(document.id)
                        ? "Attached"
                        : "Attach to chat"}
                    </span>
                  </button>
                </div>
              )}
            </div>
          </div>
        ))}
      </div>

      {error && (
        <div className="border-t border-[#2a2a2a] px-3 py-2">
          <p className="text-xs text-red-400">{error}</p>
        </div>
      )}
    </div>
  );
}
