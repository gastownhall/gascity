# Release Gate: ga-s2c6ct extmsg IsRunning race

Deploy bead: `ga-s2c6ct`  
Source review bead: `ga-jnbbpd`  
Branch: `fix/ga-thgf8q-extmsg-race`  
PR: https://github.com/gastownhall/gascity/pull/3100  
Base: `origin/main` at `b90b507f9ee984da68ab60647e1c9c1dfa763632`  
Reviewed head: `47b834362fcb92fc95d4dda323e47f0ddb073064`

Manifest note: `docs/PROJECT_MANIFEST.md` is not present in this checkout, and no `PROJECT_MANIFEST.md` file was found. Gate uses the deployer release criteria and `TESTING.md`.

## Summary

This is a test-only fix for a flaky `internal/api` extmsg test. The branch changes only `internal/api/handler_extmsg_test.go`, adding a bounded `IsRunning` poll after session bead resolution so the test waits through the known window between session bead creation and provider `Start()`.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `bd show ga-jnbbpd` reports `REVIEW VERDICT: PASS`, closed with reason `pass`, for commit `47b834362fcb92fc95d4dda323e47f0ddb073064`. |
| 2 | Acceptance criteria met | PASS | The branch implements the reviewed test-only race fix: `git diff origin/main...HEAD` touches only `internal/api/handler_extmsg_test.go` with 9 insertions. Focused test passes 5/5. |
| 3 | Tests pass | PASS | `go test -count=5 -run TestHandleExtMsgOutboundNotifiesPeerMembersAndMaterializesNamedSessions ./internal/api/` passed. `make test-fast-parallel` passed all 8 fast shards. `go vet ./...` passed. PR #3100 CI required checks are green. |
| 4 | No high-severity review findings open | PASS | Source review notes list one INFO finding and positives only; no HIGH findings. PR #3100 has no GitHub review comments or discussion comments. |
| 5 | Final branch is clean | PASS | Worktree was clean before writing this gate file; this file is committed as the final deploy evidence commit. |
| 6 | Branch diverges cleanly from main | PASS | `origin/main` is an ancestor of reviewed head `47b834362fcb92fc95d4dda323e47f0ddb073064`; PR #3100 targets `main` and was mergeable in the GitHub status rollup. |
| 7 | Single feature theme | PASS | Single-bead deploy touches one subsystem and one test file: `internal/api/handler_extmsg_test.go`. No independent feature themes are bundled. |

## Commands

```text
go test -count=5 -run TestHandleExtMsgOutboundNotifiesPeerMembersAndMaterializesNamedSessions ./internal/api/
ok  	github.com/gastownhall/gascity/internal/api	0.145s

make test-fast-parallel
All fast jobs passed

go vet ./...
PASS
```

