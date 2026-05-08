// Gas City hooks for Pi Coding Agent.
// Installed by gc into {workDir}/.pi/extensions/gc-hooks.js
//
// Pi 0.70+ extension API uses a factory function and pi.on(...)
// subscriptions. Keep this file as .js for existing Gas City provider args
// and auto-discovery paths.
//
// Events:
//   session_start    → gc prime --hook (load context side effects)
//   session_compact  → gc prime --hook (reload after compaction)
//   before_agent_start → gc nudge drain --inject + gc mail check --inject

const { execFileSync } = require("node:child_process");

const PATH_PREFIX = `${process.env.HOME}/go/bin:${process.env.HOME}/.local/bin:`;

function run(args, cwd) {
  try {
    return execFileSync("gc", args, {
      cwd: cwd || process.cwd(),
      encoding: "utf-8",
      timeout: 30000,
      env: { ...process.env, PATH: PATH_PREFIX + (process.env.PATH || "") },
    }).trim();
  } catch {
    return "";
  }
}

function appendSystemPrompt(systemPrompt, additions) {
  const extras = additions.filter(Boolean);
  if (extras.length === 0) {
    return systemPrompt;
  }
  return [systemPrompt, ...extras].filter(Boolean).join("\n\n");
}

module.exports = function gascityPiExtension(pi) {
  pi.on("session_start", (_event, ctx) => {
    run(["prime", "--hook"], ctx.cwd);
  });

  pi.on("session_compact", (_event, ctx) => {
    run(["prime", "--hook"], ctx.cwd);
  });

  pi.on("before_agent_start", (event, ctx) => {
    const nudges = run(["nudge", "drain", "--inject"], ctx.cwd);
    const mail = run(["mail", "check", "--inject"], ctx.cwd);
    const systemPrompt = appendSystemPrompt(event.systemPrompt, [nudges, mail]);
    if (systemPrompt !== event.systemPrompt) {
      return { systemPrompt };
    }
  });
};
