# Release Gate: ga-i7l0r4 chaos socket path

Deploy bead: ga-i7l0r4
Source review bead: ga-wo4rp5
Reviewed source commit: 9e3c5d659 fix(coordstore): use short socket path in chaos tests to fix macOS sun_path limit
Release branch commit: 7a90774d5 fix(coordstore): use short socket path in chaos tests to fix macOS sun_path limit

Branch gated: release/ga-i7l0r4-chaos-socket-path

## Gate Checklist

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead ga-wo4rp5 is closed with `REVIEW VERDICT: pass` for commit 9e3c5d659. |
| 2 | Acceptance criteria met | PASS | Both affected chaos test files now allocate Unix socket paths under a short `/tmp/cx*` directory instead of `t.TempDir()`, guard `sun_path` length with `len(p) >= 104`, and register cleanup through `t.Cleanup`. This fixes the macOS socket-path overflow while keeping each test package self-contained. |
| 3 | Tests pass | PASS | `go test ./internal/benchmarks/coordstore -run 'TestChaos(ClientForwardsCreateAndPersistsAckBeforeReturn\|ClientMapsRemoteNotFoundToErrNotFound\|ProcessReexecKillAndRestart\|ClientResetAckLedgerClearsSeedWrites)' -count=1` PASS. `GOOS=darwin go test -c ./internal/benchmarks/coordstore -o /tmp/coordstore-chaos-darwin-ga-i7l0r4.test` PASS. First `make test-fast-parallel` hit a transient unrelated `internal/runtime/tmux` assertion; the exact failed test passed on rerun, and a second full `make test-fast-parallel` PASSed. `go vet ./...` PASS. |
| 4 | No high-severity review findings open | PASS | Reviewer notes for ga-wo4rp5 report no blockers. The only finding is LOW/style, explicitly marked necessary duplication because the two files are in different Go packages. No unresolved HIGH findings are recorded in deploy or review bead notes. |
| 5 | Final branch is clean | PASS | Release branch was clean before writing this checklist; the only deployer-authored change is this gate file, committed before PR creation. |
| 6 | Branch diverges cleanly from main | PASS | Release branch was cut from current `origin/main`; `git merge-tree $(git merge-base origin/main HEAD) origin/main HEAD` produced no conflicts. |
| 7 | Single feature theme | PASS | The commit set touches one subsystem and behavior: coordstore chaos tests use short Unix socket paths that fit macOS `sun_path`. |

## Commands Run

```text
go test ./internal/benchmarks/coordstore -run 'TestChaos(ClientForwardsCreateAndPersistsAckBeforeReturn|ClientMapsRemoteNotFoundToErrNotFound|ProcessReexecKillAndRestart|ClientResetAckLedgerClearsSeedWrites)' -count=1
GOOS=darwin go test -c ./internal/benchmarks/coordstore -o /tmp/coordstore-chaos-darwin-ga-i7l0r4.test
make test-fast-parallel
go test ./internal/runtime/tmux -run TestDoStartSession_TreatsDeadlineAfterReadyAsSuccessWhenSessionAlive -count=1 -v
make test-fast-parallel
go vet ./...
```
