# Release Gate: ga-p6dhla proctable unparam

Deploy bead: ga-p6dhla
Source review bead: ga-1ej9i1
Reviewed source commit: 9539e5bb2 fix(proctable): rename liveScanRoot to liveScanGuard returning only error
Release branch commit: 9572b798e fix(proctable): rename liveScanRoot to liveScanGuard returning only error

Branch gated: release/ga-p6dhla-proctable-unparam

## Gate Checklist

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead ga-1ej9i1 is closed with `REVIEW VERDICT: pass` for commit 9539e5bb2. |
| 2 | Acceptance criteria met | PASS | `internal/runtime/proctable/guard.go` now exposes `liveScanGuard() error` instead of returning the scan-root string. Darwin and Linux scanner call sites compile against the narrower guard contract and read the package-level `scanRoot` directly where needed. The live `/proc` test guard behavior is unchanged. |
| 3 | Tests pass | PASS | `go test ./internal/runtime/proctable -count=1` PASS. `GOOS=darwin go test -c ./internal/runtime/proctable -o /tmp/proctable-darwin-ga-p6dhla.test` PASS. `make test-fast-parallel` PASS. `go vet ./...` PASS. |
| 4 | No high-severity review findings open | PASS | Reviewer notes for ga-1ej9i1 report `FINDINGS: none`, no security issues, and no blockers. No unresolved HIGH findings are recorded in deploy or review bead notes. |
| 5 | Final branch is clean | PASS | Release branch was clean before writing this checklist; the only deployer-authored change is this gate file, committed before PR creation. |
| 6 | Branch diverges cleanly from main | PASS | Release branch was cut from current `origin/main`; `git merge-tree $(git merge-base origin/main HEAD) origin/main HEAD` produced no conflicts. |
| 7 | Single feature theme | PASS | The commit set touches one subsystem and behavior: `internal/runtime/proctable` scan guard API cleanup for the Darwin lint path. |

## Commands Run

```text
go test ./internal/runtime/proctable -count=1
GOOS=darwin go test -c ./internal/runtime/proctable -o /tmp/proctable-darwin-ga-p6dhla.test
make test-fast-parallel
go vet ./...
```
