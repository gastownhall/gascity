# Release Gate: ga-f963pu scale_check partial create gate

Date: 2026-06-23
Branch: `builder/ga-0xljyj-scale-check-partial-gate`
Head under test: `4785b702cf8eb71dc527281542a6ed27a77b1fc2`
Base: `origin/main`
Deploy bead: `ga-f963pu`
Source requirements bead: `ga-0xljyj`
Review bead: `ga-4s192e`

Note: `docs/PROJECT_MANIFEST.md` is not present in this checkout. This gate applies the deployer release criteria from the active role instructions plus the source bead quality gates.

## Summary

PASS. The branch is a single supervisor pool desired-state fix. It blocks new pool session creates when a template's `scale_check` read is partial, keeps reuse paths ahead of the create gate, and preserves only valid existing capacity for partial-read retention.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `ga-4s192e` is closed with `Verdict: PASS`; `ga-f963pu` records reviewer PASS from `reviewer-gm-wisp-des5opg`. |
| 2 | Acceptance criteria met | PASS | `agentBuildParams.poolScaleCheckPartialTemplates` is threaded into desired-state planning; `selectOrPlanPoolSessionBead` checks `errPoolScaleCheckPartialCreate` after preferred/canonical/reusable session paths and before `tryClaimPoolSessionCreate`; `scaleCheckPartialSessionRetainable` counts active/awake plus fresh `pending_create_claim` creates, while stale creates stop inflating desired count. Focused regression tests pass. |
| 3 | Tests pass | PASS | `go test ./cmd/gc -run 'TestBuildDesiredState_ScaleCheckPartialPoolBlocksNewCreates|TestRetainScaleCheckPartialPoolDesired_InFlightCreatingBeadRetained|TestCityRuntimeBeadReconcileTick_ScaleCheckPartialKeepsOnlyAffectedPoolSession|TestCityRuntimeBeadReconcileTick_ScaleCheckPartialPreservesDormantAffectedPoolSessionWithoutDrain|TestBuildDesiredState_NamedBackedPoolPartialRetainsGenericPoolSession|TestRetainScaleCheckPartialPoolDesiredNormalizesLegacyBoundTemplate|TestBuildDesiredState_PoolInFlightSessionsPreservePartialScaleDemand'` passed. `make test-fast-parallel` passed all fast jobs. `go vet ./...` passed. `go build -o <tmp>/gc ./cmd/gc` plus `<tmp>/gc --help` passed. |
| 4 | No high-severity review findings open | PASS | Reviewer notes contain a PASS verdict and no unresolved HIGH findings. `bd search ga-0xljyj` and `bd search "partial scale_check"` showed no high-finding follow-up beads for this branch. |
| 5 | Final branch is clean | PASS | `git status --short --branch` was clean before adding this gate file. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree HEAD origin/main` completed without conflicts, producing tree `3b2f4148a145406a9cfd4e68ed78785242e0872d` before this gate commit. |
| 7 | Single feature theme | PASS | Diff from `origin/main` is limited to `cmd/gc/agent_build_params.go`, `cmd/gc/build_desired_state.go`, and related `cmd/gc` desired-state regression tests. All commits support the same supervisor pool partial-read create gate. |

## Acceptance Checks

| Requirement | Result | Evidence |
|-------------|--------|----------|
| Block new pool creates on partial demand reads | PASS | `poolScaleCheckPartialTemplates` is populated before pool realization and checked before fresh create planning. |
| Keep existing reuse/resume paths unaffected | PASS | Preferred session, canonical non-expanding reuse, and reusable pool session paths return before the partial-create guard. |
| Retain alive sessions during partial reads | PASS | Active/awake states remain retainable and covered by `TestBuildDesiredState_ScaleCheckPartialPoolBlocksNewCreates`. |
| Preserve but do not count stale in-flight creates | PASS | Stale creating bead is preserved during the partial tick but drops from desired state after the next non-partial tick. |
| Count fresh pending creates as retained capacity | PASS | `TestRetainScaleCheckPartialPoolDesired_InFlightCreatingBeadRetained` verifies `pending_create_claim=true` creates are retained and stale creates are not. |

## Test Log Summary

```text
ok  	github.com/gastownhall/gascity/cmd/gc	0.708s
[unit-core] ok
All fast jobs passed
go vet ./...: pass
go build ./cmd/gc and gc --help smoke: pass
```
