import type { ModelOption, ReasoningEffort } from "./types";

export const MODEL_OPTIONS: ModelOption[] = [
  { value: "gpt-5.4", label: "GPT-5.4" },
  { value: "gpt-5.4-mini", label: "GPT-5.4 Mini" },
  { value: "gpt-5.4-nano", label: "GPT-5.4 Nano" },
];

export const DEFAULT_MODEL = "gpt-5.4";
export const COLLECTIONS_CHANGED_EVENT = "explorer:collections-changed";

export const DEFAULT_INSTRUCTIONS = [
  "You are a helpful assistant. Be concise and clear.",
  "If parallel work would materially help, you may call spawn_threads once with up to 10 child threads.",
  "Only use child threads when the task benefits from decomposition; otherwise answer normally.",
  "After child results return, synthesize one final answer for the user.",
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
      "Launch up to 10 focused child threads for parallel work and synthesize their results back in the parent thread.",
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        children: {
          type: "array",
          minItems: 1,
          maxItems: 10,
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
