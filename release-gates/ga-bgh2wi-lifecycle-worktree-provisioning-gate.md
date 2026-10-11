# Release Gate: lifecycle worktree provisioning convergence

Deploy bead: `ga-bgh2wi`
Source bead: `ga-g8lt3x`
Reviewed commit: `da9099c3c73609a9ecc45c796177cbf163ac8ff4`
Reviewed commits: `31dc58d72`, `da9099c3c73609a9ecc45c796177cbf163ac8ff4`
Planned deploy branch: `deploy/ga-bgh2wi-gate`
Base: `origin/main` at `c31a67ea0fdbc13bff05b7a821cfead0d165dbc8`
Gate evaluated: `2026-07-28`

`docs/PROJECT_MANIFEST.md` is not present in this checkout, so this gate uses
the deployer role's release criteria, the source bead's done-when criteria,
and the repository test policy in `TESTING.md`.

## Result

PASS.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 6 | Branch diverges cleanly from main | PASS | Evaluated first after `git fetch origin main`. `git merge-tree --write-tree origin/main da9099c3c73609a9ecc45c796177cbf163ac8ff4` exited 0 and produced merged tree `61de7fb992c0c890122e49eef7c5d0b7697408d9`. No self-rebase was needed. |
| 1 | Review PASS present | PASS | `ga-bgh2wi` records `verdict: pass` for reviewed tip `da9099c3c73609a9ecc45c796177cbf163ac8ff4`; the reviewer independently checked the diff, reproduction, build, vet, style, security, and regression coverage. |
| 2 | Acceptance criteria met | PASS | `ensure_worktree_provisioning` owns the bead redirect, submodule initialization, and local excludes; it is called from both the pre-existing-worktree early exit and fresh-create path, after the existence check. Tier A passed `TestLifecycleWorktreeSetupRedirectAppliesToPreExistingWorktree` and the fresh-create control `TestLifecycleWorktreeSetupBeadRedirect`. The related worktree tests also passed. |
| 3 | Tests pass | PASS | `go build ./...` and `go vet ./...` passed. Documented `make test` passed with `34,129 PASS / 0 FAIL / 169 SKIP` tests (`163 PASS / 0 FAIL / 18 SKIP` packages). Documented per-PR Tier A `make test-acceptance` passed all 6 packages; structured replay recorded `344 PASS / 0 FAIL / 8 SKIP` tests. The fast-unit skips are the repository's documented process/integration/build-tag exclusions. Tier A's eight skips are seven explicit pending self-host UX tests and one opt-in live pack-registry smoke requiring `GC_TEST_GASCITY_PACKS_REGISTRY`; none touches the lifecycle worktree script or its tests. |
| 4 | No high-severity review findings open | PASS | Reviewer notes report no style or security findings and no uncovered acceptance criteria; no HIGH finding remains open. |
| 5 | Final branch is clean | PASS | The detached reviewed commit was clean before this checklist was added, and `git diff --check origin/main...da9099c3c73609a9ecc45c796177cbf163ac8ff4` passed. The deploy branch will contain only the reviewed two-commit series plus this gate checklist. |
| 7 | Single feature theme | PASS | The series changes one lifecycle example script plus its acceptance tests. Both commits are the red/green pair for making worktree provisioning converge on pre-existing worktrees. |

## Diff Scope

```text
examples/lifecycle/packs/lifecycle/assets/scripts/worktree-setup.sh | 96 ++++++++++--------
test/acceptance/worktree_lifecycle_test.go                          | 110 +++++++++++++++++++++
test/acceptance/worktree_test.go                                    | 15 ++-
3 files changed, 175 insertions(+), 46 deletions(-)
```

## Focused Acceptance Evidence

```text
PASS TestLifecycleWorktreeSetupBeadRedirect
PASS TestLifecycleWorktreeSetupRedirectAppliesToPreExistingWorktree
PASS TestWorktreeBranchNamespacing
PASS TestWorktreeIdempotent
PASS TestWorktreeBeadRedirect
```
