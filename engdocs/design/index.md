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
| `api-ops-design` | Implemented | State-mutation API surface |
| `agent-pools` | Implemented | Feature shipped before the current template existed |
| `dependency-aware-bounded-parallel-lifecycle` | Implemented | Bounded parallel start/stop waves for session lifecycle |
| `beads-dolt-contract-redesign` | Accepted | Canonical bd+Dolt contract, topology commands, migration, and provider-boundary redesign |
| `idle-session-sleep` | Accepted | Idle-sleep policy, precedence, and wake mechanics |
| `named-configured-sessions` | Accepted | Explicit canonical named sessions backed by reusable templates |
| `external-messaging-fabric` | Implemented | Provider-neutral external conversation bindings, delivery context, and group sessions |
| `external-messaging-shared-threads` | Implemented | Transcript-backed shared-thread model with membership replay and speaker-only group routing |
