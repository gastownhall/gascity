# Release Gate: provider-health snapshot nil-state cleanup

- Bead: `ga-wujrgt`
- Source review bead: `ga-vwj3q3`
- Branch: `builder/ga-4qbgqf.2-partial-demand-create-gate`
- Reviewed commit: `56b01ea0fc46e59e8c4226738b679f2adbefd71f`
- PR: https://github.com/gastownhall/gascity/pull/3687
- Gate worktree: clean detached checkout of `origin/builder/ga-4qbgqf.2-partial-demand-create-gate`
- Manifest note: `docs/PROJECT_MANIFEST.md` and `PROJECT_MANIFEST.md` are not present in this checkout, so this gate uses the deployer release criteria plus `TESTING.md`.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `ga-vwj3q3` is closed with `Reviewer Verdict: PASS`. Reviewer found no severity findings and verified provider-health fail-open semantics. |
| 2 | Acceptance criteria met | PASS | Cleanup removes the dead `bp.providerHealthSnapshot != nil` guard and updates the stale `agentBuildParams` comment. `loadProviderHealthSnapshot` always returns a non-nil snapshot; `check()` preserves fail-open behavior through `present=false`. |
| 3 | Tests pass | PASS | `go test ./cmd/gc -run 'Test(SnapshotCheck_|SnapshotHealthyProviders|Gate_|BuildDesiredState_ProviderRedBlocksNewPoolSessionCreate)' -count=1`; `make test-fast-parallel`; `go vet ./...`. All passed locally. PR #3687 checks were green before this gate commit. |
| 4 | No high-severity review findings open | PASS | Review notes record no findings beyond informational observations. PR #3687 had no comments or reviews at gate time. |
| 5 | Final branch is clean | PASS | Gate worktree was clean before writing this file. Only this gate checklist is staged for the gate commit. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree $(git merge-base HEAD origin/main) HEAD origin/main` produced no conflict output before this gate commit. |
| 7 | Single feature theme | PASS | The commit set remains one pool/provider-health subsystem theme on `cmd/gc` desired-state creation behavior and its release-gate evidence. The reviewed cleanup is a two-file maintenance commit on the provider-health gate introduced by the same PR. |

## Reviewed Cleanup

The reviewed cleanup touches:

- `cmd/gc/agent_build_params.go`
- `cmd/gc/build_desired_state.go`

The change removes a dead nil guard from `selectOrPlanPoolSessionBead` and aligns the field comment with the actual contract: `loadProviderHealthSnapshot` returns a non-nil snapshot, using `present=false` when the registry is absent, unreadable, or empty.

## Test Log

```text
go test ./cmd/gc -run 'Test(SnapshotCheck_|SnapshotHealthyProviders|Gate_|BuildDesiredState_ProviderRedBlocksNewPoolSessionCreate)' -count=1
ok  	github.com/gastownhall/gascity/cmd/gc	0.153s

make test-fast-parallel
[fsys-darwin-compile] ok
[unit-cmd-gc-2-of-6] ok
[unit-cmd-gc-4-of-6] ok
[unit-cmd-gc-5-of-6] ok
[unit-cmd-gc-1-of-6] ok
[unit-cmd-gc-3-of-6] ok
[unit-core] ok
[unit-cmd-gc-6-of-6] ok
All fast jobs passed

go vet ./...
PASS
```
