---
title: "Cross-Machine Nudge Delivery"
type: satellite-issue
epic: 000-epic-cross-machine-city
status: proposed
component: messaging
current_state: broken-cross-machine
priority: high
author: trillium
date: 2026-03-21
upstream_ref: "steveyegge/gastown#3066"
labels: [nudge, messaging, cross-machine, control-plane]
---

# Cross-Machine Nudge Delivery

## Parent Epic

[Epic: Cross-Machine City Operation](000-epic-cross-machine-city.md)

## Upstream Reference

Gas City equivalent of [steveyegge/gastown#3066](https://github.com/steveyegge/gastown/issues/3066)
("feat: cross-machine nudge via session registry + proxy relay").

## The Problem

Nudge delivery is local-only. In Gas City, nudging writes to a local filesystem queue
or sends a tmux `send-keys` to a local session. When an agent on mini3 nudges the
mayor on mini2, the nudge is either written to mini3's local filesystem (never
delivered) or fails because the tmux session doesn't exist locally.

This makes cross-machine agents one-directional: work can be dispatched out, but
there is no mechanism for a satellite agent to signal completion, report blockers,
or request decisions. Sequential workflow orchestration across machine boundaries
breaks.

### Why This Is Different From Mail (007)

Mail is stored in beads (Dolt) — if Dolt is shared, mail works cross-machine.

Nudge is a **control-plane operation** — it's a real-time prompt injection into
a running agent's tmux session. It requires knowing which machine the target agent
is on and being able to reach its tmux session. This is fundamentally different
from asynchronous message storage.

```
Mail:  sender → write bead to Dolt → recipient polls inbox    (async, works if Dolt shared)
Nudge: sender → send-keys to tmux  → recipient interrupted    (sync, requires machine routing)
```

## Current State in Gas City

### How Nudge Works Today

The nudge queue (`internal/nudgequeue/`) batches nudge messages and delivers them
via the runtime provider's `SendPrompt()` method:

```
Controller/Agent → nudgequeue.Enqueue(target, message)
                 → nudgequeue.Flush()
                 → provider.SendPrompt(ctx, target, message)
                 → tmux send-keys -t target "message"
```

All of this is local. `SendPrompt` goes to the local tmux server.

### What Breaks Cross-Machine

1. `provider.SendPrompt()` only reaches local tmux sessions
2. No session-to-machine mapping to know where the target is
3. No relay mechanism to forward nudges to remote machines
4. Nudge queue is filesystem-based (`.gc/runtime/nudge_queue/`)

## Proposed Design

### Option A: Route Through Provider (recommended)

If the hybrid/multi provider (002) correctly implements `SendPrompt()` by routing
to the right machine's provider, nudge delivery works transparently:

```go
func (h *MultiHybrid) SendPrompt(ctx, name, message string) error {
    machine := h.resolve(name)          // which machine is this agent on?
    provider := h.providers[machine]
    return provider.SendPrompt(ctx, name, message)
}
```

The SSH provider (003) would implement `SendPrompt` by running
`tmux -L gc send-keys -t name "message"` over SSH.

This is clean: no new components, nudge routing is just another provider operation.

### Option B: Proxy Relay (Gastown's design)

The Gastown issue (#3066) proposed a proxy relay endpoint:

```
POST /v1/relay/nudge
{ "target": "mayor/", "message": "Done: work completed" }
```

The proxy writes the nudge to the local nudge queue on the target machine. This
requires a persistent proxy/daemon on each machine.

### Option C: Dolt-Backed Session Registry + Relay

A `sessions` table in Dolt tracks which machine hosts each agent:

```sql
CREATE TABLE sessions (
  session_name   VARCHAR PRIMARY KEY,
  machine_id     VARCHAR,
  proxy_addr     VARCHAR,
  last_heartbeat DATETIME
);
```

Nudge routing looks up the target in this table and sends via the proxy.

### Recommendation

**Option A** is simplest and most consistent with Gas City's provider abstraction.
If the SSH provider implements `SendPrompt` correctly, nudge delivery is solved
without any new infrastructure. The hybrid provider already routes operations —
nudge is just another operation.

Options B and C add value for observability (knowing where agents are) but are
more infrastructure to build and maintain. They can come later if needed.

## The "City Doesn't Care" Principle

From [Gastown #2794](https://github.com/steveyegge/gastown/issues/2794):

> `gc nudge mayor` works identically whether the target is local or remote.

The caller never needs to know which machine the target is on. The provider
abstraction handles routing transparently.

## Dependencies

- [002 — Hybrid Provider Config](002-hybrid-provider-config.md) (routing layer)
- [003 — Remote Transport](003-remote-transport.md) (SSH provider with SendPrompt)
- [009 — Session Tracking](009-session-tracking.md) (knowing which machine has which agent)

## Dependents

- [007 — Cross-Machine Mail](007-cross-machine-mail.md) (mail delivery also nudges recipient)
