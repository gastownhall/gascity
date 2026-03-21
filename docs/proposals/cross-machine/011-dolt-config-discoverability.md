---
title: "Bug: Dolt Config Discoverability and Wiring"
type: satellite-issue
epic: 000-epic-cross-machine-city
status: confirmed
component: config, beads
current_state: broken
priority: highest
author: trillium
date: 2026-03-21
upstream_ref: "steveyegge/gastown#2830"
labels: [bug, config, dolt, discoverability]
---

# Bug: Dolt Config Discoverability and Wiring

## Parent Epic

[Epic: Cross-Machine City Operation](000-epic-cross-machine-city.md)

## Upstream Reference

This is the Gas City equivalent of [steveyegge/gastown#2830](https://github.com/steveyegge/gastown/issues/2830)
("Dolt server host configuration is hard to discover for remote setups").

The same problem exists in Gas City but is arguably worse — in Gastown, the config
existed but was undocumented. In Gas City, **the config is parsed but never consumed**.

## The Problem (User Experience)

When running Dolt on a separate machine (e.g., mini2 accessed over Tailscale), there
is no clear path to configuring the server host. The natural approach — adding
`[dolt]` to `city.toml` — silently does nothing.

### What a user tries

```toml
# city.toml
[dolt]
host = "mini2.hippo-tilapia.ts.net"
port = 3307
```

### What happens

Nothing. Agents connect to localhost and fail. The `[dolt]` values are parsed into
a `DoltConfig` struct and then ignored by all runtime code.

### What actually works (undocumented workaround)

```bash
export GC_DOLT_HOST=mini2.hippo-tilapia.ts.net
export GC_DOLT_PORT=3307
gc start
```

This works because `passthroughEnv()` forwards `GC_*` env vars to agent sessions.
But this is fragile — it doesn't survive new shells, non-zsh subprocesses, or
agent environments that don't source shell profiles.

## Root Cause Analysis

See [005 — Distributed Beads](005-distributed-beads.md) for the full technical trace.
In summary:

| Step | Status |
|------|--------|
| `[dolt]` TOML parsing | Works |
| Config fragment merging | Works |
| `cfg.Dolt` → runtime env vars | **Missing** |
| `readDoltPort()` checking config | **Missing** |
| `bdRuntimeEnv()` using config | **Missing** |
| `initBeadsForDir()` passing host/port | **Missing** |
| Skip managed Dolt when external host set | **Missing** |

## Gastown's Journey (for context)

From the upstream issue, the Gastown user (us) went through this painful path:

1. Manually patched `dolt.host` in 23 `.beads/config.yaml` files
2. Built an entire failover system (PR #2819) solving the wrong problem
3. Discovered `BEADS_DOLT_SERVER_HOST` existed in beads docs but was never
   referenced from gastown
4. Discovered `gt` propagated `BEADS_DOLT_PORT` but never `BEADS_DOLT_SERVER_HOST`
5. Discovered `daemon.json` had a `dolt_server.host` field that was invisible to users

The config-file solution existed the entire time but was undiscoverable. Gas City
has the same pattern: `DoltConfig` exists, is parsed, and is invisible at runtime.

## Discoverability Gaps in Gas City

| What exists | Where | Discoverability |
|-------------|-------|-----------------|
| `[dolt]` section in city.toml | `config.go:670` | Low — parsed but does nothing |
| `GC_DOLT_HOST` env var | `passthroughEnv()` | Very low — not documented |
| `GC_DOLT_PORT` env var | `passthroughEnv()` | Very low — not documented |
| `BdStore.Init()` host/port params | `bdstore.go:85` | Very low — only in Go source |
| `GC_K8S_DOLT_HOST` for K8s pods | `k8s/provider.go:648` | Medium — K8s-specific |

## Required Fix

1. **Wire `cfg.Dolt` to runtime** — convert `[dolt].host` and `[dolt].port` to
   `GC_DOLT_HOST` and `GC_DOLT_PORT` environment variables at controller startup
2. **Skip managed Dolt** — when `[dolt].host` is set to a non-local address, do not
   start the managed Dolt server
3. **`gc doctor` check** — detect when `[dolt]` is set but env vars don't match,
   or when agents are silently falling back to localhost
4. **Document the config** — add `[dolt]` examples to getting-started docs

## Dependencies

- [005 — Distributed Beads](005-distributed-beads.md) (full technical analysis)

## Dependents

- All cross-machine features depend on external Dolt working reliably
