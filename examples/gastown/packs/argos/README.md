# Argos — rate-limit-recovery watchdog pack

Argos is the city's **watchdog**: an LLM agent that watches every live
session and recovers the ones that have stalled — the canonical case being
a session sitting behind a rate-limit wall while it still holds claimed,
in-progress work. Named after Odysseus's hound.

It is modeled on the in-tree `boot` agent
(`../gastown/agents/boot/`): a city-scoped, always-resident
named session that the controller re-wakes each patrol tick, runs a single
pass of triage with a fresh provider context, then drain-acks and exits.

## Status: wired, tested, documented

Argos **detects, classifies, and recovers**, is **composed into the example
city** (it comes up as a city agent the same way `boot` and `dog` do), and is
covered by a regression suite that pins the patrol-scenario contract. Each wake
runs one single-pass triage:

1. wake (fresh context, single pass),
2. enumerate `gc session list --json` and pre-filter candidates
   (`provider == claude`, alive, `last_active` as a loose advisory),
3. cross-reference claimed work — join each candidate's `session_name`
   against the `assignee` of an `in_progress` bead, and read that bead's
   `gc.last_heartbeat_at` (the clean liveness signal): a **fresh** heartbeat
   clears the holder as progressing, a **stale/absent** one keeps it a
   candidate,
4. confirm by reading the pane (`gc session peek`),
5. classify each candidate — `healthy` / `idle-no-work` /
   `rate-limit-stalled` / `context-frozen`,
6. **recover** a candidate the fire gate clears — `gc session nudge <id>
   "continue"` for a rate-limit wall (`gc session wake <id>` first if it is
   suspended), `gc session nudge <id> "/compact"` for a frozen context, then
   emit a role-free `session.recovered` event (`gc event emit`) so the
   recovery is observable in the event log,
7. print one verdict line per candidate, then `gc runtime drain-ack` and
   exit.

The **fire gate** for a recoverable stall is a conjunction: the pane shows
a stall marker as its current state **and** the session holds a claimed
`in_progress` bead. `rate-limit-stalled` is the proven, primary case;
`context-frozen` is the lower-confidence sibling, recovered with `/compact`.

**Recovery is timed and rate-limited:**

- **Reset-window timing (primary).** Argos reads the wall's reset time
  ("resets 7:30pm") off the pane and waits for the window to reopen — a
  `continue` sent while the limit is still up is futile and only storms the
  pane.
- **Anti-storm backoff.** Argos derives "when did I last nudge this
  session" from **observable state** — the `last_nudge_delivered_at` the
  system stamps on the session bead (read via `gc bd show <id>`; it is
  **not** in `gc session list --json`) — and escalates the wait between
  pokes: immediate → 15m → 30m → 1h → 2h → 4h, resetting once the session
  recovers. It keeps **no status file**: a fresh Argos reads the clock and
  the bead each wake and resumes exactly where the last one left off.
- **No unclaim.** A stall keeps its claimed bead; the **orphan sweep**, not
  Argos, reclaims dead-owner work. Argos owns the *alive* stall only — it
  does not unclaim, mail-escalate, file warrants, or kill.

All of this judgment lives in the **prompt**, not in Go (Zero Framework
Cognition: the model reads the pane and decides; no Go string-matcher, no
per-provider format zoo).

The patrol-scenario contract — which live state maps to which verdict and which
action — is pinned in the prompt's **"Patrol scenarios (the contract)"** table
and regression-tested in `examples/gastown/argos_test.go`
(`TestArgosPatrolScenarioContract`, `TestArgosWiredIntoCity`,
`TestArgosGatesOnHeartbeatAndEmitsRecovered`).

Heartbeat adoption and the `session.recovered` event (`.6`) have landed:

- **Workers emit heartbeats.** The polecat prompt calls `gc bd heartbeat
  <work-bead>` at each checkpoint, stamping the clean `gc.last_heartbeat_at`
  signal the watchdog gates on (the worker half of the contract; any pool
  worker can adopt the same one-line call). Pinned by
  `TestPolecatPromptEmitsWorkBeadHeartbeat`.
- **The watchdog gates on heartbeat freshness.** A fresh heartbeat on a
  claimed bead clears the holder as progressing where the polluted
  `last_active` never could; a stale/absent one keeps it a candidate. The gate
  only ever *clears* on a positive fresh signal — it adds no new recovery
  trigger (the pane-marker + claimed-bead fire gate still decides recovery).
- **`session.recovered` is a role-free typed event.** After a recovery lands,
  the watchdog emits `gc event emit session.recovered` with a free-form
  `reason` (`rate_limit` / `context_frozen`) and `action`, never a role name.
  The typed payload (`SessionRecoveredPayload`, registered in
  `internal/api/event_payloads.go`) is the only Go in the whole pack; it makes
  the event a first-class typed-wire shape on `gc events`/the SSE stream. View
  the trail with `gc events --type session.recovered`.

## Design notes

### Why `last_active` is a loose advisory, not a staleness gate (v1)

The obvious way to find a stalled session is "it has been quiet too long" — gate
on `last_active` age past a threshold. Argos deliberately does **not** do that.
`last_active` is bumped by *any* pane I/O, including the controller's own
`nudge`/`wake` send-keys and the rate-limit wall's own countdown redraw, so a
genuinely stalled session keeps looking freshly active. The pollution runs in
the **false-negative** direction: a staleness gate would *suppress* recovery on
exactly the sessions that need it.

So Argos demotes `last_active` to a **loose advisory pre-filter** — it only
bounds how many panes Argos peeks (peek the least-recently-active candidates
first) and never suppresses a verdict. The authoritative signals are the two
fire-gate clauses Argos reads directly: the **rate-limit marker on the live
pane** and a **claimed `in_progress` bead**.

The **clean** liveness signal is `gc.last_heartbeat_at`, stamped by
`gc bd heartbeat`. Because a heartbeat moves only when the worker advances, the
controller's own send-keys cannot fake it — so, unlike `last_active`, a *fresh*
heartbeat is allowed to **clear** a holder (it is positive proof of progress).
As of `.6` the worker prompt adopts it (the polecat calls `gc bd heartbeat` at
each checkpoint), so the gate is live rather than null. Note the asymmetry: a
fresh heartbeat clears, but a *stale or absent* heartbeat never recovers on its
own — an honest long build between checkpoints can look stale too, and a worker
whose prompt never adopted the call has no heartbeat at all. Absence is treated
as "candidate, peek to confirm," never as a clear. Heartbeat freshness only
chooses **where to look**; the fire gate still decides **what to recover**.

### Threshold tuning (pack env)

Argos has **no Go thresholds and no config flags** — by Zero Framework Cognition
every judgment (is this a wall? has the reset passed? which backoff tier am I
on?) lives in the prompt, where the model decides from live state. Two
consequences for an operator who wants to tune behavior:

- **The backoff ladder and the reset-window rule are prose in the prompt.** The
  escalating wait (immediate → 15m → 30m → 1h → 2h → 4h) is a table in
  `agents/argos/prompt.template.md`; change the cadence by editing that table,
  not a Go constant.
- **Patrol cadence is a city/controller setting, not a pack knob.** How often
  the always-resident `argos` session is re-woken is the controller's patrol
  tick, configured where the city configures reconciliation — independent of
  this pack.

If a very large deployment ever needs an operator-set numeric knob (say, a
minimum `last_active` age before a pane is even worth peeking, to bound peek
volume), expose it as a **pack env var** on the argos agent — an `[env]` table in
`agents/argos/agent.toml` — and reference it as `$VAR` in the prompt's shell
commands. That is the pack-env path the `#2194` design anticipated ("threshold …
pack env, not Go"), and it keeps the knob out of Go. v1 ships **no** such knob:
the loose `last_active` advisory already bounds peek volume, and adding a flag
before a deployment needs one would violate "no capability flags."

### How Argos relates to the rest of the city's recovery story

Argos owns one narrow gap and is bounded on every side by existing machinery:

- **vs. `#1411` (rate-limit detection for *exited* sessions).** The reconciler
  already peeks the pane and, when a session has **exited** into a recognized
  rate-limit screen, parks it asleep with a 30-minute quarantine that
  auto-clears. That path is gated on the session having *exited* and on a
  dialog-style screen. Claude's **inline** "You've hit your session limit ·
  resets …" wall leaves the session **alive**, so it slips both gates. That
  alive-and-inline case is precisely Argos's domain — the slice `#1411` cannot
  see.
- **vs. orphan-sweep (`packs/maintenance/orders/orphan-sweep.toml`).** The
  orphan sweep reclaims a bead whose owner is **dead** — it resets `in_progress`
  beads assigned to non-existent agents back to the pool on a 5-minute cooldown.
  Argos handles the **alive** stall and never touches a work bead; the two are
  complementary with a clean dead-vs-alive boundary. This is why Argos
  deliberately does not unclaim: if a nudge never takes and the owner truly
  dies, orphan-sweep collects the bead.
- **vs. `#571` (controller-level non-LLM stuck-sweep).** `#571` is the
  complement, not a competitor: a core, non-LLM controller sweep is the
  **backstop** for when the LLM watchdog itself wedges or is rate-limited — the
  "who watches the watchman" gap. The mature architecture is the LLM watchdog
  (Argos / `#2194`) as the judgment-rich primary recoverer and a non-LLM sweep
  (`#571`) as the dumb-but-reliable backstop. They are layered, not redundant.

## Why Argos is its own pack (pack-home decision)

The watchdog could have been added as a sibling of `boot` inside the
`gastown` pack, or folded into the generic `maintenance` pack. It is a
**dedicated, composable pack** instead, for three reasons:

1. **It is not a Gas Town role.** `gastown` defines a specific domain
   roster — mayor, deacon, boot, witness, refinery, polecat. The watchdog
   is generic infrastructure that enumerates *all* sessions and is blind
   to roles; welding it into `gastown` would muddy that domain definition
   (and its roster tests).
2. **It is reusable, role-agnostic infrastructure** — exactly the kind of
   thing a city "includes alongside any domain pack," the same role
   `maintenance` plays. A self-contained pack lets any city opt in with a
   single import.
3. **Cohesion.** The whole Argos capability (scaffold → detection →
   recovery → wiring) lives in one pack with one identity, rather than
   sprawling across an unrelated domain pack.

`maintenance` was rejected as the home because it is the **non-LLM**
housekeeping layer (exec orders + a utility dog pool); Argos is an
always-resident **LLM triage agent** modeled on `boot`, so it belongs with
its own identity rather than buried among shell orders.

## How to compose it

Import the pack from a city's **root** pack definition so its
city-scoped named session expands into the city:

```toml
[imports.argos]
source = "packs/argos"
```

The `examples/gastown` city does exactly this, so Argos comes up as a
city-scoped agent (`mayor`, `deacon`, `boot`, `dog`, `argos`) there.

## Layout

```
packs/argos/
├── pack.toml                       ← [pack] argos + [[named_session]] always
├── README.md                       ← this file
└── agents/argos/
    ├── agent.toml                  ← scope=city, wake_mode=fresh, max_active=1
    └── prompt.template.md          ← single-pass detect + recover triage
```
