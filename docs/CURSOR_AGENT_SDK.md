# Cursor Agent CLI — SDK notes (Go)

This document captures findings from exercising the Cursor Agent CLI and outlines how a **Go SDK** can wrap it. The SDK’s job is to **spawn the official `agent` binary** (named `agent` on macOS/Linux; **`agent.exe`** on Windows), parse its structured output, and optionally run **many** such processes (many threads, many users, or many concurrent turns).

Official references:

- [Cursor CLI overview](https://www.cursor.com/docs/cli/overview)
- [Output format (JSON / stream-json)](https://cursor.com/docs/cli/reference/output-format)

---

## 1. Why wrap the CLI (not a private HTTP API)

- The **supported** programmatic surface for headless use is **`agent` with `--print` (`-p`)** and structured stdout.
- **`--output-format stream-json`** emits **NDJSON** (one JSON object per line) suitable for tools and a Go SDK.
- Cursor may add fields over time; consumers should **ignore unknown JSON fields** (forward-compatible parsing).

---

## 2. Installing the binary

Install (macOS / Linux / WSL):

```bash
curl https://cursor.com/install -fsS | bash
```

Typical location after install: `~/.local/bin/agent` (ensure that directory is on `PATH`, or pass an **absolute path** to the binary from config).

Windows (PowerShell):

```powershell
irm 'https://cursor.com/install?win32=true' | iex
```

The Go SDK should take **`AgentPath`** (or `CURSOR_AGENT_PATH`) pointing at the executable: `agent` vs `agent.exe` per OS.

---

## 3. Commands that matter for an SDK

| Goal | Pattern |
|------|--------|
| **Headless run** | Always pass **`-p` / `--print`**. |
| **Machine-readable stream** | **`--output-format stream-json`**; add **`--stream-partial-output`** for smaller assistant text chunks. |
| **Repo / cwd context** | **`--workspace <abs path>`** — workspace root for tools (defaults to process cwd if omitted). |
| **Skip trust prompts (headless)** | **`--trust`** (only meaningful with print/headless; still align with your security model). |
| **Model** | **`--model <id>`** (e.g. `composer-2`, `gpt-5.3-codex-spark-preview`). List IDs with **`agent models`**. |
| **New “thread” (session)** | First invocation **without** `--resume`. Parse **`session_id`** from the stream (see below). |
| **Continue same thread** | **`--resume <session_id>`** on the next invocation, plus the new user message. |
| **Auth** | Interactive: **`agent login`**. Automation: document **`CURSOR_API_KEY`** / **`--api-key`** per CLI help. |

Example (Unix-style binary name):

```bash
agent -p --output-format stream-json --stream-partial-output \
  --workspace /path/to/repo --trust --model composer-2 \
  "Your prompt"
```

Follow-up on the same thread:

```bash
agent -p --output-format stream-json --stream-partial-output \
  --workspace /path/to/repo --trust --model composer-2 \
  --resume "<session_id>" "Follow-up prompt"
```

---

## 4. Events: what consumers actually receive

### 4.1 `stream-json` (recommended for SDKs)

- **Stdout** is **NDJSON**: **one JSON object per line**, each line terminated with `\n`.
- The Go SDK should use a **line reader** (`bufio.Scanner` with large buffer, or `ReadString('\n')`) and **`json.Unmarshal`** per line into a **discriminated union** keyed by **`type`** (and sometimes **`subtype`**).

Documented event families include (non-exhaustive):

- **`system`** / **`init`** — includes **`session_id`**, **`cwd`**, **`model`**, etc.
- **`user`** — user message payload.
- **`assistant`** — assistant segments; with **`--stream-partial-output`**, many small updates; concatenate text per your product needs.
- **`tool_call`** — **`started`** / **`completed`** with structured tool payloads (e.g. read/glob/write); correlate with **`call_id`**.
- **`result`** — terminal success summary for that **invocation** (includes **`session_id`**, timings, aggregated **`result`** text, **`usage`** when present).

**Parsing `session_id` for a new thread:** Prefer the terminal **`result`** line’s **`session_id`**; the **`system`** / **`init`** line also carries **`session_id`** (useful if you need it before the run finishes).

### 4.2 `json` (single blob per invocation)

- **`--output-format json`** without streaming: **one JSON object** (plus newline) at **end** of a successful run. Simpler, but **no** per-event tool streaming.

### 4.3 Reasoning / “thinking”

- Official docs note that **`thinking`** may be **suppressed** in print mode in some cases. In practice, **`stream-json`** runs have been observed to emit **`thinking`** (`delta` / `completed`) for some models (e.g. Composer 2). **Do not rely on thinking content** for core logic; treat it as optional telemetry.

---

## 5. stderr, exit codes, failures

- On failure, the CLI typically writes to **stderr** and exits **non-zero**; stdout may not end with a well-formed terminal `result` event.
- The SDK should always capture **stderr** for logging and surface errors to callers.

---

## 6. Go: running `agent` / `agent.exe` and managing **many** processes

The SDK will often manage **many concurrent agent runs** (many sessions, many tenants, or overlapping work). Treat each **`agent` invocation as an isolated subprocess** unless Cursor documents a long-lived IPC mode (today, the CLI is **process-per-invocation** friendly).

### 6.1 Process model

- **`exec.CommandContext(ctx, agentPath, args...)`** — use a **context** for cancellation/timeouts.
- **Working directory:** set **`Cmd.Dir`** to the workspace root if you rely on relative paths; prefer passing **`--workspace`** with an **absolute path** so behavior is explicit.
- **Environment:** inherit a clean env; inject **`CURSOR_API_KEY`** (or document login for interactive dev machines).
- **Stdin:** usually **not** needed for simple prompt strings (prompt is a CLI argument). If future CLI versions support stdin-only prompts, adapt behind an interface.

### 6.2 One process per “turn” (recommended baseline)

- Each user message = **one** `agent` process with **`-p`** and **`--resume`** when continuing a session.
- **Pros:** simple lifecycle, natural **timeout/cancel** per turn, easy horizontal scaling, no global state in the SDK.
- **Cons:** process spawn overhead; acceptable for most automation if batch size is moderate.

### 6.3 Many concurrent processes

- **No fixed global limit** in this doc — cap concurrency in your service (**weighted semaphore** / worker pool) so you do not exhaust CPU, file descriptors, or Cursor rate limits.
- Each subprocess: **its own stdout pipe**, **its own stderr pipe** (merge or tag logs with a **request ID** / **session ID**).
- Avoid sharing one `agent` stdin/stdout across threads without careful multiplexing; **one goroutine per process** reading stdout is the straightforward pattern.

### 6.4 Cancellation

- **`context` cancellation** should **`Process.Kill()`** (or interrupt group) if the CLI does not exit promptly on SIGTERM on your platform; document behavior per OS.
- Assume partial NDJSON on stdout after kill; consumers must tolerate **truncated** streams.

### 6.5 Session affinity

- **Session continuity** is entirely in **`session_id`** + **`--resume`**. Persist **`session_id`** in your app DB keyed by your **conversation / user / tenant** model.
- Reuse **`--workspace`** (same repo path) across turns when the thread should see the same files.

### 6.6 Windows vs Unix

- Binary: **`agent.exe`** on Windows; **`agent`** elsewhere.
- Argument escaping: use **`exec.Command` with separate args** (no shell) to avoid injection; **do not** build a single shell string for untrusted input.

### 6.7 Resource accounting

- Long **`stream-json`** runs can produce **large stdout** (tool outputs embedded in JSON). Prefer **streaming line reads** and optional **size caps** per event type in your mapper layer.

---

## 7. Optional: benchmark script in this repo

The repo may include **`scripts/run-agent-codebase-prompts.sh`**, which runs five prompts with **`stream-json`**, **`--stream-partial-output`**, and **`composer-2`**, logging to **`$HOME/logs/`**. Useful as a manual reference for event volume and timing.

---

## 8. Summary

| Idea | Detail |
|------|--------|
| **Consumer API (conceptual)** | Start a **new thread** = run `agent` once without `--resume`; consume **NDJSON events** on stdout. |
| **Continue thread** | Run again with **`--resume <session_id>`**. |
| **Go orchestration** | **Many agents** = **many subprocesses** (typically one per turn), each with **context**, **line-buffered NDJSON decode**, **stderr** logging, and **concurrency limits**. |
| **Binary name** | Use **`agent.exe`** on Windows and **`agent`** on Unix; configurable **absolute path** recommended. |

This file is descriptive only; it is not an official Cursor product document. Verify flags against **`agent --help`** and the Cursor docs for your installed CLI version.
