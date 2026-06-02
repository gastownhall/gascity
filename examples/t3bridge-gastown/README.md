# T3 Bridge Gas Town Example

This example shows the smallest Gas City configuration needed to run an
existing Gas Town pack through the native `t3bridge` session provider.

The example does not fork the Gas Town pack. It imports
`../gastown/packs/gastown` so T3Code sees the same resolved agents, named
sessions, patches, pool settings, and suspend/on-demand controls that GC would
use normally.

## Sidebar-Relevant Configuration

T3Code's Gas City sidebar should be driven by the resolved `city.toml`, not by
T3-specific pack copies.

- `[session].provider = "t3bridge"` makes GC create sessions through T3Code.
- `[workspace].provider = "codex"` remains the default agent runtime provider.
- `[imports.gastown]` exposes city-scoped pack agents and patches to the
  workspace section of the sidebar.
- `[[rigs]]` defines the rig row T3Code should display.
- `[rigs.imports.gastown]` exposes rig-scoped agents, pools, and named sessions
  under that rig.
- Pack-defined `[[named_session]]`, `[[patches.agent]]`, pool bounds, suspend,
  `always`, and `on_demand` state should flow through GC's resolved config API
  unchanged.

If a T3Code sidebar action changes an agent mode, suspension state, or rig
binding, it should update the real runtime `city.toml` using the same GC config
mutation path as the CLI/API.

## Run It

From a T3Code fork with the bridge support:

```sh
cd /data/projects/t3code
bun dev
```

If the T3Code server does not publish runtime state, export the WebSocket URL:

```sh
export T3_WS_URL=ws://127.0.0.1:3773/ws
```

From this repository:

```sh
go install ./cmd/gc
tmp="$(mktemp -d)"
cp -R examples/t3bridge-gastown "$tmp/city"
```

Edit `$tmp/city/city.toml` and point the sample rig at a real checkout:

```toml
[[rigs]]
name = "gascity"
path = "/data/projects/gascity"
```

Then start the city:

```sh
gc start "$tmp/city"
gc status "$tmp/city"
gc session list --city "$tmp/city"
```

Expected behavior:

- GC creates sessions through T3Code, not tmux.
- T3Code sidebar groups the workspace and rig from resolved `city.toml`.
- Sidebar controls reflect the imported Gas Town pack's agents and modes.
- T3Code threads receive GC metadata and startup envelope fields.
