# Shard watchdog process-group safety (ga-ksfalp)

## Goal

Keep the shard watchdog from signalling an unrelated process group when its
runner dies without executing a trap. The previous fix, ga-880tzy, tears down
the watchdog on INT, TERM, HUP, and normal exit. A SIGKILL or OOM kill bypasses
those traps. The watchdog can then outlive the original group and later see a
different group using the same numeric PGID. This is a P3 robustness follow-up,
separate from the landed signal-trap fix.

`scripts/lib/harness-reap.sh` currently sleeps for the watchdog budget, checks
`kill -0` on the group number, sends SIGQUIT, sleeps through the grace period,
then sends SIGKILL. Group existence alone does not prove group identity, and
there is no second identity check before KILL.

## Work packages

| Order | Bead | Route | Acceptance outcome |
| --- | --- | --- | --- |
| 1 | ga-plyjh4 | architect (`needs-architecture`) | Decide how to prove the target is the original run before each signal, how to handle unknown identity, and what applies on supported platforms. |
| 2 | ga-66qxub | validator (`needs-tests`) | Write safe RED regressions for a killed runner with a gone/reused group, an original group that survives the runner, and identity changing between QUIT and KILL. |
| 3 | ga-i8fygn | builder (`ready-to-build`) | Apply the chosen identity rule while preserving real hang termination, stack diagnostics, trap cleanup, and bounded runner exit. |

The blocker graph is ga-plyjh4 → ga-66qxub → ga-i8fygn. Each package also has a
`discovered-from:ga-ksfalp` edge back to the reviewer finding.

## Decision boundary and verification

PM is not choosing a PID, PGID, or platform mechanism. The architect must
settle the identity rule first, including the case where the group leader has
exited but descendants remain. The validator must test both safety and the
watchdog's intended effect on a genuinely stuck original run; a change that
simply disables the watchdog would fail the latter.

The regression fixture must be contained or use a safe signal seam. It must
not send a real signal to another agent's process group. Existing tests in
`scripts/harness_reap_test.go` cover termination of a wedged run and the absence
of an orphaned watchdog holding the caller's pipe; those remain part of the
builder's acceptance gate.

Source: reviewer bead ga-ksfalp, prior fix ga-880tzy, and current
`scripts/lib/harness-reap.sh` plus `scripts/test-go-test-shard`.
