---
title: "Distributed Beads Store"
type: satellite-issue
epic: 000-epic-cross-machine-city
status: proposed
component: beads
current_state: declared-but-not-wired
priority: highest
author: trillium
date: 2026-03-21
labels: [beads, dolt, networking, data]
---

# Distributed Beads Store

## Parent Epic

[Epic: Cross-Machine City Operation](000-epic-cross-machine-city.md)

## Summary

The beads store looks like the closest component to working cross-machine — Dolt is a
network-accessible SQL database, and the BdStore implementation accepts remote connection
parameters. However, investigation reveals a critical wiring gap: **the `[dolt]` config
section is parsed but never consumed by any runtime code.**

## Current State: Declared But Not Wired

### The Config-to-Runtime Gap

The `city.toml` `[dolt]` section parses correctly into a `DoltConfig` struct
(`internal/config/config.go:670-678`):

```go
type DoltConfig struct {
    Port int    `toml:"port,omitempty" jsonschema:"default=0"`
    Host string `toml:"host,omitempty" jsonschema:"default=localhost"`
}
```

Config merging works — fragments can override `[dolt]` via last-writer-wins
(`internal/config/compose.go:233-238`). The struct is populated correctly.

**But zero code ever reads `cfg.Dolt`.** The values are parsed, merged, and then
ignored. Here's where the chain breaks:

```
city.toml [dolt]  →  config.DoltConfig (parsed ✓)  →  ??? (NOTHING)  →  bd CLI
                                                        ↑
                                                   THE GAP
```

### What Actually Happens at Runtime

| Step | Code Location | What It Does | Problem |
|------|---------------|--------------|---------|
| Config parsing | `config.go:670` | Parses `[dolt]` into struct | Works fine |
| Config merging | `compose.go:235` | Merges fragment overrides | Works fine |
| **Dolt port resolution** | `beads_provider_lifecycle.go:196` | `readDoltPort()` reads managed Dolt port file | **Ignores `cfg.Dolt.Port`** |
| **BD env construction** | `bd_env.go:26` | `bdRuntimeEnv()` builds env for bd CLI | **Ignores `cfg.Dolt.Host`** |
| **Beads init** | `beads/bdstore.go:85` | `BdStore.Init()` accepts host/port params | **Never called with config values** |
| **Env passthrough** | `cmd_start.go:772` | `passthroughEnv()` forwards `GC_*` vars | Only works if env vars already set |

### The Only Working Path (Environment Variables)

The only way to point Gas City at an external Dolt server today is via environment
variables set before `gc start`:

```bash
export GC_DOLT_HOST=mini3.hippo-tilapia.ts.net
export GC_DOLT_PORT=3307
gc start
```

This works because `passthroughEnv()` in `cmd/gc/cmd_start.go` forwards all `GC_*`
environment variables to agent sessions. But the `[dolt]` config values are never
converted to these environment variables.

### What's Broken (Detailed)

1. **`readDoltPort()` ignores config** (`beads_provider_lifecycle.go:196-206`):
   Only reads the ephemeral port file from a controller-managed Dolt server
   (`.gc/runtime/packs/*/dolt-state.json`). Never checks `cfg.Dolt.Port`.

2. **`bdRuntimeEnv()` ignores config** (`bd_env.go:26-43`):
   Builds the environment map for `bd` CLI calls using only the managed Dolt
   port. Never sets `GC_DOLT_HOST` from `cfg.Dolt.Host`.

3. **`initBeadsForDir()` ignores config** (`beads_provider_lifecycle.go:~170`):
   Calls `BdStore.Init()` without passing host/port, even though `Init()`
   accepts them.

4. **Managed Dolt startup not skipped**: When `[dolt].host` points to an external
   server, the controller should skip starting its own Dolt. There is no check
   for this.

### What Does Work

| Component | Status |
|-----------|--------|
| `[dolt]` TOML parsing | Works |
| Config fragment merging for `[dolt]` | Works |
| `BdStore.Init()` host/port parameters | Works (params exist, never called with values) |
| `GC_DOLT_*` env var passthrough to agents | Works (if set before `gc start`) |
| K8s Dolt config via `GC_K8S_DOLT_HOST/PORT` | Works |
| Network Dolt connectivity (bd CLI → remote Dolt) | Works (if env vars are correct) |

### Additional Gaps

5. **Single Dolt instance**: No replication or failover. If the Dolt server goes
   down, all agents (local and remote) lose bead access.

6. **No connection resilience**: BdStore calls `bd` CLI commands that connect per
   invocation. Network interruptions cause immediate failures with no retry.

7. **No cross-city bead sharing**: Each city has an independent store. Federation
   (reading beads from another city) is not supported.

8. **Latency considerations**: Every bead operation is a CLI invocation over the
   network. For high-frequency operations (hook checks, mail polling), this could
   be slow.

9. **Credential propagation**: Satellite machines need Dolt credentials. No
   mechanism to distribute these securely.

## What Needs to Happen

### Bug Fix: Wire `[dolt]` Config to Runtime (blocking)

The core fix is small — connect the parsed config to the runtime environment:

1. **In controller startup**: If `cfg.Dolt.Host` is set and not `localhost`/`127.0.0.1`,
   set `GC_DOLT_HOST` and `GC_DOLT_PORT` in the process environment and **skip
   starting the managed Dolt server**.

2. **In `bdRuntimeEnv()`**: Check `cfg.Dolt.Host`/`cfg.Dolt.Port` before falling
   back to the managed Dolt port file.

3. **In `initBeadsForDir()`**: Pass `cfg.Dolt.Host` and `cfg.Dolt.Port` to
   `BdStore.Init()` when set.

4. **In `readDoltPort()`**: Check config before reading the managed port file.
   Config should take precedence over managed Dolt.

### Minimum Viable Cross-Machine Beads

5. **Dolt bind address**: When hosting for remote machines, configure Dolt to
   listen on `0.0.0.0` or a specific interface instead of `localhost`
6. **Firewall/Tailscale**: Ensure Dolt port is accessible across Tailscale network

### Reliability Improvements

7. **Connection retry**: Wrap BdStore operations with retry logic for transient
   network failures
8. **Health check**: Include Dolt connectivity in doctor checks for remote machines
9. **Connection pooling**: Reduce per-operation connection overhead

### Future Enhancements

10. **Dolt replication**: Read replicas on satellite machines for lower latency
11. **Write-ahead cache**: Buffer bead writes locally, sync to hub asynchronously
12. **Federation**: Cross-city bead queries for multi-city setups

## city.toml Configuration

This is what the config should look like and what it should do:

```toml
# External Dolt server (on another machine)
[dolt]
host = "mini3.hippo-tilapia.ts.net"     # Remote hostname
port = 3307                             # Fixed port

# Effect: controller skips managed Dolt startup, sets GC_DOLT_HOST and
# GC_DOLT_PORT for all agent sessions, bd CLI connects to remote server.
```

```toml
# Managed Dolt server exposed to network (hub mode)
[dolt]
host = "0.0.0.0"                        # Bind to all interfaces
port = 3307                             # Fixed port (not ephemeral)

# Effect: controller starts Dolt bound to all interfaces on port 3307,
# remote agents can connect via the machine's Tailscale hostname.
```

## Workaround (works today)

Until the config wiring is fixed:

```bash
# On the machine running gc start:
export GC_DOLT_HOST=mini3.hippo-tilapia.ts.net
export GC_DOLT_PORT=3307
gc start path/to/city
```

This bypasses the `[dolt]` config entirely and uses the env var passthrough path.

## Risk Assessment

**The config wiring fix is low risk** — it connects existing, working pieces. The
network path works (proven by K8s pods using remote Dolt). The config parsing works.
The `bd` CLI respects `GC_DOLT_HOST/PORT`. The only missing piece is the glue in
between.

**Testing**: The env var workaround can validate the network path today without any
code changes. The config wiring fix can then be verified by removing the env vars
and using `[dolt]` config instead.

## Dependencies

- None (this is the most independent cross-machine component)

## Dependents

- [007 — Cross-Machine Mail](007-cross-machine-mail.md) (mail is stored in beads)
- [006 — Cross-Machine Events](006-cross-machine-events.md) (events could use beads as transport)
