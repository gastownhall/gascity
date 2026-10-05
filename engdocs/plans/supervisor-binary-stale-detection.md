# Plan: Supervisor binary/pack drift detection + auto-restart (`ga-a3ry` family)

> Owner: `gascity/pm-1` · Created: 2026-04-30
> Source: architecture decision `ga-a3ry` (closed)
> Designer addendum: `gascity/designer` (in `ga-a3ry.1` notes)
> Cluster siblings (independent landings): `ga-5mym`, `ga-r8iz`

## Why this work exists

After `go install ./cmd/gc`, the on-disk binary is fresh but the
long-running `gascity-supervisor` process keeps the prior binary's
code path AND its in-memory pack snapshot. `gc start` returns
stale answers silently — the operator sees "city started" and
hours later notices their changes never took effect.

Architecture (`ga-a3ry`) chose to **query live state** (`/proc/<pid>/exe`,
on-disk mtimes, supervisor `/status`) **and auto-restart on drift**.
Honors CLAUDE.md "no status files" — discovery, not bookkeeping.

## Goal

Make the silent-staleness bug impossible: every `gc start` either
runs against a supervisor matching the local binary + pack
snapshot, or surfaces a clear drift report and (by default)
auto-restarts the supervisor to converge.

## Work breakdown

| Bead         | Title                                                                          | Priority | Routes to | Gate           |
|--------------|--------------------------------------------------------------------------------|----------|-----------|----------------|
| `ga-a3ry.1`  | Implement supervisor binary/pack drift detection + auto-restart in `gc start`  | P1       | builder   | ready-to-build |

The architect+designer broke this work down to one bead covering:
extending supervisor `/status` (BuildID + PackRoots), the new
`cmd/gc/drift.go` (DetectBinaryDrift, DetectPackDrift,
RestartSupervisor, PollReady), the `--no-auto-restart` and
`--dry-run` flags, the `[daemon].auto_restart_on_drift` config
field, the always-printed `Supervisor:` identity line, and unit +
integration tests.

This is on the larger end of single-bead scope. The builder may
split into two PRs at their discretion (e.g., supervisor `/status`
extension + drift detection in PR 1; CLI flags + integration tests
in PR 2). The bead remains the single tracking unit; PR splits are
mechanical.

## Coordination

Cluster siblings under "supervisor lifecycle robustness" —
`ga-5mym` (`gc-beads-bd-op-init-timeout`) and `ga-r8iz` (gc stop
lenient validation). All three land independently; no shared
code.

## Routing rationale

Designer addendum is present in the bead notes — covers operator
workflow comparison, terminal output anatomy (`Supervisor:`,
`Drift detected:`, `Restarting supervisor (`), the full flag-
combination matrix (six combos × two drift states = twelve test
cases), accessibility audit, edge cases, and the validator
acceptance checklist. No more design hops needed. Routed to
**builder** with `ready-to-build`.

## Acceptance criteria (rolled up)

- **`Supervisor:` line always prints** as the first line of
  `gc start` output. Format pinned: `Supervisor: pid=… exe=…
  buildID=… started=…`.
- **Bug repro test passes.** Rebuild gc, run `gc start`, observe
  drift report and auto-restart. Post-restart buildID matches
  local. (FR-2's pinned test.)
- **Headline strings pinned by test:** `Supervisor:`,
  `Drift detected:`, `Restarting supervisor (`,
  `error: supervisor binary/pack drift`.
- **Flag-combo matrix coverage.** All six combos × two drift
  states = twelve table-driven cases.
- **`--dry-run` never restarts.** Pinned by test.
- **`--no-auto-restart` exits 1.** Pinned by test.
- **Config kill-switch overrides flag.** `auto_restart_on_drift =
  false` + (no flags) → no restart; exit 1.
- **Permission-denied path returns descriptive error**, not a
  panic, not a silent mtime-fallback.
- **Restart loop guard.** 4th restart in 60s is refused with a
  loop-detected error.
- **No status files written.** CLAUDE.md "no status files"
  principle; grep for new files under `.gc/` or `~/.cache/gc`
  introduced by this change.
- **NFR-4: detection cost when no drift exists < 10 ms.**
  Benchmark; `BenchmarkDriftDetect_NoDrift`.
- **NFR-1: drift detection < 100 ms p95** with a realistic 5-pack
  city.
- **NFR-2: restart < 5s p95** captured in the integration test.
- **systemd path AND direct path both work.** Two integration
  tests under `//go:build integration`.

Full acceptance checklist is in the bead body's "Acceptance
checklist (for validator)" section.

## Risks and unknowns

- **systemd vs. direct launch divergence.** Test both paths.
  `systemctl status --user gascity-supervisor` is the systemd-
  managed signal; absent → direct-launch path.
- **Pack mtime races during rebuild.** Tolerate by re-reading
  once on drift detection before deciding.
- **buildID format drift.** Centralize the constant (commit
  `acc19d24` already provides build identity); pass through one
  helper.
- **Auto-restart loops.** Architect-mandated backoff: max 3
  restarts in 60 s, then exit with error. Pinned by test.
- **`pgrep -f` false positives** on a `grep gascity-supervisor`
  in another shell. Architect §11 says use the supervisor's
  known socket / PID-discovery path, not `pgrep -f`. Honor that.

## Out of scope (explicit)

- Detecting drift in agents already running.
- A `gc drift` standalone command.
- Auto-restart on third-party binary drift (only the gc binary).
- Applying drift detection to `gc reload` (PRD non-goal; designer
  recommends a follow-up bead `ga-a3ry-followup-reload`).
- Adding the `Supervisor:` line to `gc stop` (designer recommends
  a separate follow-up).
- A `--verbose-drift` flag listing per-file drift.

## Validation gates

- `go test ./...` green (includes new `cmd/gc/drift_test.go`).
- `go test -tags=integration ./test/...` green for the new
  `start_drift_integration_test.go` (systemd path + direct path).
- `go vet ./...` clean.
- `BenchmarkDriftDetect_NoDrift` passes the 10 ms NFR-4 budget.
- Manual smoke: rebuild gc, run `gc start`, confirm the drift
  detection + restart sequence reads as designed.
- One `bd remember` entry from the builder when this lands.
