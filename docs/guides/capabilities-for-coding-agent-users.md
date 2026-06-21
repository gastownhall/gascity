---
title: Capabilities for Coding Agent Users
description: How context, state, skills, history, messaging, roles, and identity work in Gas City — mapped to what you already know from coding agents.
---

You know these capabilities from coding agents (Claude Code, Codex, Gemini
CLI, Cursor, …) as features of a single agent. In Gas City they are
infrastructure shared across many agents. Here is the quick map, ordered from
the most basic to the most multi-agent.

| Capability | Coding agents (Claude Code, Codex, …) | Gas City |
|---|---|---|
| Context | The window you fill: `CLAUDE.md`, open files, chat, … | An agent role, plus injected work items and mail (all beads) |
| State | The context window; persist by hand to files | **Beads** — durable, queried live |
| Skills | A `.claude/skills/<name>/` directory | The same directory, shared by scope (whole city, or one role) |
| History | Recorded session transcripts (resumable) + manual hand-off notes | Bead history + per-session logs + a city-wide event log |
| Messaging | None between distinct agent sessions; at most a communication mechanism between subagents in the same session | **Mail** (a bead) + **nudge** (wake a live session) |
| Roles | A subagent file (`.claude/agents/<name>.md`) | An agent folder (`agents/<name>/`) |
| Identity | The one session you're in | A stable name per running agent (`hello-world/gastown.polecat_furiosa`, …) |

The rest of this page is one short answer per capability.

## Context

**Definition**: the context window — everything the model currently sees.

**In a coding agent**:

- You fill it: `CLAUDE.md` (or `AGENTS.md`), the files you open, the running
  conversation, declared skills and MCP tools, …

**In Gas City**, the window is seeded automatically, per agent:

- It starts from the agent's **role** — a prompt template rendered with
  deployment data (city, rig, working directory, branch, custom variables).
- Two live sources then flow in as it works: its current **work items** and its
  **mail** — all [beads](/tutorials/06-beads).
- You don't hand-assemble context per agent; Gas City builds it from durable
  sources.

## State

**Definition**: what an agent knows between steps.

**In a coding agent**:

- Lives in the context window, but you can persist it by hand — writing notes to
  markdown files or committing to the repo — so it survives a fresh session.

**In Gas City**, durable state is first-class:

- Everything is a [**bead**](/tutorials/06-beads) — a stored work item with
  status, labels, relationships, and metadata.
- Beads are queried live (`bd`, `gc`), never tracked in status or lock files
  that go stale on a crash. Sessions come and go; the beads remain.

## Skills

**Definition**: a packaged capability — a **directory**, not a single file.

**In a coding agent**:

- In Claude Code it's `.claude/skills/<name>/`: a `SKILL.md` plus whatever it
  needs — scripts, reference docs, progressively-disclosed files the model loads
  on demand.

**In Gas City**, skills are materialized to agents by scope:

- Author a skill once, at the scope you want:
  - `skills/<name>/` at **pack level** → shared with **every** agent in the
    city.
  - `agents/<role>/skills/<name>/` at **role level** → only agents of that
    role (and all its pooled instances). On a name collision, the role-local
    skill wins.
- At startup Gas City **symlinks** the pack level and role level skill directories into
  each agent's provider-specific skill sink — `.claude/skills/`,
  `.agents/skills/` (codex), `.gemini/skills/`, `.opencode/skills/`. List with
  `gc skill list`.
- It *places* the files into each provider's own convention; it doesn't
  translate them. Providers whose convention isn't confirmed (copilot, cursor,
  pi, omp) are skipped for now.
- No framework *around* skills: no per-agent allow-lists. Within a scope every
  eligible agent gets every skill; the model decides when one applies.
- MCP is list-only today (`gc mcp list` shows what's catalogued; you wire the
  servers yourself).

## History

**Definition**: the record of what happened.

**In a coding agent**:

- Claude Code records each session's transcript and resumes it
  (`claude --continue` / `--resume`).
- History can be manually recorded by writing a hand-off markdown file, for
  instance before clearing the window so the next session picks up.

**In Gas City**, history is structured and queryable across every agent, not
per-session files you manage yourself:

- **Bead history** — the record that matters most. Every work item keeps its own
  create → update → close trail, independent of any session. The durable memory
  of *what was done*.
- **Session logs** — one agent's conversation (your prompts, the model's
  replies, its tool calls): `gc session logs <agent>` (`-f` to follow).
- **Event log** — an append-only, city-wide feed of system activity (sessions
  waking, mail sent, work created): `gc events`.

## Messaging

**Definition**: how agents communicate.

**In a coding agent**:

- None between distinct agent sessions. At most a communication mechanism
  between subagents in the same session.

**In Gas City**, two channels:

- **Mail** — durable. A message *is a [bead](/tutorials/06-beads)* (type
  `message`) with sender, recipient, subject, body. It survives crashes,
  threads, and waits in an inbox until read. Send with `gc mail`. Agents
  typically pull new mail into context each turn via a hook.
- **Nudge** — a direct poke to a live session: text typed straight into a
  running agent to wake or redirect it now. Not saved. Send with
  `gc session nudge <agent> "msg"`.

## Roles

**Definition**: the *kind* of agent — what it does.

**In a coding agent**:

- The closest thing is a **subagent** — in Claude Code a markdown file
  (`.claude/agents/<name>.md`) defining a named helper: description, tools,
  system prompt.

**In Gas City**, a role is a folder:

- `agents/<name>/` with an `agent.toml` (provider, pool, timeouts) and a
  `prompt.template.md` defining what that *kind* of agent does.

## Identity

**Definition**: the *specific running instance* you can address (vs. role, the
*kind*).

**In a coding agent**:

- Nothing to address: there's one session, and "who it is" is just the window
  you're in.

**In Gas City**:

- Each live agent has a stable, deterministic session name (e.g.
  `hello-world/gastown.polecat_furiosa`), so you — and other agents — can
  message, wake, peek at, and resume exactly that one across restarts.
- One role can be instantiated into many identities (a pool of
  `polecat_furiosa`, `polecat_nux`, …). See them with `gc session list`.

## See also

- [Coming from Gas Town](/getting-started/coming-from-gastown)
- [Tutorial 04: Communication](/tutorials/04-communication) — mail and nudge.
- [Config Reference](/reference/config)
