import { useLocation, useParams } from "react-router";
import { CollectionDetailView } from "../mid-panel/views/CollectionDetailView";

export function MidPanelHost() {
  const location = useLocation();
  const { collectionId } = useParams<"collectionId">();

  if (location.pathname.startsWith("/collections")) {
    if (collectionId) {
      return <CollectionDetailView collectionId={collectionId} />;
    }

    return (
      <div className="flex h-full w-full items-center justify-center bg-[#1e1e1e] px-8 text-center">
        <div>
          <p className="text-[0.68rem] font-semibold uppercase tracking-[0.24em] text-[#666]">
            Collections
          </p>
          <p className="mt-3 text-sm text-[#777]">
            Select a collection from the left to view and organize its documents.
          </p>
        </div>
      </div>
    );
  }

  return <div className="h-full w-full bg-[#1e1e1e]" />;
}
