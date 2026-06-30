---
title: "herdr Session Provider"
---

[herdr](https://herdr.dev) is a terminal workspace manager built for AI coding
agents. Gas City ships a native **herdr** session-provider backend as an
**opt-in** alternative to tmux: one shared herdr session-server per city, one
workspace per rig (and one for the town), and one tab per agent. tmux stays the
default backend and the fallback — herdr is additive, selected through the same
runtime-selection setting that picks tmux, k8s, ssh, or exec.

## Prerequisites

Install the `herdr` binary and make sure it is on `PATH`:

```bash
herdr --version   # the provider is verified against herdr 0.7.1+
```

The backend is registered as a builtin runtime name (`herdr`) — no pack or
`[runtimes.*]` declaration is needed. If the binary is missing, sessions
selected onto herdr fail to start; install it before flipping the selector.

## Enabling herdr

`herdr` is selected with the same runtime selector used for every other
backend, at one of three scopes.

### City default

Set the session provider in `city.toml`:

```toml
[session]
provider = "herdr"
```

Every agent the city starts then runs under herdr, except agents pinned to
another backend by a patch (see below).

### Per-agent / per-rig

Override the backend for a single agent — or every agent in a rig — with an
agent patch. The override field is `session`:

```toml
# one agent by name
[[patches.agent]]
name = "dog-1"
session = "herdr"

# every agent in a rig (match by the rig's working dir)
[[patches.agent]]
dir = "webapp"
session = "herdr"
```

A per-agent `session` override wins over the `[session]` city default, so you
can run the whole city on herdr while pinning specific agents to tmux (or the
reverse — keep tmux as the default and pilot herdr on one agent).

### Environment (one-off)

For a quick local trial without editing config, export the selector:

```bash
export GC_SESSION=herdr
gc start <city>
```

`GC_SESSION` overrides the effective provider name for that process, the same
way it selects `exec:<script>` or any other backend.

## Piloting safely

herdr is opt-in precisely so you can roll it out gradually. Recommended path:

1. **Keep the mayor on tmux first.** Don't run the orchestrator on an
   experimental backend. If you flip the city default to herdr, pin the mayor
   back to tmux with a patch:

   ```toml
   [session]
   provider = "herdr"

   [[patches.agent]]
   name = "mayor"
   session = "tmux"
   ```

2. **Start with one low-stakes agent** — a dog, or a single rig's witness —
   by giving just that agent a `session = "herdr"` patch while the city default
   stays tmux. Watch it through a normal work cycle before widening.

3. **Widen** to a rig (`dir = "<rig>"`), then to the city default, once the
   pilot agents are stable.

## Applying and verifying

- Reload or restart the city to apply a selector change (`gc reload`, or restart
  the city). Agents launched after the switch run under herdr; already-running
  sessions keep their current backend until they next restart.
- Confirm the effective selector with `gc config show` (the `[session]`
  `provider` value, plus any agent `session` overrides).
- Once agents are on herdr, their workspaces and tabs are visible through
  herdr's own UI (`herdr` lists the per-rig/town workspaces and per-agent tabs).

## Layout

Within the single per-city herdr session-server, gc places agents so the
workspace/tab structure mirrors the town:

- **One workspace per rig**, plus one for the town (mayor and town-level
  agents).
- **One tab per agent.** Rig polecats land in their rig's workspace as
  `polecat-<themed-name>` tabs; the placement is display-only and does not
  change agent identity.

See `internal/runtime/herdr-provider-design.md` in the source tree for the
provider's design notes, capabilities, and pilot rationale.
