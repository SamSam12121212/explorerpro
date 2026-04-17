import { useCallback, useEffect, useState } from "react";
import { LuEllipsis, LuFileText, LuTrash2 } from "react-icons/lu";
import { useNavigate, useParams } from "react-router";
import { apiDelete, apiGet } from "../../api";
import { DOCUMENTS_CHANGED_EVENT } from "../../constants";
import type {
  AttachedDocument,
  DocumentEntry,
  DocumentListResponse,
} from "../../types";

function documentTitle(document: DocumentEntry) {
  const filename = document.filename.trim();
  return filename || document.id.toString();
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
  attachedDocumentIds: number[];
  onAttachDocument: (document: AttachedDocument) => void;
}

export function DocumentsView({
  attachedDocumentIds,
  onAttachDocument,
}: DocumentsViewProps) {
  const { documentId } = useParams<"documentId">();
  const navigate = useNavigate();
  const [documents, setDocuments] = useState<DocumentEntry[]>([]);
  const [openMenuDocumentId, setOpenMenuDocumentId] = useState<number | null>(null);
  const [deletingDocumentId, setDeletingDocumentId] = useState<number | null>(null);

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
    const handleDocumentsChanged = () => {
      void fetchDocuments();
    };

    window.addEventListener(DOCUMENTS_CHANGED_EVENT, handleDocumentsChanged);
    return () => {
      window.removeEventListener(DOCUMENTS_CHANGED_EVENT, handleDocumentsChanged);
    };
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

  const handleDelete = useCallback(
    async (document: DocumentEntry) => {
      if (deletingDocumentId) return;
      const confirmed = window.confirm(
        `Delete "${documentTitle(document)}"? This removes it from the app; the uploaded file in blob storage is retained.`,
      );
      if (!confirmed) return;

      setDeletingDocumentId(document.id);
      try {
        await apiDelete(`/documents/${document.id.toString()}`);
        setOpenMenuDocumentId(null);
        window.dispatchEvent(new Event(DOCUMENTS_CHANGED_EVENT));
        if (documentId === document.id.toString()) {
          void navigate("/");
        }
      } catch {
        /* swallow delete errors */
      } finally {
        setDeletingDocumentId(null);
      }
    },
    [deletingDocumentId, documentId, navigate],
  );

  return (
    <div className="flex h-full w-full min-w-0 flex-col bg-[#1e1e1e]">
      <div className="min-h-0 flex-1 overflow-y-auto">
        {documents.length === 0 && (
          <div className="px-3 py-6 text-center text-xs text-[#666]">
            No documents yet. Upload a PDF to add one.
          </div>
        )}

        {documents.map((document) => {
          const isActive = documentId === document.id.toString();

          return (
            <div
              className={`group flex items-start transition ${
                isActive
                  ? "bg-[#2a2a2a] shadow-[inset_2px_0_0_#007acc]"
                  : "bg-transparent hover:bg-[#252525]"
              }`}
              key={document.id}
            >
              <button
                className={`min-w-0 flex-1 px-3 py-2.5 text-left transition ${
                  isActive ? "text-white" : "text-[#b2b2b2] group-hover:text-[#d4d4d4]"
                }`}
                onClick={() => {
                  if (document.status === "ready") {
                    setOpenMenuDocumentId(null);
                    void navigate(`/doc/${document.id.toString()}`);
                  }
                }}
                type="button"
              >
                <span className="block truncate text-sm font-medium">
                  {documentTitle(document)}
                </span>
              </button>

              <div
                className="relative flex items-start pr-2 pt-2"
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
                          : "Attach to thread"}
                      </span>
                    </button>
                    <button
                      className="flex w-full items-center gap-2 px-2 py-1.5 text-left text-xs text-[#d4d4d4] transition hover:bg-[#2a2a2a] disabled:cursor-not-allowed disabled:opacity-50"
                      disabled={deletingDocumentId === document.id}
                      onClick={() => {
                        void handleDelete(document);
                      }}
                      type="button"
                    >
                      <LuTrash2 className="h-3.5 w-3.5" />
                      <span>
                        {deletingDocumentId === document.id
                          ? "Deleting…"
                          : "Delete document"}
                      </span>
                    </button>
                  </div>
                )}
              </div>
            </div>
          );
        })}
      </div>
    </div>
  );
}
