import { describe, expect, it } from "vitest";
import type { ThreadMessage } from "../types";
import {
  applyOutputTextDelta,
  buildMessageFromOutputItemDone,
  buildStreamingMessageFromOutputItemAdded,
  finalizeStreamingMessages,
  upsertMessage,
} from "./eventInterpreter";

describe("buildStreamingMessageFromOutputItemAdded", () => {
  it("creates an empty streaming assistant message for a message item", () => {
    const msg = buildStreamingMessageFromOutputItemAdded({
      type: "response.output_item.added",
      output_index: 0,
      item: { id: "msg_1", type: "message", role: "assistant", content: [] },
    });

    expect(msg).toEqual({
      id: "msg_1",
      role: "assistant",
      text: "",
      streaming: true,
    });
  });

  it("returns null for non-message items (reasoning, function_call, etc)", () => {
    expect(
      buildStreamingMessageFromOutputItemAdded({
        type: "response.output_item.added",
        item: { id: "rs_1", type: "reasoning" },
      }),
    ).toBeNull();
  });

  it("returns null when item has no id", () => {
    expect(
      buildStreamingMessageFromOutputItemAdded({
        type: "response.output_item.added",
        item: { type: "message" },
      }),
    ).toBeNull();
  });
});

describe("applyOutputTextDelta", () => {
  it("appends the delta to an existing streaming message", () => {
    const initial: ThreadMessage[] = [
      { id: "msg_1", role: "assistant", text: "Hello ", streaming: true },
    ];

    const next = applyOutputTextDelta(initial, {
      type: "response.output_text.delta",
      item_id: "msg_1",
      delta: "world",
    });

    expect(next).toEqual([
      { id: "msg_1", role: "assistant", text: "Hello world", streaming: true },
    ]);
  });

  it("creates a streaming message on demand when delta arrives before added", () => {
    const next = applyOutputTextDelta([], {
      type: "response.output_text.delta",
      item_id: "msg_2",
      delta: "first",
    });

    expect(next).toEqual([
      { id: "msg_2", role: "assistant", text: "first", streaming: true },
    ]);
  });

  it("leaves state unchanged when delta is empty", () => {
    const initial: ThreadMessage[] = [
      { id: "msg_1", role: "assistant", text: "abc", streaming: true },
    ];
    expect(
      applyOutputTextDelta(initial, {
        type: "response.output_text.delta",
        item_id: "msg_1",
        delta: "",
      }),
    ).toBe(initial);
  });

  it("leaves state unchanged when item id is missing", () => {
    const initial: ThreadMessage[] = [];
    expect(
      applyOutputTextDelta(initial, {
        type: "response.output_text.delta",
        delta: "x",
      }),
    ).toBe(initial);
  });

  it("falls back to event.item.id when item_id is absent", () => {
    const next = applyOutputTextDelta([], {
      type: "response.output_text.delta",
      item: { id: "msg_3", type: "message" },
      delta: "hi",
    });

    expect(next).toEqual([
      { id: "msg_3", role: "assistant", text: "hi", streaming: true },
    ]);
  });
});

describe("upsertMessage", () => {
  it("replaces an existing message by id (completion-as-truth)", () => {
    const initial: ThreadMessage[] = [
      { id: "msg_1", role: "assistant", text: "partial", streaming: true },
    ];

    const truth: ThreadMessage = {
      id: "msg_1",
      role: "assistant",
      text: "final authoritative text",
    };

    expect(upsertMessage(initial, truth)).toEqual([truth]);
  });

  it("appends when id is new", () => {
    const initial: ThreadMessage[] = [
      { id: "msg_1", role: "assistant", text: "a" },
    ];
    const incoming: ThreadMessage = { id: "msg_2", role: "assistant", text: "b" };

    expect(upsertMessage(initial, incoming)).toEqual([
      { id: "msg_1", role: "assistant", text: "a" },
      incoming,
    ]);
  });

  it("replaces an optimistic user message with matching content when server id differs", () => {
    const initial: ThreadMessage[] = [
      {
        id: "optimistic-abc",
        role: "user",
        text: "hello",
        optimistic: true,
      },
    ];

    const serverMessage: ThreadMessage = {
      id: "server-123",
      role: "user",
      text: "hello",
    };

    expect(upsertMessage(initial, serverMessage)).toEqual([serverMessage]);
  });
});

describe("end-to-end stream sequence", () => {
  it("added → deltas → done produces the authoritative message with streaming cleared", () => {
    let messages: ThreadMessage[] = [];

    // 1. output_item.added → streaming stub
    const stub = buildStreamingMessageFromOutputItemAdded({
      type: "response.output_item.added",
      item: { id: "msg_x", type: "message", role: "assistant", content: [] },
    });
    expect(stub).not.toBeNull();
    if (stub) {
      messages = upsertMessage(messages, stub);
    }

    // 2. deltas accumulate
    messages = applyOutputTextDelta(messages, {
      type: "response.output_text.delta",
      item_id: "msg_x",
      delta: "Hello ",
    });
    messages = applyOutputTextDelta(messages, {
      type: "response.output_text.delta",
      item_id: "msg_x",
      delta: "world",
    });

    expect(messages).toEqual([
      { id: "msg_x", role: "assistant", text: "Hello world", streaming: true },
    ]);

    // 3. output_item.done → authoritative replacement
    const truth = buildMessageFromOutputItemDone({
      type: "response.output_item.done",
      item: {
        id: "msg_x",
        type: "message",
        role: "assistant",
        content: [{ type: "output_text", text: "Hello world" }],
      },
    });
    expect(truth).not.toBeNull();
    if (truth) {
      messages = upsertMessage(messages, truth);
    }

    expect(messages).toEqual([
      { id: "msg_x", role: "assistant", text: "Hello world" },
    ]);
    expect(messages[0]?.streaming).toBeUndefined();
  });
});

describe("finalizeStreamingMessages", () => {
  it("clears the streaming flag from any lingering streaming messages", () => {
    const initial: ThreadMessage[] = [
      { id: "a", role: "assistant", text: "done", streaming: true },
      { id: "b", role: "user", text: "q" },
    ];

    expect(finalizeStreamingMessages(initial)).toEqual([
      { id: "a", role: "assistant", text: "done" },
      { id: "b", role: "user", text: "q" },
    ]);
  });

  it("returns the same array when no streaming messages are present (referential equality)", () => {
    const initial: ThreadMessage[] = [
      { id: "a", role: "assistant", text: "done" },
    ];
    expect(finalizeStreamingMessages(initial)).toBe(initial);
  });
});
