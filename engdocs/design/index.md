---
title: Design Docs
description: Forward-looking proposals and historical design context for Gas City.
---

Design docs describe how Gas City should work in the future. Current behavior
lives in the [Architecture](../architecture/index.md) section.

## Status Meanings

- `Accepted`: approved direction
- `Implemented`: code landed, doc kept for context
- `Proposed`: drafted direction pending approval

## Current Design Set

| Document | Status | Notes |
|---|---|---|
| `machine-wide-supervisor-v0` | Accepted | Current supervisor direction |
| `convoy-first-formulas-and-drain-v0` | Implemented | Convoy-first graph.v2 formula inputs and drain scatter semantics |
| `api-ops-design` | Implemented | State-mutation API surface |
| `agent-pools` | Implemented | Feature shipped before the current template existed |
| `dependency-aware-bounded-parallel-lifecycle` | Implemented | Bounded parallel start/stop waves for session lifecycle |
| `beads-dolt-contract-redesign` | Accepted | Canonical bd+Dolt contract, topology commands, migration, and provider-boundary redesign |
| `idle-session-sleep` | Accepted | Idle-sleep policy, precedence, and wake mechanics |
| `runtime-partial-discipline` | Accepted (source-level), follow-ups Proposed | Treat a failed tmux-liveness observation as partial (defer destructive arms) instead of "nothing running"; mirrors storeQueryPartial |
| `idle-claim-nudge-followups` | Proposed | Widen the stalled-pool-claim backstop key to unassigned pool-routed beads (the case the tmux warm-slot un-gate does not cover) |
| `idle-controller-call-rate` | Proposed | Layer-3 (#2463/#3543) cut of the controller's idle bd/Dolt call *rate*: demand-gated ticking, per-pass snapshot, quiescent-scope skipping |
| `session-store-fences` | Accepted | Cross-process write fences for session-owned metadata: store facts, flock and token-reread fences, residual convergence-through-persistence |
| `named-configured-sessions` | Accepted | Explicit canonical named sessions backed by reusable templates; partially superseded by `session-model-unification` |
| `session-model-unification` | Accepted | Unified post-pool session model: config factories, canonical named identities, exact session ownership, and `scale_check`-only controller demand |
| `session-lifecycle-domain-cleanup-plan` | Implemented with hardening | Red-green-refactor plan for centralizing session lifecycle projection and transition writes behind typed abstractions |
| `pack-import-export-surface` | Proposed | Replace `transitive` / `export` with explicit imports plus exports |
| `external-messaging-fabric` | Implemented | Provider-neutral external conversation bindings, delivery context, and group sessions |
| `external-messaging-shared-threads` | Implemented | Transcript-backed shared-thread model with membership replay and speaker-only group routing |
| `worker-conformance` | Proposed | Canonical WorkerCore/WorkerInference contract, transcript-first conformance, and migration toward `internal/worker` |
| `two-minute-ci-blacksmith` | Proposed | Planner-driven Blacksmith CI architecture targeting two-minute required PR feedback |
| `runtime-provider-packs` | Proposed | Runtime providers as pack-shipped executables speaking a versioned protocol (RPP); agent-provider specs as pack TOML; cloudflare-first PoC |
| `packv2/` | Historical / rollout ledger | PackV2 engineering design notes moved out of public docs; use user-facing guides and generated reference for current authoring guidance |
