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
it), the store would go down with the agent. So a gc command run from inside
an agent session starts the managed Dolt server in a transient scope of its
own:

```
systemd-run --user --scope --slice=${GC_INFRA_SLICE:-gascity-infra.slice} --collect --quiet \
  --unit=gascity-infra-<random>.scope --description="gascity infra: managed dolt for city <path>" -- <command>
```

gc wraps the spawn only when both hold:

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
   with every scope in it, when the user's last session ends. Without
   lingering, a caller in a login session's scope can outlive logout
   (`KillUserProcesses=no`) while a user scope would not, so gc leaves its
   spawn where it is. A caller already under the user manager dies with it
   anyway.

| Spawn | Wrapped from a session |
|---|---|
| Managed Dolt server (or its scope watchdog, whose server inherits its scope) | yes |
| `gc supervisor start` fork, and the fork `gc start` falls back to | no |
| Supervisor respawn after `gc start` detects drift | no |
| The nudge poller and bd's db-proxy child | no |
| The bd pack's shell-native Dolt fallback (`gc-beads-bd.sh` with no `gc` helper resolved) | no: out of scope |

The supervisor is never wrapped, not even when forked from a session: every
session it starts would follow it into the infra slice. A supervisor forked
from a session stays in that session's cgroup, as before.

gc ships no unit for the infra slice. `systemd-run` creates it implicitly,
with no resource limits and no memory protection (`MemoryLow=0`), and
systemd removes it again when it is empty. A store moved there leaves the
agent's cgroup and its limits, but nothing reserves memory for it. Defining
a `gascity-infra.slice` unit (or naming another slice in `GC_INFRA_SLICE`)
is up to the operator and outside what gc configures.

Unlike `GC_AGENT_SLICE`, scoping is on by default: `GC_INFRA_SLICE` only
names the slice. When it cannot be used (condition 2 does not hold (no
lingering and the caller is outside `user@<uid>.service`); no
`systemd-run`; no user bus; macOS) gc checks once per process (the
`systemd-run` probe is bounded at 5 seconds) and starts the server
unwrapped exactly as before, warning only when `GC_INFRA_SLICE` is set.

### Finding a scoped server after it exits

A scope has no restart policy, and `--collect` removes the unit once its
processes exit, so `systemctl --user status` has nothing to show for a
scoped server that died. What survives:

- The unit name, recorded with the server's PID in the managed Dolt state
  (`infra_scope` in `.gc/runtime/packs/dolt/dolt-state.json` and
  `dolt-provider-state.json`). When the store is not reachable, `gc doctor`'s
  `dolt-server` check names that scope and the commands below.
- The user manager's journal records for the scope, found by its unit name
  (`journalctl --user-unit=gascity-infra-<random>.scope`) or, for every
  infra scope, by its description (`journalctl -g 'gascity infra:'`). The
  unit name is random per spawn, so a restart never collides with a scope
  still being torn down.
- The server's own output, which goes to the managed `dolt.log` as for an
  unscoped server.

On a host that keeps no per-user journal files, the user manager's records
land only in the system journal, so reading them needs journal read access
(membership in the `adm` or `systemd-journal` group). `journalctl -g` also
needs a journalctl built with PCRE2; `--user-unit` does not.

## Resource attribution

Agent pane scopes (`GC_AGENT_SLICE`) are created with systemd auto-generated
names (`run-rNNNNNNNN.scope`); infra scopes are named `gascity-infra-<random>.scope`
and carry a `gascity infra:` description. For agent pane scopes,
`systemd-cgls --user` shows anonymous units under the slice rather than
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
