#!/usr/bin/env node
// Mayor Chat — Ink terminal UI for interactive conversation with a
// containerized Claude Code agent.
//
// Uses the Agent SDK V1 query() + resume for multi-turn conversation.
// Each turn spawns a Claude Code subprocess; --resume maintains context.
//
// Usage (inside container):
//   node chat.mjs
//
// Usage (from local tmux pane, via SSH + docker exec):
//   ssh namurim docker exec -it mayor-chat node /app/chat.mjs

import React, { useState, useCallback, useRef, useEffect } from "react";
import { render, Box, Text, useInput, useApp, useStdout } from "ink";
import TextInput from "ink-text-input";
import Spinner from "ink-spinner";
import { MayorTransport } from "./transport.mjs";

// ── Message types for the UI ──────────────────────────────────────

// { role: "user", text: "..." }
// { role: "assistant", text: "...", streaming: bool }
// { role: "tool", name: "Bash", input: "ls -la", result: "..." }
// { role: "system", text: "..." }
// { role: "result", subtype: "success", cost: 0.05, turns: 3 }

// ── Main Chat Component ──────────────────────────────────────────

function Chat() {
  const [messages, setMessages] = useState([]);
  const [input, setInput] = useState("");
  const [busy, setBusy] = useState(false);
  const [status, setStatus] = useState("ready");
  const [sessionId, setSessionId] = useState(null);
  const transportRef = useRef(null);
  const { exit } = useApp();
  const { stdout } = useStdout();

  // Initialize transport on first render.
  if (!transportRef.current) {
    transportRef.current = new MayorTransport();
  }

  // Calculate visible height for message scrolling.
  const maxVisible = (stdout?.rows ?? 24) - 6; // reserve for header + input + padding

  const handleSubmit = useCallback(async (value) => {
    const trimmed = value.trim();
    if (!trimmed) return;

    if (trimmed === "/quit" || trimmed === "/exit") {
      if (transportRef.current) transportRef.current.close();
      exit();
      return;
    }

    setInput("");
    setBusy(true);
    setStatus("thinking...");

    // Add user message.
    setMessages((prev) => [...prev, { role: "user", text: trimmed }]);

    // Add placeholder for assistant response.
    const assistantIdx = { current: -1 };
    setMessages((prev) => {
      assistantIdx.current = prev.length;
      return [...prev, { role: "assistant", text: "", streaming: true }];
    });

    // Track accumulated text for streaming deltas.
    let accumulatedText = "";

    try {
      for await (const msg of transportRef.current.send(trimmed)) {
        // Capture session ID from any message.
        if (msg.session_id && !sessionId) {
          setSessionId(msg.session_id);
        }

        // Stream events: incremental text deltas (token by token).
        if (msg.type === "stream_event" && msg.event?.type === "content_block_delta") {
          const delta = msg.event.delta;
          if (delta?.type === "text_delta" && delta.text) {
            accumulatedText += delta.text;
            setMessages((prev) => {
              const updated = [...prev];
              if (assistantIdx.current >= 0 && assistantIdx.current < updated.length) {
                updated[assistantIdx.current] = {
                  role: "assistant",
                  text: accumulatedText,
                  streaming: true,
                };
              }
              return updated;
            });
          }
        }

        // Complete assistant message (final text + tool calls).
        if (msg.type === "assistant" && msg.message?.content) {
          let text = "";
          const tools = [];

          for (const block of msg.message.content) {
            if (block.type === "text") {
              text += block.text;
            } else if (block.type === "tool_use") {
              tools.push({
                role: "tool",
                name: block.name,
                input: typeof block.input === "string"
                  ? block.input
                  : JSON.stringify(block.input, null, 2),
              });
            }
          }

          // Use the complete text (may differ from accumulated deltas).
          accumulatedText = text;
          setMessages((prev) => {
            const updated = [...prev];
            if (assistantIdx.current >= 0 && assistantIdx.current < updated.length) {
              updated[assistantIdx.current] = {
                role: "assistant",
                text,
                streaming: true,
              };
            }
            const existingTools = updated.filter((m) => m.role === "tool").length;
            for (let i = existingTools; i < tools.length; i++) {
              updated.push(tools[i]);
            }
            return updated;
          });
        }

        if (msg.type === "result") {
          // Finalize the assistant message.
          setMessages((prev) => {
            const updated = [...prev];
            if (assistantIdx.current >= 0 && assistantIdx.current < updated.length) {
              updated[assistantIdx.current] = {
                ...updated[assistantIdx.current],
                streaming: false,
              };
            }
            // Add result summary.
            updated.push({
              role: "result",
              subtype: msg.subtype,
              cost: msg.total_cost_usd,
              turns: msg.num_turns,
            });
            return updated;
          });
        }
      }
    } catch (err) {
      setMessages((prev) => [
        ...prev,
        { role: "system", text: `Error: ${err.message}` },
      ]);
    }

    setBusy(false);
    setStatus("ready");
  }, [exit]);

  useInput((ch, key) => {
    if (key.ctrl && ch === "c") {
      if (transportRef.current) transportRef.current.close();
      exit();
    }
  });

  // Slice messages to fit visible area.
  const visible = messages.length > maxVisible
    ? messages.slice(-maxVisible)
    : messages;

  return React.createElement(
    Box,
    { flexDirection: "column", paddingX: 1 },

    // ── Header ──
    React.createElement(
      Box,
      { borderStyle: "single", paddingX: 1, justifyContent: "space-between" },
      React.createElement(Text, { bold: true, color: "cyan" }, "Mayor Chat"),
      React.createElement(
        Text,
        { dimColor: true },
        busy
          ? React.createElement(
              React.Fragment,
              null,
              React.createElement(Spinner, { type: "dots" }),
              " ",
              status
            )
          : sessionId
            ? `session: ${sessionId.slice(0, 8)}...`
            : "new session"
      )
    ),

    // ── Messages ──
    React.createElement(
      Box,
      { flexDirection: "column", marginY: 1, flexGrow: 1 },
      visible.length === 0
        ? React.createElement(
            Text,
            { dimColor: true },
            "Type a message to start chatting with the mayor. /quit to exit."
          )
        : visible.map((msg, i) => React.createElement(MessageRow, { key: i, msg }))
    ),

    // ── Input ──
    React.createElement(
      Box,
      null,
      React.createElement(
        Text,
        { color: busy ? "gray" : "green", bold: true },
        busy ? "  " : "> "
      ),
      busy
        ? React.createElement(Text, { dimColor: true }, "waiting for response...")
        : React.createElement(TextInput, {
            value: input,
            onChange: setInput,
            onSubmit: handleSubmit,
          })
    )
  );
}

// ── Message Row Component ────────────────────────────────────────

function MessageRow({ msg }) {
  switch (msg.role) {
    case "user":
      return React.createElement(
        Box,
        { marginBottom: 1 },
        React.createElement(Text, { color: "green", bold: true }, "You: "),
        React.createElement(Text, null, msg.text)
      );

    case "assistant":
      return React.createElement(
        Box,
        { flexDirection: "column", marginBottom: 1 },
        React.createElement(
          Box,
          null,
          React.createElement(Text, { color: "cyan", bold: true }, "Mayor: "),
          msg.streaming && !msg.text
            ? React.createElement(Spinner, { type: "dots" })
            : null
        ),
        msg.text
          ? React.createElement(
              Box,
              { marginLeft: 2 },
              React.createElement(Text, null, msg.text)
            )
          : null
      );

    case "tool":
      return React.createElement(
        Box,
        { flexDirection: "column", marginLeft: 2, marginBottom: 1 },
        React.createElement(
          Text,
          { color: "yellow" },
          `▶ Tool: ${msg.name}`
        ),
        msg.input
          ? React.createElement(
              Box,
              { marginLeft: 2 },
              React.createElement(
                Text,
                { dimColor: true },
                msg.input.length > 200
                  ? msg.input.slice(0, 200) + "..."
                  : msg.input
              )
            )
          : null
      );

    case "result":
      return React.createElement(
        Text,
        { dimColor: true },
        `[${msg.subtype}, turns: ${msg.turns ?? "?"}, cost: $${msg.cost?.toFixed(4) ?? "?"}]`
      );

    case "system":
      return React.createElement(
        Text,
        { color: "red" },
        msg.text
      );

    default:
      return null;
  }
}

// ── Entry Point ──────────────────────────────────────────────────

render(React.createElement(Chat));
