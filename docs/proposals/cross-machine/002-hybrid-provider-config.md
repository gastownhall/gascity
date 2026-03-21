---
title: "Hybrid Provider Configuration"
type: satellite-issue
epic: 000-epic-cross-machine-city
status: proposed
component: runtime
current_state: minimal
priority: medium
author: trillium
date: 2026-03-21
labels: [runtime, hybrid, config]
---

# Hybrid Provider Configuration

## Parent Epic

[Epic: Cross-Machine City Operation](000-epic-cross-machine-city.md)

## Summary

The hybrid provider (`internal/runtime/hybrid/`) already exists as a routing layer that
dispatches operations to either a local or remote backend. However, it has no configuration
support — the routing function (`isRemote`) must be provided programmatically. This issue
tracks making it configurable via city.toml.

## Current State: Minimal Implementation

### What Exists

**`internal/runtime/hybrid/hybrid.go`** provides:

- `Hybrid` struct wrapping a `local` and `remote` provider
- All `runtime.Provider` methods delegated based on `isRemote(name string) bool`
- `ListRunning()` merges results from both backends
- Capability negotiation (intersection of local + remote)
- Best-effort error handling (returns first error encountered)

### What Works

```go
h := hybrid.New(localProvider, remoteProvider, func(name string) bool {
    return strings.HasPrefix(name, "remote-")
})
// All provider operations route correctly
```

### What's Missing

- No way to configure hybrid routing via city.toml
- No machine-aware routing (which agents go to which machines)
- No fallback behavior if remote is unavailable
- No multi-remote support (currently exactly one local + one remote)
- No runtime reconfiguration (routing function is set at construction)
- No metrics or observability on routing decisions

## Proposed Design

### city.toml Integration

```toml
[session]
provider = "hybrid"

[session.hybrid]
local_provider = "tmux"
default_machine = "mini2"              # hub machine

# Remote provider per satellite
[[session.hybrid.remote]]
machine = "mini3"
provider = "tmux"                       # tmux-over-ssh on remote
transport = "ssh"

[[session.hybrid.remote]]
machine = "k8s-cluster"
provider = "k8s"
```

### Agent-to-Machine Routing

With the machine registry (001), routing becomes:

```toml
[[agent]]
name = "polecat"
machines = ["mini2", "mini3"]           # from machine registry
```

The hybrid provider reads machine assignments and routes accordingly.

### Multi-Remote Extension

Current hybrid supports exactly two backends (local + remote). For N machines,
it needs to become a router over N providers:

```go
type MultiHybrid struct {
    providers map[string]runtime.Provider  // machine-name -> provider
    resolve   func(agentName string) string // agent -> machine
}
```

## Dependencies

- [001 — Machine Registry](001-machine-registry.md) (for machine-aware routing)
- [003 — Remote Transport](003-remote-transport.md) (for the remote provider implementation)

## Dependents

- [004 — Cross-Machine Dispatch](004-cross-machine-dispatch.md)
