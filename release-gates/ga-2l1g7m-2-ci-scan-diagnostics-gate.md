# Release Gate: CI Scan-Time Fix + Dispatcher Diagnostics

Bead: `ga-2l1g7m.2`

Source review bead: `ga-avsn13`

Builder clean-branch bead: `ga-2l1g7m.1`

Branch under review: `deploy/ci-scan-time-fix`

Candidate head: `b468b0605563faf9ee34488be85657f52ea24171`

Base checked: `origin/main` at `895ddf83676869c24b528db4cd102f3bdd9a78f2`

Merge-clean tree: `add3c9d92e839e57ad87f01c15c1d19d785a24e8`

## Gate Checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | `ga-avsn13` is closed with close reason `pass`; notes contain `REVIEWER PASS (2026-06-20)`, commit `278675e1c`, and `No blockers. Deploy.` |
| 2 | Acceptance criteria met | PASS | `ga-2l1g7m.1` recorded clean branch `deploy/ci-scan-time-fix`, base `895ddf836`, and head `b468b0605`. The effective diff is limited to GC_WORKFLOW_TRACE artifact collection, ralph check-start/check-done tracing, and targeted review-formula check queries. |
| 3 | Tests pass | PASS | `make build` passed. `go vet ./...` passed. `make test-fast-parallel` passed all 8 fast jobs. `make test-integration-review-formulas` passed: basic `226.614s`, retries `416.849s`, recovery `62.174s`. |
| 4 | No high-severity review findings open | PASS | Review notes for `ga-avsn13` list no security concerns and no blockers; no HIGH findings are recorded on the review or deploy beads. |
| 5 | Final branch is clean | PASS | Before writing this gate artifact, `git status --short --branch` showed a clean `deploy/ci-scan-time-fix` branch at `b468b0605`, up to date with `origin/deploy/ci-scan-time-fix`. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` exited 0 and produced tree `add3c9d92e839e57ad87f01c15c1d19d785a24e8`. `git diff --check origin/main...HEAD` exited 0. |
| 7 | Single feature theme | PASS | Before this gate artifact, `git log origin/main..HEAD` contained one commit and `git diff --name-only origin/main...HEAD` listed only `internal/dispatch/ralph.go` and `test/integration/review_formula_test.go`. The contaminated `builder/ga-omnkls` commit stack is excluded. |

## Pre-Gate Diff Scope

```text
internal/dispatch/ralph.go
test/integration/review_formula_test.go
```

```text
internal/dispatch/ralph.go              |  2 ++
test/integration/review_formula_test.go | 13 ++++++++++---
2 files changed, 12 insertions(+), 3 deletions(-)
```

## Commands Run

```bash
gh auth status
git switch deploy/ci-scan-time-fix
git diff --check origin/main...HEAD
git merge-tree --write-tree origin/main HEAD
make build
go vet ./...
make test-fast-parallel
make test-integration-review-formulas
```

## Decision

Gate PASS. Push `deploy/ci-scan-time-fix`, open a PR to
`gastownhall/gascity:main`, then route the merge-request to mayor/mpr. Do not
merge from the deployer session.
