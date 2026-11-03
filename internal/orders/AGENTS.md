# Order dispatch lifetime

Create every tracked dispatch with `RunOpts.Deadline`, using the same absolute
deadline that bounds its execution context. Persist `gc.order.dispatch_deadline`
in the initial tracking-bead creation; a later update leaves a sweep race.

Stale and startup recovery sweeps must preserve a run without an outcome until
its deadline plus the completion grace period. This applies across processes
and controller reloads, so process-local in-flight counts alone are insufficient.
Reject malformed deadline metadata when selecting runs for destructive recovery.
Tracking beads without a deadline retain the legacy age/orphan recovery policy.

Keep the existing `no_work_gate` and idempotent gate-timeout overlap policies.
Ordinary orders use their open tracking bead to suppress overlapping dispatches.
