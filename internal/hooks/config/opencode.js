// Gas City hooks for OpenCode.
// Installed by gc into {workDir}/.opencode/plugins/gascity.js
//
// OpenCode's plugin API is ESM and hook-oriented:
//   - event() is side-effect-only (no prompt injection)
//   - experimental.chat.system.transform mutates output.system
//
// Gas City uses:
//   - session.created / session.compacted → gc prime --hook (side effects such
//     as session-id persistence and poller bootstrap)
//   - experimental.chat.system.transform → inject gc prime --hook, queued
//     nudges, and unread mail into the system prompt for each turn

import { execFile } from "node:child_process";
import { promisify } from "node:util";

const execFileAsync = promisify(execFile);
const PATH_PREFIX =
  `${process.env.HOME}/go/bin:${process.env.HOME}/.local/bin:`;

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
  async function buildPrefix() {
    const prime = await run(directory, "prime", "--hook");
    const nudges = await run(directory, "nudge", "drain", "--inject");
    const mail = await run(directory, "mail", "check", "--inject");
    return {
      prime,
      nudges,
      mail,
      extras: [prime, nudges, mail].filter(Boolean),
    };
  }

  return {
    event: async ({ event }) => {
      switch (event.type) {
        case "session.created":
        case "session.compacted":
          await run(directory, "prime", "--hook");
          return;
        default:
          return;
      }
    },

    "chat.message": async (_input, output) => {
      const { prime, nudges, mail, extras } = await buildPrefix();
      if (extras.length > 0) {
        const prefix = extras.join("\n\n");
        output.message.system = output.message.system
          ? prefix + "\n\n" + output.message.system
          : prefix;
      }
    },

    "experimental.chat.system.transform": async (_input, output) => {
      const { prime, nudges, mail, extras } = await buildPrefix();
      if (extras.length > 0) {
        const prefix = extras.join("\n\n");
        output.system.unshift(prefix);
        if (output.system[1]) {
          output.system[1] = prefix + "\n\n" + output.system[1];
        }
      }
    },
  };
}
