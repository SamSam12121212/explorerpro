import { useNavigate } from "react-router";
import type { ReactNode } from "react";
import { useThread } from "../../thread";

// `[display text][citation_id]` in the model's response becomes one of these.
// On click: navigate to the document's first cited page with citation=N in
// the URL so the PDF painter knows which highlights to draw. Resolution of
// the id to bboxes happens in-context via useThread().citations; the URL
// carries the id only to survive reloads / deep-link sharing.
export function CitationChip({
  citationId,
  children,
}: {
  citationId: number;
  children: ReactNode;
}) {
  const { citations } = useThread();
  const navigate = useNavigate();
  const citation = citations[citationId];

  if (!citation) {
    // Fallback while citations are still fetching, or the id points at a
    // row we haven't seen yet. Render the display text as a flat span so
    // the message still reads naturally — worst case the user can't click.
    return <span className="text-[#9aa0a6]">{children}</span>;
  }

  const firstPage = citation.bboxes?.[0]?.page ?? 1;
  const href = `/doc/${citation.document_id.toString()}?page=${firstPage.toString()}&citation=${citationId.toString()}`;

  return (
    <button
      className="inline rounded border border-[#4a4a4a] bg-[#2d2d2d] px-1.5 py-0.5 text-[#ffcc00] underline-offset-2 hover:border-[#ffcc00] hover:underline"
      onClick={() => { navigate(href); }}
      title={citation.instruction}
      type="button"
    >
      {children}
    </button>
  );
}
