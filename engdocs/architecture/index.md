---
title: Architecture Overview
description: Current-state subsystem documentation for Gas City.
---

Current-state documentation for Gas City's subsystems. Each document
describes how the subsystem works **today**, not how we wish it worked.
For proposed changes, write a design doc in [`engdocs/design/`](../design/).

## Reading Order

Start with the overview, then dive into the subsystem you need.

### Foundation

1. **[Glossary](./glossary.md)** — authoritative definitions of all terms
2. **[Nine Concepts Overview](./nine-concepts.md)** — the 5 primitives + 4
   derived mechanisms that compose Gas City

### Layer 0-1: Primitives

These are irreducible. Removing any makes it impossible to build a
multi-agent orchestration system.

3. **[Bead Store](./beads.md)** — universal persistence substrate for all
   work units (tasks, mail, molecules, convoys)
4. **[Event Bus](./event-bus.md)** — append-only pub/sub log of all system
   activity
5. **[Config System](./config.md)** — TOML loading, progressive activation,
   multi-layer override resolution
6. **[Agent Protocol](./agent-protocol.md)** — agent lifecycle backed by
   session providers (tmux, subprocess, k8s)
7. **[Prompt Templates](./prompt-templates.md)** — Go `text/template` in
   Markdown defining role behavior

### Layer 2-4: Derived Mechanisms

Each is provably composable from the primitives.

8. **[Messaging](./messaging.md)** — inter-agent mail via beads + nudge
   via agent protocol
9. **[Formulas & Molecules](./formulas.md)** — work definitions (TOML) and
   their runtime instances (bead trees)
10. **[Dispatch](./dispatch.md)** — sling: agent selection + formula
    instantiation + convoy creation
11. **[Health Patrol](./health-patrol.md)** — supervision model,
    reconciliation, crash tracking, idle detection

### Infrastructure

12. **[Controller](./controller.md)** — the main loop: config watch,
    reconciliation tick, order dispatch
13. **[Orders](./orders.md)** — gate-conditioned formula/exec
    dispatch, rig-scoped labels

### End-to-End Traces

These trace a concrete operation through all layers. The most effective
way to understand how the system fits together.

14. **[Life of a Bead](./life-of-a-bead.md)** — create → hook → claim →
    execute → close
15. **[Life of a Molecule](./life-of-a-molecule.md)** — formula parse →
    dispatch → molecule create → step execution → completion

## Document Types

Gas City uses four document types (following CockroachDB's tech-note /
RFC distinction):

| Type | Directory | Purpose | Lifecycle |
|---|---|---|---|
| Architecture doc | `engdocs/architecture/` | How it works **now** | Living; update when code changes |
| Design doc | `engdocs/design/` | How we **want** it to work | Proposal → accepted → implemented → obsolete |
| Reference doc | `docs/reference/` | Exhaustive lookup (CLI, config, API) | Must stay in sync; partially generated |
| Tutorial | `docs/tutorials/` | Learning path with exercises | Ordered progression |

## Conventions

- **Code references** use repo-relative paths: `internal/beads/store.go`
- **Cross-references** use descriptive link text explaining why you'd
  follow the link
- **No role names** in examples — Gas City has zero hardcoded roles
- **Invariants** are stated as testable assertions
- **Update date** at the top of each doc tracks when it was last
  verified against code
