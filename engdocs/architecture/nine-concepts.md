---
title: "Nine Concepts"
---

> Last verified against code: 2026-03-01

## Summary

Gas City has five irreducible primitives and four derived mechanisms.
Removing any primitive makes it impossible to build a multi-agent
orchestration system. Every derived mechanism is provably composable
from the primitives. This document maps the nine concepts to their
implementations and links to their detailed architecture docs.

## The Primitive Test

Before adding a new primitive, apply three necessary conditions (see
[`engdocs/contributors/primitive-test.md`](../contributors/primitive-test.md)):

1. **Atomicity** — can it be decomposed into existing primitives? If
   yes, it's derived, not primitive.
2. **Bitter Lesson** — does it become MORE useful as models improve?
   If it becomes less useful, it fails.
3. **ZFC** — does Go handle transport only, with no judgment calls?
   If Go makes decisions, it's a violation.

## Layer 0-1: Primitives

These are irreducible. Each has a dedicated architecture doc.

### 1. Agent Protocol

Start/stop/prompt/observe agents regardless of session provider.
Covers identity, pools, sandboxes, resume, and crash adoption.

- **Interface**: `runtime.Provider` with naming and startup hints from
  `internal/agent/`
- **Implementations**: tmux (production), subprocess (remote),
  k8s (Kubernetes), Fake (test)
- **Key insight**: The SDK manages agent lifecycle. The prompt defines
  agent behavior. These concerns never cross.

**Details**: [Agent Protocol](agent-protocol.md)

### 2. Task Store (Beads)

CRUD + parent-child + dependencies + labels + query over work units.
Everything is a bead: tasks, mail, molecules, convoys, and epics.

- **Interface**: `beads.Store` with Create, Get, Update, Close, List,
  Ready, Children, ListByLabel, SetMetadata, MolCook
- **Implementations**: BdStore (production, Dolt-backed), FileStore,
  MemStore, exec Store
- **Key insight**: Beads is the universal persistence substrate.
  All domain state flows through a single interface.

**Details**: [Bead Store](beads.md)

### 3. Event Bus

Append-only pub/sub log of all system activity. Two tiers: critical
(bounded queue for infrastructure) and optional (fire-and-forget for
audit).

- **Interface**: `events.Provider` with Record, List, LatestSeq,
  Watch, Close
- **Storage**: `.gc/events.jsonl` (JSONL format)
- **Key insight**: Events are immutable. Seq is monotonically
  increasing. Watch() provides reactive notification without polling.

**Details**: [Event Bus](event-bus.md)

### 4. Config

TOML parsing with progressive activation (Levels 0-8 from section
presence) and multi-layer override resolution.

- **Entry point**: `config.Load()` / `config.LoadWithIncludes()`
- **Key types**: City, Agent, Rig, ProviderSpec, Pack
- **Key insight**: Config IS the feature flag. An empty `city.toml`
  gives Level 0-1. Adding sections activates capabilities. No feature
  flags, no capability flags — the config presence is sufficient.

**Details**: [Config System](config.md)

### 5. Prompt Templates

Go `text/template` in Markdown defining what each role does. The
behavioral specification.

- **Entry point**: `renderPrompt()` in `cmd/gc/prompt.go`
- **Template data**: PromptContext with city, agent, rig, git metadata
- **Key insight**: All role behavior is user-supplied configuration.
  The SDK contains zero hardcoded role names.

**Details**: [Prompt Templates](prompt-templates.md)

## Layer 2-4: Derived Mechanisms

Each is provably composed from primitives. The derivation proof for
each mechanism shows which primitives it uses and why no new
infrastructure is needed.

### 6. Messaging

Mail + nudge. No new primitive needed.

- **Mail derivation**: `beads.Store.Create(Bead{Type:"message"})` →
  message is a bead. Inbox = query open message beads by assignee.
  Archive = close the bead.
- **Nudge derivation**: `runtime.Provider.Nudge(text)` → text typed
  into the agent's session. Fire-and-forget.
- **Proof**: Mail uses only Bead Store (primitive 2). Nudge uses only
  Agent Protocol (primitive 1). No new infrastructure.

**Details**: [Messaging](messaging.md)

### 7. Formulas & Molecules

Formula = TOML discovered through formula layers. Molecule = provider-backed
runtime bead tree. Wisps = ephemeral molecules. Orders =
formulas with gate conditions on Event Bus.

- **Formula derivation**: Config (primitive 4) resolves formula layers and
  active files.
- **Molecule derivation**: Bead Store (primitive 2) holds the root bead and
  any provider-created step beads.
- **Wisp derivation**: Molecule + TTL + garbage collection.
- **Order derivation**: Formula + Event Bus (primitive 3) gate
  evaluation + Config (primitive 4) scheduling.
- **Proof**: Uses Config, Bead Store, and Event Bus. No new
  infrastructure.

**Details**: [Formulas & Molecules](formulas.md) |
[Orders](orders.md)

### 8. Dispatch (Sling)

Find/spawn agent → select formula → create molecule → hook to agent →
nudge → create convoy → log event.

- **Derivation**: Agent Protocol (find/spawn) + Config (select formula)
  + Bead Store (create molecule, convoy) + Agent Protocol (nudge) +
  Event Bus (log event).
- **Proof**: Pure composition of primitives 1-4. No new infrastructure.

**Details**: [Dispatch](dispatch.md)

### 9. Health Patrol

Ping agents (Agent Protocol), compare thresholds (Config), publish
stalls (Event Bus), restart with backoff.

- **Derivation**: Agent Protocol (primitive 1) for liveness. Config
  (primitive 4) for thresholds and backoff parameters. Event Bus
  (primitive 3) for stall publication.
- **Proof**: Uses Agent Protocol, Config, and Event Bus. The
  controller drives all operations — no user-configured agent role
  is required.

**Details**: [Health Patrol](health-patrol.md)

## Layering Invariants

These hold across all nine concepts:

1. **No upward dependencies.** Layer N never imports Layer N+1.
2. **Beads is the universal persistence substrate** for domain state.
3. **Event Bus is the universal observation substrate.**
4. **Config is the universal activation mechanism.**
5. **Side effects (I/O, process spawning) are confined to Layer 0.**
6. **The controller drives all SDK infrastructure operations.**
   No SDK mechanism may require a specific user-configured agent role.

## Progressive Capability Model

Capabilities activate based on config section presence:

| Level | Config Required | Adds |
|---|---|---|
| 0-1 | `[workspace]` + `[[agent]]` | Agent + tasks |
| 2 | `[daemon]` | Task loop (controller) |
| 3 | `[[agent]]` with `[agent.pool]` | Multiple agents + pool |
| 4 | `[mail]` | Messaging |
| 5 | Formula files + `[formulas]` | Formulas & molecules |
| 6 | `[daemon]` health fields | Health monitoring |
| 7 | `orders/` directories | Orders |
| 8 | All sections | Full orchestration |

## See Also

- [Glossary](glossary.md) — authoritative definitions of all terms
  used across the nine concepts
- [Primitive Test](../contributors/primitive-test.md) — the three necessary
  conditions for adding a new primitive
- [CLAUDE.md](https://github.com/gastownhall/gascity/blob/main/CLAUDE.md) — project-level design principles and
  code conventions
