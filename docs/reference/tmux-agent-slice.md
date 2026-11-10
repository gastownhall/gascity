---
title: "Tmux Agent Slice (GC_AGENT_SLICE)"
---

Setting the `GC_AGENT_SLICE` environment variable to a systemd user slice
(for example `gascity-agents.slice`) makes the tmux session provider wrap
every pane's initial command in a transient systemd user scope:

```
systemd-run --user --scope --slice=<slice> --collect --quiet -- sh -c '<command>'
```

Default-off: when the variable is unset or empty, pane commands run
unwrapped exactly as before.

## Why

systemd-enabled tmux builds (stock Ubuntu) move every pane into a transient
`tmux-spawn-*.scope` under the default user slice, so agent processes escape
whatever slice the tmux server itself runs in. Wrapping the pane command
re-parents the agent's process tree into a dedicated user slice where
resource weights (`CPUWeight`, `MemoryHigh`, ...) can be applied to all
agents collectively.

## Scope and activation

- **Tmux provider only.** The subprocess and exec session providers spawn
  children of the gc process directly, so those already inherit gc's own
  cgroup; only tmux panes escape and need re-parenting.
- **Env var, not `city.toml`.** This is a host-level deployment knob, not
  per-city configuration: it is set by whatever supervises the gc process
  (a systemd unit, shell profile, or CI environment) and applies to every
  city served by that process. The slice it names is host systemd state
  that must exist on the user manager, outside any city's config layering.
- **Keep the value stable for the process lifetime.** The availability
  probe runs at most once per tmux provider instance, for the first
  non-empty value that instance sees; a value changed while gc is running
  is embedded in later wrapper commands without being re-probed. gc
  constructs provider instances both long-lived (the orchestrator's
  reconcile loop) and fresh per operation (template session starts), so a
  changed or repaired slice takes effect on some spawn paths and not
  others. Restart gc to converge every path on one verdict.

## Probe and fallback

Before its first wrapped spawn, each tmux provider instance probes
`systemd-run --user --scope --slice=<slice> --collect --quiet -- true`
(bounded at 5 seconds). If the probe fails — no `systemd-run` binary, no
reachable user manager, or an invalid slice — that instance logs one
warning and every pane command it spawns runs unwrapped:

```
tmux agent slice: GC_AGENT_SLICE="..." set but transient user scopes are unavailable; pane commands run unwrapped: ...
```

Because operations like template session starts construct fresh provider
instances, a persistently broken host repeats this warning as new
instances probe, while long-lived instances (the orchestrator's reconcile
loop) keep their first verdict until restart.

The probe runs in the gc process's environment, while pane commands execute
with the tmux server's environment. gc normally spawns the tmux server
itself, so the two match; if you point gc at a pre-existing tmux server
whose global environment lacks a reachable user bus (`XDG_RUNTIME_DIR`,
`DBUS_SESSION_BUS_ADDRESS`), wrapped spawns can fail even after a
successful probe. The systemd-run error is visible in the dead pane's
captured output in startup diagnostics.

## User-manager lifecycle coupling

Wrapped agents live under the user's `user@<uid>.service` manager. Ending
that user session — `loginctl terminate-user`, or logging out without
lingering — kills every agent scope. For unattended hosts, enable
lingering so the user manager (and the agents) survive logout:

```bash
loginctl enable-linger <user>
```

## City infrastructure (GC_INFRA_SLICE)

A cgroup-wide kill of an agent's scope kills everything in it. If that agent
started the city's managed Dolt server (a `gc` or `bd` call that autostarts
it) or the supervisor (`gc start`), the store or the supervisor would go down
with the agent. So a gc command run from inside an agent session starts city
infrastructure in a transient scope of its own:

```
systemd-run --user --scope --slice=${GC_INFRA_SLICE:-gascity-infra.slice} --collect --quiet -- <command>
```

gc wraps a spawn only when both hold:

1. **The gc process runs inside an agent session**: it carries the
   `GC_SESSION_ID` gc gives every session. Other gc processes (the
   supervisor, whether a bare fork or a systemd/launchd service; a CLI run
   from a login shell; the bd provider script the supervisor launches) have
   no session cgroup to escape, so their spawns stay exactly as before.
2. **A user scope cannot shorten the spawn's life**: the user lingers
   (`/var/lib/systemd/linger/<user>`, set by `loginctl enable-linger
   <user>`), or the gc process already runs under `user@<uid>.service` (for
   example in a `GC_AGENT_SLICE` pane scope). Scopes live under
   `user@<uid>.service`, which, unless the user lingers, systemd stops,
   with every scope in it, when the user's last session ends. Without lingering, a caller in a login
   session's scope can outlive logout (`KillUserProcesses=no`) while a user
   scope would not, so gc leaves its spawn where it is. A caller already
   under the user manager dies with it anyway.

| Spawn | Wrapped from a session |
|---|---|
| Managed Dolt server (or its scope watchdog, whose server inherits its scope) | yes |
| `gc supervisor start` fork, and the fork `gc start` falls back to | yes |
| Supervisor respawn after `gc start` detects drift | yes |
| The nudge poller and bd's db-proxy child | no |
| The bd pack's shell-native Dolt fallback (`gc-beads-bd.sh` with no `gc` helper resolved) | no: out of scope |

The supervisor is never told apart from other callers: it simply carries no
session identity. A supervisor forked from a session runs in an infra scope,
and the sessions it starts live there too unless their runtime gives them a
cgroup of their own (tmux with `GC_AGENT_SLICE` does), so limits an operator
sets on the infra slice apply to them.

Unlike `GC_AGENT_SLICE`, scoping is on by default: `GC_INFRA_SLICE` only
names the slice. When it cannot be used (condition 2 does not hold (no
lingering and the caller is outside `user@<uid>.service`); no
`systemd-run`; no user bus; macOS) gc checks once per process (the
`systemd-run` probe is bounded at 5 seconds) and starts infrastructure
unwrapped exactly as before, warning only when `GC_INFRA_SLICE` is set.

## Resource attribution

Scopes are created with systemd auto-generated names (`run-rNNNNNNNN.scope`),
so `systemd-cgls --user` shows anonymous units under the slice rather than
per-agent names. To attribute a scope to an agent session, list the
processes inside it:

```bash
systemd-cgls --user --unit <slice>
ps -o pid,args --forest -g <pid-from-cgls>
```

The agent command line (and its tmux session name, via `tmux list-panes -a
-F '#{session_name} #{pane_pid}'`) identifies the owner.

## Detection

A wrapped pane reports `pane_current_command = "systemd-run"` instead of
the agent process name. All gc liveness, zombie-cleanup, and pane-finding
paths handle this by walking pane process descendants, so health patrol
and nudge targeting behave the same with wrapping on or off.
