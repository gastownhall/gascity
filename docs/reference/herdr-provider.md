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
backend. That selector is city-wide (`[session] provider`) or, for a one-off
run, process-wide (`GC_SESSION`); herdr cannot be selected for individual
agents.

### City default

Set the session provider in `city.toml`:

```toml
[session]
provider = "herdr"
```

Every agent the city starts runs under herdr, except agents on the ACP transport
(whether pinned `session = "acp"` or because their provider defaults to ACP),
which route to the separate ACP backend instead (see below).

### Per-agent / per-rig

herdr cannot be selected for individual agents. The runtime backend is chosen
city-wide by `[session] provider` (above) or process-wide by `GC_SESSION`
(below); no patch puts an agent onto herdr when the city default is something
else.

The per-agent patch field is `session`, but it selects a **transport**, not a
backend. It accepts only `acp`, `tmux`, or omission (`IsValidSessionTransport`
in `internal/config/provider.go`), so `session = "herdr"` never selects the
herdr runtime. Config validation flags it as a warning:

```text
agent "dog-1": session "herdr" is not a valid session transport (use "acp", "tmux", or omit)
```

Under a herdr city, the transport router (`internal/runtime/auto`) sends only
ACP-registered sessions to the separate ACP backend and routes everything else
to the city's base provider, which is herdr. Two consequences follow:

- `session = "acp"` (or a provider that defaults to ACP) moves that agent off
  herdr, onto the ACP backend. It is the one per-agent lever that changes which
  backend an agent runs on.
- `session = "tmux"` does not keep an agent on tmux. The herdr provider does not
  implement the transport-capability check, so the pin is neither honored nor
  rejected; the agent falls back to the base provider and runs on herdr. To put
  an agent on tmux, the whole city (or process) must default to tmux.

### Environment (one-off)

For a quick local trial without editing config, export the selector:

```bash
export GC_SESSION=herdr
gc start <city>
```

`GC_SESSION` overrides the effective provider name for that process, the same
way it selects `exec:<script>` or any other backend.

## Piloting safely

herdr is opt-in, and the backend is a whole-city choice, so you pilot it by
scoping which city runs on herdr, not by pinning individual agents. Recommended
path:

1. **Try it per-process on a scratch city.** Select herdr with the environment
   variable on a throwaway city, so nothing is committed and your real city is
   untouched:

   ```bash
   GC_SESSION=herdr gc start <scratch-city>
   ```

   Every agent in that process runs under herdr, except agents on the ACP
   transport (whether pinned `session = "acp"` or because their provider
   defaults to ACP), which still route to the separate ACP backend. Watch it
   through a normal work cycle.

2. **Promote to the scratch city's default.** Once the per-process trial looks
   good, set the default in that city's `city.toml` and run it end to end:

   ```toml
   [session]
   provider = "herdr"
   ```

3. **Widen** to your real city by flipping its `[session] provider` to
   `"herdr"`, once the scratch city has been stable across several work cycles.
   The switch is city-wide with no way to move agents over one at a time, so
   keep any city you are not ready to migrate on the tmux default.

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
