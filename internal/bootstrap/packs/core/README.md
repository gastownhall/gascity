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
| `prune-branches` | cooldown | Clean stale `gc/*` branches from all rigs |
| `wisp-compact` | cooldown | TTL-based cleanup of expired ephemeral beads (wisps) |
| **`nudge-on-route`** | **event `bead.updated`** | **Nudge the target session when a bead is routed to it** |
| **`cascade-nudge-on-blocker-close`** | **event `bead.closed`** | **Nudge dependents' assignees when a blocker bead closes** |

The two **event-driven nudge orders** are documented in detail below.

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

**Why.** When a blocker bead closes (linked via `bd dep <dependent> --blocks
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

## Dependencies

Both nudge scripts use only `bd`, `gc`, and `jq` — already required by the
other core-pack scripts. `jq` is a hard dependency and the scripts fail
loud at startup if it is missing.
