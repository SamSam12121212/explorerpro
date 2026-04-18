import type { ModelOption, ReasoningEffort } from "./types";

export const MODEL_OPTIONS: ModelOption[] = [
  { value: "gpt-5.4", label: "GPT-5.4" },
  { value: "gpt-5.4-mini", label: "GPT-5.4 Mini" },
  { value: "gpt-5.4-nano", label: "GPT-5.4 Nano" },
];

export const DEFAULT_MODEL = "gpt-5.4-mini";
export const DEFAULT_REASONING = "medium";
export const COLLECTIONS_CHANGED_EVENT = "explorer:collections-changed";
export const DOCUMENTS_CHANGED_EVENT = "explorer:documents-changed";

// Worker-side tool name (internal/doccmd: ToolNameQueryDocument). The tool
// itself is injected by the worker when documents are attached, not declared
// in EXPLORER_TOOLS — but the frontend needs the name to recognize the
// corresponding `function_call` items in the live stream.
export const QUERY_DOCUMENT_TOOL_NAME = "query_document";

// Worker-side tool name (internal/doccmd: ToolNameReadDocumentPage). Injected
// by the worker on root threads when documents are attached.
export const READ_DOCUMENT_PAGE_TOOL_NAME = "read_document_page";

export const DEFAULT_INSTRUCTIONS = [
  "You are a helpful assistant. Be concise and clear.",
  "If parallel work would materially help, you may call spawn_threads once with up to 50 child threads per turn.",
  "When attached documents are available, you may issue up to 50 query_document calls in parallel per turn. If you need more parallelism than that, do follow-up turns rather than exceeding the cap — calls beyond the cap will be rejected.",
  "Only use child threads when the task benefits from decomposition; otherwise answer normally.",
  "After child results return, synthesize one final answer for the user.",
  "Citing evidence from an attached document: (1) Use read_document_page to open the page(s) you need to verify. (2) Call store_citation once per contiguous region of evidence with document_id, pages (1 entry for a single-page region, or 2 consecutive entries like [17, 18] when the region crosses a page break), an instruction describing what to highlight (include your authoritative quoted text as an anchor — OCR text may be noisy), and include_images=false for digital documents, true only for noisy scans. You may call store_citation in parallel (up to 20 per turn). (3) When the citation_id arrives, embed the reference inline in your reply as [display text][citation_id]. Example: `[4.7.1.10 states serotonin syndrome causes neuromuscular hyperactivity][42]`. The frontend renders that as a clickable chip that jumps to the document and highlights the evidence.",
  "Always use the [display text][citation_id] format for citations — not markdown links. Never invent citation_ids, document_ids, or page numbers; only reference documents that appear in <available_documents>.",
].join(" ");

// GPT-5.4 does not support `minimal`; use `none` for the lowest effort.
export const REASONING_OPTIONS: { value: ReasoningEffort; label: string }[] = [
  { value: "none", label: "\u00B7 (none)" },
  { value: "low", label: "\u00B7\u00B7\u00B7 (low)" },
  { value: "medium", label: "\u00B7\u00B7\u00B7\u00B7 (medium)" },
  { value: "high", label: "\u00B7\u00B7\u00B7\u00B7\u00B7 (high)" },
  { value: "xhigh", label: "\u00B7\u00B7\u00B7\u00B7\u00B7\u00B7 (xhigh)" },
];

export const EXPLORER_TOOLS = [
  {
    type: "function",
    name: "spawn_threads",
    description:
      "Launch up to 50 focused child threads for parallel work and synthesize their results back in the parent thread.",
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        children: {
          type: "array",
          minItems: 1,
          maxItems: 50,
          items: {
            type: "object",
            additionalProperties: false,
            properties: {
              thread_id: { type: "integer" },
              input: {
                description:
                  "Responses API input items for the child thread.",
              },
              prompt: {
                type: "string",
                description:
                  "A focused task for the child. Prefer this for normal playground use.",
              },
              model: { type: "string" },
              instructions: { type: "string" },
              metadata: { type: "object" },
            },
            required: [],
          },
        },
      },
      required: ["children"],
    },
  },
];
