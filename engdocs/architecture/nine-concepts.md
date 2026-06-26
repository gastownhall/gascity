---
title: "Code-layering View (implements the six primitives)"
---

> Last verified against code: 2026-04-25

> **The authoritative user-facing model is [the six primitives](../../docs/getting-started/how-gas-city-works.md)** —
> Agent (who), Bead (what), Formula (how), Rig (where), Pack (configures),
> and Event (observe). Read that first. This document is the deeper
> *implementation-layering* reference: it maps the Go code substrate
> (sessions, the bead store, the event bus, config, prompt templates, and
> the derived dispatch/health machinery) onto those six primitives, and
> records the layering invariants and Primitive Test that CI depends on.
> It does not introduce a competing taxonomy.

## Summary

This document maps Gas City's code substrate onto the six user-facing
primitives. The product is v2 orchestration: you write a Formula and the
controller runs it as a graph of beads — decomposing the job, fanning ready
work out to many agents at once, gating each step on its dependencies,
retrying failures, draining convoys in parallel, and driving the graph to
completion outside any user session. That orchestration is implemented by
the control dispatcher in `internal/dispatch` executing control beads
(check, retry, fan-out, tally, drain, scope-check, workflow-finalize). A
central design property is that this orchestration is composed entirely from
the substrate primitives with zero hardcoded roles — which is why the same
SDK can express Gas Town, Ralph, or any other pack. Composability here means
substitutable, not small. Every concept here links to its detailed
architecture doc.

### Code substrate → six-primitive mapping

| Code substrate (this doc)                                      | User-facing primitive |
| -------------------------------------------------------------- | --------------------- |
| Session + Prompt Templates                                     | **Agent** (who)       |
| Task Store (Beads)                                             | **Bead** (what)       |
| Formulas + Molecules + Dispatch (Sling) + Orders + Health Patrol | **Formula** (how)   |
| _(rigs register projects with the city)_                       | **Rig** (where)       |
| Config                                                         | **Pack** (configures) |
| Event Bus                                                      | **Event** (observe)   |

The substrate has no separate layer for **Rig** (a project/repo registered
with the city) — it is a config-declared location that work runs in. The
City is the local (root) pack; it imports shared packs.

## The Primitive Test

Before adding a new primitive, apply three necessary conditions (see
[`engdocs/contributors/primitive-test.md`](../contributors/primitive-test.md)):

1. **Atomicity** — can it be decomposed into existing primitives? If
   yes, it's derived, not primitive.
2. **More useful as models improve** — does it become MORE useful as
   models improve? If it becomes less useful, it fails.
3. **Judgment out of Go** — does Go handle transport only, with no
   judgment calls? If Go makes decisions, it's a violation.

## Layer 0-1: Substrate (Agent, Bead, Event, Pack)

These layers are irreducible. Each has a dedicated architecture doc. They
implement four of the six primitives: Session + Prompt Templates render the
**Agent**; the Task Store holds every **Bead**; the Event Bus delivers
**Events**; and Config is how a **Pack** activates capabilities.

### 1. Session — under the Agent primitive

Start/stop/prompt/observe sessions regardless of provider. Covers
identity, pools, sandboxes, resume, and crash adoption.

- **Interface**: `runtime.Provider` (low-level) plus
  `internal/session/` for bead-backed lifecycle and naming/startup
  hints from `internal/agent/`
- **Implementations**: tmux (production), subprocess (remote),
  exec (script), k8s (Kubernetes), Fake (test); acp / auto / hybrid
  routing layers compose these
- **Key insight**: The SDK manages session lifecycle. The prompt
  defines agent behavior. These concerns never cross.

**Details**: [Session](session.md)

> **History.** This primitive was named "Agent Protocol" and exposed
> a dedicated `agent.Agent` / `agent.Handle` interface until commit
> `dd90ac0a` (Mar 8 2026). The interface was removed; responsibilities
> live in `internal/session/` and `internal/runtime/`.

### 2. Task Store (Beads) — under the Bead primitive

CRUD + parent-child + dependencies + labels + query over work units.
Everything is a bead: tasks, mail, convoys, and epics (and the v1 molecule
materialization of a formula run).

- **Interface**: `beads.Store` with Create, Get, Update, Close, List,
  Ready, Children, ListByLabel, SetMetadata, MolCook
- **Implementations**: BdStore (production, Dolt-backed), FileStore,
  MemStore, exec Store
- **Key insight**: Beads is the universal persistence substrate.
  All domain state flows through a single interface.

**Details**: [Bead Store](beads.md)

### 3. Event Bus — under the Event primitive

An **Event** is the OBSERVE primitive: fired by activity so humans and
agents can watch. The "bus" is the delivery machinery beneath it — an
append-only log that carries fired events to subscribers. Two tiers:
critical (bounded queue for infrastructure) and optional (fire-and-forget
for audit).

- **Interface**: `events.Provider` with Record, List, LatestSeq,
  Watch, Close
- **Storage**: `.gc/events.jsonl` (JSONL format)
- **Key insight**: Events are immutable outbound notifications. Seq is
  monotonically increasing. Watch() delivers fired events reactively
  without polling.

**Details**: [Event Bus](event-bus.md)

### 4. Config — under the Pack primitive

A **Pack** is the CONFIGURES primitive: it declares agents, formulas, and
orders. The City is the local (root) pack; it imports shared packs. Config
is the machinery that activates a pack's capabilities — TOML parsing with
progressive activation (Levels 0-8 from section presence) and multi-layer
override resolution.

- **Entry point**: `config.Load()` / `config.LoadWithIncludes()`
- **Key types**: Pack (declares agents/formulas/orders), City (the local
  pack as deployed), Agent, Rig, ProviderSpec
- **Key insight**: Pack config IS the feature flag. An empty `city.toml`
  gives Level 0-1. Adding sections activates capabilities. No feature
  flags, no capability flags — the config presence is sufficient.

**Details**: [Config System](config.md)

### 5. Prompt Templates — under the Agent primitive

Go `text/template` in Markdown defining what each agent does. The
behavioral-spec facet of the **Agent** primitive: a pack supplies the
templates, and they render into a running agent.

- **Entry point**: `renderPrompt()` in `cmd/gc/prompt.go`
- **Template data**: PromptContext with city, agent, rig, git metadata
- **Key insight**: All agent behavior is pack-supplied configuration.
  Role names are config facets of an Agent, never Go source — the SDK
  contains zero hardcoded role names.

**Details**: [Prompt Templates](prompt-templates.md)

## Layer 2-4: Composed machinery (Formula and messaging)

The machinery in this section — Formulas, Dispatch, Orders, Health Patrol —
is Gas City's orchestration engine. It is built entirely from the substrate
primitives with zero hardcoded roles, which is what lets one SDK express
many orchestration packs. Each entry notes which primitives it composes;
that composability is a substitution property, not a statement that the
machinery is minor. The control dispatcher driving a formula graph to
completion is substantial work that happens outside any user session.
**Formula** is a user-facing primitive (the HOW): the Formulas, Dispatch
(Sling), Orders, and Health Patrol machinery below all implement it — they
are not a separate "derived" concept. Messaging composes the Agent and Bead
substrate.

### 6. Messaging — composes the Bead and Agent substrate

Mail + nudge. No new substrate needed.

- **Mail derivation**: `beads.Store.Create(Bead{Type:"message"})` →
  message is a bead. Inbox = query open message beads by assignee.
  Archive = close the bead.
- **Nudge derivation**: `runtime.Provider.Nudge(text)` → text typed
  into the agent's session. Fire-and-forget.
- **Proof**: Mail uses only Bead Store (primitive 2). Nudge uses only
  Session (primitive 1). No new infrastructure.

**Details**: [Messaging](messaging.md)

### 7. Formula — the HOW primitive

A **Formula** is the HOW: a method for getting a job done. Under the v2
contract the compiler turns a formula into a flat graph — a workflow root
plus independently routable step beads plus controller-owned control beads
(check, retry, fan-out, tally, drain, scope-check, workflow-finalize). The
controller's control dispatcher (`internal/dispatch`) executes that graph:
gating each step on its dependencies, fanning ready work out to many agents
or pools at once, retrying failures, draining convoys in parallel, and
finalizing the workflow — all outside any user session. v1 remains a
supported peer shape: a single agent running the steps as a sequence inside
its own session, materialized as a molecule (a root bead plus child step
beads; ephemeral runs are wisps). A formula is the method, not the work —
the beads are the work. An **Order** automates WHEN a formula runs (Health
Patrol is one kind of order).

- **Formula resolution**: Config (the Pack substrate) resolves formula
  layers and active files.
- **Run materialization**: the Bead Store (the Bead substrate) holds the
  workflow root, step beads, and control beads a run produces.
- **Graph execution**: the control dispatcher (`internal/dispatch`) drives
  the compiled graph to completion via control beads.
- **Order automation**: a formula plus Event Bus (the Event substrate)
  trigger evaluation plus Config (the Pack substrate) scheduling.
- **Composition**: Built from Config, the Bead Store, and the Event Bus
  with zero hardcoded roles — which is what makes the orchestration
  substitutable across packs, not a sign that it is small.

**Details**: [Formulas](formulas.md) | [Orders](orders.md)

### 8. Dispatch (Sling) — kicks off a Formula run

Sling kicks off a formula run: select the formula, compile and materialize
it as a graph of beads, create the convoy, and log the event. For a v1
formula the run is then worked by the single slung agent in its own session.
For a v2 formula, slinging hands the compiled graph to the controller's
control dispatcher, which gates, fans out, retries, and drains the work
across many agents until the workflow finalizes — no further user
involvement.

- **Derivation**: Session (find/spawn) + Config (select formula)
  + Bead Store (compile and materialize the graph, create convoy) +
  Session (nudge) + Event Bus (log event); for v2, the control dispatcher
  (`internal/dispatch`) then drives the graph to completion.
- **Composition**: Built from the substrate layers with zero hardcoded
  roles, so the same kickoff path serves any pack.

**Details**: [Dispatch](dispatch.md)

### 9. Health Patrol — one kind of Order

Probe sessions (Session), compare thresholds (Config), publish stalls
(Event Bus), restart with backoff. Health Patrol is one kind of order: it
automates WHEN a remediation formula runs.

- **Derivation**: Session for liveness. Config for thresholds and backoff
  parameters. Event Bus for stall publication.
- **Proof**: Uses Session, Config, and the Event Bus. The controller
  drives all operations — no user-configured agent role is required.

**Details**: [Health Patrol](health-patrol.md)

## Layering Invariants

These hold across every layer of the substrate:

1. **No upward dependencies.** Layer N never imports Layer N+1.
2. **Beads is the universal persistence substrate** for domain state.
3. **Events are the universal outbound notification.** Activity fires
   events so humans and agents can watch; the bus is delivery machinery.
4. **Config is the universal activation mechanism.**
5. **Side effects (I/O, process spawning) are confined to Layer 0.**
6. **The controller drives all SDK infrastructure operations.**
   No SDK mechanism may require a specific user-configured agent role.

## Progressive Capability Model

Capabilities activate based on config section presence:

| Level | Config Required | Adds |
|---|---|---|
| 0-1 | `[workspace]` + `[[agent]]` | Session + tasks |
| 2 | `[daemon]` | Task loop (controller) |
| 3 | `[[agent]]` with `[agent.pool]` | Multiple agents + pool |
| 4 | `[mail]` | Messaging |
| 5 | Formula files + `[formulas]` | Formulas |
| 6 | `[daemon]` health fields | Health monitoring |
| 7 | `orders/` directories | Orders |
| 8 | All sections | Full orchestration |

## See Also

- [Primitives](../../docs/getting-started/how-gas-city-works.md) — the six-primitive user-facing model
  (Agent, Bead, Formula, Rig, Pack, Event); **start here**
- [Glossary](glossary.md) — authoritative definitions of all terms used
  across the architecture docs
- [Primitive Test](../contributors/primitive-test.md) — the three necessary
  conditions for adding a new primitive
- [CLAUDE.md](https://github.com/gastownhall/gascity/blob/main/CLAUDE.md) — project-level design principles and
  code conventions
