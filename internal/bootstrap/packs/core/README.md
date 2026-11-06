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
| `gate-sweep` | cooldown 30s | Evaluate and close pending gates (timer, GitHub) in the city store and every non-suspended rig store |
| `orphan-sweep` | cooldown 5m | Reset beads assigned to dead agents back to the work pool |
| `cross-rig-deps` | cooldown 5m | Convert satisfied cross-rig `blocks` deps to `related` |
| `convoy-autoclose-backstop` | cooldown 1h | Close open, un-owned convoys whose members are all terminal (reconciles a missed autoclose) |
| `order-tracking-sweep` | cooldown | Close stale order-tracking beads and prune expired tracking history |
| `spawn-storm-detect` | cooldown | Detect beads repeatedly bouncing back to pool |
| `prune-branches` | cooldown | Clean stale `gc/*` branches from all rigs |
| **`nudge-on-route`** | **condition** | **Nudge newly routed work and retry unsuccessful delivery** |
| **`cascade-nudge-on-blocker-close`** | **event `bead.closed`** | **Nudge dependents' assignees or routed targets when a blocker bead closes** |
| **`notify-on-human-gate-creation`** | **event `bead.created`** | **Mail + nudge the addressee when a human gate bead is created** |
| **`renudge-stale-human-gates`** | **cooldown 5m** | **Re-mail + re-nudge the addressee of a human gate left open past a staleness threshold** |
| **`notify-on-claims-stalled`** | **event `agent.claims_stalled`** | **Mail the escalation recipient when an agent's seats stop claiming their work (once per stall episode)** |
| **`notify-on-claims-resumed`** | **event `agent.claims_resumed`** | **Resolve that incident and mail a short note when the agent claims its work again** |

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
active member receives one nudge, and any other target, with no members or
several, is nudged directly. This preserves named-session and explicit-slot
routing. Failed or malformed lookups retain pending work for retry instead of
guessing a target.
This pack revision requires a CLI exposing `routes_to_pool` in agent-list
output. Deploy it with its matching binary; older CLI output fails the read
and retains pending work rather than silently misclassifying named sessions.

The script atomically records an event cursor, pending deliveries, retry times,
the last observed route, and the last successfully notified route in
`$GC_PACK_STATE_DIR/nudge-on-route-delivery.json`. A complete event read advances
the cursor together with pending work, before delivery. Failed reads retain the
cursor; failed sends stay pending without requiring another event. Retry delays
grow from one minute to a maximum of one hour. Blocked work is rechecked after
one minute, or sooner when its bead changes. This includes held, deferred and
`human`-labeled work. Successful requests to the nudge CLI include requests
accepted by its durable queue; the supervisor owns subsequent runtime delivery.
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

Each run processes at most 20 due beads and leaves the remainder pending. It
takes the oldest due first, with work not yet tried ahead of retries and blocked
rechecks. Each eligible routing receives its own notification request, even when
targets are shared. The queue may combine requests into a single turn; the default message
asks the worker to claim and execute work repeatedly until no work remains.

Condition checks run on the controller's orders lane. Dispatch latency depends
on that lane's cadence and backlog. Before reading events, a check records an
unfinished-probe marker. Normal completion removes it; interruption or the
five-second check deadline leaves it for the next pass to schedule exec recovery
without another network probe. Exec clears the marker under its delivery lock
and owns read-failure backoff. The marker is recovery intent, not a process lock.

Repeated observations of the same route do not resend. A claim/release, a
deferral, or a change to another route makes the work eligible again. A process
crash after nudge acceptance but before saving success can cause a duplicate;
this is at-least-once delivery, not an exactly-once promise.
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
the previous version could advance it past a failed delivery. A state file that
holds only whitespace (jq 1.6 can leave one, #8611) is read as absent.
Time-window and retention overrides (`GC_NUDGE_ON_ROUTE_WINDOW`,
`GC_NUDGE_ON_ROUTE_LOOKBACK`, `GC_NUDGE_ON_ROUTE_RETENTION`) no longer govern
correctness. The nudge text can
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

Either way, it skips a dependent that a `blocks` blocker still holds as gc's
readiness rule judges it
(`gc bd dep list <dep> --direction=down --type=blocks --json`): a blocker
other than the one that closed whose status is not `closed`, or any blocker,
the closing one included, that closed blocked (`gc.work_outcome=blocked`),
unless it is a formula step (`gc.step_ref` set) whose `gc.outcome` is
`pass`. Its assignee or routed target has nothing to do yet, and a nudge to
an asleep seat parked on the dependent would wake that seat for nothing; the
seat wakes on its own once the dependent is ready. A `pinned` blocker holds
too, whether the read reports it `pinned` (the bd passthrough) or `open` (the
in-process class-binding read, which folds it). bd's own readiness counts it
satisfied, so a dependent only a pinned blocker holds is not nudged, but a
seat parked on it still wakes: the park reads the store's ready query. gc
itself never pins a bead.
A pair the closing blocker's own blocked close holds is not recorded, whether
or not another blocker holds the dependent too, so that blocker's later close
that passes still nudges. A pair only another blocker holds is recorded like
a nudged one, so later runs in the lookback do not read its blockers again;
the close of the dependent's last blocker is a pair of its own, and nudges. A
row the read cannot judge (an external row, for a blocker outside the
dependent's store, or a row with no status) holds nothing, and a blocker read
that fails proves nothing: the nudge goes out.

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

## `notify-on-claims-stalled` and `notify-on-claims-resumed`

**Why.** The controller's claims-stall guard notices when an agent
template's seats stop claiming the routed work they were spawned for: a
provider out of usage, an expired account, a CLI that never reads its prompt.
It holds the template's spawns back and records `agent.claims_stalled`; a seat
that claims again records `agent.claims_resumed`. Events alone reach only the
event log. On 2026-10-31 a review lane stood dead for ten hours while ~1,400
seats came and went, and nobody was told.

**Event contract.** Both orders run `assets/scripts/notify-claims-stall.sh`,
which reads both event types over a lookback window and mails through
`escalate.sh` to `$GC_ESCALATION_RECIPIENT` (default `human`):

- a stall mails once per episode: the incident key and fingerprint are the
  episode the guard names, so its probe alerts are not mailed again
  (`escalate.sh` reminds once a day while the stall stands);
- a resumption resolves that incident and mails one short note; an episode
  that is already over is reported by its resumption alone.

**Loud-fail.** An undeliverable mail exits non-zero, so the controller logs
the order's output. It is sent again by the next run that still finds the
event inside the lookback: the next matching event (a probe alert, the
resumption) runs the order. An event read that fails, or `gc events` output
that does not decode, also exits non-zero and mails nothing; a window with no
event is a clean run.

**Overrides.** An `escalate.sh` override written before incident keys, which
rejects `--incident-key` as a usage error, still alerts: the script sends
each alert again without the incident flags, so such a city is mailed on
every run that finds the event instead of once per episode, and the order's
output names the override.

**Configuration** (all optional):

| Variable | Default | Meaning |
| -------- | ------- | ------- |
| `GC_NOTIFY_CLAIMS_STALL_LOOKBACK` | `15m` | Event lookback window |
| `GC_ESCALATION_RECIPIENT` | `human` | Who is mailed |

## Dependencies

Both nudge scripts use only `gc`, `bd`, and `jq` — already required by the
other core-pack scripts. `gc bd` routes the request, then delegates to the
underlying `bd` binary. `jq` is a hard dependency and the scripts fail loud
at startup if it is missing.
