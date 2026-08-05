## Summary

Every silent `continue` in `dispatchAllQueuedNudges`'s per-session loop
(`cmd/gc/nudge_dispatcher.go`) now records a skip reason: a `GC_DEBUG`-gated
debug log line, plus a running counter persisted in the queue state and
surfaced by `gc nudge status` (text and `--json`).

## Problem

Before this change, `dispatchAllQueuedNudges` returned `(0, nil)` whether
the queue was empty, nothing matched an open session, a matched target's
`workerObserveNudgeTarget` call errored, or a matched-and-*live* target's
observation reported `Running == false`. There was no way to tell these
apart after the fact — `grep -c "nudge dispatcher" supervisor.log` returns 0
hits in the entire log on a healthy-looking run, because the only line that
code path can ever emit is a hard-error line that never fires for a silent
skip.

ra-oudpha finding-3 documents a live specimen of exactly this gap: 8 queued
entries targeting `core.control-dispatcher`, a session confirmed live and
idle well past the poller's quiescence window, that were nonetheless never
claimed or attempted. The investigating mechanic could not safely bisect
which of the loop's several skip points was responsible without adding a
trace and restarting the supervisor — out of scope for a read-only pass.
This patch adds that instrumentation so the *next* occurrence is a `gc
nudge status --json` read (or a `GC_DEBUG=1` log line), not another cold
investigation.

## Fix

- `dispatchAllQueuedNudges` (`cmd/gc/nudge_dispatcher.go`) now takes a
  `debugOut io.Writer` parameter (the supervisor tick passes `cr.stderr`;
  tests pass `nil` to suppress, or a buffer to assert on).
- Every skip point in the loop is tagged with a reason: `no-target`,
  `not-matched` (routine — no pending item targets this session),
  `observe-error`, `not-running`, `not-delivered` /
  `not-delivered-error` (matched a live, running session, yet
  `tryDeliverQueuedNudgesByPoller` claimed/delivered nothing this tick —
  quiescence gate not yet clear, or nothing left claimable).
- Each skip both logs via the existing `logRoute`/`GC_DEBUG` route-audit
  convention (`route_log.go`) — `cmd=nudge-dispatch-tick route=skip
  reason=<reason> agent=<agent> session=<session> [detail=<err>]` — and
  increments an in-memory `skipCounts` map for the tick.
- After the loop, `skipCounts` is merged once into a new
  `nudgequeue.State.DispatchSkips map[string]int64` field via a new
  `recordNudgeDispatchSkips` helper (one extra `withNudgeQueueState`
  transaction per tick, only when there's something to record — never
  nested inside the loop's own claim-path transactions, since the queue
  flock is per-process and not reentrant).
- `gc nudge status` reads `DispatchSkips` straight off the persisted state
  (a raw `nudgequeue.LoadState`, not through the target-scoped
  `listQueuedNudgesForTarget`) and prints it — city-wide, not agent-scoped —
  in both `--json` (`dispatch_skips`) and text output (`dispatch-tick skips
  (city-wide, all agents, cumulative):`).

The counters are a running total since the queue state file was created;
there's no reset/rotation in this patch.

### Also investigated: the second silent gate for live sessions

The bead asked me to identify and fix the specific gate zero-stamping the 8
`core.control-dispatcher` entries, if the trace made it obvious. It
doesn't, from static analysis alone — `workerObserveNudgeTarget` collapses
"genuinely not running" and "generation-mismatch zeroed obs.Running"
(via `nudgeTargetLiveGenerationMatches`) into the same `obs.Running ==
false`, and `pollerSessionIdleEnough`'s `IdleWaitProvider` fallback branch
can also silently return `false` for a provider that doesn't implement that
interface. Both are now covered by this patch's `not-running` /
`not-delivered` counters respectively, so a live recurrence will show up as
a nonzero, attributable count instead of nothing — but pinning which of
those (or another path) explains a specific past incident needs a live
trace against the actual failing session, which this fork-patch pass did
not have access to (the city this bead is scoped against is live and
read-only for this pass). Reporting per the bead's "report if not obvious"
instruction rather than guessing at a fix.

## Testing

- `TestDispatchAllQueuedNudgesRecordsSkipReasons`
  (`cmd/gc/nudge_dispatcher_test.go`): drives the "matched, live session,
  `obs.Running == false`" path (a stopped ACP session — same setup as the
  existing `TestDispatchAllQueuedNudgesSkipsACPSessionWhenNotRunning`),
  asserts the `GC_DEBUG` output contains `route=skip reason=not-running
  agent=worker`, and asserts `nudgequeue.LoadState`'s `DispatchSkips["not-running"]
  == 1`.
- `TestCmdNudgeStatusSurfacesDispatchSkips` (`cmd/gc/cmd_nudge_test.go`):
  seeds `DispatchSkips` via `recordNudgeDispatchSkips`, then asserts both
  `gc nudge status --json` (`dispatch_skips` field) and text output surface
  it.
- Fail-before proven: temporarily stubbed `recordNudgeDispatchSkips` and
  `logNudgeDispatchSkip` to no-ops — both new tests fail
  (`dispatch_skips = map[string]int64(nil)`, `debug output missing
  not-running skip line`). Restored → both pass.
- `go test ./cmd/gc/... -run 'TestDispatchAllQueuedNudges|TestNudgeStatus|TestListQueuedNudges'` — all PASS, no regressions in the existing nudge suite (the 7 pre-existing call sites of `dispatchAllQueuedNudges` were updated for the new trailing parameter, all passing `nil`).
- `go build ./...` and `go vet ./...` — clean.
- Full `go test ./cmd/gc/... ./internal/nudgequeue/...`: `internal/nudgequeue` all PASS. `cmd/gc` had 2 failures on the first run — the same pre-existing `TestResolveImportRoot*` macOS tmpdir-symlink flakes documented on the sibling `fix/nudge-queue-ttl-sweep` patch — plus one run that hit Go's 10-minute test-binary timeout under heavy concurrent load from other test suites running on this box at the same time (a deliberately-injected panic-recovery test, `order boom`, plus real subprocess/dolt-server tests in this very large package); a longer-timeout rerun in isolation is the authoritative signal for this patch and is referenced in the ra-oudpha bead comment.

## Scope

`cmd/gc/nudge_dispatcher.go` (skip tracking + `logNudgeDispatchSkip`),
`cmd/gc/cmd_nudge.go` (`recordNudgeDispatchSkips`, `nudgeStatusJSON` field,
text/JSON rendering, `sort` import), `cmd/gc/city_runtime.go` (thread
`cr.stderr` through), `internal/nudgequeue/state.go` (new `DispatchSkips`
field), plus the 7 existing test call sites updated for the new trailing
parameter. No change to delivery semantics — purely additive observability.
