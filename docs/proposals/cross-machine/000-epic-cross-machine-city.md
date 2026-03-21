---
title: "Epic: Cross-Machine City Operation"
type: epic
status: proposed
author: trillium
date: 2026-03-21
labels: [architecture, cross-machine, multi-node]
upstream_refs:
  - "steveyegge/gastown#2794"
  - "steveyegge/gastown#2830"
  - "steveyegge/gastown#2850"
  - "steveyegge/gastown#2851"
  - "steveyegge/gastown#2852"
  - "steveyegge/gastown#2853"
  - "steveyegge/gastown#2854"
  - "steveyegge/gastown#3066"
satellite_issues:
  - 001-machine-registry
  - 002-hybrid-provider-config
  - 003-remote-transport
  - 004-cross-machine-dispatch
  - 005-distributed-beads
  - 006-cross-machine-events
  - 007-cross-machine-mail
  - 008-remote-health
  - 009-session-tracking
  - 010-k8s-multi-cluster
  - 011-dolt-config-discoverability
  - 012-cross-machine-nudge
  - 013-transport-security
  - 014-city-doesnt-care
  - 015-proxy-relay
---

# Epic: Cross-Machine City Operation

## Motivation

Gas City currently operates as a single-machine orchestration system. A city runs on
one host, its controller manages local agents, and all coordination happens through
local tmux sessions, local filesystem, and local process management.

The satellite transport work we prototyped in Gastown ([epic #2794](https://github.com/steveyegge/gastown/issues/2794),
PRs #2858–#2863 now closed) demonstrated a real need: distribute agent workloads across
multiple physical machines while maintaining a single logical city. Gastown has gone to
1.0 / feature-complete, so this work targets Gas City instead. Use cases include:

- **Resource scaling**: Spread polecats across machines with available CPU/memory
- **Hardware specialization**: GPU machines for certain workloads, fast-disk machines for others
- **Resilience**: Survive single-machine failures without losing city state
- **Lab environments**: Mini2/Mini3/other machines collaborating as one city

## Current State

Gas City already has several building blocks that partially support this:

| Component | Status | Notes |
|-----------|--------|-------|
| Kubernetes provider | **Fully implemented** | Pod-based agents in remote clusters |
| Hybrid provider | **Minimal** | Routing layer exists, no config support |
| Dolt/Beads over network | **Declared but not wired** | `[dolt]` config parsed but never consumed (011) |
| Supervisor (machine-wide) | **Fully implemented** | Multi-city on same machine, not cross-machine |
| ACP protocol | **Fully implemented** | Local process only, not network-aware |
| SSH/mTLS transport | **Not implemented** | No mechanism exists |
| Machine registry | **Not implemented** | No machines.json or discovery |
| Cross-machine dispatch | **Not implemented** | No policy engine for machine selection |
| Cross-machine nudge | **Broken cross-machine** | Writes to local filesystem only (012) |
| Cross-machine events | **Not implemented** | Per-machine event bus only |
| Cross-machine mail | **Not implemented** | Local addressing only |
| Remote health checks | **Not implemented** | Doctor checks are local only |
| Transport security | **Not implemented** | No cross-machine auth layer (013) |

## Approach

Rather than building everything at once, we propose evaluating each component for
cross-machine readiness and identifying the minimum viable path. The satellite issues
below each assess one component in detail.

### Guiding Principles

1. **Respect ZFC**: Cross-machine coordination is transport, not reasoning. Go code
   routes agents to machines; agents don't know or care where they run.
2. **Respect the Bitter Lesson**: Build primitives that get more useful as models improve.
   Don't build smart scheduling — build simple routing and let the human (or mayor) decide.
3. **Respect NDI**: Work survives machine failures because beads are in Dolt (network-accessible).
   Sessions come and go; the work persists.
4. **Configuration-first**: Machine topology is config, not code. A `[machines]` section
   in city.toml, not a compiled-in machine list.
5. **Incremental**: Each satellite issue should be independently valuable. No big bang.

### Potential Architecture

```
Machine A (hub)                    Machine B (satellite)
┌─────────────────────┐            ┌─────────────────────┐
│  Controller          │            │  Agent runtime       │
│  Dolt server         │◄──────────►│  (tmux sessions)     │
│  Event bus (primary) │   network   │  Event relay         │
│  Mail store          │            │  Beads client         │
│  Mayor, Deacon       │            │  Polecats, Crew       │
└─────────────────────┘            └─────────────────────┘
```

## Satellite Issues

### Bugs & Foundation

| # | Issue | Component | Current State |
|---|-------|-----------|---------------|
| 011 | [Dolt Config Discoverability](011-dolt-config-discoverability.md) | Config/Beads | **Broken** — config parsed but not consumed |
| 014 | [City Doesn't Care (Principle)](014-city-doesnt-care.md) | Architecture | Accepted principle |

### Infrastructure Layer

| # | Issue | Component | Current State |
|---|-------|-----------|---------------|
| 001 | [Machine Registry & Discovery](001-machine-registry.md) | Config | Not implemented |
| 002 | [Hybrid Provider Configuration](002-hybrid-provider-config.md) | Runtime | Minimal |
| 003 | [Remote Transport Layer](003-remote-transport.md) | Runtime | Not implemented |
| 013 | [Transport Security](013-transport-security.md) | Security | Not implemented |
| 015 | [Proxy Relay Architecture](015-proxy-relay.md) | Transport | Not implemented (may not be needed) |

### Cross-Machine Operations

| # | Issue | Component | Current State |
|---|-------|-----------|---------------|
| 004 | [Cross-Machine Dispatch Policy](004-cross-machine-dispatch.md) | Dispatch | Not implemented |
| 005 | [Distributed Beads Store](005-distributed-beads.md) | Beads | Declared but not wired |
| 006 | [Cross-Machine Event Bus](006-cross-machine-events.md) | Events | Not implemented |
| 007 | [Cross-Machine Mail Routing](007-cross-machine-mail.md) | Mail | Not implemented (likely free) |
| 008 | [Remote Health Monitoring](008-remote-health.md) | Doctor/Health | Not implemented |
| 009 | [Global Session Tracking](009-session-tracking.md) | Session | Partially implemented |
| 010 | [Kubernetes Multi-Cluster](010-k8s-multi-cluster.md) | K8s | Partially implemented |
| 012 | [Cross-Machine Nudge Delivery](012-cross-machine-nudge.md) | Messaging | Broken cross-machine |

## Upstream References (Gastown)

This work continues the satellite transport effort from Gastown, which has gone 1.0 /
feature-complete. The following Gastown issues informed this proposal:

| Gastown Issue | Gas City Equivalent |
|---------------|-------------------|
| [#2794](https://github.com/steveyegge/gastown/issues/2794) — Satellite compute nodes (epic) | This epic (000) |
| [#2830](https://github.com/steveyegge/gastown/issues/2830) — Dolt config discoverability | 011 (same bug, different codebase) |
| [#2850](https://github.com/steveyegge/gastown/issues/2850) — Machine registry & config | 001 |
| [#2851](https://github.com/steveyegge/gastown/issues/2851) — Dispatch policy framework | 004 |
| [#2852](https://github.com/steveyegge/gastown/issues/2852) — mTLS bootstrap & sling wiring | 003, 013 |
| [#2853](https://github.com/steveyegge/gastown/issues/2853) — Doctor health checks | 008 |
| [#2854](https://github.com/steveyegge/gastown/issues/2854) — Proxy, polecat manager & glue | 015 |
| [#3066](https://github.com/steveyegge/gastown/issues/3066) — Cross-machine nudge | 012 |

### Why Gas City is closer to this goal

Gas City advantages over Gastown for cross-machine work:

1. **Provider abstraction**: The `runtime.Provider` interface means adding SSH/remote
   is a new provider, not surgery on existing code
2. **Hybrid provider exists**: Routing layer between local/remote backends is built
3. **K8s proves the pattern**: Remote agent execution already works in K8s pods
4. **"City doesn't care" is native**: Zero hardcoded roles means zero assumptions
   about where agents run
5. **Config-driven**: Machine topology can be TOML config, not compiled Go

## Suggested Priority Order

1. **011 — Dolt Config Bug** (fix the wiring — unblocks everything)
2. **005 — Distributed Beads** (validate network path with the config fix)
3. **001 — Machine Registry** (config foundation everything else depends on)
4. **003 — Remote Transport** (enables remote agent spawning)
5. **012 — Cross-Machine Nudge** (critical for bidirectional agent coordination)
6. **002 — Hybrid Provider Config** (routing layer already exists, needs config)
7. **004 — Dispatch Policy** (builds on registry + transport)
8. **009 — Session Tracking** (needs transport + registry)
9. **013 — Transport Security** (Tailscale may suffice initially)
10. **007 — Mail** (likely works for free once beads are shared)
11. **006 — Events** (can defer if beads store is shared)
12. **008 — Remote Health** (needs transport)
13. **015 — Proxy Relay** (may not be needed if SSH + shared Dolt suffice)
14. **010 — K8s Multi-Cluster** (already functional for single cluster, enhancement)

## Minimum Viable Cross-Machine City

The smallest slice that proves cross-machine works:

1. Fix Dolt config wiring (011)
2. Point `[dolt]` at a shared Dolt server accessible over Tailscale (005)
3. Manually start agents on satellite via tmux over SSH
4. Verify beads, mail, and formulas work across machines

This requires **zero new Go code** — just fixing the config wiring bug. Once
validated, build the automation (machine registry, SSH provider, dispatch policy).

## Open Questions

- Should the hub machine run a full supervisor, or a lightweight coordinator?
- Is SSH sufficient for remote transport, or do we need a persistent daemon on satellites?
- Should satellite machines run their own controller, or be "dumb" agent hosts?
- How does this interact with Gas City's progressive capability model (levels 0–8)?
- Can we reuse any of the Gastown satellite Go code, or is a clean implementation better?
- Should we close the Gastown satellite issues with pointers to these Gas City proposals?
