# Release Gate: ga-f0efcs-order-retention-watchdog

- Date: 2026-06-07
- Shape: single-bead PR
- Deploy bead: ga-f0efcs - needs-deploy: order-tracking retention watchdog
- Source review bead: ga-w1t8yf - Review: ga-tjp87g.1 - order-tracking retention watchdog + bounded sweep
- Source branch: builder/ga-tjp87g-order-retention
- Source commit: 0bb03cfb1 feat(order): add retention watchdog and bounded sweep for closed order-tracking beads (ga-tjp87g.1)
- Release branch: release/ga-f0efcs-order-retention-watchdog
- Release commit before this gate file: bce3a1675 feat(order): add retention watchdog and bounded sweep for closed order-tracking beads (ga-tjp87g.1)
- Release criteria source: deployer Release Gate Criteria. `docs/PROJECT_MANIFEST.md` is not present in this repository worktree.

## Gate Summary

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead `ga-w1t8yf` is closed with `Review verdict: PASS`; deploy bead `ga-f0efcs` notes say the clean deploy branch was prepared after reviewer PASS. |
| 2 | Acceptance criteria met | PASS | The release branch adds a controller retention watchdog for closed order-tracking beads, wires it into `dispatchOrders` between the order sweep and nudge/mail sweep, uses a 15 minute interval, enforces a 100-bead per-invocation budget, preserves the retain-10 floor, skips zero/negative retention settings, and logs pruning/errors. Tests cover interval skip, pruning, logging, nil config, timestamp stamping, cross-store budget, budget exhaustion, retain floor, and zero limit. |
| 3 | Tests pass | PASS | Focused `go test ./cmd/gc` retention runs PASS; first `make test-fast-parallel` failed from `/tmp` exhaustion only (`no space left on device` in linker/compiler output), then `TMPDIR=/var/tmp make test-fast-parallel` PASS; `TMPDIR=/var/tmp go vet ./...` PASS. |
| 4 | No high-severity review findings open | PASS | Review notes list no blocking findings, no security issues, and only two minor non-blocking observations. Unresolved HIGH findings: 0. |
| 5 | Final branch is clean | PASS | `git status --short --branch` was clean on `release/ga-f0efcs-order-retention-watchdog` before adding this gate file; `core.hooksPath` is `.githooks`. |
| 6 | Branch diverges cleanly from main | PASS | Release branch was cut from current `origin/main` (`23679bb44`) and `git merge-tree --write-tree origin/main HEAD` returned a tree without conflicts. |
| 7 | Single feature theme | PASS | Diff against `origin/main` touches only `cmd/gc/city_runtime.go`, `cmd/gc/city_runtime_test.go`, `cmd/gc/order_dispatch.go`, and `cmd/gc/order_dispatch_test.go`; the change set is one order-tracking retention subsystem. |

## Acceptance Evidence

| Acceptance area | Evidence |
|-----------------|----------|
| Watchdog cadence and budget constants | `cmd/gc/order_dispatch.go` defines `orderTrackingRetentionWatchdogInterval = 15 * time.Minute` and `orderTrackingRetentionWatchdogDeleteBudget = 100`. |
| Controller wiring | `cmd/gc/city_runtime.go` calls `runOrderTrackingRetentionWatchdog` in `dispatchOrders` after `runOrderTrackingSweepWatchdog` and before `runNudgeMailSweepWatchdog`. |
| Watchdog behavior | `runOrderTrackingRetentionWatchdog` skips calls inside the interval, stamps last-run time, opens all order-tracking stores, applies the retention policy, calls the bounded cross-store sweep, joins store/sweep errors, and logs prune counts. |
| Retention safety | `sweepClosedOrderTrackingRetention` and bounded variants return without deleting when TTL or limit are non-positive, force `retainLast` up to `minClosedOrderTrackingRetained`, and delete only closed `labelOrderTracking` beads older than the cutoff and beyond the retain floor. |
| Cross-store budget | `sweepClosedOrderTrackingRetentionAcrossStoresBounded` decrements the remaining budget per store and stops once the total deletion limit is exhausted. |
| Tests | `cmd/gc/city_runtime_test.go` covers watchdog skip, prune, log, nil config, and last-run stamping. `cmd/gc/order_dispatch_test.go` covers cross-store budget, budget exhaustion, retain floor, and zero limit. |

## Test Output Summary

```text
go test ./cmd/gc -run 'TestRunOrderTrackingRetentionWatchdog|TestSweepClosedOrderTrackingRetentionAcrossStoresBounded|TestSweepClosedOrderTrackingRetention'
ok  	github.com/gastownhall/gascity/cmd/gc	0.847s

go test ./cmd/gc -run 'TestOrderTrackingRetentionPolicy|TestSweepClosedOrderTrackingRetention'
ok  	github.com/gastownhall/gascity/cmd/gc	0.823s

make test-fast-parallel
FAIL: environmental retry only; /tmp tmpfs had 4.8G free and linker/compiler writes failed with "no space left on device".

TMPDIR=/var/tmp make test-fast-parallel
All fast jobs passed

TMPDIR=/var/tmp go vet ./...
PASS
```

## Review Findings

| Severity | Finding | Gate impact |
|----------|---------|-------------|
| LOW | Comment in `sweepClosedOrderTrackingRetentionAcrossStoresBounded` mentions a patched policy though the budget is enforced through the bounded wrapper. | Non-blocking; behavior and tests are correct. |
| LOW | Map iteration across order groups is non-deterministic. | Non-blocking; reviewer accepted this because no fairness guarantee is required. |
