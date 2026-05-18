# Release Gate: order-firing-current

- Date: 2026-05-17
- Shape: single-bead PR
- Deploy bead: ga-8p06rl — Review: order-firing-current doctor check
- Source bead: ga-hlhxo7 — Add gc doctor 'order-firing-current' check (silent-failure backstop for cron/cooldown orders)
- Branch: builder/ga-hlhxo7-1
- Source commit: 3c451855f feat(doctor): add order firing freshness check
- Release criteria source: deployer Release Gate Criteria. `docs/PROJECT_MANIFEST.md` is not present in this repository worktree.

## Gate Summary

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Deploy bead notes contain `Claude Review — pass` and `Verdict: PASS`. |
| 2 | Acceptance criteria met | PASS | `OrderFiringCurrentCheck` exists, exposes the required doctor check surface, registers as `order-firing-current`, monitors cron/cooldown orders, skips manual/event/condition triggers, reads `order.fired` and controller-start events, classifies OK/warning/error thresholds, and provides the requested inspection hint. Tests cover the seven named scenarios plus cooldown freshness. |
| 3 | Tests pass | PASS | `go test ./internal/doctor/...` PASS; `go test ./cmd/gc -run ^$` PASS; `go vet ./...` PASS; `make test-fast-parallel` PASS. |
| 4 | No high-severity review findings open | PASS | Review notes list one LOW code-smell finding and one LOW coverage gap. Unresolved HIGH findings: 0. |
| 5 | Final branch is clean | PASS | `git status --short --branch` was clean on `builder/ga-hlhxo7-1` before adding this gate file; pre-commit is active via `core.hooksPath=.githooks`. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-base --is-ancestor origin/main HEAD` returned 0 and `git merge-tree --write-tree HEAD origin/main` returned a tree without conflicts. |

## Acceptance Evidence

| Acceptance area | Evidence |
|-----------------|----------|
| Check shape | `internal/doctor/checks_order_firing.go` defines `OrderFiringCurrentCheck`, `NewOrderFiringCurrentCheck`, `Name`, `CanFix`, `Fix`, and `Run`; `cmd/gc/cmd_doctor.go` registers the check. |
| Trigger filtering | `Run` monitors only `cron` and `cooldown`; `TestOrderFiringCurrent_IgnoresManualAndEventTriggers` covers manual, event, and condition triggers. |
| Expected interval | Cron schedules are sampled over a 24-hour minute window with per-run schedule caching; cooldown intervals use `time.ParseDuration`. |
| Event history | The check reads `order.fired` events and the latest `controller.started` event from `.gc/events.jsonl`. |
| Classification | Tests cover never-fired beyond uptime, never-fired within first cycle, recently fired, overdue, stale, ignored triggers, and cron interval computation. |
| Remediation surface | Non-OK results set `FixHint` to `Inspect with: gc order check && gc order history <name>`. |

## Test Output Summary

```text
go test ./internal/doctor/...        PASS
go test ./cmd/gc -run ^$             PASS
go vet ./...                         PASS
make test-fast-parallel              PASS
```

## Review Findings

| Severity | Finding | Gate impact |
|----------|---------|-------------|
| LOW | Cron interval computation samples a fixed 2026-05-12 day, so month/day-constrained schedules may report a diagnostic error. | Non-blocking; current intended schedules are daily or more frequent. |
| LOW | No test for month/day-constrained cron schedules. | Non-blocking; follow-up bead ga-gse1pe exists for order scanning cleanup and reviewer accepted this coverage shape. |
