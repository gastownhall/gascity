# Release Gate: workflow-finalize work_dir release hint

Bead: `ga-gcdmcv`
Implementation bead: `ga-vzt5pq.2`
Provenance branch: `builder/ga-vzt5pq.2`
Reviewed commit: `fe0d831ce9df79bc811fd117cd0815b04b72e2c0`
Base: `origin/main` at `bac288647e0bbbbe2e68bdbe588709eb2827f5ee`

The prompted `docs/PROJECT_MANIFEST.md` path is not present in this Gas City
checkout. No `PROJECT_MANIFEST.md` or `SOFTWARE_FACTORY_MANIFEST.md` was found
within the repository, so this gate uses the deployer release criteria from
the role prompt and the repository testing guidance in `TESTING.md`.

## Diff Scope

`git diff --name-status origin/main...fe0d831ce9df79bc811fd117cd0815b04b72e2c0`:

```text
M	internal/beadmeta/keys.go
M	internal/dispatch/runtime.go
M	internal/dispatch/runtime_test.go
```

This is one release unit: workflow finalization now writes a best-effort
`gc.work_dir_released_at` RFC3339 timestamp only when a workflow finalizes
with a Pass outcome and the workflow root carries `gc.work_dir`.

## Gate Checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | `bd show ga-vzt5pq.2` records `REVIEWER VERDICT: PASS`; `bd show ga-gcdmcv` is the deploy bead created from that reviewer PASS and records the reviewed commit `fe0d831ce9df79bc811fd117cd0815b04b72e2c0`. |
| 2 | Acceptance criteria met | PASS | The implementation adds `beadmeta.WorkDirReleasedAtMetadataKey`, registers it in `KnownMetadataKeys`, calls `stampWorkDirReleasedAt` only when `processWorkflowFinalize` computes `beadmeta.OutcomePass`, skips roots without `gc.work_dir`, and traces/swallow read or write failures so the optional hint cannot abort workflow finalization. Tests cover Pass stamping, failure-outcome non-stamping, missing-work-dir no-op, and injected metadata-write failure. |
| 3 | Tests pass | PASS | Focused acceptance passed: `go test ./internal/dispatch/... ./internal/beadmeta/... -count=1 -timeout=10m`. Static checks passed: `go vet ./...` and `git diff --check origin/main...HEAD`. `make test-fast-parallel` passed 7 of 8 fast jobs; `unit-cmd-gc-3-of-6` failed only `TestErrorReturningSessionProviderFactoriesPreserveSuccessBehavior/default`. The exact failing test failed deterministically on this branch and reproduced identically on `origin/main` at `bac288647e0bbbbe2e68bdbe588709eb2827f5ee`; this diff touches no `cmd/gc` files, so the fast-suite red is a pre-existing baseline failure, not a branch regression. |
| 4 | No high-severity review findings open | PASS | Reviewer notes for `ga-vzt5pq.2` report OWASP/security walk with no findings and no HIGH/blocker findings. |
| 5 | Final branch is clean | PASS | Before adding this gate file, `git status --porcelain=v1 -b` on `deploy/ga-gcdmcv-gate` returned only `## deploy/ga-gcdmcv-gate`; after the gate commit, cleanliness is rechecked before push. |
| 6 | Branch diverges cleanly from main | PASS | Evaluated first. `git merge-tree --write-tree origin/main fe0d831ce9df79bc811fd117cd0815b04b72e2c0` exited 0 and produced tree `4c81294555f69b0524fe5265cb5c4389bc8b90a0`; no bounded self-rebase was required. |
| 7 | Single feature theme | PASS | The commit set touches one subsystem and behavior: optional workflow-finalize metadata used as a non-authoritative worktree release hint. The changes are limited to dispatch runtime behavior, metadata-key registration, and directly owning tests. |

## Branch Isolation Note

The deploy source is the bead-recorded reviewed commit, not the provenance
branch tip. The prompt-advertised `resolve_deploy_branch_target` and
`assert_safe_push_target` functions are not present in the current
`scripts/rebase-resolve-lib.sh`; the isolated branch was therefore set
mechanically to `deploy/ga-gcdmcv-gate` at the reviewed SHA and guarded against
shared `gc-*` worktree branch naming before push.

Gate result: PASS.
