---
title: "Session Lifecycle Domain Cleanup Plan"
---

| Field | Value |
|---|---|
| Status | Implemented with boundary hardening |
| Date | 2026-04-15 |
| Owner | Codex |
| Tracking | `mc-nte1eb`; hardening follow-up `mc-tkxblx` |
| Parent design | `session-model-unification` |

## Purpose

This plan closes the gap between the accepted session model design and the
current branch implementation. The branch already has broad Phase 0 coverage,
a pure `ComputeAwakeSet` decision core, and compatibility behavior for existing
metadata. The remaining problem is that lifecycle meaning is still distributed
across raw bead metadata strings, bridge code, reconciler repair paths, manager
helpers, and API/CLI writers.

The implementation target is a single lifecycle domain boundary that can project
stored session metadata and runtime facts into typed state, then produce explicit
metadata patches for state transitions. Existing metadata remains compatible
while interpretation and writes move behind clearer abstractions.

## Working Rules

Every behavior-changing slice starts with a failing test. The expected rhythm is
red, green, refactor: first prove the missing behavior or abstraction with a
test, then add the smallest implementation, then clean up while keeping the same
test green.

At natural coding boundaries, run the `$review-pr` / fix loop before moving to
the next phase. A boundary is reached when a phase is behaviorally complete,
tests for that phase are green, and the next work would expand the touched
surface area. Continue only after blockers and majors are cleared.

The live work tracker is bead `mc-nte1eb`. This document records the plan and
the intended order, not task status.

## Current Gaps

The accepted design defines base lifecycle state, desired state, identity
projection, blockers, and wake causes. The current implementation only has a
partial `session.State` enum and multiple consumers still read or write raw
`state` metadata directly.

`ComputeAwakeSet` is a clean decision core, but its input bridge still owns too
much lifecycle interpretation. That makes rare states such as post-restart
runtime loss, stale `creating`, quarantine, named-session conflicts, and
duplicate canonical beads harder to reason about consistently.

The reconciler still performs liveness healing, drain handling, config drift,
restart, close, and repair decisions procedurally. Those decisions should depend
on a shared lifecycle view rather than local string checks.

Generation, continuation epoch, instance token, and `pending_create_claim` exist
as fencing fields, but there is no single transition contract that states when
those fields must be set, preserved, or cleared.

## Phase 1: Characterize Existing Lifecycle Semantics

Add failing tests first for the lifecycle projection we want, using current
metadata combinations as fixtures. Cover base states, legacy observed states,
runtime liveness, blockers, duplicate identities, reserved named identities, and
post-restart repair states.

Likely files:

- `internal/session/lifecycle_projection_test.go`
- `cmd/gc/compute_awake_bridge_test.go`
- `cmd/gc/session_reconcile_test.go`
- `cmd/gc/session_reconciler_test.go`

Acceptance for this phase is a set of focused tests that fail because the typed
projection API does not exist yet or because existing behavior is not expressed
through that API.

## Phase 2: Add Read-Only Lifecycle Projection

Introduce a lifecycle projection in `internal/session` that accepts stored
session facts, runtime facts, and optional named-config facts. It should expose
typed values for base state, desired state, identity projection, blockers, wake
causes, and transition eligibility.

The projection must normalize compatibility states for reading without forcing a
metadata migration. Examples include treating legacy `awake` as active for
behavioral purposes and recognizing terminal or historical states that are not
currently present in the narrow `State` enum.

Likely files:

- `internal/session/lifecycle_projection.go`
- `internal/session/lifecycle_projection_test.go`
- `internal/session/manager.go`

Acceptance for this phase is that the new projection tests pass without changing
the stored metadata contract.

## Phase 3: Move Awake Input Construction Onto Projection

Keep `cmd/gc/compute_awake_set.go` pure. Change
`cmd/gc/compute_awake_bridge.go` so it parses bead metadata through the lifecycle
projection, then adapts the result into the existing awake-set input.

Acceptance for this phase is unchanged awake decisions with fewer duplicated
metadata interpretations in the bridge.

## Phase 4: Add Transition Patch Builders

Add typed transition helpers that produce metadata patches rather than writing
directly. Initial transitions should cover request wake, confirm start, fail
start, begin drain, acknowledge drain, suspend, sleep, archive, quarantine,
reactivate, close, duplicate repair, and expired-blocker cleanup.

Each transition test should assert all fields that matter for reconciliation:
`state`, `state_reason`, `held_until`, `quarantined_until`,
`pending_create_claim`, `continuity_eligible`, wake/sleep timestamps, generation,
continuation epoch, and instance token.

Likely files:

- `internal/session/lifecycle_transition.go`
- `internal/session/lifecycle_transition_test.go`
- `internal/session/manager.go`

Acceptance for this phase is that lifecycle mutations can be tested as pure
patch construction without a running city or runtime provider.

## Phase 5: Migrate High-Risk Writers

Migrate direct state writers in the highest-risk paths first: reconcile healing,
start confirmation and rollback, drain acknowledgment, restart request, config
drift, idle timeout, close, orphan handling, and quarantine handling.

Likely files:

- `cmd/gc/session_reconcile.go`
- `cmd/gc/session_reconciler.go`
- `cmd/gc/session_lifecycle_parallel.go`
- `internal/session/manager.go`

Acceptance for this phase is behavior-preserving migration with the same
reconcile and lifecycle tests green.

## Phase 6: Move User-Facing Consumers To Lifecycle Views

Move status, doctor, API, and CLI surfaces that explain session lifecycle onto
the shared projection. These consumers should report the same concepts the
controller uses: desired-running, desired-asleep, desired-blocked,
reserved-unmaterialized, conflict, quarantine, stale creating, and runtime
missing.

Likely files:

- `cmd/gc/doctor_session_model.go`
- `cmd/gc/cmd_session_pin.go`
- `internal/api/session_resolution.go`
- `cmd/gc/session_resolve.go`

Acceptance for this phase is that user-facing lifecycle explanations come from
the projection instead of ad hoc metadata reads.

## Phase 7: Guard The Boundary

After writers have moved, make raw state mutation intentionally visible. Add a
small guard test or lint-style unit that documents the allowed compatibility
shims and fails when new direct `state` writes appear outside the lifecycle
transition layer.

Acceptance for this phase is that future session lifecycle changes have a clear
place to live.

## Boundary Hardening Audit

The implemented boundary is now:

- `internal/session/lifecycle_projection.go` owns shared interpretation of
  session lifecycle metadata for API, CLI, and controller-facing code.
- `internal/session/lifecycle_transition.go` owns shared metadata patches for
  high-risk lifecycle transitions.
- `internal/session/lifecycle_projection_test.go` has guard tests for
  user-facing consumers and high-risk writer drift.

Remaining direct metadata access is intentional in these categories:

- Storage construction and identity compatibility in `internal/session/manager.go`,
  `internal/session/resolve.go`, `internal/session/names.go`, and
  `internal/session/named_config.go`. These paths create session beads, read
  legacy aliases, or maintain runtime `session_name` compatibility.
- Controller adapter and reconciler code in `cmd/gc/compute_awake_bridge.go`,
  `cmd/gc/build_desired_state.go`, `cmd/gc/session_reconcile.go`,
  `cmd/gc/session_reconciler.go`, and `cmd/gc/session_lifecycle_parallel.go`.
  These files may read raw metadata while assembling runtime facts or trace
  payloads, but high-risk transitions should use lifecycle patch helpers.
- Materialization and repair code in `cmd/gc/session_beads.go`,
  `cmd/gc/session_name_lookup.go`, and `cmd/gc/adoption_barrier.go`. These paths
  are allowed to write initial storage metadata or apply compatibility repair,
  but retire/archive/wake transitions should use patch builders.
- Wait and nudge state machines in `internal/session/waits.go`,
  `cmd/gc/cmd_wait.go`, `cmd/gc/cmd_nudge.go`, and `cmd/gc/nudge_beads.go`.
  Their `state` metadata belongs to wait/nudge beads, not session lifecycle
  beads.

The `gc doctor` diagnostic false-negative around already archived
continuity-ineligible beads with legacy identifiers is intentionally out of
scope for this hardening pass. It is diagnostic-only and does not change the
session lifecycle transition contract.

## Verification Gates

After each implementation slice, run the narrow package tests for the touched
area. At each natural phase boundary, run `$review-pr` and fix blockers or
majors before continuing into the next phase. Before the branch is considered
ready, run:

```bash
go test ./internal/config ./internal/session ./internal/docgen ./internal/api ./cmd/gc
```

Before review, run the PR review/fix loop until no blockers or majors remain.

## Risks

Lifecycle projection can easily create import cycles if it imports reconciler,
runtime, or config packages directly. Keep the projection API built from small
plain fact structs.

Normalization can accidentally change compatibility behavior. Characterization
tests must pin observed behavior before rewiring consumers.

The PR is already large. Each phase should leave the system green and avoid
formatting churn outside files being changed for the current slice.
