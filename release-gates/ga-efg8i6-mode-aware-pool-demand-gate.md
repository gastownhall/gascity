# Release Gate: Mode-Aware Pool Demand Gate

Date: 2026-06-24

Deploy bead: `ga-efg8i6`
Source review bead: `ga-xl9qx7`
Reviewed commit: `4c42a6f09a9354539e5a688fd6e4b6c970e9492a`
Branch: `builder/ga-4qbgqf.2-partial-demand-create-gate`
PR handling: branch already has open PR `#3687`, authored by `quad341`, with no comments or reviews at gate time. This deploy updates that PR instead of creating a duplicate PR for the same head branch.

## Release Criteria Source

`docs/PROJECT_MANIFEST.md` is not present in this checkout. This gate applies the deployer release criteria from the active role instructions plus the repository testing guidance in `TESTING.md`.

## Gate Checklist

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `bd show ga-xl9qx7` records `REVIEW VERDICT: PASS`; reviewer sections Style, Spec compliance, and Coverage are PASS; Blockers: NONE. |
| 2 | Acceptance criteria met | PASS | `cmd/gc/build_desired_state.go` now tracks `namedSessionMode`, gates default pool-demand targets for `mode="always"`, leaves named partial-retention probes intact, and clamps `mode="on_demand"` named-backed pool demand to one. Focused acceptance tests passed. |
| 3 | Tests pass | PASS | `go test ./cmd/gc -run 'Test(DefaultNamedSessionDemandRecordsPartialWithoutRoutedDemand\|DefaultNamedSessionDemandIgnoresNamedIdentityRunTargetOnlyWorkflow\|BuildDesiredState_OnDemandNamedSession_DefaultRoutedWorkUsesTemplatePoolDemand\|BuildDesiredState_OnDemandNamedSession_DefaultRoutedTaskWispUsesTemplatePoolDemand\|BuildDesiredState_OnDemandNamedSession_DefaultRoutedTemplateUsesGenericPoolDemand\|BuildDesiredState_NamedBackedPoolPartialRetainsGenericPoolSession\|BuildDesiredState_NamedBackingPoolNoCap_RoutedDemandDoesNotSpawnPhantoms)$'` passed. `make test-fast-parallel` passed all 8 fast jobs. `go vet ./...` passed. |
| 4 | No high-severity review findings open | PASS | Reviewer notes list no findings requiring action and `Blockers: NONE`; no HIGH findings are present in the review bead notes. |
| 5 | Final branch is clean | PASS | Clean deploy worktree used for gate evaluation. Before writing this checklist, `git status --short --branch` returned only the branch header. The gate commit contains only this checklist file. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-base --is-ancestor origin/main HEAD` passed. `git merge-tree --write-tree origin/main HEAD` produced clean tree `67a6ef2a1ade7969efd28fd361075808534d79de`. |
| 7 | Single feature theme | PASS | The product diff is confined to `cmd/gc` pool/session desired-state behavior and its tests. The reviewed change is one pool-demand theme: prevent named-backed pool templates from spawning generic `{name}-N` sessions while preserving `on_demand` wake behavior. |

## Acceptance Evidence

- `mode="always"` named-backed templates no longer append default generic pool-demand probes, preventing routed work from spawning phantom pool sessions alongside the canonical named session.
- `mode="on_demand"` named-backed templates still append default pool-demand probes so routed template work wakes the singleton.
- Default named scale targets remain unchanged for partial-read retention bookkeeping.
- Demand counts for `mode="on_demand"` named-backed templates are clamped to one during the default-count merge, mirroring the existing cold-wake clamp.
- Repro test `TestBuildDesiredState_NamedBackingPoolNoCap_RoutedDemandDoesNotSpawnPhantoms` passed on healthy non-partial reads.

## Commands Run

```text
go test ./cmd/gc -run 'Test(DefaultNamedSessionDemandRecordsPartialWithoutRoutedDemand\|DefaultNamedSessionDemandIgnoresNamedIdentityRunTargetOnlyWorkflow\|BuildDesiredState_OnDemandNamedSession_DefaultRoutedWorkUsesTemplatePoolDemand\|BuildDesiredState_OnDemandNamedSession_DefaultRoutedTaskWispUsesTemplatePoolDemand\|BuildDesiredState_OnDemandNamedSession_DefaultRoutedTemplateUsesGenericPoolDemand\|BuildDesiredState_NamedBackedPoolPartialRetainsGenericPoolSession\|BuildDesiredState_NamedBackingPoolNoCap_RoutedDemandDoesNotSpawnPhantoms)$'
make test-fast-parallel
go vet ./...
git merge-base --is-ancestor origin/main HEAD
git merge-tree --write-tree origin/main HEAD
```
