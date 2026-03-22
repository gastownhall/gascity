---
title: "Global Session Tracking"
type: satellite-issue
epic: 000-epic-cross-machine-city
status: proposed
component: session
current_state: partially-implemented
priority: medium
author: trillium
date: 2026-03-21
labels: [session, state-machine, cross-machine]
---

# Global Session Tracking

## Parent Epic

[Epic: Cross-Machine City Operation](000-epic-cross-machine-city.md)

## Summary

Gas City tracks agent sessions (active, suspended, quarantined, etc.) via the session
manager. Currently, session state is local — the controller discovers sessions by
querying the local tmux server. For cross-machine operation, the controller needs a
global view of sessions across all machines.

## Current State: Partially Implemented

### What Exists

**Session Manager** (`internal/session/manager.go`):

- Session state machine: Creating → Active → Suspended / Draining → Archived / Quarantined
- Session info stored: template, state, provider, session name/key, work dir, command
- Resume support: provider-specific resume keys (Claude Code `--resume <key>`)
- The session manager uses the runtime provider for discovery — if the provider
  reports a session, the manager tracks it

**Session Discovery**:

- `ListRunning()` on the provider returns all active sessions
- Controller reconciles desired state (from config) with running state (from provider)
- The hybrid provider already merges `ListRunning` results from local + remote

### What Works

If the hybrid provider (002) correctly merges session listings from multiple machines,
the session manager should see all sessions globally. The reconciliation loop would
then work across machines.

### What's Missing

1. **Session-to-machine mapping**: The session manager doesn't track which machine a
   session is on. It just knows the session exists. For operations like "nudge this
   agent," it needs to know which machine to route to.

2. **Machine affinity on resume**: When resuming a suspended session, it needs to
   resume on the same machine (tmux session state is local to that machine).

3. **Cross-machine session migration**: Moving a session from one machine to another
   (e.g., for load balancing or failover) is not supported. The session would need
   to be stopped on one machine and started fresh on another.

4. **Stale session cleanup**: If a satellite machine goes offline, its sessions appear
   as stale. The controller needs to distinguish "machine unreachable" from "agent
   crashed."

## Proposed Design

### Session Info Extension

```go
type SessionInfo struct {
    // Existing fields...
    Name     string
    State    SessionState
    Provider string
    // New field:
    Machine  string  // which machine hosts this session
}
```

### Machine-Aware Operations

The hybrid/multi provider needs to tag results with machine origin:

```go
func (h *MultiHybrid) ListRunning(ctx) ([]SessionInfo, error) {
    var all []SessionInfo
    for machine, provider := range h.providers {
        sessions, err := provider.ListRunning(ctx)
        for _, s := range sessions {
            s.Machine = machine  // tag with origin
            all = append(all, s)
        }
    }
    return all, nil
}
```

### Resume Routing

When resuming, the session manager checks the stored `Machine` field and routes
the Start call to the correct provider:

```go
func (m *Manager) Resume(ctx, name string) error {
    info := m.sessions[name]
    provider := m.hybrid.ProviderFor(info.Machine)
    return provider.Start(ctx, name, info.ResumeCommand(), info.WorkDir)
}
```

### Failure Handling

```
Machine goes offline:
  → ListRunning returns error for that machine
  → Controller marks those sessions as "machine-unreachable"
  → After timeout, marks them as "orphaned"
  → If machine comes back, rediscovers sessions
  → If machine stays down, reassigns work (beads) to other agents
```

The key insight: **work survives in Dolt, sessions are ephemeral**. If a machine
dies, we lose sessions but not work. The controller creates new agents elsewhere
and they pick up the beads.

## Audit Findings (2026-03-21)

Traced against Gas City codebase. **Issue underestimates the problem.**

### Critical: Machine Affinity Lost on Restart

The hybrid provider's `isRemote()` function is a **closure captured at startup**
(`providers.go:355-357`). If the controller restarts, this closure is recreated from
config — there's no persistent record of which sessions are on which machine.

Sessions created before restart have no way to find their origin machine unless it's
stored in the bead. The `transport` metadata field (used for ACP) is a partial solution
but doesn't cover cross-machine routing.

### ListRunning Returns Flat Strings

`hybrid.ListRunning()` returns `[]string` — session names only, no machine origin.
The session manager can't tell which machine a session belongs to when reconciling.

### SessionInfo Has No Machine Field

The `Info` struct in `internal/session/manager.go` contains: ID, Template, State,
Provider, SessionName, SessionKey, WorkDir, Command, ResumeFlag, etc. **No `Machine`
field exists.**

### Required Changes (Deeper Than Proposed)

1. **Add `machine` to session bead metadata** — durable, survives restart
2. **Enrich `ListRunning()` results** — hybrid provider must tag with machine origin
3. **Resume routing** — `ensureRunning()` must check bead's machine field and route
   `Start()` to the correct provider
4. **Stale session vs machine-offline distinction** — `wakeReasons()` needs to
   differentiate "agent crashed" from "machine unreachable"

### Key Code Locations

| File | Lines | What |
|------|-------|------|
| `internal/session/manager.go` | Info struct | No machine field |
| `internal/session/chat.go` | resume logic | No machine routing |
| `cmd/gc/session_reconciler.go` | 370-385 | Wake decision (no machine check) |
| `internal/runtime/hybrid/hybrid.go` | ListRunning | Flat string merge |

## Dependencies

- [002 — Hybrid Provider Config](002-hybrid-provider-config.md) (multi-machine provider)
- [003 — Remote Transport](003-remote-transport.md) (remote session operations)

## Dependents

- [008 — Remote Health](008-remote-health.md) (health checks use session state)
