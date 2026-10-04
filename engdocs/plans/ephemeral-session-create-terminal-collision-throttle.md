# Plan: Throttle terminal ephemeral session-create collision retries

> Owner: `gascity/pm` - Created: 2026-07-30
> Source: `ga-yq48qr.2`
> Parent architecture ruling: `ga-yq48qr`
> Decomposed into: one architecture decision, one validator task, and one
> builder task

## Context

A terminal `work_dir_collision` currently ends one failed session-create bead
but does not limit creation of its successor. Generic ephemeral capacity demand
is recalculated on every reconciler tick, so the same unresolved collision can
produce another terminal failed-create bead immediately. One CI run produced
989 failed-create beads with the same agent, resolved directory, ephemeral
origin, and terminal error.

This plan separates the unresolved architecture policy from test authoring and
implementation. It is independent of `ga-yq48qr.1`, which addresses work
directory isolation rather than repeated session-create materialization.

Tracker import was a no-op because no `tracker-to-beads` skill, sibling tracker
skill, or `tracker-to-beads` command is installed in this PM worktree.

## User outcome

One persistent terminal collision cannot flood the bead ledger on every
reconciler tick. Gas City still retries legitimate ephemeral capacity demand
when the approved policy makes it eligible, and distinct demand is not
suppressed accidentally.

## Work packages

| ID | User story | Routing | Depends on |
| --- | --- | --- | --- |
| `ga-yq48qr.2.1` | As a maintainer, I have an approved cooldown design for repeated terminal ephemeral session-create collisions | `needs-architecture` → `gascity/architect` | — |
| `ga-yq48qr.2.2` | As a maintainer, regression tests prove terminal collision retries are throttled without suppressing valid demand | `needs-tests` → `gascity/validator` | `ga-yq48qr.2.1` |
| `ga-yq48qr.2.3` | As an operator, repeated terminal ephemeral session-create collisions stop flooding the bead ledger | `ready-to-build` → `gascity/builder` | `ga-yq48qr.2.1`, `ga-yq48qr.2.2` |

## Acceptance rollup

The downstream work is complete when:

- The architecture traces the full desired-state path that converts generic
  ephemeral capacity demand into fresh session-create bead materialization.
- The design resolves the suppression signature, ownership boundary,
  policy/window, persistent timestamp source, concurrency semantics,
  observability, and store-error behavior.
- The design applies the Primitive Test explicitly, separating factual
  transport checks in Go from any policy or judgment supplied by
  configuration.
- Deterministic tests fail on the current per-tick behavior and prove bounded
  creation for repeated same-signature terminal collisions.
- Tests prove retry eligibility at the approved boundary and prevent false
  suppression across changed signatures, other origins, and other error
  classes.
- The implementation reads existing failed-create bead history as persistent
  state. It introduces no status file, hardcoded role, or process-local source
  of truth.
- The final change passes the focused regression suite, the fast unit baseline,
  `go vet ./...`, and any config-generation, docs, or integration gates named
  by the approved architecture.

## Dependency graph

```text
ga-yq48qr.2.1  architecture decision
       |
       v
ga-yq48qr.2.2  deterministic red regression tests
       |
       v
ga-yq48qr.2.3  production implementation and verification
```

The implementation also depends directly on the architecture decision so later
test edits cannot obscure the approved contract.

## Routing rationale

The source bead names unresolved backend policy and ownership decisions, so the
first package routes to the architect. The validator then authors failing tests
against that approved contract. The builder receives a build-ready contract and
red suite rather than being asked to choose policy while implementing it.

Only `ga-yq48qr.2.1` is actionable initially. The blocked validator and builder
packages receive context by mail and are not slung until their dependencies
clear.

## Risks

- An over-broad signature could suppress valid capacity recovery. The
  architecture and validator packages require changed-signature and
  out-of-scope-origin coverage.
- An under-broad signature could leave the ledger flood unchanged. The
  regression baseline must exercise repeated reconciler demand after a
  terminal failed-create result.
- A bead-history lookup could replace a write storm with an expensive read
  storm. The design must state the query boundary and performance expectation
  before implementation.
- Concurrent reconciler passes could both decide a retry is eligible. The
  design must state idempotence or atomicity requirements rather than leaving
  that race to implementation guesswork.
- Timestamp ambiguity or wall-clock sleeps could make tests flaky. Validator
  coverage must use deterministic time control and test the exact eligibility
  boundary.
- `ga-yq48qr.1` may remove the observed core-pack collision while leaving this
  generic retry gap latent. Neither package is allowed to block on or absorb
  the other.

## Out of scope

- Selecting or implementing the work directory isolation mechanism from
  `ga-yq48qr.1`.
- Weakening terminal classification for one failed-create bead.
- Adding a role-specific exception for any pack-defined agent.
- Introducing status files or a process-local cache as authoritative retry
  state.
- UI or dashboard work unless the approved architecture later identifies a
  concrete user-facing requirement.
