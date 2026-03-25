// Gas City hooks for OpenCode.
// Installed by gc into {workDir}/.opencode/plugins/gascity.js
//
// Events:
//   session.created    → gc prime (load context)
//   session.compacted  → gc prime (reload after compaction)
//   session.deleted    → gc hook --inject (pick up work on exit)
//   chat.system.transform → gc nudge drain --inject + gc mail check --inject + gc transcript check --inject

const { execSync } = require("child_process");

function run(cmd) {
  try {
    return execSync(cmd, { encoding: "utf-8", timeout: 30000 }).trim();
  } catch {
    return "";
  }
}

module.exports = {
  name: "gascity",

  events: {
    "session.created": () => run("gc prime --hook"),
    "session.compacted": () => run("gc prime --hook"),
    "session.deleted": () => run("gc hook --inject"),
  },

  hooks: {
    "experimental.chat.system.transform": (system) => {
      const nudges = run("gc nudge drain --inject");
      const mail = run("gc mail check --inject");
      const transcript = run("gc transcript check --inject");
      const extras = [nudges, mail, transcript].filter(Boolean);
      if (extras.length > 0) {
        return system + "\n\n" + extras.join("\n\n");
      }
      return system;
    },
  },
};
