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
| `convoy-autoclose-backstop` | cooldown 1h | Close open, un-owned convoys whose members are all terminal (reconciles a missed autoclose) |
| `order-tracking-sweep` | cooldown | Close stale order-tracking beads and prune expired tracking history |
| `spawn-storm-detect` | cooldown | Detect beads repeatedly bouncing back to pool |
| `prune-branches` | cooldown | Clean stale `gc/*` branches from all rigs |
| `wisp-compact` | cooldown | TTL-based cleanup of expired ephemeral beads (wisps) |
| **`nudge-on-route`** | **event `bead.updated`** | **Nudge the target session when a bead is routed to it** |
| **`cascade-nudge-on-blocker-close`** | **event `bead.closed`** | **Nudge dependents' assignees or routed targets when a blocker bead closes** |
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
metadata edit — so the order fires on the transition that unblocks dependents.
Delivery is at least once: one close can reach the bus twice (from the closing
process and from the controller's cache), which the per-pair dedup below
absorbs. For each closed bead it resolves dependents via:

```
gc bd dep list <blocker> --direction=up --type=blocks --json
```

and nudges every dependent whose status is `open` or `deferred` — at its
`assignee` when one holds the bead:

```
gc session nudge <assignee> "blocker <blocker> closed — your dependent <dep> may be unblocked"
```

An unassigned dependent carrying `metadata.gc.routed_to` — the graph.v2
claim shape, where the assignee stays empty until a session claims the step
— is nudged at that target instead:

```
gc session nudge <member> "blocker <blocker> closed — routed dependent <dep> may be ready to claim"
```

`routed_to` may be a pool base: sling collapses a multi-session slot to the
pool base (`NormalizePoolRouteTarget`), which is the members' `template`,
not a name `gc session nudge` can resolve. The order enumerates the pool's
active members via `gc session list --template <routed_to>` and nudges each,
falling back to a direct `gc session nudge <routed_to>` when the target has
no members (a single-session agent or an explicit slot) — the same
resolution `nudge-on-route` applies at route time. A dependent with both an
assignee and a route is nudged at its assignee only: a claimed bead belongs
to its session.

**Cross-rig.** A `prefix -> rig` lookup built from `gc rig list` scopes the
dependency lookup and the assignee nudge to the rig that owns each bead, so
cross-rig blocker chains within a city resolve correctly. The routed nudge
runs in default scope: `gc.routed_to` is city-qualified and may name a rig
other than the dependent bead's. Cross-city cascade is out of scope.

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

## Dependencies

Both nudge scripts use only `gc`, `bd`, and `jq` — already required by the
other core-pack scripts. `gc bd` routes the request, then delegates to the
underlying `bd` binary. `jq` is a hard dependency and the scripts fail loud
at startup if it is missing.
