import { useEffect, useMemo, type ReactNode } from "react";
import { ThreadContext } from "./ThreadContext";
import { ThreadService } from "./ThreadService";

export function ThreadProvider({ children }: { children: ReactNode }) {
  const service = useMemo(() => new ThreadService(), []);

  useEffect(() => {
    service.initialize();
    return () => { service.dispose(); };
  }, [service]);

  return (
    <ThreadContext value={service}>
      {children}
    </ThreadContext>
  );
}
