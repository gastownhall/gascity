# Release gate: gate timeout backoff gateBackoffUntil

Result: PASS

Deploy bead: ga-sp8icj
Source review bead: ga-rzjqu3
Existing PR: https://github.com/gastownhall/gascity/pull/3770
Branch: fix/order-dispatcher-gate-timeout-backoff
Head evaluated: e69324b7d0112ab52af4ccb32c3e15b54e85ccc1
Base checked: origin/main at 2c4b1beaa1d0c03127732b13bff62dbf6842571f

Note: this repository does not currently contain `docs/PROJECT_MANIFEST.md`;
the release criteria below are the deployer prompt criteria.

## Summary

The branch replaces `lastRun`-based timeout throttling with an explicit
in-memory `gateBackoffUntil` deadline for order open-work gate timeouts.
The backoff is checked at the top of the per-order loop, before both the
tracking gate and the open-work gate, so cooldown, cron, and event-triggered
orders avoid repeated Dolt-heavy gate queries during timeout pressure.

## Gate criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `bd show ga-rzjqu3` contains `REVIEWER VERDICT: PASS`; `bd show ga-sp8icj` records reviewed + PASSED by gascity/reviewer. |
| 2 | Acceptance criteria met | PASS | `gateBackoffActive` is called before both gates; `markGateBackoff` is applied on `errGateTimeout` at both `hasOpenTracking` and `hasOpenWork`; `carryGateBackoffFrom` preserves the in-memory backoff across dispatcher instances; regression tests cover original open-work timeout backoff and first-gate tracking timeout backoff. |
| 3 | Tests pass | PASS | `make build`; `go vet ./...`; focused `go test ./cmd/gc -run 'Test(OrderDispatch(IdempotentFailsOpenOnGateTimeout\|GateTimeoutBackoffPreventsRethrash\|NonIdempotentBackoffOnOpenTrackingTimeout)\|GateFailClosed)$'` -> `ok github.com/gastownhall/gascity/cmd/gc 0.295s`; `LOCAL_TEST_JOBS=24 make test-fast-parallel` -> all fast jobs passed; `make check-schema` completed with no dirty diff. |
| 4 | No high-severity review findings open | PASS | Reviewer notes list only non-blocking findings: mutex granularity, unreachable nil guard, bounded expired entry retention, and incidental generated CLI doc output. No unresolved HIGH finding is recorded. PR checks show 78 SUCCESS and 23 SKIPPED, with no failed or pending checks. |
| 5 | Final branch is clean | PASS | Before writing this checklist, `git status --short` and `git diff --exit-code` were empty in the clean evaluation worktree. Final cleanliness was rechecked after committing the gate file before push. |
| 6 | Branch diverges cleanly from main | PASS | After `git fetch origin main`, `git merge-tree --write-tree origin/main HEAD` succeeded with no conflicts. `origin/main` had advanced to `2c4b1beaa1d0c03127732b13bff62dbf6842571f`; the branch still merges cleanly. |
| 7 | Single feature theme | PASS | The commit set is one dispatcher theme: order dispatch gate-timeout backoff plus direct tests and generated CLI reference output. Touched files are `cmd/gc/order_dispatch.go`, `cmd/gc/city_runtime.go`, `cmd/gc/order_dispatch_gate_policy_test.go`, and `docs/reference/cli.md`. |

## Commands run

```text
gh auth status
git diff --check origin/main...HEAD
go vet ./...
make build
go test ./cmd/gc -run 'Test(OrderDispatch(IdempotentFailsOpenOnGateTimeout|GateTimeoutBackoffPreventsRethrash|NonIdempotentBackoffOnOpenTrackingTimeout)|GateFailClosed)$'
git fetch origin main
git merge-tree --write-tree origin/main HEAD
LOCAL_TEST_JOBS=24 make test-fast-parallel
make check-schema
```

## Merge note

M4 dispatcher architecture hold remains in effect. The deploy gate passes, but
merge authority must ensure architecture sign-off before merging PR #3770.
