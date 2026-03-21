---
title: "Cross-Machine Dispatch Policy"
type: satellite-issue
epic: 000-epic-cross-machine-city
status: proposed
component: dispatch
current_state: not-implemented
priority: medium
author: trillium
date: 2026-03-21
labels: [dispatch, scheduling, cross-machine]
---

# Cross-Machine Dispatch Policy

## Parent Epic

[Epic: Cross-Machine City Operation](000-epic-cross-machine-city.md)

## Summary

When a city spans multiple machines, the controller needs a policy for deciding which
machine gets which agent. This is the dispatch layer that sits between "I need a polecat"
and "start a tmux session on mini3."

## Current State: Not Implemented

### What Exists

- **Sling dispatch** (`cmd/gc/` sling commands): Routes work to agents, but has no
  machine awareness. It picks an agent from a pool, assigns a bead, and nudges.
- **Pool scaling** (`cmd/gc/pool.go`): Evaluates whether to scale pools up/down based
  on pending work and idle agents. No machine dimension.
- **K8s scheduling**: Kubernetes itself handles pod placement across nodes, but this
  is opaque to Gas City — it just says "create a pod" and K8s decides where.

### Gastown Reference

The closed satellite PRs included dispatch policies:

| Policy | Behavior |
|--------|----------|
| `local-first` | Prefer local, fall back to satellite |
| `local-only` | Only local machines |
| `satellite-first` | Prefer satellite, fall back to local |
| `satellite-only` | Only satellite machines |
| `round-robin` | Distribute evenly across all machines |

These were in `gastown/internal/dispatch/` — not in Gas City.

## Proposed Design

### Dispatch Policy in city.toml

```toml
[dispatch]
policy = "local-first"                  # default policy

# Per-agent overrides
[[agent]]
name = "polecat"
[agent.pool]
min = 0
max = 8
machines = ["mini2", "mini3"]
dispatch_policy = "round-robin"         # override for this pool

[[agent]]
name = "mayor"
machines = ["mini2"]                    # always on hub
```

### Policy Interface

```go
// internal/dispatch/policy.go
type Policy interface {
    // SelectMachine picks a machine for a new agent instance.
    // machines is the list of eligible machines from config.
    // running is the current machine -> agent count map.
    SelectMachine(machines []Machine, running map[string]int) (string, error)
}
```

### Built-in Policies

| Policy | Description |
|--------|-------------|
| `local-first` | Prefer hub machine, use satellites when hub is at capacity |
| `local-only` | Hub only (current behavior, default) |
| `round-robin` | Rotate across eligible machines |
| `least-loaded` | Pick machine with fewest running agents |
| `manual` | Machine specified per-bead by the mayor/dispatcher |

### ZFC Compliance

The policy engine selects machines — it does NOT decide whether to scale. Scaling
decisions (should we spawn another polecat?) remain with the pool evaluator. The
policy only answers: "given that we're spawning, where?"

This respects ZFC: the Go code handles routing (transport), not judgment about
whether work should happen.

## Dependencies

- [001 — Machine Registry](001-machine-registry.md) (machine definitions and capacity)
- [002 — Hybrid Provider Config](002-hybrid-provider-config.md) (routing to remote providers)
- [003 — Remote Transport](003-remote-transport.md) (actually reaching remote machines)

## Dependents

- None directly (this is a consumer of the foundation layers)
