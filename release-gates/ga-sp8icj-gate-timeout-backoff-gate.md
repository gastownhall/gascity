# Release gate: gate timeout backoff gateBackoffUntil

Result: PASS

Deploy bead: ga-sp8icj
Source review bead: ga-rzjqu3
Existing PR: https://github.com/gastownhall/gascity/pull/3770
Branch: fix/order-dispatcher-gate-timeout-backoff
Head evaluated: 9b7ba2b2cf6e758fef8202735983b3f898632f3a
Base checked: origin/main at 8f68350bfa7c8406d63a87a3233063761cf50e90

Note: this repository does not currently contain `docs/PROJECT_MANIFEST.md`;
the release criteria below are the deployer prompt criteria.

## Post-gate addenda (gate updated 2026-06-28)

Two commits were added to the branch after the original gate PASS at
`e69324b7d0112ab52af4ccb32c3e15b54e85ccc1` and are now the PR HEAD.
Both passed all gate criteria on re-evaluation; the gate evidence below
reflects the updated head.

**`e31d2d12b` — test(order): cover event-triggered gate timeout backoff (trigger-agnostic gap)**
Adds `TestOrderDispatchEventTriggeredBackoffOnTrackingGateTimeout` to prove
that `gateBackoffUntil` suppresses re-entry for event-triggered orders after
a first-gate (`hasOpenTracking`) timeout. Addresses sjarmak's review finding
that the old `rememberLastRun` approach left event-triggered orders unprotected.

**`9b7ba2b2c` — fix(dispatch): anchor gateBackoff deadline to post-gate wall clock (critical)**
The original deadline was `tick_start + orderGateTimeout` — set *before* the
gate ran. After the gate returned (having consumed `orderGateTimeout`), the
deadline was already in the past, so `gateBackoffActive` returned false on
the very next tick. The backoff was a no-op in the exact contention scenario
it was designed to prevent.

Fix: capture `time.Now()` *after* the gate returns and use a dedicated
`orderGateBackoffDuration` (24 s, 3× the 8 s gate timeout) so the
suppression window is genuinely forward of the next tick. All three
regression tests now advance the clock by `orderGateTimeout` for tick 2 to
mirror production timing; with the broken arithmetic the assertions fail, with
the fix they pass.

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
| 2 | Acceptance criteria met | PASS | `gateBackoffActive` is called before both gates; `markGateBackoff` is applied on `errGateTimeout` at both `hasOpenTracking` and `hasOpenWork`; `carryGateBackoffFrom` preserves the in-memory backoff across dispatcher instances; regression tests cover original open-work timeout backoff, first-gate tracking timeout backoff, and event-triggered order backoff. Deadline anchored to post-gate wall clock with `orderGateBackoffDuration` (24 s). |
| 3 | Tests pass | PASS | `make build`; `go vet ./...`; focused `go test ./cmd/gc -run 'Test(OrderDispatch(IdempotentFailsOpenOnGateTimeout\|GateTimeoutBackoffPreventsRethrash\|NonIdempotentBackoffOnOpenTrackingTimeout\|EventTriggeredBackoffOnTrackingGateTimeout)\|GateFailClosed)$'` -> 5 tests, `ok github.com/gastownhall/gascity/cmd/gc 0.234s`; `make check-schema` clean; all 78 CI checks green on HEAD. |
| 4 | No high-severity review findings open | PASS | Reviewer notes list only non-blocking findings: mutex granularity, unreachable nil guard, bounded expired entry retention, and incidental generated CLI doc output. No unresolved HIGH finding is recorded. |
| 5 | Final branch is clean | PASS | `git diff --exit-code` clean after `make check-schema` re-run. |
| 6 | Branch diverges cleanly from main | PASS | After `git fetch origin main`, `git merge-tree --write-tree origin/main HEAD` succeeded with no conflicts. `origin/main` at `8f68350bfa7c8406d63a87a3233063761cf50e90`; the branch merges cleanly. |
| 7 | Single feature theme | PASS | The commit set is one dispatcher theme: order dispatch gate-timeout backoff plus direct tests and generated CLI reference output. Touched files are `cmd/gc/order_dispatch.go`, `cmd/gc/city_runtime.go`, `cmd/gc/order_dispatch_gate_policy_test.go`, and `docs/reference/cli.md`. |

## Commands run (updated evaluation)

```text
make build
go vet ./...
go test ./cmd/gc -run 'Test(OrderDispatch(IdempotentFailsOpenOnGateTimeout|GateTimeoutBackoffPreventsRethrash|NonIdempotentBackoffOnOpenTrackingTimeout|EventTriggeredBackoffOnTrackingGateTimeout)|GateFailClosed)$'
git fetch origin main
git merge-tree --write-tree origin/main HEAD
make check-schema
git diff --exit-code
```

## Merge note

M4 dispatcher architecture hold remains in effect. The deploy gate passes, but
merge authority must ensure architecture sign-off before merging PR #3770.
