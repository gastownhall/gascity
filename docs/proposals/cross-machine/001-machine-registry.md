---
title: "Machine Registry & Discovery"
type: satellite-issue
epic: 000-epic-cross-machine-city
status: proposed
component: config
current_state: not-implemented
priority: high
author: trillium
date: 2026-03-21
labels: [config, cross-machine, foundation]
---

# Machine Registry & Discovery

## Parent Epic

[Epic: Cross-Machine City Operation](000-epic-cross-machine-city.md)

## Summary

Gas City has no concept of "machines" in its configuration. There is no way to declare
that a city spans multiple hosts, what those hosts are, or what capabilities they have.
This is the foundational config layer everything else depends on.

## Current State: Not Implemented

### What Exists

- `city.toml` has no `[machines]` section or equivalent
- No machine identifier concept in the config schema
- The supervisor tracks multiple cities on one machine, but has no cross-machine awareness
- K8s provider uses `context` to select clusters, but this is not a general machine concept
- The Gastown satellite work (now closed) had `machines.json` with machine entries and
  dispatch policies — this was Go code in the `gastown` repo, not in Gas City

### What's Missing

- Machine definition schema (hostname, address, capabilities, capacity)
- Machine discovery mechanism (static config, DNS, mDNS, Tailscale)
- Machine health/availability tracking
- Agent-to-machine affinity rules
- Machine-scoped config overrides

## Proposed Design

### city.toml Addition

```toml
# Machine definitions
[[machines]]
name = "mini2"
host = "mini2.hippo-tilapia.ts.net"    # Tailscale hostname
role = "hub"                            # hub or satellite
tags = ["fast-disk", "gpu"]             # capability tags

[[machines]]
name = "mini3"
host = "mini3.hippo-tilapia.ts.net"
role = "satellite"
tags = ["general"]
max_agents = 8                          # capacity hint

# Agent affinity (optional)
[[agent]]
name = "polecat"
[agent.pool]
min = 0
max = 5
machines = ["mini2", "mini3"]           # eligible machines
prefer = "mini3"                        # soft preference
```

### Key Decisions Needed

1. **Static vs dynamic**: Start with static `[[machines]]` in city.toml, or support
   runtime discovery?
2. **Hub-satellite vs peer**: Is there always one hub, or can machines be peers?
3. **Tailscale integration**: Our machines are on Tailscale — should we use Tailscale
   API for discovery?
4. **Capacity model**: Simple `max_agents` count, or resource-based (CPU, memory)?

## Audit Findings (2026-03-21)

Traced against Gas City codebase. **Issue is accurate — no machine concept exists.**

### What Exists

- `K8sConfig.Context` provides single-cluster selection (env var `GC_K8S_CONTEXT`)
- `SessionConfig.RemoteMatch` provides substring-based routing in hybrid provider
- No `Machine` struct, no machine registry, no machine affinity in config

### Gas City-Native Implementation

The Gastown approach (`machines.json` as separate JSON file) should be replaced with
TOML inline in `city.toml`, following Gas City conventions:

- **`City.Machines []Machine`** at `config.go:~82` (alongside `Dolt`, `Beads`, `Session`)
- **`PoolConfig.Machines []string`** at `config.go:964-993` for agent-to-machine affinity
- **`PoolConfig.Prefer string`** for soft preference tiebreaker
- **`PoolOverride`** in `patch.go:92-106` needs matching fields
- **`ValidateSemantics()`** in `validate_semantics.go:10-80` needs machine reference checks

### Key Insertion Points

| File | Lines | Change |
|------|-------|--------|
| `internal/config/config.go` | 57-132 | Add `Machines []Machine` to `City` struct |
| `internal/config/config.go` | 964-993 | Add `Machines`, `Prefer` to `PoolConfig` |
| `internal/config/patch.go` | 92-106 | Add machine fields to `PoolOverride` |
| `internal/config/validate_semantics.go` | 10-80 | Add machine name reference validation |

## Dependencies

- None (this is foundational)

## Dependents

- [002 — Hybrid Provider Config](002-hybrid-provider-config.md)
- [003 — Remote Transport](003-remote-transport.md)
- [004 — Cross-Machine Dispatch](004-cross-machine-dispatch.md)
- [008 — Remote Health](008-remote-health.md)
