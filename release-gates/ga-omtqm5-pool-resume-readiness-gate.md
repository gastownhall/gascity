# Release gate: pool reconciler resume-tier readiness gate

Result: PASS

Deploy bead: ga-omtqm5
Source review bead: ga-pigugn
Source implementation bead: ga-fnhyyk
Branch: deploy/ga-omtqm5-pool-resume-readiness-gate
Code head evaluated: fda233f2d49d7c7e756a1f037038a087394386f4
Base checked: origin/main at f5cc23fefd73dffaf04034671950a035e461688a

Note: this repository does not currently contain `docs/PROJECT_MANIFEST.md`;
the release criteria below are the deployer prompt criteria.

## Summary

The branch fixes the pool reconciler resume tier so open assigned work only
causes a resume or wake-known-identity request when the bead is actually ready.
Blocked open beads still represented as `Status: "open"` no longer repeatedly
restart pool sessions just because they are assigned and routed.

The change centralizes the status/readiness judgment in `workBeadResumeReady`,
threads the existing ready-assigned snapshot through all production pool demand
call sites, and resolves store-scoped readiness to bead IDs before the pool
demand filter loses store-ref alignment.

## Gate criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `bd show ga-pigugn` contains `REVIEWER VERDICT: PASS` and records independent build, vet, formatting, targeted regression, and full fast-unit verification. |
| 2 | Acceptance criteria met | PASS | The resume tier now gates `open` work on readiness while preserving unconditional `in_progress` behavior; both resume and wake-known-identity sub-tiers share the gate. All 5 production `ComputePoolDesiredStates*` call sites pass a real readiness map. Tests cover blocked-vs-ready resume behavior, blocked-vs-ready wake-known-identity behavior, `workBeadResumeReady`, and `readyAssignedByBeadID` store-scoped resolution. |
| 3 | Tests pass | PASS | `go build ./cmd/gc/...`; targeted `go test ./cmd/gc/ -run 'TestComputePoolDesiredStates|TestWorkBeadHasAwakeDemand|TestWorkBeadResumeReady|TestReadyAssignedByBeadID|TestBuildAwakeInputFromReconciler|TestBuildDesiredState|TestFilterAssignedWorkBeadsForPoolDemand|TestComputePoolDesiredStatesWithDemandTraced' -count=1`; `go vet ./...`; `make test-fast-parallel` all passed. |
| 4 | No high-severity review findings open | PASS | Reviewer notes list one low-severity, non-blocking defensive-code consistency follow-up in `readyAssignedByBeadID`; no HIGH or MEDIUM finding is recorded. |
| 5 | Final branch is clean | PASS | Pre-gate `git status --short --branch` was clean. Final clean status is rechecked after committing this gate file. |
| 6 | Branch diverges cleanly from main | PASS | Current `origin/main` is `f5cc23fefd73dffaf04034671950a035e461688a`; `git merge-base origin/main HEAD` returned `5becf8854dc357392bef81e8da6eea9486a49999`; `git merge-tree --write-tree origin/main HEAD` succeeded with no conflicts. |
| 7 | Single feature theme | PASS | The commit set is one `cmd/gc` reconciler/pool-demand theme: readiness-aware pool resume decisions plus direct unit/regression tests. No unrelated package or user-facing feature is bundled. |

## Commands run

```text
gh auth status
git fetch origin main builder/ga-fnhyyk-pool-resume-readiness-gate
git diff --check origin/main...HEAD
gofmt -l cmd/gc/assigned_work_scope.go cmd/gc/build_desired_state.go cmd/gc/build_desired_state_test.go cmd/gc/city_runtime.go cmd/gc/cmd_sling_test.go cmd/gc/cmd_start.go cmd/gc/compute_awake_bridge_test.go cmd/gc/compute_awake_set.go cmd/gc/pool_desired_state.go cmd/gc/pool_desired_state_test.go cmd/gc/pool_desired_state_wake_test.go cmd/gc/session_model_phase0_demand_spec_test.go cmd/gc/session_reconciler_test.go
git merge-base origin/main HEAD
git merge-tree --write-tree origin/main HEAD
go build ./cmd/gc/...
go test ./cmd/gc/ -run 'TestComputePoolDesiredStates|TestWorkBeadHasAwakeDemand|TestWorkBeadResumeReady|TestReadyAssignedByBeadID|TestBuildAwakeInputFromReconciler|TestBuildDesiredState|TestFilterAssignedWorkBeadsForPoolDemand|TestComputePoolDesiredStatesWithDemandTraced' -count=1
go vet ./...
make test-fast-parallel
```
