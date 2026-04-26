// Gas City hooks for OpenCode.
// Installed by gc into {workDir}/.opencode/plugins/gascity.js
//
// OpenCode's plugin API is ESM and hook-oriented:
//   - event() is side-effect-only (no prompt injection)
//   - experimental.chat.system.transform mutates output.system
//   - experimental.session.compacting → inject context before compaction
//
// Gas City uses:
//   - session.created / session.compacted → gc prime --hook (side effects such
//     as session-id persistence and poller bootstrap)
//   - session.deleted → gc hook --inject (pick up newly queued work on exit)
//   - experimental.session.compacting → gc handoff "context cycle" (preserve
//     context across compaction by sending handoff mail and restarting)
//   - experimental.chat.system.transform → inject gc prime --hook, queued
//     nudges, and unread mail into the system prompt for each turn

import { execFile } from "node:child_process";
import { promisify } from "node:util";

const execFileAsync = promisify(execFile);
const PATH_PREFIX =
  `/opt/homebrew/bin:/usr/local/bin:${process.env.HOME}/go/bin:${process.env.HOME}/.local/bin:`;

async function run(directory, ...args) {
  try {
    const { stdout } = await execFileAsync("gc", args, {
      cwd: directory,
      encoding: "utf-8",
      timeout: 30000,
      env: { ...process.env, PATH: PATH_PREFIX + (process.env.PATH || "") },
    });
    return stdout.trim();
  } catch {
    return "";
  }
}

export default async function gascityPlugin({ directory }) {
  let cachedPrime = null;

  async function readPrime(force = false) {
    if (force || cachedPrime === null) {
      cachedPrime = await run(directory, "prime", "--hook");
    }
    return cachedPrime;
  }

  function prependText(existing, prefix) {
    return existing ? prefix + "\n\n" + existing : prefix;
  }

  async function buildPrefix() {
    const prime = await readPrime();
    const nudges = await run(directory, "nudge", "drain", "--inject");
    const mail = await run(directory, "mail", "check", "--inject");
    return [prime, nudges, mail].filter(Boolean).join("\n\n");
  }

  return {
    event: async ({ event }) => {
      switch (event.type) {
        case "session.created":
        case "session.compacted":
          await readPrime(true);
          return;
        case "session.deleted":
          await run(directory, "hook", "--inject");
          return;
        default:
          return;
      }
    },

    "chat.message": async (_input, output) => {
      const prefix = await buildPrefix();
      if (prefix) {
        output.message.system = prependText(output.message.system, prefix);
      }
    },

    "experimental.chat.system.transform": async (_input, output) => {
      const prefix = await buildPrefix();
      if (prefix) {
        if (output.system[0]) {
          output.system[0] = prependText(output.system[0], prefix);
        } else {
          output.system.unshift(prefix);
        }
      }
    },

    // Pre-compaction hook: send handoff mail and request restart to preserve
    // context across compaction. This mirrors Claude's PreCompact behavior.
    "experimental.session.compacting": async (_input, _output) => {
      // Run gc handoff which sends mail to self and requests restart.
      // The handoff mail preserves context that would otherwise be lost
      // during compaction. The restart ensures the session gets fresh
      // context with the handoff mail injected.
      await run(directory, "handoff", "context cycle");
    },
  };
}
