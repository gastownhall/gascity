# Bead Effective-State Taxonomy

A bead's raw `status` field (`open`, `in_progress`, `closed`, …) tells you
what happened last; the **effective state** tells you *who owns the next
action*. The classifier collapses status + dependencies + delivery phase +
session binding + labels + type + routing into a single unambiguous state per
bead.

## The 16 effective states

| State | Owner | Description |
|---|---|---|
| `done` | — | Closed or delivery reached a terminal phase (merged/abandoned). No action needed. |
| `deferred` | scheduler | Deferred to a future date; hidden from ready/blocked views until then. |
| `pinned` | — | Explicitly frozen; excluded from automatic routing and dispatch. |
| `orchestration` | controller | gc-internal bookkeeping bead (non-work type, wisp, nudge/order/wisp title, or gc.kind-tagged). Controller owns these. |
| `delivering` | agent | A delivery phase is active (building, ci-pending, rework, merge-pending, conflicted). The assigned agent is working. |
| `waiting-review` | human | Delivery phase is `review-pending`. Awaiting human code review. |
| `waiting-decision` | human | Delivery phase is `decision-pending`. Awaiting a human-made decision. |
| `in-progress` | agent | Actively held by a session (`status=in_progress`, `status=hooked`, or `gc.session_name` set to a live session). |
| `orphaned` | RECLAIM | Was held by a session that is no longer live. The claim should be reclaimed and re-dispatched. |
| `blocked-deps` | upstream-beads | Has at least one open blocking dependency (`blocks`, `waits-for` or `conditional-blocks` — whatever `beads.IsReadyBlockingDependencyType` accepts). Waiting for upstream work to close. |
| `waiting-human` | human | Open work that requires a human to act: `human` label, `decision` type, or `gc.do_not_auto_route=1`. |
| `epic-triage` | human/triage | An epic (type=epic or title prefix EPIC) that needs decomposition before it can be dispatched. |
| `routed-waiting` | agent-pool | Routed to a pool via `gc.routed_to` and the target rig has live sessions. Waiting for a session to claim it. |
| `routed-stalled-dispatch` | RECLAIM | Routed to a pool but the target rig has **no live sessions** (dispatcher may be down or pool exhausted). Needs operator attention. |
| `ready-unrouted` | DISPATCHER | Open, unblocked, plannable, not yet routed. The dispatcher should pick this up on the next hook cycle. |
| `unknown` | INVESTIGATE | Did not match any state in the decision tree. A gap in coverage — surface and fix. |

## Decision tree (precedence order)

The classifier evaluates each bead against the following rules in order; the
first match wins.

1. **Done** — `status=closed` OR delivery phase is `merged`/`abandoned`
2. **Deferred** — `status=deferred`
3. **Pinned** — `status=pinned`
4. **Orchestration** — non-work type (not task/bug/feature/chore/epic/decision),
   OR bead ID contains `-wisp-`, OR title starts with `nudge:`/`order:`/`wisp:`,
   OR `gc.kind` metadata is set
5. **Waiting-review** — `gc.phase=review-pending`
6. **Waiting-decision** — `gc.phase=decision-pending`
7. **Delivering** — `gc.phase` is an active agent phase (building, ci-pending,
   rework, merge-pending, conflicted)
8. **In-progress** — `status=in_progress` OR `status=hooked` OR `gc.session_name`
   is set to a live session (see "What live means" below)
9. **Orphaned** — `gc.session_name` is set but the session is no longer live
10. **Blocked-deps** — bead has an open blocking dependency
11. **Waiting-human** — `human` label, `decision` type, or `gc.do_not_auto_route=1`
12. **Epic-triage** — `type=epic` or title starts with `EPIC`
13. **Routed-stalled-dispatch** — `gc.routed_to` is set but the target rig has
    no live sessions (checked via `liveRigs` set)
14. **Routed-waiting** — `gc.routed_to` is set
15. **Ready-unrouted** — `status=open`, in the ready set, and a plannable type
    (task/bug/feature/chore)
16. **Unknown** — nothing matched

## What "live" means

`live` and `liveRigs` are built in `cmd/gc/cmd_beads_state.go` from the open
session beads, and a session counts as live when the canonical lifecycle
projection says it still occupies a slot —
`session.ProjectLifecycle(...).CountsAgainstCap`. That is the same predicate
capacity accounting uses, so liveness here cannot drift from liveness there.
It admits `active`, `creating`, `start-pending`, `draining` and `quarantined`,
and excludes `failed-create`, `orphaned`, `closing`, `stopped`, `archived`,
`asleep`, `drained` and `suspended`.

A session bead with no `state` stamped at all is treated as live on purpose.
The projection cannot judge it, and for an anomaly detector a missed orphan is
far cheaper than a fabricated one that sends an operator to reclaim a healthy
session.

**Not yet implemented: zombie exclusion.** A session whose bead still reads
`active` but whose runtime process is gone is a *zombie*, and it currently
counts as live — so a bead routed at an all-zombie rig classifies
`routed-waiting` rather than `routed-stalled-dispatch`. Detecting that needs a
runtime-alive probe (the `last_active == 0001-01-01T00:00:00Z` marker is a
runtime-enriched field on `session.Info`, not bead metadata), and the runtime
probe is only reachable through a session Manager, which non-test `cmd/gc`
files may not construct under the worker-boundary migration. Closing this needs
a `worker.Handle` route for runtime liveness; until then
`routed-stalled-dispatch` fires only when a rig has no live-*stated* sessions
at all.

## Dormant states

Four of the sixteen states cannot fire today, because nothing in gascity writes
the metadata they key on. They are implemented and tested in the classifier so
that they light up the moment a producer lands, but a report will never show
them yet:

| State | Keys on | Blocked because |
|---|---|---|
| `delivering` | `gc.phase` (building, ci-pending, rework, merge-pending, conflicted) | No writer for `gc.phase` exists. |
| `waiting-review` | `gc.phase=review-pending` | Same. |
| `waiting-decision` | `gc.phase=decision-pending` | Same. |
| `done` via delivery | `gc.phase` in merged/abandoned | Same; `done` still fires normally via `status=closed`. |

The same applies to one *input* rather than a state: rule 11 also accepts
`gc.do_not_auto_route`, which has no producer either, so `waiting-human` fires
only via the `human` label or the `decision` type.

The originating plan asserted these phase constants came from
`internal/delivery/phase.go` "confirmed present". That package does not exist.
Treat the phase-dependent rows as forward-compatible wiring, not as coverage.

## Sync requirements

Two constant sets in `internal/beads/state/classify.go` must stay in sync with
the dispatcher's unrouted-feeder definitions:

- **`workTypes`** — the set of bead types treated as human-meaningful work.
  Anything not in this set is classified as `orchestration`. Must match the
  dispatcher's `PLANNABLE_TYPES` definition (which also includes `epic` and
  `decision` as non-dispatchable work types).

- **`internalTitleRe`** — the regex matching gc-internal beads by title prefix
  (`nudge:`, `order:`, `wisp:`). Must match the dispatcher's
  `GC_INTERNAL_TITLE_RE` definition.

If either set drifts, beads that should be `ready-unrouted` will instead appear
as `orchestration`, causing the dispatcher to skip them silently.

## Home

The Go implementation lives at `internal/beads/state/classify.go`.
The first-class CLI command is `gc beads state` (`cmd/gc/cmd_beads_state.go`).
