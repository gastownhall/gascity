# Release Gate: NFR-2 per-mode restart budgets

Bead: ga-rv06grr
Source bead: ga-06grr
Branch: builder/ga-06grr-1
Commit under review: 3f96fa0a5

## Gate Checklist

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `bd show ga-rv06grr` notes contain `VERDICT: pass`; findings: none. |
| 2 | Acceptance criteria met | PASS | Diff is limited to `test/integration/start_drift_test.go`; `driftRestartReadyBudget` and `assertRestartReadyDuration` have no hits; direct/systemd budgets are `10s` and `15s`; both call sites use `assertRestartDuration(t, out, budget, mode)`. |
| 3 | Tests pass | PASS | `go test -tags integration -run TestStartDrift ./test/integration/...` PASS; `go vet ./...` PASS; `make test-fast-parallel` PASS. |
| 4 | No high-severity review findings open | PASS | Review notes list `Findings: none`; unresolved HIGH findings count is 0. |
| 5 | Final branch is clean | PASS | `git status --short` was clean before writing this gate artifact; the gate artifact is committed as the final branch change. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree $(git merge-base HEAD origin/main) HEAD origin/main` completed with no conflicts. |

## Acceptance Evidence

- Changed files: `test/integration/start_drift_test.go` only.
- Diff stat: 1 file changed, 12 insertions, 11 deletions.
- `cmd/gc/cmd_start_drift.go` remains unchanged relative to `origin/main`.
- `driftReadyTimeout` remains unchanged in production code.
- Old NFR-2 log string forms `NFR-2 violated:` and `NFR-2 OK:` have no hits under `docs`, `engdocs`, or `test`.

## Test Evidence

- `go test -tags integration -run TestStartDrift ./test/integration/...`: PASS.
- `go vet ./...`: PASS.
- `make test-fast-parallel`: PASS.
- `git diff --check origin/main...HEAD`: PASS.
