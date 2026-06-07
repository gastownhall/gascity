# Plan: live-session SHOW PROCESSLIST probe + fail-closed (ga-nw4z6 slice 2/5)

> **Status:** decomposing — 2026-05-11
> **Parent architecture:** `ga-nw4z6` — P0 DATA LOSS: `gc dolt cleanup`
> marks the live `beads` DB as orphan.
> **Designer spec:** `ga-lyv6d4` — pins SQL string verbatim, 2-second
> deadline, `live-session` Skipped reason, `live-session-probe-failed`
> ForceBlocker kind, 5 verbatim test names, interface extension on
> `CleanupDoltClient`, `applyLiveSessionsToPlan` + `hasFatalForceBlocker`
> helpers, `runDropStage` wiring, `--force` exit-1 rule.
> **Branch:** `local/integration-2026-04-30` (rig root
> `/home/jaword/projects/gascity`). Slice 1 (`ga-0txff0`, closed
> 2026-05-11) already landed the Go runner on this branch.
> **Sibling slices:** `ga-78stvc` (slice 3, bash forwarder),
> `ga-endmgy` (slice 4, audit log), `ga-qg23tv` (slice 5, regression
> test).
> **Decomposed into:** 1 builder bead (see Children below)

## Context

`gc dolt cleanup` today operates on a two-signal contract: stale
prefix match + rig-protection. Slice 2 introduces the **third**
oracle — a `SHOW PROCESSLIST` cross-check against the live Dolt
server. The probe runs once per cleanup, bounded by 2 s. Its job
is binary: any DB that has a live session at probe time is held
back from the DROP loop with `Skipped[reason=live-session]`. If
the probe itself fails (timeout / auth / network / malformed
result), the runner records a `ForceBlocker[kind=live-session-probe-failed]`
and — under `--force` only — exits 1 with no DROPs, no purge,
no reap (FAIL-CLOSED).

Dry-run survives a probe failure: the report renders the would-be
ToDrop set plus the visible ForceBlocker, exit 0. The architect
explicitly bound this asymmetry (`ga-nw4z6` §"In dry-run mode,
the report is emitted with the blocker visible, but the exit code
stays 0").

The designer (ga-lyv6d4) pinned every literal string and signature:

- Exact SQL (`SELECT db, COUNT(*) FROM information_schema.processlist
  WHERE db IS NOT NULL AND db != '' GROUP BY db`).
- Exact deadline (`2 * time.Second`).
- Exact reason / kind strings (`live-session` /
  `live-session-probe-failed`).
- Exact interface method signature
  (`ProbeLiveSessions(ctx) (map[string]int, error)`).
- Exact helper bodies (`applyLiveSessionsToPlan`,
  `hasFatalForceBlocker`).
- Exact `runDropStage` wiring (probe AFTER `planDoltDrops`, BEFORE
  the dry-run / drop branches).
- Five verbatim `Test*` names — no paraphrasing, no abbreviation.

## Why a single builder bead

The work touches four files in `cmd/gc/` that share one test
surface (`dolt_cleanup_drop_test.go`) and one interface
(`CleanupDoltClient`). Splitting interface, implementation,
helpers, and tests into separate beads would force one of them
to land in an inconsistent state (e.g., interface method declared
but no implementation, or fake gains `ProbeLiveSessions` before
the production type does). The design body is fully verbatim;
the builder has no judgment calls to spread. One PR, four files,
five new tests, ~150 LOC net. One bead.

## Children

| ID            | Title                                                                                  | Routing label    | Routes to         | Depends on |
|---------------|----------------------------------------------------------------------------------------|------------------|-------------------|------------|
| `ga-9h05hk`   | feat(dolt-cleanup): live-session SHOW PROCESSLIST probe + fail-closed (ga-nw4z6 slice 2/5) | `ready-to-build` | `gascity/builder` | (none; slice 1 closed) |

## Acceptance for the parent (ga-lyv6d4)

Met when `ga-9h05hk` closes and all of the following hold (these
mirror the designer's §8 acceptance checklist):

- [ ] `cleanupLiveSessionProbeQuery` constant in
      `cmd/gc/dolt_cleanup_drop.go` byte-equals ga-lyv6d4 §2.1 SQL.
- [ ] `cleanupLiveSessionProbeTimeout = 2 * time.Second` in
      `cmd/gc/dolt_cleanup_drop.go`.
- [ ] `DropSkipReasonLiveSession = "live-session"` exported in
      `cmd/gc/dolt_cleanup_drop_planner.go`.
- [ ] `cleanupErrorKindLiveSessionProbeFailed = "live-session-probe-failed"`
      (unexported) in `cmd/gc/cmd_dolt_cleanup.go`.
- [ ] `CleanupDoltClient` gains
      `ProbeLiveSessions(ctx) (map[string]int, error)` between
      `PurgeDroppedDatabases` and `Close` per ga-lyv6d4 §4.1.
- [ ] `*sqlCleanupDoltClient.ProbeLiveSessions` body matches
      ga-lyv6d4 §4.2 verbatim.
- [ ] `applyLiveSessionsToPlan` helper matches ga-lyv6d4 §4.3
      verbatim (pure, no context, no I/O).
- [ ] `hasFatalForceBlocker(report)` matches ga-lyv6d4 §4.4 verbatim.
- [ ] `runDropStage` wires the probe AFTER `planDoltDrops` and
      BEFORE the dry-run / drop branches per ga-lyv6d4 §5.1.
- [ ] `runDoltCleanup` exits 1 under `--force` iff
      `hasFatalForceBlocker(&report)` per ga-lyv6d4 §5.2.
- [ ] All 5 tests `TestProbeLiveSessions_HealthyServer`,
      `TestProbeLiveSessions_TimesOut`,
      `TestProbeLiveSessions_FailClosed`,
      `TestProbeLiveSessions_DryRunSurvivesFailure`,
      `TestProbeLiveSessions_RemovesFromToDrop` exist and pass.
- [ ] `fakeCleanupDoltClient` gains `liveSessions`,
      `liveSessionsErr`, `probeCalls` fields + `ProbeLiveSessions`
      method per ga-lyv6d4 §6.6.
- [ ] All existing `TestRunDoltCleanup_*` tests pass without
      modification — the fake's empty-default `liveSessions`
      yields no live sessions, preserving existing semantics.
- [ ] `go test ./cmd/gc/... -count=1` green; `go vet ./...` clean.
- [ ] No `CleanupReport.LiveSessions []` field added (ga-lyv6d4 §3.3).

## Notes for the builder

- **Read ga-lyv6d4 in full.** The design body is the contract;
  the bead body in `ga-9h05hk` is a high-level summary. The
  five tests' assertions are pinned to literal strings — tests
  must assert on equality, not substring.
- **2-second deadline is part of the contract, not a wall-clock
  reality.** `TestProbeLiveSessions_TimesOut` uses a 50 ms
  override to keep CI fast; `TestProbeLiveSessions_FailClosed`
  pins the literal constant value via direct reference. Both
  patterns ship.
- **Fake defaults to no-op.** Existing
  `TestRunDoltCleanup_*` tests MUST keep working without
  modification. Do not add a default `liveSessions` value to any
  existing test fixture — only the new five tests set the field
  explicitly.
- **No human-rendering changes.** `emitForceBlockersSection`
  already prints `kind` + `error`; the new kind needs no
  special-case rendering. Resist the urge to add one — the
  architect kept human format generic intentionally.
- **No JSON envelope additions.** `CleanupReport` stays as-is.
  Slice 4 (`ga-endmgy`) owns audit-log; a hypothetical
  `LiveSessions []` field can land then if needed.
- **`hasFatalForceBlocker` keeps it surgical.** Only the new
  kind triggers exit 1 in --force; rig-protection and
  max-orphan-refusal keep their existing behavior (the architect
  did not authorize a sweeping exit-code overhaul).

## Out of scope

These belong to siblings of `ga-nw4z6` and must not creep into
this slice:

- Bash forwarder + dog formula changes → slice 3 / `ga-78stvc`.
- `cleanup-audit.log` writes → slice 4 / `ga-endmgy`.
- `TestDoltCleanupRefusesLiveBeadsDatabase` regression test →
  slice 5 / `ga-qg23tv` (already declared via Blocks edge on
  `ga-lyv6d4`).
- Adding `CleanupReport.LiveSessions []` field → permanently
  out of scope per ga-lyv6d4 §3.3.
- Retry / cancellation logic — single shot, single deadline,
  fail-closed (architect risk-table).

## Validation gates

- `go test ./cmd/gc -run TestProbeLiveSessions -count=1` green.
- `go test ./cmd/gc -count=1` green (all existing
  `TestRunDoltCleanup_*` keep passing without edits).
- `go vet ./...` clean.
- `git diff` confined to: `cmd/gc/dolt_cleanup_drop.go`,
  `cmd/gc/dolt_cleanup_drop_planner.go`,
  `cmd/gc/cmd_dolt_cleanup.go`,
  `cmd/gc/dolt_cleanup_drop_test.go`. No other files modified.
- Typed wire: no `map[string]any` or `json.RawMessage` introduced
  on wire types (the `map[string]int` is internal to the probe
  return; it never crosses an HTTP/SSE boundary).
- ZFC: no role names in the diff.

## Risks and unknowns

- **`liveSessions` map preserves thread-count.** Slice 2 does
  not USE the thread count yet — the architect and designer
  agreed to keep the data flowing end-to-end in case slice 4's
  audit log wants it without re-probing. The builder MUST NOT
  drop the count to simplify the helper signature; preserving it
  is part of the contract.
- **System databases are never in ToDrop.** `information_schema`,
  `mysql`, `performance_schema`, `sys`, `dolt_cluster`,
  `__gc_probe` may appear in the probe's result map but never
  trigger spurious Skipped entries because the planner's
  `systemDatabaseNames` already excludes them from ToDrop
  candidates. `TestProbeLiveSessions_RemovesFromToDrop` has a
  case (`beads` in live set but not in ToDrop) verifying this.
- **`probeCancel()` placement.** The designer placed
  `probeCancel()` immediately after the `ProbeLiveSessions`
  call (not deferred to the function end) — the context is
  for the probe alone, not the surrounding stage. Keep this
  shape.
