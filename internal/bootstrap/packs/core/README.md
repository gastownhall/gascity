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
| **`nudge-on-route`** | **condition** | **Nudge newly routed work and retry unsuccessful delivery** |
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

The city-scoped condition checks both `bead.created` and `bead.updated`,
including flat log payloads and nested API payloads. A route stamped at creation
does not need a later update to trigger notification. Before sending, the script
checks the current bead: it must be open, unassigned, routed, and present in the
ready query. Claimed or closed work is discarded; blocked work stays pending.

`routed_to` may be a concrete session **or** a pool base. Sling collapses a
multi-session slot to the pool base (`NormalizePoolRouteTarget`), so a
pool-routed bead's `routed_to` is the members' `template`, not a name
`gc session nudge` can resolve. The script reads `gc agent list --json` once
per exec to identify pool bases by qualified name and `routes_to_pool`.
Capacity information in `pool` alone does not establish notification ownership.
Pool-base notifications belong to the controller's bounded idle-claim
backstop, including empty pools and pools with only one active member.
Other targets use `gc session list --template <routed_to>`: a template with one
active member receives one nudge, and a target with no members is nudged
directly. This preserves named-session and explicit-slot routing. Failed or
malformed lookups retain pending work for retry instead of guessing a target.
This pack revision requires a CLI exposing `routes_to_pool` in agent-list
output. Deploy it with its matching binary; older CLI output fails the read
and retains pending work rather than silently misclassifying named sessions.

The script atomically records an event cursor, pending deliveries, retry times,
the last observed route, and the last successfully notified route in
`$GC_PACK_STATE_DIR/nudge-on-route-delivery.json`. A complete event read advances
the cursor together with pending work, before delivery. Failed reads retain the
cursor; failed sends stay pending without requiring another event. Retry delays
grow from one minute to a maximum of one hour. Blocked work is rechecked after
one minute, or sooner when its bead changes. This includes held and deferred
work. Successful requests to the nudge CLI include requests accepted
by its durable queue; the supervisor owns subsequent runtime delivery.
ACP cities need the supervisor delivery owner enabled in `city.toml`:

```toml
[daemon]
nudge_dispatcher = "supervisor"
```

Restart the supervisor after changing this setting. The default legacy mode
does not start an ACP nudge poller, and a CLI subprocess cannot own the
supervisor's ACP connection. A queued nudge alone therefore cannot wake an idle
ACP session in that mode. The remaining default-mode gap is tracked in
[issue #6080](https://github.com/gastownhall/gascity/issues/6080).

Each run processes at most 20 due beads and leaves the remainder pending. Each
eligible routing receives its own notification request, even when targets are
shared. The queue may combine requests into a single turn; the default message
asks the worker to claim and execute work repeatedly until no work remains.

Condition checks run on the controller's orders lane. Dispatch latency depends
on that lane's cadence and backlog. Before reading events, a check records an
unfinished-probe marker. Normal completion removes it; interruption or the
five-second check deadline leaves it for the next pass to schedule exec recovery
without another network probe. Exec clears the marker under its delivery lock
and owns read-failure backoff. The marker is recovery intent, not a process lock.

Repeated observations of the same route do not resend. A claim/release or a
change to another route makes the work eligible again. A process crash after
nudge acceptance but before saving success can cause a duplicate; this is
at-least-once delivery, not an exactly-once promise.
Event observations can lag the live ledger read used for delivery. Repeated
stale observations do not repeat a newer live-route notification. An away/back
reroute wholly inside that lag can coalesce with the already accepted notification.

First installation, a reset event log, and gaps over 2,000 events recover from
`gc ready --status=open`, which federates HQ, rigs, relocated graph stores, and
ephemeral work, instead of walking the entire event history. A failed scope read
prevents committing the snapshot and schedules a retry with backoff.
Because these reads are federated, an unavailable rig can delay notifications
city-wide. After repeated read failures, recovery may wait up to one hour for
the next attempt; pending work is retained throughout the outage.
Smaller gaps use buffered sequence replay. An irrelevant tail of 500 events
triggers a maintenance run to keep later checks bounded. The order's own
tracking events cannot continuously retrigger it.

The previous success map is imported on upgrade, but its cursor is not trusted:
the previous version could advance it past a failed delivery. Time-window and
retention overrides (`GC_NUDGE_ON_ROUTE_WINDOW`, `GC_NUDGE_ON_ROUTE_LOOKBACK`,
`GC_NUDGE_ON_ROUTE_RETENTION`) no longer govern correctness. The nudge text can
still be overridden with `GC_NUDGE_ON_ROUTE_MESSAGE` through `[order.env]` or
the controller environment. Its default asks the recipient to run
`gc hook --claim --json`, execute the claimed work, and repeat until empty.

Keep the order's work gate enabled and `idempotent` unset. The script also
serializes manual runs using `flock`, or `shlock` on macOS. It requires Bash,
jq, and one of those locking utilities. Existing city-level overrides and skip
entries retain the `nudge-on-route` name; rig-specific overrides must move to
the city-scoped order.

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

## Dependencies

Both nudge scripts use only `gc`, `bd`, and `jq` — already required by the
other core-pack scripts. `gc bd` routes the request, then delegates to the
underlying `bd` binary. `jq` is a hard dependency and the scripts fail loud
at startup if it is missing.
