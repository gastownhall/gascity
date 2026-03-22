---
title: "Remote Health Monitoring"
type: satellite-issue
epic: 000-epic-cross-machine-city
status: proposed
component: doctor/health
current_state: not-implemented
priority: low
author: trillium
date: 2026-03-21
labels: [health, doctor, monitoring, cross-machine]
---

# Remote Health Monitoring

## Parent Epic

[Epic: Cross-Machine City Operation](000-epic-cross-machine-city.md)

## Summary

Gas City's health monitoring (doctor checks and health patrol) operates locally. For
cross-machine operation, the hub controller needs visibility into the health of agents
running on satellite machines.

## Current State: Not Implemented

### What Exists

- **Doctor checks** (`internal/doctor/`): Validates city state consistency, stale locks,
  orphaned sessions, event log integrity, pack dependencies, provider readiness
- **Health patrol** (controller loop): Pings agents via tmux, checks activity timestamps,
  restarts unresponsive agents with backoff
- **Crash loop detection**: Tracks restart count, applies quarantine after threshold

### How Health Patrol Works Today

```
Controller tick:
  for each agent:
    1. Check tmux session exists (tmux has-session -t name)
    2. Check session activity timestamp
    3. If stale → nudge
    4. If unresponsive → restart
    5. If crash looping → quarantine
```

All of this assumes local tmux access.

### What's Missing

- Cannot check `tmux has-session` on a remote machine
- Cannot read tmux `session_activity` on a remote machine
- No remote process liveness checks (pgrep on remote)
- No remote agent log access
- Doctor checks assume local filesystem access

## Proposed Design

### Health Checks via Provider Interface

The runtime provider already has methods for agent health. If the SSH/remote provider
(issue 003) implements these correctly, health patrol should work transparently:

```go
type Provider interface {
    // These need to work remotely:
    ListRunning(ctx) ([]SessionInfo, error)  // replaces tmux list-sessions
    IsHealthy(ctx, name) (bool, error)       // replaces tmux has-session + activity check
    // ... other methods
}
```

If the SSH provider implements `ListRunning` by running `tmux list-sessions` over SSH
and `IsHealthy` by checking activity timestamps over SSH, the health patrol loop doesn't
need to change.

### Doctor Checks

Remote doctor checks need the transport layer:

| Check | Local | Remote |
|-------|-------|--------|
| Orphaned sessions | tmux list-sessions | tmux list-sessions over SSH |
| Orphaned worktrees | git worktree list | git worktree list over SSH |
| Stale locks | filesystem check | filesystem check over SSH |
| Provider readiness | local check | SSH connectivity check |
| Dolt connectivity | localhost | network reachability |

### New Doctor Checks for Cross-Machine

| Check | Purpose |
|-------|---------|
| `machine-ssh` | Verify SSH connectivity to each satellite |
| `machine-dolt` | Verify Dolt reachability from each satellite |
| `machine-capacity` | Check agent count vs capacity limits |
| `machine-clock` | Verify clock skew between machines is acceptable |

### Gastown Reference

The closed satellite PRs included satellite doctor checks:
- `machines-config` — validate machines.json and dispatch policy
- `satellite-ssh` — check SSH connectivity
- `satellite-proxy` — check mTLS proxy reachability
- `satellite-capacity` — check load vs capacity
- `dispatch-policy` — verify routing produces valid results

## Likely Outcome

If the remote transport provider (003) implements the `runtime.Provider` interface
correctly, health patrol will work with minimal changes. Doctor checks need explicit
remote-aware implementations.

## Audit Findings (2026-03-21)

Traced against Gas City codebase. **Issue is misleading — health monitoring is already
provider-abstracted and the K8s provider proves it works remotely.**

### Correction: Health Is Provider-Abstracted

The issue describes a tmux-dependent health patrol loop. This is **wrong**. The actual
health monitoring uses provider interface methods:

| Method | Purpose | Abstracted? |
|--------|---------|-------------|
| `Provider.IsRunning()` | Session liveness | Yes |
| `Provider.ProcessAlive()` | Agent process check | Yes |
| `Provider.ListRunning()` | Session discovery | Yes |
| `Provider.GetLastActivity()` | Activity timestamp | Yes |

All four are defined in `internal/runtime/runtime.go:83-125` and implemented by every
provider (tmux, K8s, ACP, exec, hybrid).

### K8s Provider Already Does Remote Health

`internal/runtime/k8s/provider.go`:
- `IsRunning()` (line 257): runs `tmux has-session` inside pod via kubectl exec
- `ProcessAlive()` (line 323): runs `pgrep -f` inside pod
- `GetLastActivity()` (line 470): runs `tmux display-message` inside pod

This proves the architecture supports remote health checks. An SSH provider implementing
these methods would get health monitoring **for free**.

### What's Actually Local-Only

Only **doctor filesystem checks** are truly local:
- `CityStructureCheck` — validates city.toml exists
- `BinaryCheck` — validates binary availability in PATH
- `BuiltinPackFamilyCheck` — validates pack files on disk

These would need remote variants or should be skipped for satellite machines.

### Health Patrol Doesn't Exist As Described

The issue describes a "patrol loop" that pings agents. The actual implementation is
**bead-driven reconciliation** in `session_reconciler.go`:
- Idle timeout via `GetLastActivity()` (provider-abstracted)
- Crash loop detection via in-memory `crashTracker`
- No explicit "nudge on stale" behavior

### Revised Scope

This issue should be narrowed to:
1. Remote-aware doctor checks (filesystem checks for satellites)
2. Machine connectivity checks (`machine-ssh`, `machine-dolt`)
3. The core health monitoring requires **no changes** — just a working remote provider

## Dependencies

- [003 — Remote Transport](003-remote-transport.md) (SSH access to satellites)
- [001 — Machine Registry](001-machine-registry.md) (machine definitions)

## Dependents

- None
