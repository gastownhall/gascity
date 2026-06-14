# Release Gate: ga-yr2atp mail regression coverage

Date: 2026-06-08

Deploy bead: ga-yr2atp
Source bead: ga-0mhj1r
Review bead: ga-fez44r
Branch: builder/ga-0mhj1r
Reviewed commit: bad5eb9d1712c879c1a740732c245cf11171ef53
Existing PR: https://github.com/gastownhall/gascity/pull/3238

Note: docs/PROJECT_MANIFEST.md is not present in this worktree. This gate uses
the deployer release criteria from the active Gas City role prompt and the test
commands documented in TESTING.md.

## Gate Checklist

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead ga-fez44r is closed with close reason `pass` and notes contain `REVIEWER VERDICT: PASS`. |
| 2 | Acceptance criteria met | PASS | `go test ./cmd/gc -run TestMailArchiveManyJSONEmitsBatchShape -count=1` passed; `go test ./internal/mail/exec -run TestArchiveDoesNotConsumeCallerStdin -count=1` passed. The tests assert batch JSON archive shape and exec-provider stdin isolation. |
| 3 | Tests pass | PASS | `make test-fast-parallel` passed all fast jobs; `go vet ./...` completed cleanly. |
| 4 | No high-severity review findings open | PASS | Review bead ga-fez44r lists no unresolved HIGH findings. GitHub PR #3238 has no review comments or reviews. |
| 5 | Final branch is clean | PASS | Clean deploy worktree at PR head before gate file creation; final clean status verified after the gate commit. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` completed successfully after the gate commit. GitHub reports PR #3238 merge state `CLEAN`. |
| 7 | Single feature theme | PASS | Commit set is one mail regression-test theme touching only `cmd/gc/cmd_mail_test.go` and `internal/mail/exec/exec_test.go`. |

## Change Set

| Commit | Subject | Paths |
|--------|---------|-------|
| bad5eb9d1712c879c1a740732c245cf11171ef53 | test: cover mail archive regressions | `cmd/gc/cmd_mail_test.go`, `internal/mail/exec/exec_test.go` |

## Local Commands

```text
make test-fast-parallel
go vet ./...
go test ./cmd/gc -run TestMailArchiveManyJSONEmitsBatchShape -count=1
go test ./internal/mail/exec -run TestArchiveDoesNotConsumeCallerStdin -count=1
git merge-tree --write-tree origin/main HEAD
```
