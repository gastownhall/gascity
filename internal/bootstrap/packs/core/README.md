# Core pack housekeeping orders

Deterministic housekeeping for a Gas City, shipped as part of the bundled
core pack. Every order here is **mechanical** — timer comparisons,
dependency lookups, event decoding — so the controller runs them directly
via `exec` instead of spending agent context. No LLM judgment, no wisps,
no agent pipeline.

Cities that include the core pack get every order below automatically;
none requires per-city configuration.

## Orders

| Order | Trigger | What it does |
| ----- | ------- | ------------ |
| `gate-sweep` | cooldown 30s | Evaluate and close pending gates (timer, GitHub) |
| `orphan-sweep` | cooldown 5m | Reset beads assigned to dead agents back to the work pool |
| `cross-rig-deps` | cooldown 5m | Convert satisfied cross-rig `blocks` deps to `related` |
| `order-tracking-sweep` | cooldown | Close stale order-tracking beads and prune expired tracking history |
| `spawn-storm-detect` | cooldown | Detect beads repeatedly bouncing back to pool |
| `dead-run-detect` | cooldown 30m | Mail the mayor once when an in_progress workflow root's driving session is gone |
| `prune-branches` | cooldown | Clean stale `gc/*` branches from all rigs |
| `wisp-compact` | cooldown | TTL-based cleanup of expired ephemeral beads (wisps) |
| **`nudge-on-route`** | **event `bead.updated`** | **Nudge the target session when a bead is routed to it** |
| **`cascade-nudge-on-blocker-close`** | **event `bead.closed`** | **Nudge dependents' assignees when a blocker bead closes** |
| **`notify-on-human-gate-creation`** | **event `bead.created`** | **Mail + nudge the addressee when a human gate bead is created** |
| **`renudge-stale-human-gates`** | **cooldown 5m** | **Re-mail + re-nudge the addressee of a human gate left open past a staleness threshold** |

The **event-driven nudge orders** are documented in detail below.

## `nudge-on-route`

**Why.** `gc sling` does not nudge warm-idle workers (issue #1129, closed by
design: cities that reuse warm workers were told to *"introduce orders that
trigger on new beads being created and manually nudge the workers in the warm
set"*). Without that nudge, a bead whose `metadata.gc.routed_to` is newly set
or changed sits unclaimed against any worker not currently in an active turn
cycle. This order ships that workaround.

**Event contract.** Triggers on `bead.updated`. For each event whose bead
carries a non-empty `metadata.gc.routed_to`, nudges that target with
`check for assigned work`.

`routed_to` may be a concrete session **or** a pool base. Sling collapses a
multi-session slot to the pool base (`NormalizePoolRouteTarget`), so a
pool-routed bead's `routed_to` is the members' `template`, not a name
`gc session nudge` can resolve. The script handles both: it enumerates the
pool's active members via `gc session list --template <routed_to>` and nudges
each, falling back to a direct `gc session nudge <routed_to>` when the target
has no members (a single-session agent or an explicit slot). Without this,
nudges to a pool base silently no-op — defeating the warm-idle pool wake this
order exists to provide.

**Idempotence.** A `(bead, routed_to)` pair is nudged at most once. The
reconciler re-emits `bead.updated` for an actively-routed bead, so the dedup
state's last-seen timestamp is refreshed on every sighting and the pair is
never pruned-then-renudged while the routing is live.

**Dedup state.** `$GC_PACK_STATE_DIR/nudge-on-route-state.json` — a JSON object
mapping `"<bead>|<routed_to>"` to an ISO timestamp. `GC_PACK_STATE_DIR`
resolves per city + pack, so multi-city installs never cross-pollinate. Entries
older than the retention window are pruned on each run.

**Configuration** (all optional, via `[order.env]` or the controller env):

| Variable | Default | Meaning |
| -------- | ------- | ------- |
| `GC_NUDGE_ON_ROUTE_LOOKBACK` | `2m` | Event lookback window |
| `GC_NUDGE_ON_ROUTE_RETENTION` | `1h` | Dedup-entry retention (Ns/Nm/Nh) |
| `GC_NUDGE_ON_ROUTE_MESSAGE` | `check for assigned work` | Nudge text |

## `cascade-nudge-on-blocker-close`

**Why.** When a blocker bead closes (linked via `gc bd dep <dependent> --blocks
<blocker>`), the assignee of each dependent has no event-driven signal that
work can resume — they poll, get nudged by hand, or miss the unblock. This
order removes that class of "the blocker closed but my agent didn't notice"
bug, and is especially useful for human → agent handoff where a human files a
blocker and an agent owns the dependent.

**Event contract.** Triggers on `bead.closed`. This is the event the close
transition actually emits — a closed bead only emits `bead.updated` on a later
metadata edit — so the order fires once, exactly on the transition that
unblocks dependents. For each closed bead it resolves dependents via:

```
gc bd dep list <blocker> --direction=up --type=blocks --json
```

and nudges the `assignee` of every dependent whose status is `open` or
`deferred`:

```
gc session nudge <assignee> "blocker <blocker> closed — your dependent <dep> may be unblocked"
```

**Cross-rig.** A `prefix -> rig` lookup built from `gc rig list` scopes the
dependency lookup and the nudge to the rig that owns each bead, so cross-rig
blocker chains within a city resolve correctly. Cross-city cascade is out of
scope.

**Idempotence.** A `(blocker, dependent)` pair is nudged at most once.

**Dedup state.**
`$GC_PACK_STATE_DIR/cascade-nudge-on-blocker-close-state.json` — a JSON object
mapping `"<blocker>|<dependent>"` to an ISO timestamp, city- and pack-scoped.
Entries older than the retention window are pruned on each run.

**Configuration** (all optional):

| Variable | Default | Meaning |
| -------- | ------- | ------- |
| `GC_CASCADE_NUDGE_LOOKBACK` | `5m` | Event lookback window |
| `GC_CASCADE_NUDGE_RETENTION` | `1h` | Dedup-entry retention (Ns/Nm/Nh) |

## `dead-run-detect`

**Why.** A formulas-v2 (graph.v2) run is driven by the session that claimed
its workflow root. When that session dies between steps — after a
decomposition step closed and before the drain was routed — the root stays
`in_progress`, the step beads stay open and unclaimed, and the run reports as
active indefinitely with no signal to anyone. Nothing else catches that shape:
`reaper.sh`'s stale-root close needs an empty assignee, >24h of silence and
no descendant in a live status (`open` is live, so open step beads make the
root permanently ineligible — and it closes silently anyway);
`orphan-sweep.sh` resets `in_progress` beads whose assignee is unknown, and a
configured agent name counts as known with no session running; the
dashboard's run staleness (`internal/runproj/enrich.go`) is display-only; the
pool-slot backstops (`cmd/gc/execution_backstop.go`, `cmd/gc/idle_nudge.go`)
never look at workflow roots. This order ships the missing detector.

**Predicate.** Every sweep collects the live session identities from
`gc session list --json` for HQ and every non-HQ rig (the liveness source
`orphan-sweep.sh` uses; a failed list in any scope skips the whole sweep so a
partial picture never produces a false verdict), enumerates `in_progress` and
`open` beads per scope, and selects workflow roots: `status == in_progress`
and `gc.kind == workflow` or `gc.formula_contract == graph.v2`
(`sourceworkflow.IsWorkflowRoot`) and `gc.root_bead_id` empty or self. A
root's driver is read from the fields the run projection reads
(`internal/runproj/detail_sessionlink.go`): `session_name` /
`gc.session_name` / `gc.sessionName` / `session_id` / `gc.session_id` /
`gc.sessionId`, then the assignee, then `gc.routed_to`; the reconciler
back-fills `gc.session_name` onto the root from its worked steps
(`cmd/gc/build_desired_state.go` `stampRunRootFromStep`). A root is escalated
when none of those identities matches a live session (exact
id/session_name/alias/agent_name/name/template, or the rig-stripped and
pool-slot-stripped forms `orphan-sweep.sh` accepts), it has at least one
open step with an empty assignee (`gc.root_bead_id == root`), no
`in_progress` step is held by a live session, and no step bead has been
created or updated for longer than `GC_DEAD_RUN_THRESHOLD`. A root with no
recorded driver at all is left alone as unverifiable.

**Escalation.** One `gc mail send --notify` to `GC_DEAD_RUN_RECIPIENT`
(default `mayor`), falling back to `GC_ESCALATION_RECIPIENT` (default
`human`) when the primary address is undeliverable. The body names the root,
its driver, the silence age, the unclaimed step ids, and the recovery recipe
validated in production: re-enter with `gc sling <target> <convoy> --on
build-from-convoy --force` (build-from-convoy adopts the existing
implementation convoy and takes `requirements_path` / `plan_path` /
`decomposition_path` vars), then close the dead root and all of its step beads
in ONE `gc bd close` invocation (per-bead close loops time out). Undeliverable
sends surface loudly, are not marked, and exit non-zero so the controller logs
them (loud-fail, gastownhall/gascity#4543).

**Idempotence / dedup.** The marker `gc.dead_run_escalated_at=<ISO timestamp>`
is written on the root only after a delivered send; a marked root is never
re-mailed. The marker is removed when the condition clears (driver alive
again, steps claimed, run progressed), so a recurrence re-alerts. The marker is
the only mutation the order ever makes: it never closes, resets, or reassigns
work beads.

**Configuration** (all optional, via `[order.env]` or the controller env):

| Variable | Default | Meaning |
| -------- | ------- | ------- |
| `GC_DEAD_RUN_THRESHOLD` | `2h` | Silence (no step created/updated) required before escalating |
| `GC_DEAD_RUN_RECIPIENT` | `mayor` | Primary escalation address |
| `GC_ESCALATION_RECIPIENT` | `human` | Fallback address when the primary is undeliverable |
| `GC_DEAD_RUN_REENTRY_FORMULA` | `build-from-convoy` | Re-entry formula named in the recovery recipe |

## Dependencies

Both nudge scripts use only `gc`, `bd`, and `jq` — already required by the
other core-pack scripts. `gc bd` routes the request, then delegates to the
underlying `bd` binary. `jq` is a hard dependency and the scripts fail loud
at startup if it is missing.
