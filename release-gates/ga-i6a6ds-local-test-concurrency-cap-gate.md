# Release Gate: Local test concurrency cap

- Deploy bead: `ga-i6a6ds`
- Source review: `ga-8b8vzk`
- Reviewed commit: `cc194b367a62ec3d21339c095c5d354b2c9b7468`
- Candidate base: `311effd094d3a5085c364d4cab017f65442d43b8`
- Main evaluated: `origin/main@a72480ec884e5f6369f23b84cb18786affa49df5`
- Deploy branch: `deploy/ga-i6a6ds-gate`
- Evaluated: `2026-07-28T05:23:02Z`
- Overall verdict: **PASS**

`docs/PROJECT_MANIFEST.md` is not present in this repository at the evaluated
commit, so this checklist applies the deployer role's release-gate criteria.

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 6 | Branch diverges cleanly from main | **PASS** | Checked first after fetching `origin/main`. `git merge-tree --write-tree origin/main cc194b367a62ec3d21339c095c5d354b2c9b7468` exited 0 and produced tree `7d72b00c11803675166dfb90bfb9e6b33fd281f6`. The earlier real conflict was resolved on the reviewed branch; no deploy-time rebase or source-branch mutation was needed. |
| 1 | Review PASS present | **PASS** | Review bead `ga-8b8vzk` records the earlier request-changes verdict, followed by `REVIEW VERDICT: PASS (re-review after rebase + rework)` and `FINAL VERDICT: PASS` for exact commit `cc194b367a62ec3d21339c095c5d354b2c9b7468`. |
| 2 | Acceptance criteria met | **PASS** | `test-local-job-count` subtracts a validated load-derived reduction from the CPU/memory budget while preserving the minimum floor and explicit CPU override. `gc_inner_parallelism` divides that outer budget across concurrent jobs, and `test-local-parallel` exports the result through `GOFLAGS=-p=<n>`. The runner registers the 25-assertion self-test in fast and full modes. Rebase conflict resolution preserves both the prior push-gate environment controls and this feature's load-average control. Comments accurately scope `-p` to cross-package/build concurrency rather than within-package `t.Parallel()` fan-out. |
| 3 | Tests pass | **PASS** | First-attempt runtime checks on the exact reviewed SHA passed: 10 focused concurrency subtests plus the environment-allowlist test; `scripts/test-local-concurrency.sh` 25/25; full `go test ./scripts/...`; `go build ./...`; `go vet ./...`; and `make test-fast-parallel` all 10 jobs, with the runner reporting `inner_p=1`. `gofmt -l` and `bash -n` were clean. ShellCheck passed on all new/focused shell files and on the modified runner with two documented legacy info codes excluded. A broad invocation stopped only on pre-existing `SC1091`/`SC2016` informational findings outside the changed hunks; it found no new warning in this feature. |
| 4 | No high-severity review findings open | **PASS** | The prior blocking merge-conflict finding and non-blocking comment-accuracy finding were both fixed and independently re-reviewed. The final review reports no security findings or new blockers. Unresolved HIGH/CRITICAL findings: 0. |
| 5 | Final branch is clean | **PASS** | Before adding this checklist, detached `cc194b367` had an empty `git status --porcelain=v1`; `git diff --check` against its merge base passed. The configured hook path is `.githooks`; this checklist is the only deployer-authored release commit. |
| 7 | Single feature theme | **PASS** | The three reviewed commits touch five files in one subsystem: local test-runner concurrency budgeting, its direct shell self-test, and the environment-allowlist contract needed to keep the runner deterministic. No independent product feature, CI workflow, timeout, coverage, or resource-ledger change is bundled. |

## Commands

```bash
git fetch origin main
git merge-tree --write-tree origin/main cc194b367a62ec3d21339c095c5d354b2c9b7468
git diff --check 311effd094d3a5085c364d4cab017f65442d43b8..cc194b367a62ec3d21339c095c5d354b2c9b7468
gofmt -l scripts/precommit_contract_test.go
bash -n scripts/lib/inner-parallelism.sh scripts/test-local-concurrency.sh scripts/test-local-job-count scripts/test-local-parallel
shellcheck -P scripts -P scripts/lib scripts/lib/inner-parallelism.sh scripts/test-local-concurrency.sh scripts/test-local-job-count
shellcheck -e SC1091,SC2016 -P scripts -P scripts/lib scripts/test-local-parallel
go test ./scripts/... -run 'TestTestFastParallelUsesSanitizedEnvironmentAndMachineAwareConcurrency|TestLocalParallelAllowlistIncludesObservableEnv' -count=1 -v
bash scripts/test-local-concurrency.sh
go test ./scripts/... -count=1
go build ./...
go vet ./...
make test-fast-parallel
```
