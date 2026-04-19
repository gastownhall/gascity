# Implementation Plan: Dolt Contract Quality Hardening

## Overview

This plan turns the current `feature/beads-dolt-contract` branch from a
bug-driven hardening effort into a branch that can plausibly score `80+`
across the agreed quality criteria. The immediate goal is not a new product
surface. The goal is to simplify ownership, remove remaining hidden
authorities, reduce caller duplication, and make lifecycle, projection, and
error handling easier to reason about.

This plan is grounded in:

- the accepted design in
  [`engdocs/design/beads-dolt-contract-redesign.md`](../design/beads-dolt-contract-redesign.md)
- the live regression inventory in
  [`engdocs/contributors/dolt-regression-audit.md`](./dolt-regression-audit.md)
- the current branch hotspots, especially
  [`examples/bd/assets/scripts/gc-beads-bd.sh`](../../examples/bd/assets/scripts/gc-beads-bd.sh),
  [`cmd/gc/beads_provider_lifecycle.go`](../../cmd/gc/beads_provider_lifecycle.go),
  [`cmd/gc/bd_env.go`](../../cmd/gc/bd_env.go), and
  [`internal/beads/contract/connection.go`](../../internal/beads/contract/connection.go).

## Baseline Scores

These are the branch scores that motivated this plan:

| Criterion | Baseline |
|---|---:|
| TDD | 73 |
| DRY | 79 |
| Separation of Concerns | 83 |
| Single Responsibility | 56 |
| Clear Abstractions | 78 |
| Low Coupling, High Cohesion | 69 |
| KISS | 60 |
| YAGNI | 77 |
| Prefer Non-Nullable | 41 |
| Prefer Async Notifications | 23 |
| Eliminate Race Conditions | 80 |
| Errors Are Not Optional | 58 |
| Idiomatic Project Layout | 74 |
| Write for Maintainability | 81 |

## Target State

The branch reaches `80+` on all criteria by making these structural changes:

- Go owns canonical `.beads/config.yaml` and `.beads/metadata.json` shaping.
- One typed contract package owns Dolt target resolution and projection.
- Callers stop interpreting ambient env or partial config on their own.
- Managed lifecycle state becomes explicit and publish/consume oriented.
- Dolt failures emit structured events instead of disappearing into local
  stderr or caller-specific messages.
- `gc-beads-bd` shrinks to a backend bridge instead of also acting as the
  canonical contract engine.

## Architecture Decisions

- Canonical contract writes happen in Go, not in `gc-beads-bd`.
- `gc-beads-bd` remains the backend bridge for Dolt SQL/server operations
  until a later replacement exists.
- Ambient `GC_DOLT_*` and `BEADS_*` remain compatibility inputs only; no new
  code may treat them as authoritative.
- A task is not complete until it has a failing regression first, code,
  targeted verification, broader verification, and review.
- The branch should improve in vertical slices. Each slice must leave the
  installed `gc` in a dogfoodable state.

## Task List

### Phase 1: Contract Ownership Cleanup

#### Task 1: Freeze the quality hardening plan and traceability documents

**Description:**
Write the execution plan for the remaining branch hardening and keep the Dolt
regression audit as the traceability source for issues and PRs.

**Acceptance criteria:**
- [ ] This plan document exists and is committed.
- [ ] The Dolt audit links every labeled Dolt issue and PR to tests and branch
      behavior.
- [ ] The branch task tracks the active phase.

**Verification:**
- [ ] `sed -n '1,260p' engdocs/contributors/dolt-quality-hardening-plan.md`
- [ ] `sed -n '1,260p' engdocs/contributors/dolt-regression-audit.md`

**Dependencies:** None

**Files likely touched:**
- `engdocs/contributors/dolt-quality-hardening-plan.md`
- `engdocs/contributors/dolt-regression-audit.md`

**Estimated scope:** Small

#### Task 2: Move canonical `.beads` file shaping out of `gc-beads-bd`

**Description:**
Make Go the only owner of canonical `config.yaml` and `metadata.json`
materialization/normalization. `gc-beads-bd` should stop constructing the
canonical file contract itself and instead consume already-normalized scope
files.

**Acceptance criteria:**
- [ ] `gc-beads-bd` no longer owns canonical config/metadata shaping logic.
- [ ] Go callers normalize canonical scope files before backend bootstrap.
- [ ] Existing canonical file regressions continue to pass.
- [ ] A new regression proves backend init still works after the ownership move.

**Verification:**
- [ ] `go test ./cmd/gc -run 'Test(NormalizeCanonicalBdScopeFiles|GcBeadsBdInit|FinalizeInitCanonicalizesBdStoreBeforeProviderReadinessBlock)' -count=1`
- [ ] `rg -n 'ensure_config_yaml|ensure_metadata|metadata_patch_json' examples/bd/assets/scripts/gc-beads-bd.sh`

**Dependencies:** Task 1

**Files likely touched:**
- `examples/bd/assets/scripts/gc-beads-bd.sh`
- `cmd/gc/beads_provider_lifecycle.go`
- `cmd/gc/init_provider_readiness.go`
- `internal/beads/contract/files.go`
- `cmd/gc/beads_provider_lifecycle_test.go`

**Estimated scope:** Medium

#### Checkpoint: Phase 1

- [ ] Canonical contract writes have one owner.
- [ ] No targeted regressions fail.
- [ ] Installed `gc` still completes `gc init`, `gc rig add`, and `gc doctor`
      in a temp city.

### Phase 2: Typed Contract and Non-Nullable Core State

#### Task 3: Introduce a normalized resolved-target type with explicit state

**Description:**
Push nullable and empty-string state to the edge. Resolve canonical config and
runtime publication into typed structs with explicit endpoint kind, status,
auth source, and availability state before any caller consumes the result.

**Acceptance criteria:**
- [ ] Core callers stop branching on empty host/port strings.
- [ ] Managed versus external versus inherited resolution is represented as
      explicit typed state.
- [ ] Invalid canonical state is rejected before projection.

**Verification:**
- [ ] `go test ./internal/beads/contract -run 'Test(ResolveDoltConnectionTarget|ValidateCanonicalConfigState|ResolveAuthoritativeConfigState)' -count=1`
- [ ] `go test ./internal/doctor -run 'Test(DoltServerCheck|RigDoltServerCheck)' -count=1`

**Dependencies:** Task 2

**Files likely touched:**
- `internal/beads/contract/connection.go`
- `internal/beads/contract/files.go`
- `cmd/gc/bd_env.go`
- `cmd/gc/beads_provider_lifecycle.go`
- `internal/doctor/checks.go`

**Estimated scope:** Medium

#### Task 4: Centralize all env projection through the typed contract

**Description:**
Delete caller-local host/port/user/password assembly where it still exists.
Require callers to use a single projection path for `GC_STORE_*`, `GC_DOLT_*`,
and `BEADS_*` compatibility output.

**Acceptance criteria:**
- [ ] `cmd/gc`, doctor, K8s, sessions, and exec backends no longer assemble
      partial Dolt env by hand.
- [ ] Projection sanitizes ambient env before setting new values.
- [ ] Mixed raw `bd` / `gc bd` / GC-initiated flows continue to agree.

**Verification:**
- [ ] `go test ./cmd/gc -run 'Test(ManagedBdRigStoreConsistentAcrossRawBdGcBdAndProviderStore|ManagedBdCityStoreConsistentAcrossRawBdGcBdAndProviderStore|InheritedExternalBdRigStoreConsistentAcrossRawBdGcBdAndProviderStore|GcBdUsesProjectionNotAmbientEnv)' -count=1`
- [ ] `go test ./internal/runtime/k8s -run 'Test(BuildPodEnv|ManagedServiceAlias)' -count=1`
- [ ] `go test ./internal/beads/exec -run TestRunSanitizesAmbientLegacyAndStoreTargetEnv -count=1`

**Dependencies:** Task 3

**Files likely touched:**
- `cmd/gc/bd_env.go`
- `cmd/gc/cmd_bd.go`
- `cmd/gc/template_resolve.go`
- `cmd/gc/work_query_probe.go`
- `cmd/gc/build_desired_state.go`
- `internal/runtime/k8s/provider.go`
- `internal/beads/exec/exec.go`

**Estimated scope:** Medium

#### Checkpoint: Phase 2

- [ ] Core resolution and projection flow through typed state only.
- [ ] The mixed-entrypoint regression suite stays green.
- [ ] No new caller reads ambient Dolt env directly.

### Phase 3: Error Ownership and Event Reporting

#### Task 5: Introduce structured Dolt error/event emission for core flows

**Description:**
Move Dolt failures out of scattered local messages into a consistent error/event
sink that records scope, mode, target, failure class, and suggested fix.

**Acceptance criteria:**
- [ ] Resolver, lifecycle, doctor, and projection failures emit a structured
      event.
- [ ] User-facing commands still show concise actionable messages.
- [ ] Silent best-effort fallbacks are eliminated or explicitly recorded.

**Verification:**
- [ ] `go test ./cmd/gc -run 'Test(RecordDoltError|DoltErrorsSurfaceToUser|StartBeadsLifecycleFailsOnCanonicalCompatDoltDrift)' -count=1`
- [ ] `go test ./internal/doctor -run 'Test(DoltServerCheck_ManagedCityReportsStartHint|DoltServerCheck_ExternalFixHint)' -count=1`

**Dependencies:** Task 4

**Files likely touched:**
- `cmd/gc/error_store.go`
- `cmd/gc/beads_provider_lifecycle.go`
- `cmd/gc/cmd_doctor.go`
- `internal/doctor/checks.go`
- related tests

**Estimated scope:** Medium

#### Task 6: Route remaining caller-specific fix hints through shared failure types

**Description:**
Remove remaining caller-local Dolt failure classification and use shared failure
classes with mode-aware fix hints instead.

**Acceptance criteria:**
- [ ] Doctor and CLI commands derive fix hints from shared failure types.
- [ ] External versus managed guidance is consistent across commands.
- [ ] Legacy `run gc start` overuse is removed where external fixes apply.

**Verification:**
- [ ] `go test ./internal/doctor -run 'Test(DoltServerCheck_ManagedCityReportsStartHint|DoltServerCheck_ExternalFixHint|RigDoltServerCheck_ExplicitRigUsesCanonicalTarget)' -count=1`
- [ ] `go test ./cmd/gc -run 'Test(CmdRigSetEndpoint|DoBeadsCity|Doctor)' -count=1`

**Dependencies:** Task 5

**Files likely touched:**
- `internal/beads/contract/connection.go`
- `internal/doctor/checks.go`
- `cmd/gc/cmd_doctor.go`
- `cmd/gc/cmd_rig_endpoint.go`
- `cmd/gc/cmd_beads_city.go`

**Estimated scope:** Small

#### Checkpoint: Phase 3

- [ ] Every core Dolt failure path produces a shared typed failure.
- [ ] Errors are visible and actionable.
- [ ] Review shows no new silent fallback.

### Phase 4: Lifecycle Simplification and Evented State

#### Task 7: Extract managed lifecycle publication and ownership from `gc-beads-bd`

**Description:**
Shrink `gc-beads-bd` by extracting owner/state publication validation and file
serialization into Go code. Leave the backend bridge responsible for actual
server actions and SQL/database work only.

**Acceptance criteria:**
- [ ] Managed owner/state publication has one implementation in Go.
- [ ] `gc-beads-bd` stops carrying publication-format policy.
- [ ] Existing stale runtime/publication regressions remain green.

**Verification:**
- [ ] `go test ./cmd/gc -run 'Test(CurrentManagedDoltPort|CurrentDoltPort|GcBeadsBdStart|GcBeadsBdEnsureReady)' -count=1`
- [ ] `go test ./internal/beads/contract -run 'Test(ResolveDoltConnectionTargetRequiresRuntimeForManagedScopes|ResolveManagedRuntimeState)' -count=1`

**Dependencies:** Task 6

**Files likely touched:**
- `examples/bd/assets/scripts/gc-beads-bd.sh`
- `cmd/gc/beads_provider_lifecycle.go`
- `internal/beads/contract/*`
- related tests

**Estimated scope:** Medium

#### Task 8: Introduce a Dolt state broker/cache for steady-state consumers

**Description:**
Reduce repeated file and socket probing by giving steady-state consumers a
shared published state/cache path. Polling remains in the lifecycle owner and
explicit health commands; consumers read the brokered state.

**Acceptance criteria:**
- [ ] Doctor, session/runtime checks, and other steady-state readers use the
      shared published state or cache.
- [ ] Redundant probe loops are reduced.
- [ ] No regression in stale-state detection.

**Verification:**
- [ ] `go test ./cmd/gc -run 'Test(ControllerQuery|EvaluatePool|RunPoolOnBoot|CmdSessionWake)' -count=1`
- [ ] `go test ./internal/doctor -run 'Test(DoltServerCheck|RigDoltServerCheck)' -count=1`

**Dependencies:** Task 7

**Files likely touched:**
- `cmd/gc/beads_provider_lifecycle.go`
- `cmd/gc/controller.go`
- `cmd/gc/pool.go`
- `cmd/gc/cmd_session*.go`
- `internal/doctor/checks.go`

**Estimated scope:** Medium

#### Checkpoint: Phase 4

- [ ] `gc-beads-bd` is materially smaller and narrower in responsibility.
- [ ] Steady-state Dolt consumers are less poll-driven.
- [ ] Race-condition regressions remain green.

### Phase 5: Caller De-Duplication and Branch Release Readiness

#### Task 9: Remove remaining caller-local store/dolt targeting logic

**Description:**
Delete or collapse the remaining duplicated targeting logic in controller,
convoy, order, hook, sling, and API paths so store and Dolt targeting are
fully shared concerns.

**Acceptance criteria:**
- [ ] Callers resolve store root and Dolt target through shared helpers only.
- [ ] No caller opens a raw `BdStore` or shells out to `bd` to bypass the
      store contract.
- [ ] Mixed provider behavior remains correct for `bd`, `file`, and
      `exec:gc-beads-bd` where supported.

**Verification:**
- [ ] `go test ./cmd/gc -count=1 -timeout 1800s`
- [ ] `go test ./internal/api ./internal/doctor ./internal/runtime/k8s ./internal/beads/exec -count=1 -timeout 1200s`

**Dependencies:** Task 8

**Files likely touched:**
- `cmd/gc/order_dispatch.go`
- `cmd/gc/cmd_hook.go`
- `cmd/gc/cmd_sling.go`
- `internal/api/convoy_sql.go`
- `internal/api/handler_beads.go`
- `cmd/gc/*` callers still doing local resolution

**Estimated scope:** Medium

#### Task 10: Final branch gate, PR packaging, and merge readiness

**Description:**
Run the complete verification matrix, refresh the audit doc, rebase onto
`origin/main`, prepare the PR body with `fixes:` and `supersedes:` mappings,
and run the best available review workflow.

**Acceptance criteria:**
- [ ] Full targeted and broad test matrix passes.
- [ ] Lint/build/install pass.
- [ ] Audit doc matches the branch exactly.
- [ ] PR body includes issue-by-issue `fixes:` and PR-by-PR `supersedes:`.

**Verification:**
- [ ] `go test ./... -count=1 -timeout 1800s`
- [ ] `go build ./cmd/gc`
- [ ] `go install ./cmd/gc`
- [ ] `bd preflight`
- [ ] review workflow returns no blocker/major findings

**Dependencies:** Task 9

**Files likely touched:**
- `engdocs/contributors/dolt-regression-audit.md`
- PR description artifact
- any final cleanup files

**Estimated scope:** Small

#### Checkpoint: Complete

- [ ] Branch is rebased on `origin/main`.
- [ ] Full test matrix, build, install, and preflight pass.
- [ ] PR is open with complete traceability.
- [ ] Remaining quality scores can be defended at `80+` each.

## Parallelization Plan

Safe parallel work once the plan is accepted:

- One agent on canonical file ownership extraction.
- One agent on typed contract / non-null state cleanup.
- One agent on error/event reporting.
- One agent on caller de-duplication analysis and regression coverage.
- One agent on audit + PR traceability upkeep.

Sequential constraints:

- Task 2 must land before the shell extraction story is considered done.
- Task 3 must define the normalized types before large-scale caller cleanup.
- Task 7 should not start until Task 5 has stabilized shared failure classes.
- Final rebase/PR work waits until all quality gates are clean.

## Risks and Mitigations

| Risk | Impact | Mitigation |
|---|---|---|
| Moving file ownership out of `gc-beads-bd` breaks direct backend bootstrap | High | Keep one backend-init regression and local dogfood after the extraction |
| Typed-state cleanup causes broad caller churn | High | Land it behind shared helper shims and migrate callers incrementally |
| Event/error plumbing becomes noise instead of signal | Medium | Limit the first slice to scope, mode, target, class, and fix hint |
| State broker adds new complexity without removing probes | Medium | Do not add broker consumers until at least one probe path is deleted |
| Branch drift from `origin/main` complicates final rebase | Medium | Rebase after each phase checkpoint, not only at the end |

## Review Gate Per Task

Each implementation task must satisfy this gate before moving on:

- [ ] Add or update a failing regression first
- [ ] Implement the code change
- [ ] Run targeted tests
- [ ] Run the next broader package slice
- [ ] Rebuild and reinstall local `gc` when runtime behavior changed
- [ ] Run the best available review workflow and fix blocker/major findings
- [ ] Update the audit doc when the change closes or supersedes a tracked item

## Success Criteria

This plan is complete when the branch can justify `80+` on all criteria with
concrete evidence:

- Smaller responsibility boundaries
- Fewer duplicated resolution/projection paths
- Explicit typed state instead of empty-string semantics
- Shared structured error ownership
- Reduced poll-heavy steady-state behavior
- Exhaustive regression traceability from the Dolt audit
