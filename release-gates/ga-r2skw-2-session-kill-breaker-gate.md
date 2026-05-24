# Release Gate: gc session kill clears circuit breaker

Date: 2026-05-23

Branch: `deploy/ga-r2skw-2-session-kill-breaker`
Base: `origin/main` at `6a3db0d7f`
Source bead: `ga-r2skw.2`
Review bead: `ga-m5ftq`
Reviewed source commit: `7eb789d6d`
Rebased deploy branch: `deploy/ga-r2skw-2-session-kill-breaker`

## Summary

`gc session kill` now clears the named-session respawn circuit breaker after a successful runtime kill. For named sessions whose runtime is already inactive because the breaker left them asleep, kill treats a successful breaker clear as a completed remediation. If the clear operation fails after a successful runtime kill, the command prints a warning and still treats the kill as successful; unlike kill, `gc session reset` clears the breaker before restart and treats clear failure as fatal.

The deploy branch was rebased onto `origin/main` after main gained the generic explicit kill/reset breaker clear. The rebased PR branch keeps the release gate plus the inactive-runtime remediation and excludes the unrelated open PR commit from the reviewed builder branch.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead `ga-m5ftq` notes contain `verdict: pass`; reviewer mail `gm-wisp-j64wvu` states `Review bead ga-m5ftq passed`. |
| 2 | Acceptance criteria met | PASS | `cmd/gc/cmd_session.go` calls `resetSessionCircuitBreakerAfterExplicitKill` after successful `handle.Kill` and after the named-session runtime is already inactive; `TestCmdSessionKill_ClearsCircuitBreaker` verifies the live/awake path; `TestCmdSessionKill_ClearsCircuitBreakerForAsleepNamedSession` verifies the realistic open-breaker/asleep path clears in-memory and persisted breaker state; `TestCmdSessionKill_RecordsStoppedWhenCircuitBreakerResetFails` verifies clear failures warn without failing a successful kill and still record the stop event. Manual live-session kill was not run to avoid disrupting active factory sessions; fake-runtime/controller coverage exercises the mechanism. |
| 3 | Tests pass | PASS | `go test ./cmd/gc -run 'TestCmdSession(Kill\|Reset)_(ClearsCircuitBreaker\|ClearsCircuitBreakerForAsleepNamedSession\|RecordsStoppedWhenCircuitBreakerResetFails\|RequestsFreshRestartWithController\|ControllerClearFailureDoesNotQueueRestart)$\|TestResetSessionCircuitBreakerOnControllerMalformedReply$' -count=1`, `go vet ./...`, and `TMPDIR=/home/jaword/tmp/gc-pr2533-fast LOCAL_TEST_JOBS=8 CMD_GC_PROCESS_TOTAL=6 make test-fast-parallel` passed after the rebase. |
| 4 | No high-severity review findings open | PASS | Review notes list only INFO findings and state no action is needed; no HIGH findings are present. |
| 5 | Final branch is clean | PASS | `git status --short --branch` was clean after the rebase; final status will be rechecked after committing the gate refresh. |
| 6 | Branch diverges cleanly from main | PASS | Clean deploy branch from `origin/main`; `git merge-tree origin/main HEAD` reported merged results with no `CONFLICT` entries. |

## Test Log

```text
$ go test ./cmd/gc -run 'TestCmdSessionKill_(ClearsCircuitBreaker|ClearsCircuitBreakerForAsleepNamedSession|RecordsStoppedWhenCircuitBreakerResetFails)|TestCmdSessionReset_ClearsCircuitBreaker' -count=1
ok  	github.com/gastownhall/gascity/cmd/gc	1.061s

$ go test ./cmd/gc -run 'TestCmdSession(Kill|Reset)_(ClearsCircuitBreaker|ClearsCircuitBreakerForAsleepNamedSession|RecordsStoppedWhenCircuitBreakerResetFails|RequestsFreshRestartWithController|ControllerClearFailureDoesNotQueueRestart)$|TestResetSessionCircuitBreakerOnControllerMalformedReply$' -count=1
ok  	github.com/gastownhall/gascity/cmd/gc	1.200s

$ go vet ./...
PASS

$ TMPDIR=/home/jaword/tmp/gc-pr2533-fast LOCAL_TEST_JOBS=8 CMD_GC_PROCESS_TOTAL=6 make test-fast-parallel
[fsys-darwin-compile] ok
[unit-cmd-gc-1-of-6] ok
[unit-cmd-gc-2-of-6] ok
[unit-cmd-gc-3-of-6] ok
[unit-cmd-gc-4-of-6] ok
[unit-cmd-gc-5-of-6] ok
[unit-cmd-gc-6-of-6] ok
[unit-core] ok
All fast jobs passed
```
