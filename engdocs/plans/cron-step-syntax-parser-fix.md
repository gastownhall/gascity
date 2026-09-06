# Plan: fix `mol-dog-stale-db` cron silent failure (step-syntax parser bug)

> **PM:** `gascity/pm`
> **Source bead:** `ga-0ipc.4.1` (filed by `gascity/architect` from `ga-0ipc.4` burn-in audit)
> **Originating PR:** `#1548` (merged 2026-05-03, no firings since)
> **Designer handoff:** `gm-qlqfnc` (2026-05-11)
> **Authoritative design:** in `ga-0ipc.4.1` notes (~38KB)
> **Designer diagrams:**
> - `/home/jaword/projects/gc-management/.gc/worktrees/gascity/designer/ga-0ipc.4.1/cron-dispatch-flow.png` — current dispatch path with bug pinpointed
> - `/home/jaword/projects/gc-management/.gc/worktrees/gascity/designer/ga-0ipc.4.1/cron-parser-after-fix.png` — proposed `cronFieldMatches` semantics

## Problem

`mol-dog-stale-db` is the only `type=formula, trigger=cron,
schedule="0 */4 * * *", pool=dog` order in production. After 8 days
of burn-in, the architect audit (`ga-0ipc.4`) found:

- Zero `order.fired` events with subject `mol-dog-stale-db` in 240h
- `gc order check` reports `cron: schedule not matched` every tick
- Operational consequence: **272 stale `testdb_*` databases,
  3 orphan `dolt sql-server` processes, 5.3 GB reclaimable disk,
  252 MB reclaimable RSS.**

## Root cause (settled by designer)

`cronFieldMatches` in `internal/orders/triggers.go:119-130` does
not understand `*/N` step syntax. It supports `*`, exact integer,
and comma-separated values only. The deployed schedule
`0 */4 * * *` calls `strconv.Atoi("*/4")` which errors and returns
`false`. The hour field is therefore never matched, every tick
falls through to `"cron: schedule not matched"`, and no firing
event is ever emitted.

This is the only deployed schedule using step syntax — which is
exactly why no other order has tripped this and the bug went
undetected for 8 days. The `gc order check` reason text is the
**normal** reason for any cron at a non-matching minute, so the
silent failure was indistinguishable from "fine, just off
boundary right now."

## Plan

Two builder beads matching the designer's recommended PR shape.
The two are independent (no `bd dep add` edge between them);
priority (P1 vs P2) encodes the recommended landing order.

| Builder bead    | PR shape | Files                                                                                                                                              | Slice refs      |
|-----------------|----------|----------------------------------------------------------------------------------------------------------------------------------------------------|-----------------|
| **Parser fix**  | PR 1     | `internal/orders/triggers.go`, `internal/orders/triggers_test.go`                                                                                  | Design §2, §3, §5 |
| **Doctor check**| PR 2     | `internal/doctor/checks_order_firing.go` (new), `internal/doctor/checks_order_firing_test.go` (new), `cmd/gc/cmd_doctor.go` (registration only)    | Design §4       |

### PR 1 — parser fix + reason-text contract + regression (P1)

Extends `cronFieldMatches` to support `*/N`, `M-N`, `M-N/S`, and
to distinguish parse-error from no-match. Adds the load-bearing
regression test (§5) that pins the verbatim deployed schedule
string and asserts `Due=true` at `2026-05-12 00:00:00 UTC` — this
is what would have caught `#1548` at review time.

**Acceptance criteria:**

- `cronFieldMatches` extended to new signature
  `(field string, value, min, max int) (matched, parseOK bool)`
  per design §2.1; algorithm per §2.1 body decision tree.
- `checkCron` rewritten per design §2.2: scans all 5 fields in
  one pass, surfaces parse errors loudly, only returns
  `"cron: schedule not matched"` when every field parsed OK and
  at least one did not match.
- Existing `TestCronFieldMatches*` cases preserved (extended in
  place per §2.3 — no rename).
- New `TestCronFieldMatches` table: step (`*/4`), range (`9-17`),
  range-with-step (`9-17/2`), comma-composition (`0,*/15`), and
  parse-error cases (`abc`, `*/0`, `*/-1`, `5-3`, `24`, `-1`,
  `9-30`).
- New `TestCheckTriggerCronStepSchedule` verifies hours
  0/4/8/12/16/20 fire and 1/2/3/5/9/13/17/21/23 do not, all at
  minute 0; right-hour-wrong-minute (1/15/30/59) does not fire.
- New `TestCheckTriggerCronInvalidSchedule` verifies the
  reason-text contract from §3.1 (6 cases — field-count error
  and parse-field error for each).
- New `TestCheckTriggerCronMolDogStaleDbScheduleFires` (§5) is
  the **load-bearing regression**: pins
  `const deployedSchedule = "0 */4 * * *"`, asserts `Due=true`
  at `2026-05-12 00:00:00 UTC`, asserts
  `Reason == "cron: schedule matched"`. Doc comment names
  `#1548` and `ga-0ipc.4` so future readers can trace.
- `go test ./internal/orders/...` passes.
- `go vet ./...` clean.
- Reviewer audit: grep every `^schedule\s*=` under `examples/`
  and `.gc/system/packs/` and confirm each parses with
  `parseOK=true` under the new parser.

**HARD RULES carried from source:**
- Do not edit the schedule string. Cadence is correct; the parser
  is broken.
- Do not migrate the order from cron to cooldown. Designer ruled
  out Path B and Path C in §0/TL;DR.
- Do not pull in `robfig/cron/v3`. Designer declined in §3.3.

### PR 2 — doctor check `order-firing-current` (P2)

Adds a new `gc doctor` check that detects "this order has not
fired in N expected cycles" without depending on the order's own
success. Catches future silent-failure modes (suspended pool,
missing handler, parser regression) the same way it would have
caught `#1548` after a few hours.

**Acceptance criteria:**

- `OrderFiringCurrentCheck` struct implements `doctor.Check`
  per design §4.2 (`Name() string`, `CanFix() bool`,
  `Fix(*CheckContext) error`, `Run(*CheckContext) *CheckResult`).
- `Name()` returns `"order-firing-current"` (kebab-case, follows
  existing convention).
- `CanFix()` returns false; check is observation-only — the
  remediation depends on the cause.
- Status mapping per design §4.1 table:
  - `< expected * 1.5` → OK
  - `>= expected * 1.5 && < expected * 3` → Warning ("overdue")
  - `>= expected * 3` → Error ("CRITICAL: stale")
  - Never fired AND controller uptime `>= expected * 1.5` →
    Error ("never fired since controller start ...") **— this is
    the load-bearing branch that would have caught `#1548`.**
  - Never fired AND controller uptime `< expected * 1.5` → OK
    ("within first cycle").
- `computeExpectedInterval` correctly derives:
  - `0 */4 * * *` → `4h`
  - `*/15 * * * *` → `15m`
  - `0 3 * * *` → `24h`
  - `0 9-17 * * *` → `1h`
- Cooldown orders use parsed `Interval` value directly.
- Manual / event / condition triggers are skipped (never produce
  a non-OK row).
- Registered in `cmd/gc/cmd_doctor.go` near line 145 with other
  config-dependent checks.
- 7 tests per design §4.3 pass:
  - `TestOrderFiringCurrent_NeverFired_BeyondUptime` → Error
  - `TestOrderFiringCurrent_NeverFired_WithinFirstCycle` → OK
  - `TestOrderFiringCurrent_FiredRecently` → OK
  - `TestOrderFiringCurrent_Overdue` → Warning
  - `TestOrderFiringCurrent_Stale` → Error
  - `TestOrderFiringCurrent_IgnoresManualAndEventTriggers`
  - `TestComputeExpectedIntervalForCronSchedules` (table)
- Uses only existing events (`order.fired`) and controller
  uptime. **No new event type, no new bead label.** (Designer
  rejected `events.OrderSkipped` in §4.4 — volume + Bitter Lesson.)
- `clock` field is injectable (`func() time.Time`, default
  `time.Now`) so tests can fix uptime windows.
- `go test ./internal/doctor/...` passes.
- `go test ./...` passes overall.

## Sequence

The designer notes "no path-dependency between them" and "PR 2 is
the observability backstop and lands independently." I encode the
recommended order via priority, not dep edges:

- **PR 1 is P1** — fixes the production bug; ships first.
- **PR 2 is P2** — observability backstop; ships independently.

Builder may technically work on PR 2 first, but `bd ready` will
sort P1 above P2. If the builder picks PR 2 first the design still
holds — the doctor check is the canary that confirms PR 1 worked.

## Out of scope (separate beads)

- One-time backlog drain (272 DBs / 3 PIDs / 5.3 GB) — tracked in
  sibling bead `ga-0ipc.4.2`.
- Cadence audit (4h vs nightly) — deferred per `ga-0ipc.4`'s
  closing note until one full week of post-fix firing data exists.
- `gc dolt-cleanup --force` TTY-only operator gesture — covered by
  `#1549`, not a builder task.
- Cron library swap (`robfig/cron/v3`) — declined per design §3.3.

## Verification (after both PRs land)

```bash
# Slice A behavioural check:
gc order check 2>&1 | grep stale-db
# At a 4-hour boundary minute (00:00, 04:00, 08:00, 12:00, 16:00, 20:00 UTC):
#   mol-dog-stale-db   cron   yes   cron: schedule matched
# Otherwise:
#   mol-dog-stale-db   cron   no    cron: schedule not matched

# After at least one firing window has elapsed post-deploy:
gc events --since 24h --type order.fired | grep stale-db
# expect: at least 1 row

# Slice C doctor backstop:
gc doctor --verbose | grep -A2 order-firing-current
# Before PR 1 lands AND after PR 2 lands: CRITICAL (never fired in 8d uptime).
# After PR 1 fix runs at least once: OK.
```

## Builder beads

- **`ga-0x1yxl`** — PR 1, parser fix + reason-text + regression (P1, `ready-to-build`)
- **`ga-hlhxo7`** — PR 2, doctor check `order-firing-current` (P2, `ready-to-build`)

Both routed to `gascity/builder` via `gc.routed_to` metadata. No
`bd dep add` edge between them — designer says no path-dependency.
