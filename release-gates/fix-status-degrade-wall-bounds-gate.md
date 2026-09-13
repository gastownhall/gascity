# Release Gate: fix status-degrade wall bounds

Bead: ga-tjhsrf
Source bead: ga-2w956b
Branch: work/ga-stllj9-rebase
Reviewed commit: 414ba420d
Base: origin/main fc4fd1802
Gate run: 2026-06-07

Note: `docs/PROJECT_MANIFEST.md` is not present in this checkout; this gate applies the deployer release criteria from the agent prompt.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead `ga-2w956b` is closed with reason `pass`; notes contain `REVIEW: PASS by gascity/reviewer`. |
| 2 | Acceptance criteria met | PASS | `internal/api/handler_status_test.go` changes only the elapsed-time bound in both status-degrade stall tests from an absolute 500ms threshold to the named 750ms stall delay. Behavioral assertions for `Partial=true` and timeout diagnostics are unchanged. |
| 3 | Tests pass | PASS | `go test ./internal/api -run 'TestHandleStatusDegradesWhen(ReadModelStore\|MailCount)Stalls' -count=3` passed. `make test-fast-parallel` passed all fast shards. `go vet ./...` passed. `make dashboard-check` passed. |
| 4 | No high-severity review findings open | PASS | Review notes report style PASS, security PASS, spec compliance PASS, coverage PASS, and verdict PASS; no high-severity findings are listed. |
| 5 | Final branch is clean | PASS | Verified `git status --short --branch` clean before committing this gate file. |
| 6 | Branch diverges cleanly from main | PASS | `origin/main` is an ancestor of `414ba420d`; merge base is `fc4fd18021e0d6e257fece81693d60eb35694b9b`. |
| 7 | Single feature theme | PASS | Commit set touches one test file in `internal/api` and addresses one flaky status-degrade wall-clock bound issue. |

## Scope

Changed file before this gate:

- `internal/api/handler_status_test.go`

## Test Evidence

```text
ok  	github.com/gastownhall/gascity/internal/api	0.659s
All fast jobs passed
go vet ./...: passed
make dashboard-check: passed
```
