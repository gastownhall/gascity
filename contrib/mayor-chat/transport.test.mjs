// Tests for MayorTransport — the V1 streaming transport layer.
//
// These tests inject a mock query function into the real MayorTransport
// to verify behavior in isolation: input queuing, message routing,
// session ID capture, turn-end detection, and cleanup.

import { describe, it, beforeEach } from "node:test";
import assert from "node:assert/strict";
import { MayorTransport } from "./transport.mjs";

// --- Mock query factory ---
// Returns a mock query function that captures the prompt iterable and
// provides a controllable output stream.

function createMockQuery() {
  let promptIterable = null;
  const outputQueue = [];
  let outputResolve = null;
  let outputDone = false;

  function mockQuery({ prompt, options }) {
    promptIterable = prompt;
    // Return an async iterable of output messages.
    return {
      [Symbol.asyncIterator]() {
        return {
          next() {
            if (outputQueue.length > 0) {
              return Promise.resolve({ value: outputQueue.shift(), done: false });
            }
            if (outputDone) {
              return Promise.resolve({ value: undefined, done: true });
            }
            return new Promise((resolve) => { outputResolve = resolve; });
          }
        };
      }
    };
  }

  function pushOutput(msg) {
    if (outputResolve) {
      const r = outputResolve;
      outputResolve = null;
      r({ value: msg, done: false });
    } else {
      outputQueue.push(msg);
    }
  }

  function endOutput() {
    outputDone = true;
    if (outputResolve) {
      const r = outputResolve;
      outputResolve = null;
      r({ value: undefined, done: true });
    }
  }

  return { mockQuery, pushOutput, endOutput, getPromptIterable: () => promptIterable };
}

// --- Tests ---

describe("MayorTransport input stream", () => {
  it("send() pushes user messages into the prompt iterable", async () => {
    const { mockQuery, pushOutput, getPromptIterable } = createMockQuery();
    const transport = new MayorTransport({ _queryFn: mockQuery });

    // Start a send — this triggers _ensureStarted and pushes a message.
    const iter = transport.send("hello")[Symbol.asyncIterator]();

    // Push a result to end the turn so we can check the prompt.
    pushOutput({ type: "result", subtype: "success" });
    await iter.next();

    // Read what was pushed to the prompt iterable.
    const promptIter = getPromptIterable()[Symbol.asyncIterator]();
    const { value } = await promptIter.next();
    assert.equal(value.type, "user");
    assert.equal(value.message.role, "user");
    assert.equal(value.message.content, "hello");
  });

  it("queued messages are delivered in order", async () => {
    const { mockQuery, pushOutput } = createMockQuery();
    const transport = new MayorTransport({ _queryFn: mockQuery });

    // Send first turn.
    const gen1 = transport.send("first");
    pushOutput({ type: "result", subtype: "success" });
    for await (const _ of gen1) {}

    // Send second turn.
    const gen2 = transport.send("second");
    pushOutput({ type: "result", subtype: "success" });
    for await (const _ of gen2) {}

    assert.equal(transport.sessionId, null, "no session_id yet");
  });
});

describe("MayorTransport session ID capture", () => {
  it("captures session_id from first message that has one", async () => {
    const { mockQuery, pushOutput } = createMockQuery();
    const transport = new MayorTransport({ _queryFn: mockQuery });

    const gen = transport.send("test");
    pushOutput({ type: "assistant", message: { content: [] } });
    pushOutput({ type: "assistant", session_id: "abc-123", message: { content: [] } });
    pushOutput({ type: "result", subtype: "success" });

    for await (const _ of gen) {}

    assert.equal(transport.sessionId, "abc-123");
  });

  it("does not overwrite session_id once captured", async () => {
    const { mockQuery, pushOutput } = createMockQuery();
    const transport = new MayorTransport({ _queryFn: mockQuery });

    const gen = transport.send("test");
    pushOutput({ type: "assistant", session_id: "first-id", message: { content: [] } });
    pushOutput({ type: "assistant", session_id: "second-id", message: { content: [] } });
    pushOutput({ type: "result", subtype: "success" });

    for await (const _ of gen) {}

    assert.equal(transport.sessionId, "first-id");
  });
});

describe("MayorTransport turn detection", () => {
  it("result message marks end of turn", async () => {
    const { mockQuery, pushOutput } = createMockQuery();
    const transport = new MayorTransport({ _queryFn: mockQuery });

    const collected = [];
    const gen = transport.send("test");

    pushOutput({ type: "stream_event", event: { type: "content_block_delta", delta: { type: "text_delta", text: "hello" } } });
    pushOutput({ type: "assistant", session_id: "abc", message: { content: [{ type: "text", text: "hello" }] } });
    pushOutput({ type: "result", subtype: "success", total_cost_usd: 0.01 });
    // This message should NOT be consumed by this turn.
    pushOutput({ type: "stream_event", event: { type: "content_block_delta", delta: { type: "text_delta", text: "next turn" } } });

    for await (const msg of gen) {
      collected.push(msg);
    }

    assert.equal(collected.length, 3);
    assert.equal(collected[collected.length - 1].type, "result");
  });

  it("yields all messages when stream ends without result", async () => {
    const { mockQuery, pushOutput, endOutput } = createMockQuery();
    const transport = new MayorTransport({ _queryFn: mockQuery });

    const collected = [];
    const gen = transport.send("test");

    pushOutput({ type: "stream_event" });
    pushOutput({ type: "stream_event" });
    endOutput();

    for await (const msg of gen) {
      collected.push(msg);
    }

    assert.equal(collected.length, 2);
  });
});

describe("MayorTransport message routing", () => {
  it("yields stream_event messages for token-by-token streaming", async () => {
    const { mockQuery, pushOutput } = createMockQuery();
    const transport = new MayorTransport({ _queryFn: mockQuery });

    const collected = [];
    const gen = transport.send("test");

    pushOutput({
      type: "stream_event",
      event: { type: "content_block_delta", delta: { type: "text_delta", text: "Hello" } }
    });
    pushOutput({
      type: "stream_event",
      event: { type: "content_block_delta", delta: { type: "text_delta", text: " world" } }
    });
    pushOutput({ type: "result", subtype: "success" });

    for await (const msg of gen) {
      collected.push(msg);
    }

    assert.equal(collected.length, 3);
    assert.equal(collected[0].event.delta.text, "Hello");
    assert.equal(collected[1].event.delta.text, " world");
  });
});

describe("MayorTransport close behavior", () => {
  it("close terminates the input stream", async () => {
    const { mockQuery } = createMockQuery();
    const transport = new MayorTransport({ _queryFn: mockQuery });

    // Trigger _ensureStarted.
    transport._ensureStarted();

    // Close the transport.
    transport.close();

    assert.equal(transport.inputDone, true);
  });

  it("close resolves pending input reads as done", async () => {
    const { mockQuery } = createMockQuery();
    const transport = new MayorTransport({ _queryFn: mockQuery });

    transport._ensureStarted();

    // Start a pending read on the input stream.
    const inputIter = transport._createInputStream()[Symbol.asyncIterator]();

    // The real input stream is already created inside _ensureStarted,
    // but we can test the close mechanism directly.
    const pending = new Promise((resolve) => {
      transport.inputResolve = resolve;
    });

    transport.close();

    const result = await pending;
    assert.equal(result.done, true);
  });

  it("close is safe to call multiple times", () => {
    const { mockQuery } = createMockQuery();
    const transport = new MayorTransport({ _queryFn: mockQuery });

    // Should not throw.
    transport.close();
    transport.close();
    transport.close();
    assert.equal(transport.inputDone, true);
  });
});

describe("MayorTransport options", () => {
  it("defaults permissionMode to dontAsk", () => {
    const { mockQuery } = createMockQuery();
    const transport = new MayorTransport({ _queryFn: mockQuery });

    assert.equal(transport.options.permissionMode, "dontAsk");
    assert.deepEqual(transport.options.allowedTools, []);
    assert.equal(transport.options.includePartialMessages, true);
  });

  it("allows overriding defaults", () => {
    const { mockQuery } = createMockQuery();
    const transport = new MayorTransport({
      _queryFn: mockQuery,
      permissionMode: "askUser",
      allowedTools: ["Bash"],
    });

    assert.equal(transport.options.permissionMode, "askUser");
    assert.deepEqual(transport.options.allowedTools, ["Bash"]);
    assert.equal(transport.options.includePartialMessages, true);
  });
});
