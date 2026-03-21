---
title: "Proxy Relay Architecture"
type: satellite-issue
epic: 000-epic-cross-machine-city
status: proposed
component: transport
current_state: not-implemented
priority: medium
author: trillium
date: 2026-03-21
upstream_ref: "steveyegge/gastown#2794, steveyegge/gastown#3066"
labels: [proxy, relay, transport, architecture]
---

# Proxy Relay Architecture

## Parent Epic

[Epic: Cross-Machine City Operation](000-epic-cross-machine-city.md)

## Upstream Reference

Draws from the mTLS proxy design in [steveyegge/gastown#2794](https://github.com/steveyegge/gastown/issues/2794)
and the relay endpoint in [steveyegge/gastown#3066](https://github.com/steveyegge/gastown/issues/3066).

## Summary

The Gastown satellite work used a proxy server on the hub machine to relay all
cross-machine control plane calls (gt/bd commands, nudges, cert operations). This
issue evaluates whether Gas City needs a similar proxy or if the SSH provider (003)
is sufficient.

## Gastown's Proxy Model

### Architecture

```
SATELLITE (mini3)                    HUB (mini2)
┌────────────────────┐               ┌────────────────────────┐
│ Agent runs here     │               │ gt-proxy-server        │
│                     │               │   :9876 (mTLS)         │
│ gt mail inbox       │──── mTLS ────▶│   :9877 (admin)        │
│ bd update X         │               │                        │
│ gt nudge mayor      │               │ Dolt :3307             │
│                     │               │ Nudge queue            │
│ (uses gt-proxy-     │               │ Local gt/bd binaries   │
│  client shim)       │               └────────────────────────┘
└────────────────────┘
```

On satellites, `gt` and `bd` commands were shimmed: instead of running locally,
they called the hub's proxy server, which executed the real command and returned
the result. This meant:

- Only the hub needed Dolt running
- Only the hub needed the full town root
- Satellites were pure compute: tmux + agent binary + proxy client

### Relay Endpoints

```
POST /v1/relay/nudge     → write nudge to local queue
POST /v1/relay/command   → execute gt/bd command locally
GET  /v1/admin/issue-cert → issue mTLS certificate
POST /v1/admin/deny-cert  → revoke certificate
GET  /healthz             → proxy health check
```

## Gas City: Do We Need a Proxy?

### Arguments For

1. **Simplifies satellite setup**: Satellites don't need Dolt access, just proxy URL
2. **Centralized security**: All cross-machine calls go through one authenticated endpoint
3. **Command routing**: Hub can route commands to the correct local subsystem
4. **Nudge relay**: Solves cross-machine nudge delivery (012) elegantly
5. **Observability**: Single point to monitor/log all cross-machine traffic

### Arguments Against

1. **Single point of failure**: Proxy down = all cross-machine operations fail
2. **Latency**: Every bd/gc call adds a network round-trip through the proxy
3. **Complexity**: Another daemon to run, monitor, and upgrade
4. **SSH may suffice**: If the SSH provider (003) can execute remote commands
   directly, the proxy is unnecessary middleware
5. **Shared Dolt may suffice**: If satellites connect directly to Dolt (005),
   most proxy functionality is redundant

## Design Options

### Option A: No Proxy (SSH + Shared Dolt)

```
Satellite agent → bd CLI → shared Dolt (direct TCP)
Satellite agent → gc nudge → SSH provider → hub tmux
```

- Satellites connect to Dolt directly over network
- Nudges route through the SSH provider
- No proxy needed

**When this works**: Small setups (2-3 machines), Tailscale network, trusted environment

### Option B: Lightweight API Relay

A minimal HTTP relay on the hub for operations that can't go through Dolt:

```
Satellite → HTTP relay → nudge delivery, session registry, health reports
Satellite → Dolt (direct) → beads, mail, events
```

Only relay what needs relaying. Beads traffic goes direct to Dolt.

**When this works**: When nudge delivery and session tracking need a central coordinator

### Option C: Full Proxy (Gastown Model)

All cross-machine traffic through the proxy:

```
Satellite → mTLS proxy → gt/bd commands, nudges, Dolt, everything
```

**When this works**: Untrusted networks, multi-tenant, strict security requirements

### Recommendation

**Start with Option A** (SSH + shared Dolt). For our mini2/mini3 Tailscale setup,
direct Dolt access and SSH-routed nudges are sufficient. No proxy infrastructure
needed.

If we later need centralized coordination (Option B) or strict security (Option C),
the provider abstraction makes this an infrastructure change, not a city change
(per principle 014).

## Relationship to Other Issues

| Issue | Impact |
|-------|--------|
| 003 — Remote Transport | SSH provider may make proxy unnecessary |
| 005 — Distributed Beads | Direct Dolt access removes biggest proxy use case |
| 012 — Cross-Machine Nudge | Proxy relay is one solution; SSH provider is another |
| 013 — Transport Security | Proxy centralizes auth; Tailscale distributes it |

## Dependencies

- [003 — Remote Transport](003-remote-transport.md) (determines if proxy is needed)
- [005 — Distributed Beads](005-distributed-beads.md) (determines if command relay is needed)

## Dependents

- None (this is an optional architecture choice)
