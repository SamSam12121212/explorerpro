import type { ReasoningEffort } from "./types";

export const DEFAULT_INSTRUCTIONS = [
  "You are a helpful assistant. Be concise and clear.",
  "If parallel work would materially help, you may call spawn_subagents once with up to 10 child agents.",
  "Only use subagents when the task benefits from decomposition; otherwise answer normally.",
  "After child results return, synthesize one final answer for the user.",
].join(" ");

export const REASONING_OPTIONS: { value: ReasoningEffort; label: string }[] = [
  { value: "low", label: "Low" },
  { value: "medium", label: "Med" },
  { value: "high", label: "High" },
];

export const EXPLORER_TOOLS = [
  {
    type: "function",
    name: "spawn_subagents",
    description:
      "Launch up to 10 focused child agent threads for parallel work and synthesize their results back in the parent thread.",
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
              thread_id: { type: "string" },
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
