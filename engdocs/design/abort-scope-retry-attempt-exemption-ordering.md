# Retry-attempt exemption ordering in `beadOutcomeFailed`

`internal/dispatch/runtime.go`'s `beadOutcomeFailed` is the shared predicate
behind three `gc.on_fail=abort_scope` decision sites: the non-retry abort
branch in `processScopeCheck` (:482), the abort branch in
`reconcileTerminalScopedMember` (:1491), and `terminalAbortScopeFailure`,
which the workflow finalizer uses to name the failing scope member.

The retry-attempt exemption (`isRetryAttemptSubject`) sat *after* the
unconditional `gc.outcome=fail` short-circuit, so it was only ever reached by
a bead whose outcome was bare or unrecognized. A retry attempt that actually
closed `gc.outcome=fail` hit the short-circuit first and was reported as a
terminal scope-abort failure regardless of whether the retry controller went
on to close pass. Because the retry controller itself defaults
`gc.on_fail=abort_scope` onto every attempt it spawns
(`internal/formula/ralph.go:237`), this made retries functionally inert for
any step carrying `gc.on_fail=abort_scope`: the first attempt's failure
aborted the scope before, or independently of, a later attempt's outcome.

Measured on the `mtg` rig, workflow `mtg-sjcls`: attempt 1 failed transiently
at 04:04, the retry controller spawned and passed attempt 2 by 04:32:22, and
five seconds later the scope's other members closed skipped with "an earlier
member of the same scope failed" — `reconcileTerminalScopedMember` reconciling
attempt 1 directly, independent of the retry controller's own outcome.

## Fix

Reorder the check: `beadOutcomeFailed` now tests
`onFailAbortScope && isRetryAttemptSubject(subject)` first and returns `false`
immediately when both hold, before looking at `gc.outcome` at all. Every other
branch is unchanged — a non-retry-attempt bead with `gc.outcome=fail` still
fails closed, a bare/unknown outcome on a non-retry `abort_scope` bead is
still fail-closed, and `gc.outcome=canceled` is still a terminal non-failure.
`isRetryAttemptSubject` and `terminalAbortScopeFailure`'s superseded-attempt
exclusion (#4008) are unchanged.

This is not lenience: exempting the individual attempt does not remove the
abort, it moves it to the level that owns the retry budget. The retry
controller carries `gc.on_fail=abort_scope` itself and still aborts the scope
when a retry is exhausted under `gc.on_exhausted=hard_fail`.

Because all three call sites already delegated to `beadOutcomeFailed` (and
`terminalAbortScopeFailure` calls it internally before layering its own
transient/superseded filters), fixing the one shared predicate converges all
three; no call site needed its own change.

## Status

Implemented. See `gc-pl7ujz` for the fault report and
`TestBeadOutcomeFailedRetryAttemptExemptionAndOptInTrim`,
`TestReconcileTerminalScopedMemberRetryAttemptExplicitFailDoesNotAbortScope`,
and `TestBeadOutcomeFailedConvergesAcrossAbortScopeDecisionSites` in
`internal/dispatch/runtime_test.go` for the regression coverage.
