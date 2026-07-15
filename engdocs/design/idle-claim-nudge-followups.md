# Idle-claim nudge — follow-ups

The reconcile-tick backstop `nudgeStalledPoolClaims` (cmd/gc/idle_nudge.go)
re-delivers a claim nudge to a pool slot that is running but whose assigned
trigger bead is still unclaimed. It now runs for every runtime (herdr and
tmux); the call-site capability gate was removed because tmux's relaunch/respawn
path only heals a session that died, never a live-but-idle slot, and activity
reporting lets the controller see such a slot without ever waking it to claim.

## Open follow-up: widen the trigger key to unassigned pool-routed beads

The backstop keys on the slot's own `gc.trigger_bead_id`. That value is stamped
only when the desired-state builder binds a specific bead to the slot — the
`resume` and `wake-known-identity` tiers, both of which act on work that already
carries an assignee (`cmd/gc/pool_desired_state.go`). A bead slung to the pool
**after** the slot went idle and left **unassigned** (`gc.routed_to = <pool>`,
status `open`, no assignee) never stamps `trigger_bead_id`, so it is invisible
to the backstop: `triggerID == ""` short-circuits the loop.

Result: the un-gate closes the bound-slot case (the reconciler handed this slot
a specific bead, but its submit-CR was swallowed or it survived a `gc restart`
without a re-Start). The scale-from-zero-style case — an unclaimed pool bead
waiting for any warm slot to notice it — is still not woken on tmux.

### Sketch of the fix

For each running pool slot with an empty `trigger_bead_id`, look for a bead
where `gc.routed_to` resolves to the slot's template, status is `open`, and the
assignee is empty; past the observe grace, nudge the slot to run its claim hook.

Constraints to preserve the churn-free property:

- Keep the persisted `observe → nudge → backoff → give-up` marker, but key it on
  the candidate bead id (or the slot when no single candidate dominates) so a
  restart cannot replay it.
- The unclaimed pool bead may not be present in the reconciler's
  `AssignedWorkBeads` snapshot (that slice is assignment-oriented). The widened
  path needs a source of open+routed+unassigned pool beads; confirm which
  snapshot already carries them before adding a new read to the hot path.
- Multiple idle slots seeing one unclaimed bead will each nudge. That is bounded
  by the grace/backoff/attempt caps and self-limits the instant the first slot
  claims (the bead flips to `in_progress`), but measure it before shipping.

This is deliberately left for its own PR: it changes what the backstop reads,
not just when it runs, and the churn analysis is the load-bearing part.
