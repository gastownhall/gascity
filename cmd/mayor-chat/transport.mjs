// Transport layer: V1 streaming input mode for persistent multi-turn
// conversation with token-by-token streaming.
//
// Uses query() with an AsyncIterable prompt source. This keeps a single
// Claude Code process alive and yields stream_event messages with
// incremental text deltas as tokens are generated.

import { query } from "@anthropic-ai/claude-agent-sdk";

export class MayorTransport {
  constructor(options = {}) {
    this.sessionId = null;
    this.query = null;
    this.messageStream = null;
    this.inputResolve = null;
    this.inputQueue = [];
    this.inputDone = false;
    this.options = {
      permissionMode: options.permissionMode ?? "dontAsk",
      allowedTools: options.allowedTools ?? [],
      includePartialMessages: true,
      ...options,
    };
  }

  // Create a push-based async iterable for the query's prompt source.
  _createInputStream() {
    const self = this;
    return {
      [Symbol.asyncIterator]() {
        return {
          next() {
            if (self.inputQueue.length > 0) {
              return Promise.resolve({ value: self.inputQueue.shift(), done: false });
            }
            if (self.inputDone) {
              return Promise.resolve({ value: undefined, done: true });
            }
            return new Promise((resolve) => { self.inputResolve = resolve; });
          }
        };
      }
    };
  }

  // Push a user message into the input stream.
  _pushMessage(text) {
    const msg = {
      type: "user",
      message: { role: "user", content: text },
    };
    if (this.inputResolve) {
      const r = this.inputResolve;
      this.inputResolve = null;
      r({ value: msg, done: false });
    } else {
      this.inputQueue.push(msg);
    }
  }

  // Lazily start the query on first send.
  _ensureStarted() {
    if (!this.query) {
      const inputStream = this._createInputStream();
      this.query = query({ prompt: inputStream, options: this.options });
      // Start consuming the query's output in background.
      // Messages are pulled by the send() caller via _readUntilTurnEnd().
      this.messageStream = this.query[Symbol.asyncIterator]();
    }
  }

  // Send a prompt and yield messages until the turn completes (result message).
  async *send(prompt) {
    this._ensureStarted();
    this._pushMessage(prompt);

    // Read messages until we see a result (turn complete).
    while (true) {
      const { value: msg, done } = await this.messageStream.next();
      if (done) break;

      // Capture session ID.
      if (msg.session_id && !this.sessionId) {
        this.sessionId = msg.session_id;
      }

      yield msg;

      // Result message marks end of this turn.
      if (msg.type === "result") break;
    }
  }

  close() {
    this.inputDone = true;
    if (this.inputResolve) {
      const r = this.inputResolve;
      this.inputResolve = null;
      r({ value: undefined, done: true });
    }
  }
}
