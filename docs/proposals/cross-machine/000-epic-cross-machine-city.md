---
title: "Epic: Cross-Machine City Operation"
type: epic
status: proposed
author: trillium
date: 2026-03-21
labels: [architecture, cross-machine, multi-node]
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
---

# Epic: Cross-Machine City Operation

## Motivation

Gas City currently operates as a single-machine orchestration system. A city runs on
one host, its controller manages local agents, and all coordination happens through
local tmux sessions, local filesystem, and local process management.

The satellite transport work we prototyped in Gastown (PRs #2858–#2863, now closed)
demonstrated a real need: distribute agent workloads across multiple physical machines
while maintaining a single logical city. Use cases include:

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
| Dolt/Beads over network | **Partially implemented** | Remote Dolt server works, single store per city |
| Supervisor (machine-wide) | **Fully implemented** | Multi-city on same machine, not cross-machine |
| ACP protocol | **Fully implemented** | Local process only, not network-aware |
| SSH/mTLS transport | **Not implemented** | No mechanism exists |
| Machine registry | **Not implemented** | No machines.json or discovery |
| Cross-machine dispatch | **Not implemented** | No policy engine for machine selection |
| Cross-machine events | **Not implemented** | Per-machine event bus only |
| Cross-machine mail | **Not implemented** | Local addressing only |
| Remote health checks | **Not implemented** | Doctor checks are local only |

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

| # | Issue | Component | Current State |
|---|-------|-----------|---------------|
| 001 | [Machine Registry & Discovery](001-machine-registry.md) | Config | Not implemented |
| 002 | [Hybrid Provider Configuration](002-hybrid-provider-config.md) | Runtime | Minimal |
| 003 | [Remote Transport Layer](003-remote-transport.md) | Runtime | Not implemented |
| 004 | [Cross-Machine Dispatch Policy](004-cross-machine-dispatch.md) | Dispatch | Not implemented |
| 005 | [Distributed Beads Store](005-distributed-beads.md) | Beads | Partially implemented |
| 006 | [Cross-Machine Event Bus](006-cross-machine-events.md) | Events | Not implemented |
| 007 | [Cross-Machine Mail Routing](007-cross-machine-mail.md) | Mail | Not implemented |
| 008 | [Remote Health Monitoring](008-remote-health.md) | Doctor/Health | Not implemented |
| 009 | [Global Session Tracking](009-session-tracking.md) | Session | Partially implemented |
| 010 | [Kubernetes Multi-Cluster](010-k8s-multi-cluster.md) | K8s | Partially implemented |

## Suggested Priority Order

1. **005 — Distributed Beads** (closest to working, Dolt is already network-capable)
2. **001 — Machine Registry** (config foundation everything else depends on)
3. **003 — Remote Transport** (enables remote agent spawning)
4. **002 — Hybrid Provider Config** (routing layer already exists, needs config)
5. **004 — Dispatch Policy** (builds on registry + transport)
6. **009 — Session Tracking** (needs transport + registry)
7. **006 — Events** (can defer if beads store is shared)
8. **007 — Mail** (works if beads store is shared)
9. **008 — Remote Health** (needs transport)
10. **010 — K8s Multi-Cluster** (already functional for single cluster, enhancement)

## Open Questions

- Should the hub machine run a full supervisor, or a lightweight coordinator?
- Is SSH sufficient for remote transport, or do we need a persistent daemon on satellites?
- Should satellite machines run their own controller, or be "dumb" agent hosts?
- How does this interact with Gas City's progressive capability model (levels 0–8)?
- What is the minimum viable cross-machine city? (Hub + 1 satellite + shared Dolt?)
