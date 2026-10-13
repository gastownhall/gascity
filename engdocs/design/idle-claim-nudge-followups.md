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

## Resolved follow-up: a lapsed claim window stops counting as capacity

Giving up used to end the backstop's involvement: the slot stayed bound to its
trigger, idle at its prompt, and pool accounting kept reusing it as the
capacity for that ready work. Nothing replaced it, so the work starved for as
long as the slot lived (ga-2m9dwp: three awake slots, wedged after a provider
503 or simply done with their last bead, held four ready beads for hours).

The give-up step now latches the incarnation it gave up on. Once the slot has
been nudged to the cap on its current incarnation and the last nudge's backoff
has elapsed with the trigger still unclaimed, `poolClaimBackstop.exhausted`
writes the slot's `instance_token` to `idle_claim_nudge_lapsed_token`. Pool
accounting reads the latch through `poolSeatClaimLapsedInfo`:

- anonymous new demand does not reuse a lapsed slot
  (`reusablePoolSessionInfosForRequest`), so desired state mints a fresh slot
  for the work;
- scale demand does not keep a lapsed slot awake (`collectActiveBeads`), so the
  slot, holding no work and no other wake reason, drains like any idle slot and
  is closed, and the fresh slot takes its place.

The latch decides accounting only. Nothing nudges, interrupts or restarts the
lapsed slot because of it; its drain is the ordinary no-wake-reason drain of
an idle slot. A slot that holds or claims work keeps that work's wake reason,
and attached or pending slots stay up. Both reconcilers read the latch, because
legacy and v2 share the realization and awake-set code
(`TestSessionDifferentialFixtures`, "a seat whose claim window lapsed drains
while a fresh seat takes its ready work").

The attempt markers outlive a restart, so they are scoped to the slot's
incarnation: markers written before its wake stamp (`last_woke_at`) or its
confirmed start (`creation_complete_at`) pace its predecessor, and the restarted
slot is observed afresh with its own grace, nudges and give-up. The latch names
one `instance_token`, so a restarted slot counts as capacity again.

Two gaps stay open. The markers are keyed by the trigger the slot is bound to,
and anonymous demand binds slots to work by position, so when the head of a
busy queue keeps getting claimed, an idle slot is rebound before its window
runs out and may never lapse. And demand counted without per-bead provenance (a
custom `scale_check` that reports only a count) binds no trigger, so the
backstop never engages for it. Separately, demand that no slot can claim now
cycles fresh slots, one window apart per bead, instead of parking one slot on
it; demand/claim agreement (`demand_serve_predicate.go`) is what keeps such
demand from being counted.
