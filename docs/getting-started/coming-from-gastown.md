---
title: Coming from Gas Town
description: Map Gas Town's roles, mechanisms, layout, commands, and workflows onto Gas City's primitives.
---

If you have run Gas Town, you already know its roles, its `~/gt/...` layout, and its `gt` commands. This page carries that knowledge across to Gas City.

Gas City is the platform that machinery was extracted into. Two things changed. First, the orchestrator hardcodes **zero roles** — every role you knew is now configuration, and you express Gas Town (or any orchestration) on top of a few primitives. Second, and bigger: the orchestrator can now run a formula as a **graph across many agents, out of your session** — decomposing a job into beads, fanning the ready ones out in parallel, gating each step on its dependencies, and retrying failures to completion. Your single-agent, in-session formulas still run (v1); this fleet orchestration (v2) is what's new. Because it is a platform, a feature added to Gas City lifts *every* orchestrator built on it — Gas Town included.

For the system-level mental model first, read [How Gas City Works](/getting-started/how-gas-city-works).

## Mapping tables

The five tables map Gas Town onto Gas City one domain at a time. Every entry on the right is **configuration** — a user-configured agent plus a prompt template — not a built-in platform type.

### Roles

| Gas Town role | What it did | Gas City equivalent |
|---|---|---|
| **Mayor** | Planner/coordinator; the human's point of contact | Configured agent + coordinating prompt (e.g. the Gastown pack's `mayor`). Reach it with `gc session attach mayor`. |
| **Deacon** | Watchdog: stall detection, restart, SLA enforcement | Orchestrator health patrol + config thresholds; optionally a configured agent. You tune thresholds, not run a role. |
| **Witness** | Lifecycle observer; publishes health/transition events | Events + waits, formulas, session scale config. Modeling a "witness" on top is optional pack behavior. |
| **Refinery** | Post-processor; reshapes raw agent output | Configured agent + a formula or order post-processing step. A workflow step, not a standing role. |
| **Polecat** | Ephemeral on-demand worker, often in a worktree | Scalable/transient agent config (a pool — `min`/`max_active_sessions`). An operating *style*. |
| **Crew** | Persistent worker pool claiming from a queue | Persistent named agent config. An operating *style*. |
| **Dog** | Integration / external-messaging relay | Core-pack exec orders (most relay work needs no LLM); the Gastown pack's `dog` pool covers work that does need an agent. |

### Mechanisms and behaviors

*What those roles and features actually **do**, and where that logic now lives.*

| Gas Town behavior | Gas City equivalent | Notes |
|---|---|---|
| Deacon watchdog logic | Orchestrator health patrol + reconciliation | Stall detection, restart-with-backoff, reconcile-to-desired-state are orchestrator concerns, not a role agent. |
| Witness lifecycle tracking | Waits, formulas, session scale config, orchestrator wake/sleep, events | Mechanisms are first-class; modeling a "witness" on them is optional. |
| Plugin (scheduled/event/conditional automation) | Order — exec order or formula order | **Exec order** for shell or orchestrator-side logic; **formula order** to instantiate agent-driven work. |
| Convoy as orchestration runtime | Convoy beads + `gc sling` + formulas | Convoys stay bead-backed grouping and lineage; no special convoy runtime to use. |
| Formula runner inside Town workflows | In-process formula compiler + orchestrator execution | Gas City compiles and runs formulas over the convoy's beads itself. For v2 formulas (host-enabled by default), the orchestrator executes control beads; agents execute work beads. See [Choosing a Compiler Contract](/guides/understanding-formulas#choosing-a-compiler-contract). |
| Path-derived identity | Explicit agent identity, rig scope, env, bead metadata | Do not port code or prompts that assume directory path implies who the agent is. |

### Filesystem and state

*Gas Town encodes architecture into directories; Gas City treats directories as an implementation detail.*

| Gas Town location | Gas City equivalent | Notes |
|---|---|---|
| `~/gt/...` directory tree | City directory + `.gc/` runtime state | A city is a directory holding `city.toml` and `.gc/`. Rigs are registered in `city.toml`; each `[[rigs]]` has a `path` defaulting under `rigs/` but allowed anywhere (absolute or city-relative). Live state is queried, not read from a fixed home tree. |
| Town config | `pack.toml` (reusable behavior) + `city.toml` (this city's deployment) | One town config splits along a definition/deployment seam. |
| Rig config | `city.toml` `[[rigs]]` entries + `.gc/` (machine-local path bindings) | *Which* rigs and their scale is deployment; *where* a rig lives on this machine is a local binding. |
| Role homes | `agents/<name>/` (`agent.toml` + `prompt.template.md`) in the root city pack or an imported pack | Only the agent *definition* lives here. No on-disk role "home"; identity is not path-derived. |
| Role home dirs (e.g. `~/gt/mayor/`) | `dir` (identity scope) + `work_dir` (session working dir, only when needed) | Set both in `agent.toml` (or patch per-rig in `city.toml`): `dir` carries scope/identity; `work_dir` only when a role truly needs filesystem isolation. |
| Role-specific startup files | Prompt templates, overlays, provider hooks, `pre_start`, `session_setup`, `gc prime` | Startup shaping is explicit and provider-aware, not inferred from disk location. |

### Workflows

*The operator verbs — the things you actually type. (Formulas are a mechanism, above; these are day-to-day moves.)* The transcripts below show them end to end.

| Gas Town workflow | Gas City command | Deeper |
|---|---|---|
| Spin up a worker | `gc start` + a persistent agent config (`agents/<name>/`) | [Tutorial 02 — Agents](/tutorials/02-agents), [Shareable Packs](/guides/shareable-packs) |
| Send a task to the mayor | `gc sling mayor "<description>"` (or `bd create` + a bead hook) | [`gc sling`](/reference/cli#gc-sling), [Tutorial 06 — Beads](/tutorials/06-beads) |
| Inspect what's stuck | `gc session list`, then `gc session peek <name>` | [`gc session list`](/reference/cli#gc-session-list) |
| Restart a stalled agent | `gc session reset <name>` (or let health patrol auto-restart) | [`gc session reset`](/reference/cli#gc-session-reset) |
| Share config across teams | A shareable pack (`pack.toml` + `agents/<name>/`), imported by each city | [Shareable Packs](/guides/shareable-packs) |
| Run a one-shot job | A formula or exec order, dispatched on demand | [Tutorial 07 — Orders](/tutorials/07-orders), [Tutorial 05 — Formulas](/tutorials/05-formulas) |
| Watch live agent output | `gc session attach <name>` (interactive) or `gc session peek <name>` (snapshot) | [`gc session attach`](/reference/cli#gc-session-attach) |

A first work cycle — start the city, hand the mayor a task, watch it land:

```bash
gc start                                  # boots the city under the supervisor, reconciles
gc sling mayor "add a /health endpoint"   # creates a task bead from text, routes it to mayor
gc session list                           # see what's running (mayor, pools, …)
gc session attach mayor                    # interactive live view of the mayor working
```

Triage a stuck agent without attaching:

```bash
gc session list                  # find the suspect by name/state
gc session peek mayor --lines 80 # snapshot of recent output (default 50 lines)
gc session reset mayor           # fresh restart; bead, alias, mail, and queued work survive
gc events --follow               # system-wide live feed (or let health patrol act on its own)
```

<Note>
`gc session peek` takes `--lines` for a point-in-time snapshot — there is no `--follow`. For a continuously updating view, `gc session attach`. For the system-wide live feed, `gc events --follow`.
</Note>

### Commands

The full `gt` → `gc`/`bd` mapping lives in the **[Gas Town → Gas City Command Map](/reference/gastown-command-map)**.

## What usually maps cleanly

**Roles become pack agents.** Adding a role follows an escalation ladder — stop at the first rung that solves your problem:

```text
edit local city.toml  →  include a pack that already solves most of it
  →  override the stamped agent (local-only change)
  →  edit the pack (change the shared default for everyone)
  →  add formulas/orders for workflow automation
```

**Start with the root city pack plus `city.toml`, not an imported pack.** Edit a pack only when the change should become the reusable default for every consumer.

```toml
# pack.toml      — imports reusable packs, defines city-specific behavior
# agents/<name>/ — city-owned named agents
# city.toml      — deployment: rigs, substrates, scale
# .gc/           — site bindings such as local rig paths
```

**Plugins become orders** — the most important practical translation. "Run something automatically on a schedule, on an event, or when a condition holds" is an order: **exec order** for shell/orchestrator-side logic, **formula order** for agent-driven work. Exec orders matter most — they run non-agent commands with no prompt, no session, no extra role agent.

**Convoys stay bead-shaped.** Keep the convoy mental model for tracking work; the implementation boundary moved. Convoys are bead-backed grouping and lineage, `gc sling` creates convoy structure while routing, and formulas/orders/waits compose around that bead graph. The orchestration that runs over it is the orchestrator's control dispatcher — it executes the control beads (check, retry, fan-out, tally, drain) that drive a convoy's work to completion across many agents.

**Crew and polecats are operating modes, not types.** *Crew* = persistent named agents; *polecats* = scalable or transient agents, often with worktrees. The platform does not force the distinction — a pack can adopt, relax, or replace it.

## Where Gas City deliberately differs

**The orchestrator owns infrastructure behavior.** It is the canonical owner of reconciling desired→running sessions, session scaling, order evaluation, health patrol, and garbage-collecting ephemeral run beads (the v1 *wisp* container). If something is fundamentally platform infrastructure, put it on the orchestrator path rather than inventing another deacon-like role.

**Filesystem layout is not the architecture.** Use `dir` (in `agent.toml`, or patched per-rig in `city.toml`) for scope and identity; `work_dir` only when the session must run elsewhere; bead metadata for durable handoff state.

| Use a separate `work_dir` when… | Not when… |
|---|---|
| the role mutates a repo and needs an isolated worktree | "Gas Town has a separate folder for this role" |
| provider scratch files would collide with another role | |
| the role needs a durable sandbox independent from the rig root | |

**Roles are examples, not platform law.** The Gastown pack ships familiar roles as an example operating model, not a type system. Adding a behavior means editing a pack, formula, order, or prompt — not adding a hardcoded role. A **local city change** edits `city.toml` (rig overrides, patches, a city-specific agent); a **shared product change** edits the pack for a better default everywhere. Most onboarding work is local.

## Common translation patterns

| Old Town instinct | Ask first | Default answer |
|---|---|---|
| "I need a new dog" | Can this be an exec order? | Prefer the order — trigger logic, history, orchestrator ownership, no agent slot. Reach for a scalable agent only if it needs a long-lived session, rich interactive context, or repeated agent judgment. |
| "I need a witness-like lifecycle manager" | Which parts are orchestrator infra vs. bead transitions vs. formula logic vs. prompt guidance? | Only orchestrator infrastructure belongs in platform code; the rest lives in the pack. |
| "I need another special directory tree" | Do I really? | Canonical repo root from the rig; isolated `work_dir` only for roles that mutate repos or need provider-file isolation; explicit env and metadata, never path inference. |
| "I need to run something without an agent" | Could an exec order do it? | Use an exec order before inventing a plugin, helper role, or hidden session. |

### "How do I get to my mayor?"

```bash
gc session attach mayor
```

The Mayor session is the familiar Gas Town entry point — an interactive session with full city context that you coordinate from. It is one window onto the city; the orchestrator is still running the fleet and driving formula graphs to completion behind it. The CLI is plumbing for reaching that session. City-scoped Gastown agents (`mayor`, `deacon`, `boot`) attach the same way; `gc session list` shows what is running. This replaces `gt session at mayor/` or `tmux attach -t gt-mayor`.

## What not to port literally

These Town habits create unnecessary complexity in Gas City:

- exact `~/gt/...` directory trees
- path-derived identity
- new hardcoded role names in platform code
- plugin systems when an order is enough
- special helper agents for work that is really a shell command
- duplicating durable state outside beads when labels or metadata suffice

The most common mistake is importing Town's surface area instead of re-expressing the intent in Gas City's primitives.

## Editing Gastown config

The common edits — registering rigs, scaling pools, swapping providers, patching agents, tweaking prompts — live in [Gastown on Gas City: Config Recipes](/guides/gastown-config-recipes).

## Fast ramp checklist

The shortest path to effective:

1. Read [How Gas City Works](/getting-started/how-gas-city-works) for the six primitives in user terms.
2. Skim the [CLI reference](/reference/cli) alongside the [Command Map](/reference/gastown-command-map) so `gt` → `gc` muscle memory transfers.
3. Read [Tutorial 07 — Orders](/tutorials/07-orders) and remap "plugins" → "orders".
4. Read [Tutorial 05 — Formulas](/tutorials/05-formulas): Gas City compiles and instantiates formulas itself; for v2 formulas the orchestrator drives control beads while agents execute work beads.
5. Work through [Tutorial 02 — Agents](/tutorials/02-agents) and [Shareable Packs](/guides/shareable-packs) for the `agents/<name>/` layout end to end.
6. Read [A Complete Gastown Example](/guides/gastown-config-recipes#a-complete-gastown-example) — city, root pack, and nested pack assembled into one runnable topology.
