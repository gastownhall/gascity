# Idle-claim nudge — follow-ups

The reconcile-tick backstop `nudgeStalledPoolClaims` (cmd/gc/idle_nudge.go)
re-delivers a claim nudge to a pool slot that is running but whose assigned
trigger bead is still unclaimed. It now runs for every runtime (herdr and
tmux); the call-site capability gate was removed because tmux's relaunch/respawn
path only heals a session that died, never a live-but-idle slot, and activity
reporting lets the controller see such a slot without ever waking it to claim.

## Resolved follow-up: include ready unassigned pool-routed triggers

The backstop still keys on the slot's own `gc.trigger_bead_id`. Desired-state
now carries the concrete ready, routed, unassigned beads selected by the
default pool-demand probe alongside the assigned-work snapshot. The reconcile
tick gives both snapshots to `nudgeStalledPoolClaims`, so a bead slung after a
warm slot's startup turn remains visible once desired-state binds it as that
slot's trigger.

The ready-routed snapshot is separate from `AssignedWorkBeads`; assignment and
wake semantics remain assignee-only. It is also derived from the same
`Ready()` demand result that selected the trigger, rather than the broader open
route-repair scan, so blocked open work cannot drive the nudge backstop.

Store refs remain attached to the snapshot. The backstop matches
`gc.trigger_bead_store_ref` as well as the bead ID, preventing independent city
and rig stores with the same bead ID from waking the wrong slot. Legacy
sessions without a trigger store ref use an unambiguous ID-only fallback.

The existing persisted `observe → nudge → backoff → give-up` pacing and attempt
cap are unchanged, and the fix adds no store read to the reconcile hot path.
