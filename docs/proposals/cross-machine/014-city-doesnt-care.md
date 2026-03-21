---
title: "Architectural Principle: City Doesn't Care"
type: satellite-issue
epic: 000-epic-cross-machine-city
status: accepted
component: architecture
current_state: principle
priority: n/a
author: trillium
date: 2026-03-21
upstream_ref: "steveyegge/gastown#2794"
labels: [architecture, principle, design]
---

# Architectural Principle: City Doesn't Care

## Parent Epic

[Epic: Cross-Machine City Operation](000-epic-cross-machine-city.md)

## Origin

From [steveyegge/gastown#2794](https://github.com/steveyegge/gastown/issues/2794):

> **A town should not know or care about the physical infrastructure it runs on.**
> The town is a logical construct — rigs, beads, polecats, crew, mayor, deacon,
> witness. Where its database lives, which computers polecats actually run on,
> whether it's 1 laptop or 30 machines — the town doesn't care.

This principle already aligns with Gas City's existing design: **zero hardcoded
roles**, behavior lives in configuration, the SDK provides infrastructure primitives.
This document formalizes the cross-machine extension of that principle.

## The Principle

**A city should not know or care about the physical infrastructure it runs on.**

All infrastructure concerns — machine registry, dispatch policy, capacity,
networking, transport security — live **below the city abstraction**:

```
┌─────────────────────────────────────────────────┐
│  CITY LAYER (doesn't care about machines)       │
│                                                  │
│  city.toml, agents, beads, formulas, events,    │
│  prompts, orders, health patrol, mail, nudge    │
│                                                  │
├─────────────────────────────────────────────────┤
│  INFRASTRUCTURE LAYER (knows about machines)     │
│                                                  │
│  machine registry, dispatch policy, transport,  │
│  session routing, Dolt networking, provider      │
│  selection, security/auth                        │
│                                                  │
├─────────────────────────────────────────────────┤
│  PHYSICAL LAYER (actual machines)               │
│                                                  │
│  mini2, mini3, K8s cluster, laptop              │
│                                                  │
└─────────────────────────────────────────────────┘
```

## What This Means Concretely

### Single-machine user (no config)

```toml
# city.toml — no [machines] section, no dispatch config
[session]
provider = "tmux"       # default, could even be omitted
```

No machine registry. No dispatch policy. No proxy. No SSH. Everything runs
locally. Zero cross-machine concepts visible.

### Multi-machine user (infrastructure config only)

```toml
# city.toml — same agents, same formulas, same prompts
# Only the infrastructure layer changes:

[[machines]]
name = "mini2"
host = "mini2.hippo-tilapia.ts.net"
role = "hub"

[[machines]]
name = "mini3"
host = "mini3.hippo-tilapia.ts.net"
role = "satellite"

[dolt]
host = "mini2.hippo-tilapia.ts.net"
port = 3307

[dispatch]
policy = "round-robin"
```

Same agents. Same formulas. Same prompts. Same pack. The city layer is identical.
Only the infrastructure layer changes.

### Switching from single to multi-machine

Adding machines is an infrastructure change, not a city change:

1. Add `[[machines]]` entries to city.toml
2. Point `[dolt]` at a network-accessible server
3. Optionally set a dispatch policy

No agent definitions change. No formulas change. No prompts change.
No orders change. The city doesn't care.

## Design Tests

When evaluating any cross-machine feature, apply this test:

> **Does any city-layer concept (agent, bead, formula, event, prompt, order)
> need to know which machine it's on?**

If yes, the design violates this principle. Push the machine awareness down
to the infrastructure layer.

### Examples

| Operation | City Layer | Infrastructure Layer |
|-----------|-----------|---------------------|
| "Spawn a polecat" | Pool evaluator says "need one more" | Dispatch policy picks machine, transport spawns there |
| "Nudge the mayor" | nudgequeue says "send this message" | Provider routes to correct machine's tmux |
| "Read my hook" | Agent runs `bd list --assignee=self` | bd CLI connects to Dolt wherever it is |
| "Check agent health" | Health patrol says "is agent alive?" | Provider checks remote tmux via SSH |
| "Send mail" | Write bead to store | Store writes to shared Dolt over network |

## Relationship to Existing Principles

This principle reinforces and extends:

- **ZFC (Zero Framework Cognition)**: Agents don't reason about machines.
  Machine selection is transport, not cognition.
- **NDI (Nondeterministic Idempotence)**: Work survives machine failures
  because beads are in Dolt, not local state. Sessions are ephemeral;
  the work persists.
- **SDK self-sufficiency**: No SDK operation depends on which machine
  it runs on. The controller drives infrastructure; agents execute work.
- **Progressive capability model**: Single-machine is Level 0.
  Multi-machine is activated by config presence, just like every other
  capability level.

## Impact on Cross-Machine Issues

Every satellite issue in this proposal set should be evaluated against this
principle:

| Issue | Principle Compliance |
|-------|---------------------|
| 001 Machine Registry | Infrastructure layer (correct) |
| 002 Hybrid Provider | Infrastructure layer (correct) |
| 003 Remote Transport | Infrastructure layer (correct) |
| 004 Dispatch Policy | Infrastructure layer (correct) |
| 005 Distributed Beads | Infrastructure layer (correct) |
| 006 Events | Needs care — event bus is city-layer but transport is infra |
| 007 Mail | City layer stores, infra routes (correct) |
| 008 Health | City layer checks, infra reaches (correct) |
| 009 Sessions | Infra tracks location, city sees sessions (correct) |
| 011 Dolt Config | Infrastructure bug (correct location, broken wiring) |
| 012 Nudge | City sends, infra routes (correct) |
| 013 Security | Pure infrastructure (correct) |
