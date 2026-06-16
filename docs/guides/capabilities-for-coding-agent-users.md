---
title: Coming from Coding Agents
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
| Identity | The one session you're in | A stable name per running agent (`hello-world/pack.worker_furiosa`, …) |

The rest of this page is one delta per capability — what changes when the
single-agent feature becomes shared infrastructure.

![Agents coordinate only through the store: mayor, reviewer, and worker each read and write the shared bead store (slinging work, claiming ready work, mailing a bead) — no arrow connects two agents directly. A nudge can wake a live session, but the work itself only ever moves through beads.](/diagrams/excalidraw-rendered/coordination-through-store.svg)

## Context

The window is seeded automatically, per agent, from durable sources — you never
hand-assemble it.

- It starts from the agent's **role**: a prompt template rendered with
  deployment data (city, rig, working directory, branch, custom variables).
- Its current **work items** and **mail** — all [beads](/tutorials/06-beads) —
  flow in live as it works.

## State

Durable state is first-class, not something you persist by hand.

Everything is a [**bead**](/tutorials/06-beads): a stored work item with status,
labels, relationships, and metadata, queried live (`bd`, `gc`) — never tracked
in status or lock files that go stale on a crash. Sessions come and go; the
beads remain.

## Skills

You author a skill once at a scope, and Gas City materializes it to every
eligible agent — no per-agent allow-lists, and the model decides when a skill
applies.

- Pick the scope:
  - `skills/<name>/` at **pack level** → shared with **every** agent in the
    city.
  - `agents/<role>/skills/<name>/` at **role level** → only agents of that role
    (and its pooled instances). On a name collision, the role-local skill wins.
- At startup Gas City **symlinks** both scopes into each agent's
  provider-specific skill sink — `.claude/skills/`, `.codex/skills/`,
  `.gemini/skills/`, `.opencode/skills/`. List with `gc skill list`.
- It *places* files into each provider's convention; it doesn't translate them.
  Providers whose convention isn't confirmed (copilot, cursor, pi, omp) are
  skipped for now.
- MCP is list-only today (`gc mcp list` shows what's catalogued; you wire the
  servers yourself).

## History

History is structured and queryable across every agent, not per-session files
you manage yourself.

| Layer | What it records | Read with |
|---|---|---|
| **Bead history** | Each work item's create → update → close trail, independent of any session — the durable memory of *what was done* | `bd`, `gc` |
| **Session logs** | One agent's conversation: your prompts, the model's replies, its tool calls | `gc session logs <agent>` (`-f` to follow) |
| **Event log** | An append-only, city-wide feed of system activity (sessions waking, mail sent, work created) | `gc events` |

## Messaging

Agents that share no session still communicate, over two channels.

| Channel | Durable? | What it is | Send with |
|---|---|---|---|
| **Mail** | Yes — a [bead](/tutorials/06-beads) (type `message`) | Sender, recipient, subject, body; threads and waits in an inbox until read. Agents typically pull new mail into context each turn via a hook | `gc mail` |
| **Nudge** | No | A direct poke into a live session — text typed straight into a running agent to wake or redirect it now | `gc session nudge <agent> "msg"` |

## Roles

A role is a folder, not a single subagent file.

`agents/<name>/` holds an `agent.toml` (provider, pool, timeouts) and a
`prompt.template.md` defining what that *kind* of agent does.

## Identity

A specific running instance has a stable name you can address — across
restarts.

- Each live agent has a deterministic session name (e.g.
  `hello-world/pack.worker_furiosa`), so you and other agents can message, wake,
  peek at, and resume exactly that one.
- One role instantiates into many identities (a pool of `worker_furiosa`,
  `worker_nux`, …). See them with `gc session list`.
- Role (the kind), identity (the running instance), and pool (the set) are all
  facets of the single **Agent** primitive — see
  [the six primitives](/getting-started/how-gas-city-works).

## See also

- [The six primitives](/getting-started/how-gas-city-works) — the canonical model; start here.
- [Coming from Gas Town](/getting-started/coming-from-gastown)
- [Tutorial 04: Communication](/tutorials/04-communication) — mail and nudge.
- [Config Reference](/reference/config)
