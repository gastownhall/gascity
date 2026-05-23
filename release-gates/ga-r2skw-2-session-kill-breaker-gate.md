# Release Gate: gc session kill clears circuit breaker

Date: 2026-05-23

Branch: `deploy/ga-r2skw-2-session-kill-breaker`
Base: `origin/main` at `8fe542295`
Source bead: `ga-r2skw.2`
Review bead: `ga-m5ftq`
Reviewed source commit: `7eb789d6d`
Deploy commit: `b8e2e2af9`

## Summary

`gc session kill` now clears the named-session respawn circuit breaker after a successful runtime kill. If the clear operation fails, the command prints a warning and still treats the kill as successful, matching the existing `gc session reset` best-effort behavior.

The deploy branch was cut from `origin/main` and cherry-picked only the reviewed source commit. The reviewed builder branch also contained an unrelated open PR commit, so this deploy branch excludes that unrelated change.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead `ga-m5ftq` notes contain `verdict: pass`; reviewer mail `gm-wisp-j64wvu` states `Review bead ga-m5ftq passed`. |
| 2 | Acceptance criteria met | PASS | `cmd/gc/cmd_session.go` calls `resetSessionCircuitBreakerOnController` after successful `handle.Kill`; `TestCmdSessionKill_ClearsCircuitBreaker` verifies in-memory and persisted breaker state are cleared; `TestCmdSessionKill_CircuitBreakerClearFailureWarns` verifies clear failures warn without failing the kill. Manual live-session kill was not run to avoid disrupting active factory sessions; fake-runtime/controller coverage exercises the mechanism. |
| 3 | Tests pass | PASS | `go test ./cmd/gc -run 'TestCmdSessionKill_(ClearsCircuitBreaker\|CircuitBreakerClearFailureWarns)$' -count=1` passed; `go vet ./...` passed; `make test-fast-parallel` passed all fast shards. |
| 4 | No high-severity review findings open | PASS | Review notes list only INFO findings and state no action is needed; no HIGH findings are present. |
| 5 | Final branch is clean | PASS | `git status --short --branch` was clean after cherry-pick before this gate file was added; final status will be rechecked after committing the gate. |
| 6 | Branch diverges cleanly from main | PASS | Clean deploy branch from `origin/main`; `git merge-tree $(git merge-base origin/main HEAD) origin/main HEAD` reported merged results with no `CONFLICT` entries. |

## Test Log

```text
$ go test ./cmd/gc -run 'TestCmdSessionKill_(ClearsCircuitBreaker|CircuitBreakerClearFailureWarns)$' -count=1
ok  	github.com/gastownhall/gascity/cmd/gc	0.519s

$ go vet ./...
PASS

$ make test-fast-parallel
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
