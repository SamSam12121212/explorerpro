import { useCallback, useEffect, useState } from "react";
import { apiGet, apiPost } from "../../api";
import type { RepoAddResponse, RepoEntry, RepoListResponse } from "../../types";

function statusColor(status: string) {
  switch (status.toLowerCase()) {
    case "ready":
      return "text-emerald-400";
    case "cloning":
      return "text-amber-400";
    case "failed":
      return "text-red-400";
    default:
      return "text-[#888]";
  }
}

function shouldShowRepoStatus(status: string): boolean {
  const s = status.trim().toLowerCase();
  return s.length > 0 && s !== "ready";
}

export function ReposView() {
  const [repos, setRepos] = useState<RepoEntry[]>([]);
  const [urlInput, setUrlInput] = useState("");
  const [adding, setAdding] = useState(false);
  const [error, setError] = useState("");

  const fetchRepos = useCallback(async () => {
    try {
      const data = await apiGet<RepoListResponse>("/repos");
      setRepos(data.repos);
    } catch {
      /* swallow fetch errors */
    }
  }, []);

  useEffect(() => {
    void fetchRepos();
  }, [fetchRepos]);

  const handleAdd = async () => {
    const trimmed = urlInput.trim();
    if (!trimmed || adding) return;
    setAdding(true);
    setError("");
    try {
      await apiPost<RepoAddResponse>("/repos", { url: trimmed });
      setUrlInput("");
      await fetchRepos();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to add repo");
    } finally {
      setAdding(false);
    }
  };

  return (
    <div className="flex h-full w-full min-w-0 flex-col bg-[#1e1e1e]">
      <div className="flex items-center justify-between border-b border-[#333] px-4 py-2">
        <span className="text-xs font-semibold uppercase tracking-widest text-[#888]">
          Repos
        </span>
        <span className="text-[0.68rem] text-[#555]">
          {repos.length > 0 ? `${repos.length.toString()} repo${repos.length === 1 ? "" : "s"}` : ""}
        </span>
      </div>

      <div className="border-b border-[#2a2a2a] px-4 py-3">
        <form
          className="flex gap-2"
          onSubmit={(e) => {
            e.preventDefault();
            void handleAdd();
          }}
        >
          <input
            className="min-w-0 flex-1 border border-[#333] bg-[#252525] px-3 py-1.5 text-sm text-[#d4d4d4] outline-none placeholder:text-[#555] focus:border-[#007acc]"
            disabled={adding}
            onChange={(e) => { setUrlInput(e.target.value); }}
            placeholder="https://github.com/org/repo"
            type="text"
            value={urlInput}
          />
          <button
            className="bg-[#007acc] px-3 py-1.5 text-xs font-semibold text-white transition hover:bg-[#1b8de4] disabled:cursor-not-allowed disabled:opacity-40"
            disabled={adding || !urlInput.trim()}
            type="submit"
          >
            {adding ? "Adding…" : "Add"}
          </button>
        </form>
        {error && (
          <p className="mt-1.5 text-xs text-red-400">{error}</p>
        )}
      </div>

      <div className="min-h-0 flex-1 overflow-y-auto">
        {repos.length === 0 && (
          <div className="px-4 py-8 text-center text-sm text-[#555]">
            No repos yet. Paste a GitHub URL above to add one.
          </div>
        )}

        {repos.map((repo) => (
          <div
            className="flex flex-col gap-1 border-b border-[#2a2a2a] px-4 py-3 transition hover:bg-[#252525]"
            key={repo.id}
          >
            <div className="flex items-center justify-between gap-2">
              <span className="truncate text-sm font-medium text-[#d4d4d4]">
                {repo.name}
              </span>
              {shouldShowRepoStatus(repo.status) && (
                <span className={`shrink-0 text-[0.7rem] font-medium uppercase ${statusColor(repo.status)}`}>
                  {repo.status}
                </span>
              )}
            </div>
            <span className="truncate text-[0.78rem] text-[#777]">
              {repo.url}
            </span>
            <div className="flex items-center gap-3 text-[0.68rem] text-[#555]">
              <span>{repo.ref}</span>
              {repo.commit_sha && (
                <span className="font-mono">{repo.commit_sha.slice(0, 8)}</span>
              )}
            </div>
            {repo.error && (
              <p className="mt-0.5 text-[0.72rem] text-red-400">{repo.error}</p>
            )}
          </div>
        ))}
      </div>
    </div>
  );
}
