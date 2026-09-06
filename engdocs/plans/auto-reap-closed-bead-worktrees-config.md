# Auto-Reap Closed Bead Worktrees Config Plan

Root bead: `ga-xxsd7k`
Architecture source: `ga-uq92xy`
Design source: `gascity/designer`, completed 2026-06-02

## Goal

Expose a daemon-level opt-out for automatic closed-bead worktree cleanup and
wire the controller tick to run the cleanup phase when enabled. The default is
on, preserving automated cleanup while allowing operators to retain worktrees
for diagnostics.

## Child Beads

| Order | Bead | Route | Title |
| --- | --- | --- | --- |
| 1 | `ga-xxsd7k.1` | `gascity/builder` | As an operator, I can enable or disable closed-bead worktree auto-reaping |
| 2 | `ga-xxsd7k.2` | `gascity/validator` | Validate daemon config schema and runtime gate for closed-bead worktree auto-reaping |

Dependency graph:

- `ga-xxsd7k.1` depends on `ga-plhh3l.1`
- `ga-xxsd7k.2` depends on `ga-xxsd7k.1`

## Acceptance Summary

The work is complete when:

- `DaemonConfig.AutoReapClosedBeadWorktrees *bool` exists with TOML key
  `auto_reap_closed_bead_worktrees` and JSON schema default `true`.
- `AutoReapClosedBeadWorktreesEnabled()` returns true when unset and returns
  the configured bool value when set.
- `CityRuntime.tick()` runs `reap_closed_bead_worktrees` after the stale
  session bead reap phase when enabled.
- The phase records `TraceSiteControllerTickPhase` with field `reaped`.
- `make dashboard-check` passes and generated schema artifacts are in sync.

## Out Of Scope

- No `config.Agent`, `AgentPatch`, or `AgentOverride` changes.
- No requirement for a user-configured agent role to drive the cleanup.
- No new health-patrol decision heuristics.

## Risks

- The call site must align with the event payload work from `ga-plhh3l`.
- Schema generation must be run after adding the daemon config field.
