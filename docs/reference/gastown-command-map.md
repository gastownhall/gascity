---
title: Gas Town → Gas City Command Map
description: The closest `gc` or `bd` equivalent for each `gt` command, for operators migrating from Gas Town.
---
This is a closest-match map: the `gt` and `gc` CLIs have non-identical architectures, so each row gives the nearest `gc` or `bd` equivalent.

Two rules help a lot to find the closest home (`gc` or `bd`) for a command equivalent:

- the closest home is `gc` if the `gt` command is about _orchestration, sessions, routing, hooks, or runtime behavior_ 
- the closest home is `bd` if the `gt` command is really about _bead CRUD or bead content_.

See the [`gc` CLI reference](/reference/cli) and the [`bd` CLI reference](https://gastownhall.github.io/beads/cli-reference) for the full CLI surfaces, and [The six primitives](/getting-started/how-gas-city-works) for the canonical model these commands map onto.

## Workspace And Runtime

| `gt` | Closest in Gas City | Notes |
|---|---|---|
| `gt install` | `gc init` | Gas City uses `gc init` to create a city. |
| `gt init` | `gc rig add` or `gc init` | In Gas Town, `gt install` and `gt init` set up the workspace and its projects. Gas City splits those duties differently — `gc init` creates the city, `gc rig add` registers a repo as a rig — so `gt init` maps to whichever half you were doing. |
| `gt rig` | `gc rig` | Near-direct mapping. |
| `gt start` | `gc start` | Starts the city under the machine-wide supervisor. |
| `gt up` | `gc start` | Same high-level intent. |
| `gt down` | `gc stop` | Stops the city's agent sessions (graceful interrupt, then force-kill); also stops Dolt and clears orphans. |
| `gt shutdown` | `gc stop` | Collapses onto the same `gc stop` as `gt down`; use `--force` for an immediate kill instead of the graceful wave. |
| `gt daemon` | `gc supervisor` | Supervisor is the canonical long-running runtime in Gas City. |
| `gt status` | `gc status` | City-wide overview. |
| `gt dashboard` | `gc dashboard` | Same general purpose; `gc dashboard serve` still exists as the explicit form. |
| `gt doctor` | `gc doctor` | Near-direct mapping. |
| `gt config` | `gc config` plus editing `city.toml` | Gas City config is file-first; `gc config` is mostly inspect/explain. |
| `gt disable` | `gc suspend` | Closest operational match is per-city suspension, not a system-wide Town toggle. |
| `gt enable` | `gc resume` | Resumes a suspended city. |
| `gt uninstall` | no direct equivalent | Gas City has supervisor install/uninstall, but not a Town-style global uninstall command. |
| `gt version` | `gc version` | Direct mapping. |
| `gt completion` | no direct equivalent | Gas City does not currently expose a matching completion command. |
| `gt help` | `gc help` | Direct mapping. |
| `gt info` | `gc version`, `gc status`, docs | No single `gc info` command. |
| `gt stale` | no direct equivalent | Closest checks are `gc version` and `gc doctor`. |
| `gt town` | split across `gc start`, `gc status`, `gc stop`, `gc supervisor` | Gas City does not keep a separate Town namespace. |

## Configuration And Extension

| `gt` | Closest in Gas City | Notes |
|---|---|---|
| `gt git-init` | `git init` plus `gc rig add` | Git repo setup and city registration are separate concerns in Gas City. |
| `gt hooks` | config-driven hook install plus `gc doctor` | Gas City does not have Town's hook-management namespace; hook install is primarily config and lifecycle driven. |
| `gt plugin` | `gc order` | Plugin-like orchestrator automation usually becomes an exec order or formula order. |
| `gt issue` | no direct equivalent | Usually replaced by bead metadata or session context, depending intent. |
| `gt account` | no direct equivalent | Provider account management is outside Gas City's core CLI. |
| `gt shell` | no direct equivalent | Gas City does not ship a Town-style shell integration namespace. |
| `gt theme` | no direct equivalent | Pack scripts or tmux config are the normal path. |

## Work Routing And Workflow

| `gt` | Closest in Gas City | Notes |
|---|---|---|
| `gt sling` | `gc sling` | Direct mapping in spirit and name. |
| `gt handoff` | `gc handoff` | Near-direct mapping. |
| `gt convoy` | `gc convoy` | Near-direct mapping for convoy creation and tracking. |
| `gt hook` | `gc hook` | Same name, narrower surface: `gc hook` is work-query and hook injection behavior, not the full Town hook manager. |
| `gt ready` | `bd ready` | This stays bead-centric more than city-centric. |
| `gt done` | no single direct equivalent | In Gas City this is usually a bead close, metadata transition, convoy action, or formula step. |
| `gt unsling` | no direct equivalent | Usually replaced by bead edits plus re-routing with `bd` and `gc sling`. |
| `gt formula` | `gc formula list/show/cook/version-check`, `gc sling --formula`, `gc order` | `gc formula` manages formulas (list, show, cook, version-check — `version-check` detects whether the on-disk formula file changed after a bead was cooked from it). `gc sling --formula` dispatches a v2 formula as a workflow (a v1 formula materializes as a wisp). |
| `gt mol` | `gc formula cook`, `bd mol ...` | `gc formula cook` creates workflows (v2) (or molecules under the v1 materialization); `bd` handles bead-level operations. |
| `gt mq` | no direct generic `gc` command | Gastown-style merge queue behavior lives in the pack and formulas, not a generic platform namespace. |
| `gt gate` | `gc wait`; formula `[steps.gate]` | Durable waits are the closest platform concept. Formulas also carry gates natively — a step-level `[steps.gate]` (types such as `gh:run`, `gh:pr`, `timer`, `human`, `mail`) or `[[compose.gate]]` rules — the closer match when migrating molecule workflows. See [Steps](/reference/specs/formula-spec-v2#13-steps). |
| `gt park` | `gc wait` | Same underlying idea: stop and resume around a dependency or gate. |
| `gt resume` | `gc wait ready`, `gc session wake`, `gc mail check` | Depends on whether the action is a parked wait, sleeping session, or handoff/mail resume. |
| `gt synthesis` | partial: `gc converge`, formulas, convoys | No one-command parity. |
| `gt orphans` | no direct generic command | In Gas City this is usually pack logic plus witness/refinery formulas and bead inspection. |
| `gt release` | mostly `bd` state edits | No single `gc release` command. |

## Sessions, Roles, And Agents

| `gt` | Closest in Gas City | Notes |
|---|---|---|
| `gt agents` | `gc session` plus `gc status` | Session management is generic in Gas City; not a Town-specific agent switcher. |
| `gt session` | `gc session` | Same broad idea, but not polecat-specific. |
| `gt crew` | `city.toml` agents plus `gc session` | Crew is a pack convention, not a first-class platform command family. |
| `gt polecat` | Gastown pack `polecat` agent plus `gc status` / `gc session` / `gc sling` | No dedicated top-level platform namespace. |
| `gt witness` | Gastown pack `witness` agent plus `gc session` / `gc status` | No dedicated top-level platform namespace. |
| `gt refinery` | Gastown pack `refinery` agent plus `gc session` / `gc status` | No dedicated top-level platform namespace. |
| `gt mayor` | Gastown pack `mayor` agent plus `gc session attach mayor` / `gc status` | Managed as a configured agent, not a baked-in command family. |
| `gt deacon` | Gastown pack `deacon` agent plus `gc session`, `gc status`, orchestrator behavior | In Gas City, much of what deacon does lives in the orchestrator/supervisor. |
| `gt boot` | Gastown pack `boot` agent | Same pattern as other role agents. |
| `gt dog` | usually `gc order`, sometimes a pack-owned `dog` pool | Dog-like helpers are exec orders shipped by the builtin core pack, or a pack-owned dog pool (e.g. the Gastown pack's `dog`). |
| `gt role` | `gc config explain`, `gc session list`, prompt/config inspection | Role is not a first-class platform concept. |
| `gt callbacks` | no direct equivalent | Callback behavior is folded into runtime, hooks, waits, and orders. |
| `gt cycle` | no direct generic command | Closest equivalents are tmux bindings or pack-specific session UX. |
| `gt namepool` | config-only today | Gas City supports namepool files in config, but does not expose a top-level `gc namepool` command. |
| `gt worktree` | `work_dir`, `pre_start`, `git worktree`, pack scripts | Worktree behavior is explicit config and script wiring, not a generic `gc worktree` namespace. |

## Communication And Nudges

| `gt` | Closest in Gas City | Notes |
|---|---|---|
| `gt mail` | `gc mail` | Near-direct mapping. |
| `gt nudge` | `gc session nudge` | Use `gc session nudge <target> "msg"` to send messages to a live session. The `gc nudge` subcommand only exposes deferred-delivery controls (`drain`, `status`, `poll`); it does not accept a positional `<target> "msg"` form. |
| `gt peek` | `gc session peek` | Near-direct mapping. |
| `gt broadcast` | no single direct equivalent | Usually modeled as `gc mail send` to a group or multiple explicit targets. |
| `gt notify` | no direct equivalent | Notification policy is not a top-level platform command family. |
| `gt dnd` | no direct equivalent | Closest behavior usually lives in mail or local workflow policy. |
| `gt escalate` | no direct equivalent | Model escalations with beads, mail, orders, or pack-specific workflow. |
| `gt whoami` | no direct equivalent | Identity is explicit in config, session metadata, and `GC_*` env rather than a dedicated CLI. |

## Beads, Events, And Diagnostics

| `gt` | Closest in Gas City | Notes |
|---|---|---|
| `gt bead` | mostly `bd` | Bead CRUD is still primarily the bead tool's job. |
| `gt cat` | mostly `bd` | Same rule: bead content inspection is bead-centric. |
| `gt show` | mostly `bd` | Use the bead tool for detailed bead state/content. |
| `gt close` | mostly `bd close` | Still bead-centric. |
| `gt commit` | `git commit` | Gas City does not wrap commit the way Town does. |
| `gt activity` | `gc event emit` and `gc events` | Same basic event/logging space. |
| `gt trail` | `gc events`, `gc session peek`, `gc session logs` | No one-command parity. |
| `gt feed` | `gc events` | Closest live system feed. |
| `gt log` | `gc events` or `gc supervisor logs` | Depends on whether you want event history or runtime logs. |
| `gt audit` | partial: `gc events`, `gc graph`, `bd` queries | No single audit namespace equivalent. |
| `gt checkpoint` | no direct equivalent | Session durability lives in the runtime and bead/session model rather than a user-facing checkpoint CLI. |
| `gt patrol` | no direct equivalent | Patrol behavior is generally modeled with orders plus formulas. |
| `gt migrate-agents` | `gc migration` | Same general migration/upgrade bucket. |
| `gt prime` | `gc prime` | Direct mapping. |
| `gt costs` | no direct equivalent | No matching top-level cost accounting command today. |
| `gt seance` | no direct equivalent | Gas City has resume and session metadata, but not a seance command. |
| `gt thanks` | no direct equivalent | No matching command. |

## Practical Translation Rule

If you are unsure where a `gt` command went, ask this in order:

1. Is it now just `gc` with nearly the same name?
2. Is it really a bead operation that should stay in `bd`?
3. Is it no longer a special command because Gas City moved that behavior into config, orders, waits, formulas, or orchestrator logic?
