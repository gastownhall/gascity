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
  constructs provider instances both long-lived (the controller's
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
instances probe, while long-lived instances (the controller's reconcile
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
