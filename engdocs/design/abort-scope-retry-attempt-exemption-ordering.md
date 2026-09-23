# Retry-attempt exemption ordering in `beadOutcomeFailed`

`internal/dispatch/runtime.go`'s `beadOutcomeFailed` is the shared predicate
behind three `gc.on_fail=abort_scope` decision sites: the non-retry abort
branch in `processScopeCheck` (:482), the abort branch in
`reconcileTerminalScopedMember` (:1499), and `terminalAbortScopeFailure`
(:1955), which the workflow finalizer uses to name the failing scope member.
The predicate has three further consumers that aggregate outcomes rather than
decide aborts — `internal/dispatch/fanout.go:72`, `firstFailedFinalizeBlocker`
(:1857) and `resolveBlockedOutcome` (:1922); they see the changed answer too
but are unaffected in practice, because downstream steps block on the logical
control step rather than on attempt roots
(`internal/formula/retry.go:20`), so a retry attempt is not among the blockers
they aggregate over.

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

Reordering alone is not sufficient, because Attach-created attempt roots were
not recognized as retry attempts in the first place. `buildAttemptRecipe`
(`internal/dispatch/control.go:952`) now stamps
`gc.logical_bead_id = control.ID` alongside `gc.control_for`, so an attempt
spawned through `molecule.Attach` satisfies `isRetryAttemptSubject` at all.
The exemption stays confined to attempt and iteration roots: ralph body
children carry `gc.attempt` and a hardcoded `gc.on_fail=abort_scope` but never
`gc.logical_bead_id`, because `logicalRecipeStepID`
(`internal/molecule/molecule.go:1771`) only resolves step IDs ending in
`.attempt.N` or `.iteration.N`.

This is not lenience: exempting the individual attempt does not remove the
abort, it moves it to the level that owns the retry budget. The retry
controller carries `gc.on_fail=abort_scope` itself and still aborts the scope
when a retry is exhausted under `gc.on_exhausted=hard_fail`. The tradeoff is a
liveness one: the abort now depends on the controller eventually closing the
retry control `fail`, so a wedged controller leaves the scope open rather than
falsely aborting it.

Because all three call sites already delegated to `beadOutcomeFailed` (and
`terminalAbortScopeFailure` calls it internally before layering its own
transient/superseded filters), fixing the one shared predicate converges all
three; no call site needed its own change.

## Status

Implemented. See `gc-pl7ujz` for the ordering fault report and `gc-yydp6f` for
the missing `gc.logical_bead_id` stamp, plus
`TestBeadOutcomeFailedRetryAttemptExemptionAndOptInTrim`,
`TestReconcileTerminalScopedMemberRetryAttemptExplicitFailDoesNotAbortScope`,
and `TestBeadOutcomeFailedConvergesAcrossAbortScopeDecisionSites` in
`internal/dispatch/runtime_test.go`, and
`TestRetryLifecycleAttachedAttemptExemptFromFalseScopeAbort` in
`internal/dispatch/control_integration_test.go`, for the regression coverage.
