---
title: "Cross-Machine Event Bus"
type: satellite-issue
epic: 000-epic-cross-machine-city
status: proposed
component: events
current_state: not-implemented
priority: low
author: trillium
date: 2026-03-21
labels: [events, pub-sub, cross-machine]
---

# Cross-Machine Event Bus

## Parent Epic

[Epic: Cross-Machine City Operation](000-epic-cross-machine-city.md)

## Summary

The event bus is currently a per-city append-only JSONL file on disk. The supervisor
aggregates events from multiple cities on the same machine. For cross-machine operation,
events from satellite machines need to be visible to the hub's controller and other
observers.

## Current State: Not Implemented

### What Exists

- **Per-city event log**: `.gc/events.jsonl` — append-only JSONL file
- **Recorder** (`internal/events/recorder.go`): Writes events to local file
- **Reader** (`internal/events/reader.go`): Reads and filters events from local file
- **Supervisor multiplexer**: Aggregates events from multiple cities on the same machine
  via composite cursor format `{city}:{seq}`
- **SSE streaming**: `GET /v0/events/stream` serves events via Server-Sent Events
- **Monotonic sequence numbers**: Events are ordered within a city

### What's Missing

- No mechanism for satellite machines to publish events to the hub
- No remote event subscription
- No event replication or forwarding
- No cross-machine sequence ordering
- Each machine has a completely independent event bus

## Design Considerations

### Why This May Be Low Priority

If the beads store is shared (issue 005), much of what events provide is already
available through bead state changes. The event bus is primarily used for:

1. **Reactive notification** — "something happened, go check"
2. **Audit trail** — "what happened and when"
3. **Order gates** — events trigger order dispatch

For (1) and (2), querying the shared bead store may suffice. For (3), only the
hub controller evaluates order gates, so only hub events matter.

### When This Becomes Important

- If satellite machines run their own controller logic (not planned initially)
- If we need real-time cross-machine observability (dashboards, alerts)
- If order gates need to react to satellite-originated events

## Possible Approaches

### A: Event Forwarding (push)

Satellites push events to hub via HTTP:

```
Satellite event recorder → HTTP POST → Hub event ingestion → Hub event log
```

### B: Event Polling (pull)

Hub polls satellites for new events:

```
Hub controller → HTTP GET /events?since=N → Satellite event reader
```

### C: Shared Event Store

Write events to the shared Dolt database instead of local JSONL:

```
Any machine → bd event write → Dolt (shared) → Any machine reads
```

This aligns with "beads is the universal persistence substrate."

### Recommendation

Defer this until cross-machine operation is running. If shared Dolt (005) works
well, **Option C** may be the natural path. Start without cross-machine events
and evaluate the need once agents are running remotely.

## Audit Findings (2026-03-21)

Traced against Gas City codebase. **Issue is accurate.** Events are per-city JSONL files
with no cross-machine replication.

### Key Detail: Order Gates Only See Local Events

`orders/gates.go:149-170` — `checkEvent()` receives a single `events.Provider`, which
is the local city's provider. Satellite order gates **cannot** react to hub events.
This matters if orders need to fire based on cross-machine activity.

### Shared Dolt Impact

If events moved to Dolt (Option C), `gates.go` would need refactoring to accept a
remote-capable event provider. But if order gates only run on the hub controller (the
likely architecture), this is a non-issue.

### Recommendation Confirmed

Defer until cross-machine operation is running. Fix 005 (Dolt config wiring) first,
then evaluate whether event gates need satellite-side visibility.

### Key Code Locations

| File | Lines | What |
|------|-------|------|
| `internal/events/events.go` | 52-88 | Event struct and Provider interface |
| `internal/events/recorder.go` | 20-127 | FileRecorder (JSONL append) |
| `internal/api/supervisor.go` | multiplexer | Same-machine city aggregation |
| `internal/orders/gates.go` | 149-170 | `checkEvent()` — local provider only |

## Dependencies

- [005 — Distributed Beads](005-distributed-beads.md) (if using shared store approach)
- [003 — Remote Transport](003-remote-transport.md) (if using push/pull approach)

## Dependents

- None directly (event bus is consumed, not depended on by other cross-machine features)
