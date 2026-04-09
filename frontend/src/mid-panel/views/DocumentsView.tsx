import { useCallback, useEffect, useRef, useState } from "react";
import { apiGet, uploadDocument } from "../../api";
import type { DocumentEntry, DocumentListResponse } from "../../types";

function documentTitle(document: DocumentEntry) {
  const filename = document.filename.trim();
  return filename || document.id;
}

export function DocumentsView() {
  const [documents, setDocuments] = useState<DocumentEntry[]>([]);
  const [uploading, setUploading] = useState(false);
  const [error, setError] = useState("");
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
          onChange={handleFileChange}
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
            className="border-b border-[#2a2a2a] px-3 py-3 transition hover:bg-[#252525]"
            key={document.id}
          >
            <span className="block truncate text-sm font-medium text-[#d4d4d4]">
              {documentTitle(document)}
            </span>
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
