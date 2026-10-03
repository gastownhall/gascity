# Shard watchdog process-group safety (ga-ksfalp)

## Goal

Keep the shard watchdog from signalling an unrelated process group when its
runner dies without executing a trap. The ga-880tzy signal-trap fix tears down
the watchdog on INT, TERM, HUP, and normal exit, but it has **not landed on
`main`**. It is reviewed on `fix/ga-880tzy-watchdog-trap-teardown` at
`7a41b36c18`; deploy bead ga-ki4b1e is still open. A SIGKILL or OOM kill
bypasses those traps. The watchdog can then outlive the original group and
later see a different group using the same numeric PGID. This P3 robustness
follow-up builds on that pending fix.

`scripts/lib/harness-reap.sh` currently sleeps for the watchdog budget, checks
`kill -0` on the group number, sends SIGQUIT, sleeps through the grace period,
then sends SIGKILL. Group existence alone does not prove group identity, and
there is no second identity check before KILL.

## Work packages

| Order | Bead | Route | Acceptance outcome |
| --- | --- | --- | --- |
| 1 | ga-plyjh4 | architect (`needs-architecture`) | Decide how to prove the target is the original run before each signal, how to handle unknown identity, and what applies on supported platforms. |
| 2 | ga-66qxub | validator (`needs-tests`, completed) | Write safe RED regressions for a killed runner with a gone/reused group, a leaderless original group that survives the runner, and identity changing between QUIT and KILL. Tests are on `validator/ga-66qxub-tests`, based on the pending trap-fix commit. |
| 3 | ga-i8fygn | builder (`ready-to-build`) | Apply the chosen identity rule while preserving real hang termination, stack diagnostics, trap cleanup, and bounded runner exit. |

The blocker graph is ga-plyjh4 → ga-66qxub → ga-i8fygn. Each package also has a
`discovered-from:ga-ksfalp` edge back to the reviewer finding.

The builder should base its work on the validator branch, which itself is based
on the reviewed trap-fix commit. The trap fix must land before this follow-up
can land. Closing deploy bead ga-ki4b1e only means its PR was opened; it does
not prove the fix merged. The PM will track that shipment separately from the
work-package blocker graph.

## Decision boundary and verification

The architect settled the rule in ga-plyjh4, including Amendment 1: refresh a
proven-member lease every 30 seconds, and verify an original member immediately
before each signal. This lets the watchdog stop a genuinely wedged descendant
whose group leader has exited, while refusing to signal a recycled group when
no original member remains. If identity cannot be proven, the watchdog does not
signal. The validator tests both safety and real hang termination; a change
that simply disables the watchdog fails the latter.

The regression fixture must be contained or use a safe signal seam. It must
not send a real signal to another agent's process group. Existing tests in
`scripts/harness_reap_test.go` cover termination of a wedged run and the absence
of an orphaned watchdog holding the caller's pipe; those remain part of the
builder's acceptance gate.

Source: reviewer bead ga-ksfalp, prior fix ga-880tzy, and current
`scripts/lib/harness-reap.sh` plus `scripts/test-go-test-shard`.
