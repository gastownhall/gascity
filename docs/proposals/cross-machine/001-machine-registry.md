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

## Dependencies

- None (this is foundational)

## Dependents

- [002 — Hybrid Provider Config](002-hybrid-provider-config.md)
- [003 — Remote Transport](003-remote-transport.md)
- [004 — Cross-Machine Dispatch](004-cross-machine-dispatch.md)
- [008 — Remote Health](008-remote-health.md)
