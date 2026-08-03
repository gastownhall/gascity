# Release Gate: SA5011 lint-cache poisoning fix

Bead: `ga-i2s8h4`
Source review bead: `ga-pmswi7`
Candidate commit: `0e64d5db79c405b02499a209cc7f99fe48e544ca`
Planned deploy branch: `deploy/ga-i2s8h4-gate`
Base: `origin/main` at `146abca78d1446d1ab227f258cb213ac12d8b133`
Gate evaluated: `2026-07-21T01:49:08Z`
Gate refreshed: `2026-07-21T02:21:07Z`

Note: `docs/PROJECT_MANIFEST.md` is not present in this checkout, so this gate
uses the deployer release criteria from the role prompt, plus the bead's
assignment-specific build-and-smoke requirement.

## Result

PASS.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 6 | Branch diverges cleanly from main | PASS | Evaluated first. `git fetch origin main` succeeded. `origin/main` is `146abca78d1446d1ab227f258cb213ac12d8b133`. `git merge-tree --write-tree origin/main 0e64d5db79c405b02499a209cc7f99fe48e544ca` exited 0 and returned tree `5b3fe01ef02d62c38207e502b72f5d748265e190`; refreshed `git merge-tree --write-tree origin/main HEAD` on the deploy branch exited 0 and returned tree `574848f6a24e2f28c7cecca747983477661e0fbc`. |
| 1 | Review PASS present | PASS | Review bead `ga-pmswi7` is closed with `REVIEW VERDICT: PASS` for commit `0e64d5db79c405b02499a209cc7f99fe48e544ca`, with no blocking findings. |
| 2 | Acceptance criteria met | PASS | `Makefile` pins `GOLANGCI_LINT_VERSION := 2.12.0`; both lint workflow files derive the version from `Makefile`. `rg '^[[:space:]]*restore-keys:' .github/workflows/ci.yml .github/workflows/mac-regression.yml` returned no YAML keys. `git grep 'reflect\.Ptr' -- '*.go'` returned no matches. `make test-ci-policy` passed, including the Go cipolicy hash check. A full `GOLANGCI_LINT_CACHE=$(mktemp -d) make lint-full` run passed with `0 issues`, proving a fresh cache is clean under 2.12.0. |
| 3 | Tests pass | PASS | `make build` passed, producing `bin/gc` from the deploy branch. Built binary smoke `bin/gc version` exited 0 and printed `dev`. `go vet ./...` passed. `make test-integration-huma` passed twice (`54.581s`, refreshed `58.545s`). `make test-ci-policy` passed: Python workflow-policy suites, Go `scripts/cipolicy`, and static-scope policy tests all green. Fresh-cache `make lint-full` passed with `0 issues`. `go test ./internal/api/... ./internal/config/... ./internal/worker/...` passed. |
| 4 | No high-severity review findings open | PASS | Review bead `ga-pmswi7` records PASS and "No blocking findings"; no HIGH or actionable finding is open in the review notes. |
| 5 | Final branch is clean | PASS | Deploy worktree was clean before the gate rerun. `git diff --check origin/main...0e64d5db79c405b02499a209cc7f99fe48e544ca` passed before updating this gate file; the deploy branch contains only the reviewed commit plus this committed gate checklist. |
| 7 | Single feature theme | PASS | Single commit with one coherent CI/lint theme: golangci-lint version, exact-key-only lint cache restore, CI execution policy hash, and mechanical `reflect.Ptr` to `reflect.Pointer` fallout required by the lint version bump. |

## Diff Scope

```text
.github/workflows/ci.yml                             | 8 ++++++--
.github/workflows/mac-regression.yml                 | 8 ++++++--
Makefile                                             | 2 +-
internal/api/structured_leakage_test.go              | 2 +-
internal/config/field_sync_test.go                   | 4 ++--
internal/worker/structured_wire.go                   | 2 +-
internal/worker/workertest/structured_conformance.go | 2 +-
scripts/cipolicy/policy.go                           | 2 +-
8 files changed, 19 insertions(+), 11 deletions(-)
```

## Test Log Summary

```text
make build
go build ... -o bin/gc ./cmd/gc
PASS

bin/gc version
dev
PASS

go vet ./...
PASS

make test-integration-huma
ok  	github.com/gastownhall/gascity/test/integration	54.581s

make test-ci-policy
Python test_runner_policy: 5 tests OK
Python test_ci_suite_coverage: 15 tests OK
go test ./scripts/cipolicy: ok
go test ./scripts static-scope policy subset: ok

GOLANGCI_LINT_CACHE=$(mktemp -d) make lint-full
0 issues.
PASS

go test ./internal/api/... ./internal/config/... ./internal/worker/...
ok  	github.com/gastownhall/gascity/internal/api	70.102s
ok  	github.com/gastownhall/gascity/internal/config	1.783s
ok  	github.com/gastownhall/gascity/internal/worker	23.620s
ok  	github.com/gastownhall/gascity/internal/worker/workertest	11.977s
PASS
```
