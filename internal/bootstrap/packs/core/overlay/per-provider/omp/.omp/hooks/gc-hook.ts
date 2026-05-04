// Gas City hooks for Oh My Pi (OMP).
// Installed by gc into {workDir}/.omp/hooks/gc-hook.ts
//
// Events:
//   session.created    → gc prime (load context)
//   session.compacted  → gc prime (reload after compaction)
//   chat.system.transform → gc nudge drain --inject + gc mail check --inject

import { execSync } from "child_process";

const PATH_PREFIX =
  `${process.env.HOME}/go/bin:${process.env.HOME}/.local/bin:`;

function run(cmd: string): string {
  try {
    return execSync(cmd, {
      encoding: "utf-8",
      timeout: 30000,
      env: { ...process.env, PATH: PATH_PREFIX + (process.env.PATH || "") },
    }).trim();
  } catch {
    return "";
  }
}

export default {
  name: "gascity",

  events: {
    "session.created": () => run("gc prime --hook"),
    "session.compacted": () => run("gc prime --hook"),
  },

  hooks: {
    "experimental.chat.system.transform": (system: string): string => {
      const nudges = run("gc nudge drain --inject");
      const mail = run("gc mail check --inject");
      const extras = [nudges, mail].filter(Boolean);
      if (extras.length > 0) {
        return system + "\n\n" + extras.join("\n\n");
      }
      return system;
    },
  },
};
