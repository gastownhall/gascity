---
title: "Session-First Architecture"
---

**Status:** Approved with risks (v6 — post-review round 5)
**Author:** Design review collaboration
**Date:** 2026-03-08

## Problem Statement

Gas City has two parallel session management models that don't interoperate:

1. **Agent-centric (controller path):** `config.Agent` → `buildOneAgent()` →
   `agent.Agent` → `runtime.Provider.Start()`. The controller rebuilds the
   full agent list from config on every tick. Sessions are an implementation
   detail — tmux session names derived from agent names.

2. **Session-centric (`gc session` path):** `session.Manager.Create()` →
   bead (type="session") → `runtime.Provider.Start()`. Sessions are
   persistent, resumable, bead-backed. But the controller doesn't know
   about them.

This creates several problems:

- **Pool members have no persistence.** When a pool member is stopped, its
  history disappears. There's no way to query old pool sessions.
- **The agent.Agent interface is redundant.** It's a thin wrapper around
  `runtime.Provider` + session name. `session.Manager` already provides the
  same operations plus persistence.
- **Config-driven identity is fragile.** Pool instances get slot-based names
  (`worker-3`) that change when scaling happens. Sessions need stable identity.
- **Two code paths to maintain.** `buildOneAgent` (200+ lines) and
  `session.Manager.Create` do overlapping work with different models.

## Design Principles

1. **Session is the primitive.** A session is a persistent, bead-backed
   conversation between a human/system and an agent. It has stable identity
   (bead ID), lifecycle state, and history.

2. **Templates replace agent types.** `config.Agent` becomes a session
   template. The single/multi/pool distinction becomes a policy on how many
   concurrent sessions a template allows and how they're scaled.

3. **The controller manages sessions, not agents.** Instead of rebuilding
   `agent.Agent` objects from config each tick, the controller reconciles
   session beads against desired state.

4. **Pool growth = new session. Pool shrink = drain + archive session.** Old
   pool sessions remain queryable but don't receive new work.

5. **Single-writer per lifecycle.** At every migration phase, exactly one
   system owns runtime lifecycle mutations. No dual-writer ambiguity.

6. **Fail closed.** On partial failure (bead store errors, stale reads),
   the controller aborts the tick rather than acting on incomplete data.

## Core Invariants

**INV-1: Creating a session requires only a target template.**
A template is a reusable agent definition (provider, prompt, env, hooks, etc.)
drawn from `[[agent]]` config. Creating a session from a template resolves
the provider, builds the command, and starts the runtime.

**INV-2: Non-pool templates allow unlimited concurrent sessions.**
Any template without pool config can have an arbitrary number of sessions.
The controller doesn't enforce a count limit — sessions are created on demand
and persist until closed.

**INV-3: Pool templates have bounded occupancy.**
A pool template's `max` field caps occupancy: the count of `creating` +
`active` + `suspended` + `quarantined` sessions. Archived, draining, and
closed sessions do NOT count. Growing = create new session (reserves a
`creating` slot) or reactivate archived. Shrinking = drain + archive
excess sessions.

**INV-4: Sessions support template overlay at creation time.**
A session can override a strict allowlist of template defaults (model, name,
title, prompt) and per-template-allowed env vars at creation time. The
overlay is stored on the session bead so resume uses the same overrides.
Overlays are a second config source — the session bead records the effective
configuration, and `gc session inspect` shows both template defaults and
overlay overrides for full transparency.

**INV-5: Single controller exclusivity and single source of truth.**
Only one controller process manages session lifecycle at a time. Enforced by
`controller.lock` (flock). The reconciliation loop is single-threaded —
no concurrent tick execution. **All lifecycle mutations** (including CLI
commands like `gc session close`) go through the controller socket. The
CLI sends mutation requests via `controller.sock`; the controller applies
them within the event loop and updates the in-memory index synchronously.
No out-of-band bead store writes for lifecycle state.

## Architecture

### Session Bead Schema

Every session is a bead with `type = "session"`. The bead stores all state
needed to start, resume, suspend, and query the session.

```
type:       "session"
status:     "open" | "closed"
labels:     ["gc:session", "template:{name}"]  # pool sessions also get "member:{name}"
title:      "{user-provided or auto-generated}"

metadata:
  template:       "polecat"              # source template name
  state:          "creating" | "active" | "suspended" | "draining" | "archived" | "quarantined"
  state_reason:   "scale_down"           # why this state was entered (see below)
  provider:       "claude"               # resolved provider name
  command:        "claude --dangerously..." # resolved start command
  work_dir:       "/path/to/workdir"
  session_name:   "polecat-a3f2"         # tmux session name ({template}-{short-hash})
  session_key:    "{uuid}"               # provider resume handle (scrubbed on close)
  resume_flag:    "--resume"
  resume_style:   "flag"
  config_hash:    "{fingerprint}"        # canonical hash for drift detection
  pool_template:  "worker"               # set only for pool sessions
  generation:     "3"                    # incremented on each reactivation
  instance_token: "{random}"             # set on create/reactivate, checked on drain
  created_at:     "2026-03-08T..."
  suspended_at:   "2026-03-08T..."
  archived_at:    "2026-03-08T..."
  drain_started:  "2026-03-08T..."
  crash_count:    "0"                    # crashes in current window
  last_crash_at:  ""                     # timestamp of most recent crash
  quarantine_until: ""                   # earliest time to attempt restart
  quarantine_cycle: "0"                  # number of quarantine→active attempts
  creating_at:    ""                     # when state=creating was set

  # Overlay fields (only if overridden at creation)
  overlay.model:  "sonnet"
  overlay.name:   "quick-fix"
```

#### State Reason Values

Every state transition records the reason. Valid values:

| State | Valid Reasons |
|---|---|
| creating | `pool_scale_up`, `user_request`, `config_drift_replace` |
| active | `creation_complete`, `resumed`, `reactivated`, `quarantine_cleared` |
| suspended | `user_request`, `idle_timeout`, `dependency_down` |
| draining | `scale_down`, `config_drift`, `manual` |
| archived | `drain_complete`, `drain_timeout`, `crash_during_drain`, `suspended_scale_down`, `quarantine_evicted` |
| quarantined | `crash_loop` |
| closed | `user_request`, `pruned`, `manual`, `stale_creating` |

Note: `crash_recovery` is used internally by the repair table for
`active → suspended` transitions during crash recovery, mapping to
`suspended` with `state_reason=crash_recovery`.

#### Two-Axis State Model

Session state uses two axes:

- **`bead.status`** ∈ {`open`, `closed`}: Record-level lifecycle. `closed`
  is terminal and immutable. Maps to the bead store's native status field.
- **`metadata.state`** ∈ {`creating`, `active`, `suspended`, `draining`,
  `archived`, `quarantined`}: Operational lifecycle within an open bead.

Invariants:
- `closed` beads MUST have `bead.status = "closed"`. The `state` field is
  not meaningful for closed beads (set to empty string on close).
- All other states require `bead.status = "open"`.
- CLI output maps both axes: a bead with `status=closed` shows state
  `closed` regardless of the metadata `state` field.

#### Pool Occupancy Accounting

Which states count against pool `max`:

| State | Counts Against `max` | Rationale |
|---|---|---|
| creating | Yes | Reserves capacity; prevents creation burst |
| active | Yes | Running and receiving work |
| suspended | Yes | Holds context, temporarily paused |
| draining | No | Being retired, already de-routed |
| archived | No | Retired, no resources held |
| quarantined | Yes* | Holds a slot; see note below |
| closed | No | Terminal |

*Quarantined sessions count against `max` to prevent replacement. When a
quarantined session's cooldown expires, the reconciler checks current pool
occupancy. If the pool is at `max` (because other sessions were created),
the quarantined session transitions to `archived` instead of `active`.
This prevents `max` violations from quarantine reactivation.

#### Session Name Convention

Session names use `{template}-{short-hash}` format where `short-hash` is the
first 6 characters of the bead ID. Examples: `polecat-a3f2b7`, `worker-b7c1d9`.
Six characters provide ~16 million values per template, making collisions
negligible. On collision (detected at creation), a 7th character is appended.
This preserves operator ergonomics (tab-completable, human-readable) while
maintaining stable identity via the bead ID internally. Pool sessions also
store a `pool_slot` metadata field with a sequential number, visible in
default `gc session list` output and usable as a CLI selector via
`gc session attach worker~3` syntax.

#### Generation and Instance Token

- **`generation`:** Incremented each time a session transitions from
  `archived` → `active` (reactivation). Starts at `1` on creation. Used for
  auditing how many incarnations a pool slot has had.
- **`instance_token`:** Random value set on `create` and `reactivate`. The
  drain protocol checks this token — if the token on the bead doesn't match
  the controller's expected value, the drain targets a stale incarnation and
  is aborted. Prevents races where a drain for incarnation N arrives after
  incarnation N+1 has started.

### Session States

```
                create
                  │
                  ▼
              ┌──────────┐
              │ creating  │──── stale? ──▶ closed
              └─────┬─────┘
                    │ runtime alive
                    ▼
              ┌─────────┐
         ┌───▶│  active  │◀──── resume / reactivate
         │    └────┬─────┘
         │         │
         │    suspend │ drain    crash-loop
         │         │    │           │
         │         ▼    ▼           ▼
         │    ┌────────┐ ┌────────┐ ┌─────────────┐
    resume│   │suspended│ │draining│ │ quarantined │
         │    └────┬───┘ └───┬────┘ └──────┬──────┘
         │         │    crash│  │           │
         │    archive*  ─────┘ archive  cooldown (if room)
         │         │           │           │
         │         ▼           ▼           ▼
         │    ┌──────────────────┐    back to active
         │    │    archived      │    (or archived if at max)
         │    └────────┬─────────┘
         │             │
         └─────────────┘ (reactivate, pool only)

         Any state ──close──▶ closed (bead.status="closed", terminal)

  * suspended → archived only for pool sessions during scale-down
```

**creating:** Bead created, runtime not yet confirmed. The `pool:` label is
NOT set. `creating_at` records when this state was entered. If the runtime
starts successfully and `IsRunning()` confirms liveness, transitions to
`active` (and `pool:` label is added for pool sessions). If the bead remains
in `creating` for longer than `creation_timeout` (default 60s), the
reconciler treats it as stale: checks `IsRunning()` — if alive, completes
the transition to `active`; if dead, closes the bead with
`state_reason=stale_creating`. **`creating` beads count against pool `max`**
to prevent creation bursts during slow provider starts. Visible in
`gc session list` default output with state `creating`.

**active:** Has a live runtime session. Receives work (for pool sessions).
Crash bookkeeping: `crash_count` incremented on unexpected exit, reset on
successful operation. If `crash_count` exceeds `max_restarts_per_window`
within `restart_window`, transitions to `quarantined`.
**Single crash (below threshold):** On unexpected runtime exit while
`crash_count` is below the quarantine threshold, the controller
restarts the runtime in-place (re-runs `Start()` on the existing bead)
without changing `state`. The `pool:` label remains set during the brief
restart window; the next tick detects non-liveness if restart fails and
increments `crash_count`. This is a restart-in-place, not a state
transition — the session remains `active` throughout.

**suspended:** No runtime resources. Resumable with full context. User- or
system-initiated pause. Counts against pool `max` (the session is paused,
not retired). For pool sessions, the `pool:` label is removed on suspend
(same pattern as draining — a non-running session must not be routable).
The `member:{template}` label preserves pool membership for queries.
`suspended → archived` occurs when the controller needs to scale down and
finds suspended sessions (archived first before draining active sessions).

**draining:** Transitional state for pool sessions being scaled down. The
`pool:` label is removed (stops new work routing), the runtime continues
until in-flight work completes or `drain_timeout` expires. On completion,
transitions to `archived`. The runtime is NOT killed until drain completes.
If the runtime crashes during drain, transitions immediately to `archived`
with `state_reason=crash_during_drain` (no quarantine — already being
retired). Does not increment `crash_count`.

**archived:** No runtime resources. Queryable but does NOT receive new work.
Used for old pool sessions. Can be reactivated if the pool needs to grow
and `wake_mode=resume`. Non-pool sessions cannot enter this state.

**quarantined:** No runtime resources. Auto-restarts blocked until
`quarantine_until` timestamp passes (exponential backoff, capped at 5min).
`quarantine_cycle` is incremented on each `quarantined → active` transition
(persisted on the bead, survives controller restart). On cooldown expiry,
the reconciler checks pool occupancy: if the pool is at `max`, the session
transitions to `archived` instead of `active`. If it can reactivate, it
transitions to `active` and resets `crash_count` (but not `quarantine_cycle`).
After `quarantine_max_attempts` (default 3) cycles without sustained healthy
operation (defined as `quarantine_healthy_duration`, default 5 minutes,
without crash after reactivation), the session is **evicted**: transitioned
to `archived` with `state_reason=quarantine_evicted`. This frees the slot
for fresh capacity. A `session.quarantine.evicted` event is emitted for
operator attention.

**closed:** Terminal. Bead `status` set to `"closed"`. The metadata `state`
field is cleared. History preserved. Sensitive metadata (`session_key`,
`overlay.env.*`, `overlay.prompt`) scrubbed on close (scrub BEFORE marking
closed on ExecStore to ensure fail-closed). Any beads claimed by this
session are marked `blocked` with `reason=session_closed`.

#### Orphan Work Cleanup

All state transitions that terminate or abandon a runtime MUST clean up
claimed work. This applies to:

| Transition | Orphan Action |
|---|---|
| drain timeout | Mark claimed beads `blocked` (`reason=session_archived`) |
| crash during drain | Mark claimed beads `blocked` (`reason=session_crash_drain`) |
| `gc session close` | Mark claimed beads `blocked` (`reason=session_closed`) |
| `gc session suspend` | Mark claimed beads `blocked` (`reason=session_suspended`) |
| active → quarantined | Mark claimed beads `blocked` (`reason=session_quarantined`) |

The cleanup uses the session's `session_name` or bead ID to identify
claimed work. This is a single query + batch update, executed before the
state transition is written.

### Atomic State Mutations

State transitions that involve multiple field changes (e.g., archive requires
`state→archived` + label removal + `archived_at` timestamp) MUST be written
as a single `SetMetadataBatch` call. The bead store guarantees batch writes
are atomic for `MemStore` and `FileStore` (single lock). For `ExecStore`
(bd/br CLI), writes are sequential but ordered to fail closed:

**Creation ordering (fail closed):**
1. Create bead with `state=creating`, NO `pool:` label
2. Start runtime
3. Confirm liveness (`IsRunning()`)
4. Set `state=active`, `state_reason=creation_complete` (batch)
5. Add `pool:` label (enables routing — only after runtime confirmed)

**Suspend ordering (fail closed, pool sessions):**
1. Remove `pool:` label (stops routing)
2. Set `state=suspended`, `suspended_at`, `state_reason` (batch)
3. Kill runtime

**Archive ordering (fail closed):**
1. Remove `pool:` label (stops routing — safe even if crash follows)
2. Set `state=archived`, `archived_at`, `state_reason` (batch)
3. Kill runtime

**Reactivate ordering (fail closed):**
1. Start runtime (session must be alive before routing)
2. Confirm runtime liveness (`IsRunning()`)
3. Set `state=active`, `state_reason=reactivated`, `generation++` (batch)
4. Add `pool:` label (enables routing — only after runtime confirmed)

**Resume ordering (fail closed, pool sessions):**
1. Start runtime
2. Confirm runtime liveness (`IsRunning()`)
3. Set `state=active`, `state_reason=resumed` (batch)
4. Add `pool:` label (enables routing — only after runtime confirmed)

If any step fails, the controller logs the partial state and retries on the
next tick. The ordering ensures that at no point is a session routable without
a live runtime, or running without being routable.

#### ExecStore Partial-Failure Repair Table

For ExecStore (sequential writes), a crash between steps can leave
intermediate states. The reconciler detects and repairs these
deterministically. The guiding principle is **fail closed**: when in
doubt, leave the session de-routed (no `pool:` label) rather than
accidentally routing work to a broken session.

| `state` | Has `pool:` label | Runtime running | Is pool session? | Repair Action |
|---|---|---|---|---|
| `creating` | No | Yes | Yes | Complete: set `state=active`, add `pool:` label |
| `creating` | No | Yes | No | Complete: set `state=active` |
| `creating` | No | No | Any | Close bead (`stale_creating`) if age > `creation_timeout` |
| `active` | No | Yes | Yes | If pool under `max`: restore label. If at `max`: begin drain. |
| `active` | No | Yes | No | No repair needed (non-pool, no label expected) |
| `active` | No | No | Any | Set `state=suspended`, `state_reason=crash_recovery` |
| `draining` | Yes | Yes | Yes | Remove `pool:` label (interrupted drain start) |
| `draining` | Yes | No | Yes | Remove `pool:` label, set `state=archived` |
| `draining` | No | Yes | Yes | No repair needed (drain in progress) |
| `draining` | No | No | Yes | Set `state=archived` (drain crash completion) |
| `archived` | Yes | No | Yes | Remove `pool:` label (interrupted archive) |
| `archived` | No | Yes | Yes | Kill runtime (should not be running) |
| `suspended` | Yes | No | Yes | Remove `pool:` label (interrupted suspend) |
| `suspended` | No | Yes | Any | Kill runtime (should not be running) |
| `quarantined` | Yes | No | Yes | Remove `pool:` label (interrupted quarantine entry) |
| `quarantined` | Yes | Yes | Yes | Remove `pool:` label, kill runtime |
| `quarantined` | No | Yes | Any | Kill runtime (quarantined should not be running) |
| `quarantined` | No | No | Any | No repair needed (correct quarantine state) |

**Key principle:** An `active` pool session missing its `pool:` label is
auto-healed based on pool occupancy. If the pool is under `max`, the label
is restored (the session was likely interrupted during creation). If at
`max`, the session is drained (it was likely interrupted during retirement).
A `session.repair.active_no_label` event is emitted in both cases for
operator visibility.

The repair table is the single source of truth for crash recovery. Each
row is a test case in `TestExecStore_PartialFailureRepair`.

### Template Model

Templates are defined in `city.toml` via `[[agent]]` — the existing config
format. The key shift is conceptual: agents become templates, and templates
produce sessions.

```toml
[[agent]]
name = "polecat"
provider = "claude"
prompt_template = "polecat.md"

# Pool policy (optional)
[agent.pool]
min = 0
max = 5                    # max active sessions
check = "bd ready --label=pool:polecat | jq length"
routing_label = "pool:polecat"  # label managed by controller for routing
drain_timeout = "30s"      # max time to wait for in-flight work
archive_order = "lifo"     # "lifo" | "fifo" | "idle-first"
reactivate_order = "lifo"  # "lifo" | "fifo" (which archived session to wake)
max_archived = 10           # retention cap per template
quarantine_max_attempts = 3 # quarantine cycles before eviction
quarantine_backoff_cap = "5m" # max backoff between quarantine attempts
quarantine_healthy_duration = "5m" # healthy time to reset quarantine cycle
creation_timeout = "60s"    # max time in creating state before cleanup
max_unavailable = 1         # max sessions drained simultaneously for drift
archived_secret_ttl = "24h" # scrub secrets from wake_mode=fresh archives

# Session defaults (overridable per-session)
[agent.defaults]
model = "opus"             # default model
wake_mode = "fresh"        # "fresh" or "resume"
allow_overlay = ["model", "name", "title"]  # prompt requires explicit opt-in
allow_env_override = []    # no env overrides by default
```

**No pool config:** Template allows unlimited concurrent sessions. The
controller doesn't auto-scale — sessions are created/closed manually or
by other agents.

**With pool config:** Controller auto-scales active sessions between
`min` and `max` based on `check` command. Excess sessions are drained
then archived (not destroyed). Archived sessions can be reactivated (warm)
or new fresh sessions created, controlled by `wake_mode`.

**`check` command failure behavior:** If `check` returns a non-zero exit
code, times out (10s default), or produces non-numeric output, the
controller logs a warning and skips scaling for that template on this tick.
It does NOT default to 0 or any assumed count — this preserves the current
session count (fail static).

**Scale targets and tick budget:** The controller executes `check` commands
concurrently across templates (goroutine per template, bounded by
`runtime.NumCPU()`), with a hard per-tick deadline of 30 seconds.
Templates whose `check` command hasn't returned by the deadline are skipped
for that tick. The in-memory index makes drain-completion checks O(1) per
session (the index tracks claimed-work counts, updated on bead mutations).
**Claimed-work synchronization:** Work claims are made out-of-band by
agents (not through the controller socket). The controller synchronizes
its claim index via an **authoritative query** of the bead store
immediately before transitioning from `draining` → `archived`. This
ensures no race between a late claim and archival. During normal ticks,
the index maintains an approximate claim count for display/scheduling
purposes via the bead mutation feed (if available) or periodic scan
(every 10 ticks). The authoritative pre-archive query is the safety
gate — approximate counts only affect scheduling priority, not
correctness.
Target: reconciliation tick completes in &lt;1s for 50 templates × 100
sessions with warm index.

### Controller Reconciliation

The controller's tick loop changes from "rebuild agents from config" to
"reconcile session beads against desired state."

#### Current Flow (agent-centric)
```
tick:
  1. buildAgentsFromConfig() → []agent.Agent       # rebuild every tick
  2. syncSessionBeads(agents)                       # sync beads to match
  3. reconcileSessionBeads(agents, beads)           # wake/sleep decisions
```

#### Target Flow (session-first)
```
tick:
  1. evaluateTemplates(config) → desired state      # which templates, how many
  2. sessionIndex.snapshot() → current state            # in-memory, always consistent
     - Index populated at startup, maintained synchronously on mutations
     - All mutations go through controller (INV-5), no stale reads
  3. reconcile(desired, current):
     a. For each pool template:
        - Count creating + active + suspended + quarantined (= occupancy)
        - Compare to desired count from scale_check
          - Too few: create new (with state=creating marker) or reactivate
          - Too many: select sessions to retire:
            1. Suspended sessions first (no drain needed → archive directly)
            2. Active sessions per archive_order (drain → archive)
        - Check draining sessions: drain_timeout expired? → archive
        - Check creating beads: stale (>60s)? → close or complete
     b. For each session bead:
        - Config drift? → drain + recreate (rolling: max_unavailable per tick)
        - Dependency check → gate wake
        - Idle timeout? → suspend
        - Crash loop? → quarantine
        - Quarantine cooldown expired? → reactivate
     c. For non-pool templates: no count enforcement
```

#### Reconciliation Idempotency

The reconciliation loop MUST be idempotent — running the same tick twice
with the same inputs produces the same result. This is guaranteed by:

1. **Single-controller exclusivity.** `controller.lock` (flock) ensures
   only one controller process runs. The reconciliation loop is single-
   threaded within that process. No concurrent tick execution.

2. **Creation-intent markers.** When creating a new session, the controller
   first creates a bead with `state=creating` and a deterministic key
   (`template:{name}:tick:{tick_id}:slot:{n}`). Before creating, it checks
   for existing `creating` beads from prior ticks and reconciles them (either
   complete the creation or terminate the partial bead).

3. **Fail-closed startup.** If the startup index population
   (`populateIndex()`) fails, the controller does not start reconciliation.
   During normal operation, the in-memory index is the authoritative source
   (maintained synchronously). If a bead store write fails during a
   mutation, the index is NOT updated — the mutation is retried next tick.

The critical simplification: the controller no longer builds `agent.Agent`
objects. It reads config templates, evaluates pool desired counts, and
manages session beads directly. Runtime operations go through
`session.Manager` (or a thin wrapper), not `agent.Agent`.

### Config Hash Canonicalization

The `config_hash` field detects whether a session's effective configuration
has drifted from its template. The hash is computed over the **effective
resolved config** (template defaults merged with overlay overrides) to
correctly detect drift for overlaid sessions.

1. **Field inclusion list (behavioral fields only):**
   `provider`, `command` (resolved), `prompt_template` (content hash),
   `env` (sorted key=value pairs, including overlay env), `work_dir`,
   `hooks` (sorted), `model`, `wake_mode`, `session_setup`,
   `session_setup_script`, `pre_start`.

2. **Excluded from hash (non-behavioral):** TOML whitespace, comments, key
   ordering, `name`, `title`, `description`, pool scaling config (`min`,
   `max`, `check`), `drain_timeout`, `archive_order`, `max_archived`.

3. **Canonicalization:** Fields sorted lexicographically, values normalized
   (paths resolved, env sorted), concatenated as `key=value\n`, SHA-256
   hashed, truncated to 16 hex characters.

4. **Drift response:** On drift detection, sessions are drained in a
   **rolling update** — at most `max_unavailable` (default 1) sessions per
   template are drained simultaneously per tick. This prevents a template
   config change from dropping pool capacity to zero. After each drained
   session is archived, a replacement is created with the updated config.
   A bounded retry prevents churn: if drift-triggered recreates exceed 3
   per 10 minutes (tracked on the bead via `drift_recreate_count` and
   `drift_recreate_window`), the controller logs a warning and skips
   further drift recreates for that template until the window expires.

5. **Unit test requirement:** A test MUST prove that semantically identical
   configs with different TOML formatting produce identical hashes. A
   separate test MUST prove that template + overlay produces the same hash
   as the equivalent flat config.

### Pool Session Lifecycle

Pool sessions are the most complex case. Here's the complete lifecycle:

```
1. scale_check says 3 workers needed, 0 active
2. Controller creates 3 session beads from "worker" template
   - Each gets fresh context (wake_mode=fresh)
   - Names: auto-generated (e.g., "worker-a3f2", "worker-b7c1", "worker-d9e4")
   - Labels: ["gc:session", "template:worker", "member:worker"]
   - state=creating (NO pool: label yet), creating_at set
3. Runtime started for each session
4. Controller confirms liveness (IsRunning())
5. Batch-write: state=active + add pool:worker label
6. Sessions now pick up work via pool label
4. scale_check drops to 1
5. Controller selects 2 sessions to drain (per archive_order, default LIFO)
   - State → "draining", state_reason → "scale_down"
   - pool: label removed (stops new work routing)
   - Runtime continues, waiting for in-flight work to complete
   - drain_started timestamp set
6. In-flight work completes (or drain_timeout expires)
   - State → "archived", archived_at set
   - Runtime killed (if still running after timeout)
   - Bead stays open
7. scale_check goes back to 3
8. Controller needs 2 more active sessions
   - If wake_mode=fresh: create 2 new sessions, archived ones stay archived
   - If wake_mode=resume: reactivate 2 archived sessions (per reactivate_order)
     - Drift gate: config_hash must match current template. If drifted, create fresh.
     - generation incremented, new instance_token set
     - Runtime started, liveness confirmed, then pool: label restored
```

#### Drain Protocol

When the controller decides to archive a pool session:

1. **Remove `pool:` label** — prevents new work from being routed.
2. **Set `state=draining`**, `drain_started`, `state_reason`.
3. **Wait for in-flight work.** The controller checks each tick whether the
   session has any open beads **claimed by** this session (assigned work,
   not just ready-queue presence). The check uses the session's
   `session_name` or bead ID to identify claimed work, not the pool label.
4. **On drain complete** (no claimed work): set `state=archived`, send
   `SIGTERM` to runtime, wait 5s, then `SIGKILL` if still running.
5. **On drain timeout** (`drain_timeout` from pool config, default 30s):
   set `state=archived` with `state_reason=drain_timeout`, send `SIGTERM`
   then `SIGKILL`. Any orphaned beads are marked `blocked` with
   `reason=session_archived`.
6. **On crash during drain** (runtime exits unexpectedly while draining):
   set `state=archived` with `state_reason=crash_during_drain`. Any
   orphaned beads are marked `blocked` with `reason=session_crash_drain`
   (same cleanup as drain timeout).

The drain protocol ensures no silent data loss. Work in progress either
completes, or is explicitly marked as blocked for operator recovery.

#### Work Routing for Pools

Work discovery must exclude non-active sessions. The `pool:{template}`
label is the routing gate — it means "eligible for new work dispatch NOW":

- **Creating sessions:** No `pool:` label → no routing.
- **Active sessions:** Have the `pool:` label → receive work.
- **Suspended sessions:** `pool:` label removed on suspend → no routing.
  The `member:{template}` label preserves pool membership for queries.
  On resume, `pool:` label is restored after runtime liveness confirmed.
- **Draining sessions:** `pool:` label already removed → no new work.
- **Archived sessions:** `pool:` label removed → no new work. The
  `template:` and `member:` labels preserve associations for queries.

Routing eligibility is a pure function of `pool:` label presence. The
`pool:` label is ONLY present on `active` sessions with confirmed-live
runtimes. No metadata inspection needed at routing time.

### Session Creation with Overlay

When creating a session, the caller can override template defaults from a
strict allowlist:

```go
type CreateFromTemplate struct {
    Template  string            // required: template name
    Title     string            // optional: session title
    Overrides map[string]string // optional: overlay fields (allowlisted)
}
```

#### Overlay Allowlist

| Key | Description |
|---|---|
| `model` | Override provider model |
| `name` | Override session display name |
| `title` | Override session title |
| `prompt` | Append to template prompt (see note) |
| `env.{KEY}` | Override environment variable (per-template allowlist) |

**Prompt overlay semantics:** `overlay.prompt` is **appended** to the
template's `prompt_template` content (separated by `\n\n---\n\nAdditional
context provided at session creation:\n\n`). It cannot replace or remove
template prompt content — the template's safety instructions and identity
are always preserved. The overlay is explicitly framed as lower-trust
supplementary context, not as instructions that override the template.
Templates can disable prompt overlay entirely by omitting `prompt` from
`allow_overlay` (a new config field, default: `["model", "name", "title"]`
— prompt overlay requires explicit opt-in via `allow_overlay = ["model",
"name", "title", "prompt"]`). A size cap of 16KB is enforced. Logged as
`session.overlay.prompt` event. Scrubbed on close. Redacted in all
`gc session inspect` output (shows `[16KB appended]` not content).

#### Environment Variable Override Security

Environment variable overrides use a **per-template allowlist**, not a
global denylist. Templates declare which env vars may be overridden:

```toml
[agent.defaults]
allow_env_override = ["TARGET_URL", "LOG_LEVEL", "MODEL_TEMPERATURE"]
```

- Only keys listed in `allow_env_override` are accepted via `env.{KEY}`.
- If `allow_env_override` is omitted, **no** env overrides are permitted.
- Env key names must match `^[A-Z][A-Z0-9_]{0,127}$`.
- This eliminates the fragile denylist approach entirely — templates
  opt-in to exactly which variables callers may override.

#### Banned Overlay Keys (rejected at Create time)

These keys are always rejected regardless of template config:

- **Command/provider:** `command`, `provider`, `resume_flag`, `resume_style`
- **Internal state:** `session_key`, `state`, `generation`, `instance_token`
- **Any key not in the allowlist above**

Validation happens at `Create()` time. Unknown keys outside the allowlist
are rejected with an error listing valid keys.

Overlay fields are stored on the session bead (prefixed with `overlay.`)
so that resume reconstructs the same configuration.

**Overlay revalidation on resume/reactivate:** When a session is resumed
or reactivated, stored overlays are revalidated against the *current*
template policy (`allow_overlay`, `allow_env_override`). If the template
owner has revoked an overlay key since the session was created, the
offending overlay fields are stripped from the bead and the session
resumes with the template default for that field. A
`session.overlay.stripped` event is emitted listing the removed fields.
This prevents archived sessions from bypassing updated security policies.
The config hash is recomputed after stripping — if this changes the hash,
a drift event is also emitted.

Template resolution at start time merges: template defaults ← overlay fields.
`gc session inspect {session}` shows the effective configuration with both
layers visible for debugging.

### Session Key Lifecycle

The `session_key` is a provider-specific resume handle (e.g., Claude's
`--resume` session ID). It requires lifecycle management:

1. **Set on create:** Generated by the provider on first start.
2. **Preserved on suspend/archive:** Enables resume with warm context.
3. **Rotated on reactivate (if `wake_mode=fresh`):** New key, fresh context.
4. **Scrubbed on close:** Set to empty string when bead status → `closed`.
5. **Redacted in CLI output:** `gc session list` and `gc session inspect`
   show `[redacted]` instead of the raw key value.
6. **No TTL (by design):** The key's lifetime matches the session's lifetime.
   Archived sessions may hold keys for extended periods — the retention
   policy (see below) bounds this.

### Archived Bead Retention

Archived sessions accumulate over time. To prevent unbounded growth:

1. **Per-template cap:** `max_archived` in pool config (default 10). When
   creating a new archived session would exceed the cap, the oldest archived
   session is closed (bead status → `closed`, sensitive metadata scrubbed).

2. **Excluded from hot path:** The reconciliation loop's in-memory session
   index only tracks `active`, `suspended`, `draining`, and `quarantined`
   sessions. Archived sessions are not queried per-tick — only on
   reactivation (filtered query by template + state=archived).

3. **Sensitive metadata scrubbed on close:** When an archived bead is
   pruned to `closed`, `session_key` and `overlay.env.*` fields are cleared.

4. **Time-based secret scrubbing:** Archived sessions with
   `wake_mode=fresh` have their `session_key` and `overlay.env.*` scrubbed
   after `archived_secret_ttl` (default 24h) even while the bead remains
   open. These sessions will never be resumed with their old key, so early
   scrubbing is safe. Archived sessions with `wake_mode=resume` retain
   secrets until closed (they need the key for reactivation). The
   reconciler checks `archived_at + archived_secret_ttl` on each tick for
   `wake_mode=fresh` archived beads and scrubs expired secrets in-place.

### Removing agent.Agent

The `agent.Agent` interface (`internal/agent/agent.go`) becomes unnecessary.
Its operations map directly to `session.Manager` + `runtime.Provider`:

| agent.Agent method | Replacement |
|---|---|
| `Start()` | `session.Manager.Create()` or `.Attach()` |
| `Stop()` | `session.Manager.Suspend()` or `.Close()` |
| `Attach()` | `session.Manager.Attach()` |
| `IsRunning()` | `sp.IsRunning(sessionName)` |
| `IsAttached()` | `sp.IsAttached(sessionName)` |
| `Nudge()` | `sp.Nudge(sessionName, msg)` |
| `Peek()` | `session.Manager.Peek()` |
| `SessionConfig()` | Template resolution (pure function) |

The `managed` struct (internal/agent/agent.go:246-258) is replaced by the
session bead + template resolution. `buildOneAgent` (cmd/gc/build_agent.go)
becomes `resolveTemplate()` — a pure function that produces
`session.CreateParams` from config without creating in-memory objects.

### Migration Path

This is a large architectural change. Migration proceeds in phases to avoid
a big-bang rewrite. Each phase has a defined single-writer for runtime
lifecycle and a rollback procedure.

#### Phase 0: Bead Schema Migration (no risk)
Existing session beads use `type: "agent_session"` with label
`gc:agent_session` and states `active/stopped/orphaned/suspended`. This
phase adds forward-compatible handling: the controller recognizes both
`agent_session` and `session` bead types. New beads are created with
`type: "session"`. Existing beads are NOT migrated — they continue to work
and are naturally replaced as sessions are recreated. After Phase 4, any
remaining `agent_session` beads can be closed via a one-time cleanup
command.

**Legacy state mapping:**

| Legacy state (`agent_session`) | New state (`session`) | Pool occupancy |
|---|---|---|
| `active` | `active` | Counts against `max` |
| `suspended` | `suspended` | Counts against `max` |
| `stopped` | `closed` (terminal) | Does not count |
| `orphaned` | `suspended` (no runtime) | Counts against `max` |

Legacy beads count against pool `max` during the hybrid period to prevent
over-provisioning.

**Phase 0 tests:**
- `TestLegacyBeadRecognition` — controller reads `agent_session` beads
- `TestLegacyStateMapping` — legacy states map to new model correctly
- `TestHybridPoolOccupancy` — mixed legacy + new beads count correctly

#### Phase 1: Template Resolution (low risk)
Extract template resolution from `buildOneAgent` into a pure function that
returns `session.CreateParams` (command, env, hints, workDir). No behavioral
change — `buildOneAgent` calls the new function internally.

**Single writer:** `agent.Agent` (unchanged).
**Rollback:** Revert the extraction. `buildOneAgent` is self-contained again.

#### Phase 2: Controller Uses session.Manager (medium risk)
Modify the controller to create sessions via `session.Manager.Create()`
instead of `agent.Agent.Start()`. Session beads become the source of truth.
`agent.Agent` objects are still built but become **read-only** — they are
used only for operations that don't mutate lifecycle (peek, nudge, attach,
status queries). All lifecycle mutations (start, stop, suspend) go through
`session.Manager` exclusively.

**Single writer:** `session.Manager` (lifecycle). `agent.Agent` (read-only
operations only — `Peek()`, `IsRunning()`, `IsAttached()`, `Nudge()`).
**Anti-corruption boundary:** `agent.Agent.Start()` and `agent.Agent.Stop()`
are made unreachable in Phase 2 (panic if called, caught by tests).
**Rollback:** Re-enable `agent.Agent` lifecycle methods, revert controller
to use `agent.Agent.Start()`.

#### Phase 3: Pool Archival (medium risk)
Implement the drain protocol and archived state for pool sessions. Old pool
sessions transition through `draining` → `archived` instead of being
destroyed. Work routing excludes non-active sessions. Controller prefers
reactivation vs fresh creation based on `wake_mode`.

**Session naming:** The `{template}-{short-hash}` naming convention is
introduced in Phase 3 alongside the new pool lifecycle. During Phase 2,
session names remain compatible with the existing agent-name format.
**Downgrade handling:** On rollback to Phase 2, hash-named sessions are
unknown to the old binary. The rollback runbook (step 1) closes all
Phase 3 sessions before downgrading. The old binary's forward-compatibility
(skip unknown states/names with warning) prevents crashes if any are missed.
A `TestPhase3Downgrade_HashNamedSessions` integration test validates this.

**Single writer:** `session.Manager` (lifecycle, including new drain/archive).
**Rollback:** Revert to immediate destroy on scale-down. Rollback runbook:
1. While the new controller is still running, execute cleanup via socket:
   `gc session drain-all --template=X` (drains active sessions)
   `gc session close --state=archived,quarantined,creating` (closes beads)
2. Stop the new controller
3. Start the old binary (Phase 2)
4. Old binary skips unknown state values with warning (forward-compat)
If the new controller has already crashed (can't use socket), use
`gc session admin-close --offline` which: (a) acquires `controller.lock`
(non-blocking — fails if another controller is running), (b) kills
runtimes by `session_name` via `runtime.Provider.Stop()`, (c) marks
orphaned beads as `blocked`, and (d) writes state changes directly to
bead store (bypassing socket). Requires `--yes` flag for non-interactive
confirmation. This is the ONLY sanctioned offline mutation path and does
NOT require a running controller — it operates directly on the bead store
and runtime provider.
**Forward compatibility:** Unknown `state` values are skipped with a
`session.unknown_state` warning event, not errors. This allows safe
rollback from Phase 3 to Phase 2 without crashing on `draining`/`archived`
beads that the older binary doesn't understand.

#### Phase 4: Remove agent.Agent (low risk, large diff)
Replace all `agent.Agent` usage with direct `session.Manager` +
`runtime.Provider` calls. Remove `internal/agent/agent.go`, `buildOneAgent`,
`buildAgentsFromConfig`. The controller operates entirely on session beads.

**Single writer:** `session.Manager` (only writer remaining).
**Rollback:** Restore `agent.Agent` as read-only wrapper. Larger revert but
mechanically straightforward since Phase 2-3 already proved bead-driven
lifecycle.

#### Phase 5: Multi-Instance Consolidation
Remove `multiRegistry`. Multi-instance agents are just templates with
unlimited sessions — `gc session new {template}` creates a new session
from the template. `gc session suspend {session}` suspends or closes it.
The multi-instance bead tracking is subsumed by session beads.

**Single writer:** `session.Manager` (unchanged).
**Rollback:** Restore `multiRegistry` as a compatibility shim that delegates
to session beads.

### Depends-On Across Templates

Today `depends_on` is agent-to-agent. In the session model, it becomes
template-to-template: "at least one active session of the dependency
template must be alive." This is already how `allDependenciesAlive` works
for pools — generalize it.

Specifically: `depends_on: ["mayor"]` means "at least one session with
`template:mayor` label must be in `active` state." This is checked before
waking any session of the depending template.

## CLI Changes

### `gc session list` Default Output

```
NAME              TEMPLATE   SLOT  STATE       AGE    REASON
polecat-a3f2b7    polecat    -     active      2h     created
worker-b7c1d9     worker     1     active      45m    reactivated
worker-d9e4f5     worker     2     draining    2m     scale_down
worker-e5f6a7     worker     -     archived    1h     drain_complete
```

The SLOT column shows `pool_slot` for pool sessions (dash for non-pool).
`gc session inspect` redacts `session_key` and `overlay.env.*` values,
showing `[redacted]` instead.

**Default filter:** Shows `creating`, `active`, `suspended`, `draining`,
`quarantined`. Archived and closed sessions are hidden by default.

**Flags:**
- `--all` — show all states including archived and closed
- `--state=archived` — filter to specific state
- `--template=worker` — filter by template name

### Ambiguity Resolution

When `gc session peek {name}` matches multiple sessions (e.g., multiple
`polecat` sessions), the CLI returns an error:

```
Error: "polecat" matches 3 active sessions. Specify a session name:
  polecat-a3f2  (active, 2h)
  polecat-b7c1  (active, 45m)
  polecat-d9e4  (suspended, 1h)
```

For templates with exactly one active session, the template name works as
a shorthand (backward compatible with current `gc agent` commands).

## Test Strategy

Each migration phase has a defined test plan. All pool lifecycle tests use
`runtime.Fake` + `beads.MemStore` — no tmux required.

### Phase 1 Tests

| Test | Type | What It Verifies |
|---|---|---|
| `TestResolveTemplate_Basic` | Unit | Pure function produces correct CreateParams |
| `TestResolveTemplate_WithOverlay` | Unit | Overlay merges correctly with template defaults |
| `TestResolveTemplate_OverlayDenylist` | Unit | Banned keys rejected at creation |
| `TestConfigHash_Canonical` | Unit | Semantically identical configs produce identical hashes |
| `TestConfigHash_Behavioral` | Unit | Non-behavioral changes (comments, whitespace) don't change hash |

**Existing tests that break:** None (pure extraction, no behavioral change).

### Phase 2 Tests

| Test | Type | What It Verifies |
|---|---|---|
| `TestController_SessionManager_Create` | Integration | Controller creates sessions via Manager, not agent.Agent |
| `TestController_AgentStart_Panics` | Unit | agent.Agent.Start() is unreachable |
| `TestController_BeadDrivenLifecycle` | Integration | 3+ ticks with controller restart; no duplicate sessions, no orphaned beads |
| `TestController_FailedBeadRead_AbortsTick` | Unit | Bead store error → tick aborted, no mutations |

**Existing tests that break:** Tests calling `agent.Agent.Start()` directly
need updating to use `session.Manager.Create()`.

### Phase 3 Tests

| Test | Type | What It Verifies |
|---|---|---|
| `TestDrainProtocol_InFlightCompletes` | Integration | Drain waits for work, then archives |
| `TestDrainProtocol_Timeout` | Integration | Drain timeout → archive + orphan beads marked |
| `TestDrainProtocol_CrashDuringDrain` | Integration | Crash during drain → immediate archive |
| `TestArchive_LabelRemoved` | Unit | Archived session has no pool: label |
| `TestSuspend_PoolLabelRemoved` | Unit | Suspended pool session has no pool: label |
| `TestResume_LabelRestoredAfterLiveness` | Integration | Label only added after runtime confirmed alive |
| `TestReactivate_LabelRestoredAfterLiveness` | Integration | Label only added after runtime confirmed alive |
| `TestCreation_LabelAddedAfterLiveness` | Integration | pool: label only after state=active |
| `TestArchive_Reactivate_AtomicMutations` | Unit | State + label changes are batched |
| `TestArchivedSession_NoWorkRouting` | Integration | bd ready excludes archived sessions |
| `TestSuspendedSession_NoWorkRouting` | Integration | bd ready excludes suspended sessions |
| `TestRetentionPolicy_MaxArchived` | Unit | Oldest archived closed when cap exceeded |
| `TestCrashLoop_Quarantine` | Integration | N crashes → quarantined, cooldown → reactivated |
| `TestQuarantine_ReactivationBlockedAtMax` | Integration | At-max pool → quarantined→archived |
| `TestQuarantine_CycleCountPersisted` | Unit | quarantine_cycle survives controller restart |
| `TestScaleDown_SuspendedFirst` | Integration | Suspended archived before active drained |
| `TestExecStore_PartialFailureRepair` | Integration | Each repair table row (uses fault-injecting store wrapper) |
| `TestSocketConcurrency_MutationDuringTick` | Integration | CLI mutation via socket during active tick |
| `TestCreating_StaleCleanup` | Integration | Creating bead >60s → closed or completed |
| `TestForwardCompatibility_UnknownState` | Unit | Unknown state values skipped with warning |
| `TestReactivate_OverlayRevalidation` | Integration | Revoked overlay keys stripped on reactivate |
| `TestArchivedSecretTTL_FreshMode` | Integration | Secrets scrubbed after TTL for wake_mode=fresh |
| `TestAdminClose_Offline_KillsRuntimes` | Integration | Offline admin-close kills runtimes + marks beads |
| `TestDrainCompletion_AuthoritativeQuery` | Integration | Pre-archive query catches late work claims |
| `TestExecStore_QuarantineRepair` | Integration | All quarantine repair table rows |
| `TestActiveCrash_BelowThreshold_RestartInPlace` | Integration | Single crash restarts without state change |

**Existing tests that break:** Pool scaling tests that expect immediate
destroy need updating to expect drain → archive flow.

### Phase 4 Tests

| Test | Type | What It Verifies |
|---|---|---|
| `TestNoAgentAgentImports` | Build | No package imports `internal/agent` |
| `TestController_DirectManagerOps` | Integration | All operations work without agent.Agent |

**Existing tests that break:** All tests using `agent.Agent` interface
directly. Mechanical update to `session.Manager` equivalents.

### Phase 5 Tests

| Test | Type | What It Verifies |
|---|---|---|
| `TestMultiInstance_ViaSessionBeads` | Integration | gc session new creates session, gc session suspend closes |
| `TestNoMultiRegistry` | Build | multi_registry.go removed, no references |

**Existing tests that break:** Multi-instance tests. Rewritten to use
session-based operations.

### Conformance Suite Additions

The session conformance suite (`internal/session/conformance_test.go`) gains:

- `TestConformance_CreatingState` — creating → active with liveness check
- `TestConformance_CreatingStale` — creating cleanup after timeout
- `TestConformance_DrainState` — draining → archived transition
- `TestConformance_DrainCrash` — crash during drain → immediate archive
- `TestConformance_QuarantineState` — crash loop → quarantine → recovery
- `TestConformance_QuarantineAtMax` — quarantine reactivation blocked at max
- `TestConformance_ArchivedReactivation` — archived → active with generation bump
- `TestConformance_OverlayValidation` — per-template env allowlist enforcement
- `TestConformance_AtomicStateTransitions` — batch writes for multi-field transitions
- `TestConformance_SuspendedPoolRouting` — suspended pool session not routable
- `TestConformance_TwoAxisState` — bead.status × metadata.state consistency
- `TestConformance_UnknownStateForwardCompat` — unknown states skipped safely

## Impact Analysis

### Files to Change

| File | Phase | Change |
|---|---|---|
| `internal/session/manager.go` | 1-2 | Extend Create to accept template resolution |
| `cmd/gc/build_agent.go` | 1 | Extract resolveTemplate() |
| `cmd/gc/build_agents.go` | 2-4 | Rewrite to produce desired template counts |
| `cmd/gc/session_reconciler.go` | 2-3 | Reconcile against templates, not agents |
| `cmd/gc/session_beads.go` | 2-3 | Simplify (beads are now canonical) |
| `cmd/gc/session_wake.go` | 3 | Add drain/archived/quarantine state transitions |
| `cmd/gc/pool.go` | 3-4 | Pool scaling creates/drains/archives sessions |
| `cmd/gc/multi_registry.go` | 5 | Remove entirely |
| `internal/agent/agent.go` | 4 | Remove entirely |
| `cmd/gc/city_runtime.go` | 2-4 | Remove agent.Agent fields |
| `internal/config/config.go` | 1 | Add defaults section to Agent |

### Backward Compatibility

- **city.toml format:** No breaking changes. `[[agent]]` syntax is
  unchanged. Pool config is unchanged. The `[agent.defaults]` section
  and new pool fields (`drain_timeout`, `archive_order`, etc.) are additive.
- **CLI commands:** `gc session new/suspend/peek/attach` are the primary
  interface. `gc agent` is config-only (add/suspend/resume).
- **Bead schema:** New metadata fields are additive. Existing session
  beads are compatible (missing fields use defaults).
- **Environment variables:** `GC_SESSION_NAME` and `GC_TEMPLATE` (already
  emitted) become canonical. Legacy `GC_AGENT` continues during migration.

### Risks

1. **Drain protocol complexity.** The `draining` state adds a transitional
   lifecycle path. Implementation must handle edge cases: drain of a session
   that crashes during drain, drain timeout racing with work completion,
   double-drain of the same session.

2. **Migration duration.** Five phases over multiple PRs. The intermediate
   states increase code complexity temporarily, but the single-writer
   contract and anti-corruption boundary (Phase 2) limit the blast radius.

3. **Performance.** The reconciliation hot path uses an **in-memory session
   index** (same pattern as the convergence active index). The index maps
   bead ID → {template, state, labels} for all non-closed, non-archived
   sessions. It is populated at startup via a one-time full scan, then
   maintained synchronously on every mutation by the single-writer
   controller. Since all lifecycle mutations (including CLI commands) go
   through the controller socket (INV-5), the index is always consistent
   — no periodic full reconcile needed. The index eliminates per-tick
   store queries. Archived sessions are queried on-demand only during
   reactivation.

4. **Naming transition.** Pool instances today have deterministic names
   (`worker-3`). Session-based naming uses `{template}-{short-hash}`.
   The `pool_slot` metadata field provides backward-compatible sequential
   references for operators who need them.

## Resolved Questions

1. **Archived session pruning:** Yes, auto-pruned via `max_archived` per
   template (default 10). Oldest archived sessions are closed when the cap
   is exceeded. Sensitive metadata is scrubbed on close.

2. **Reactivation semantics:** When `wake_mode=resume`, the controller
   reactivates an archived session (same bead, same key, warm context).
   When `wake_mode=fresh`, the controller creates a new session bead with
   fresh context — archived sessions are NOT reactivated. The archived
   beads stay archived until pruned by `max_archived`.

3. **Template overlay scope:** Overlays are limited to a strict allowlist
   (`model`, `name`, `title`, `prompt`, `env.*` with denylist). Unknown
   keys are rejected at creation time.

4. **`depends_on` across templates:** Template-to-template: "at least one
   active session of the dependency template must be alive." Generalized
   from the existing pool dependency check.

5. **Routing label as ZFC compromise:** The controller manages the `pool:`
   routing label (adding/removing it during state transitions). The label
   string is parameterized via `routing_label` in pool config, so Go code
   manipulates a configured value, not a hardcoded prefix. This is a v1
   pragmatic compromise — future versions could externalize routing
   entirely to agent-driven label management via hooks.

## Open Questions

1. **Should `draining` sessions be visible to `gc session peek`?** They're
   still running but about to be archived. Current recommendation: yes,
   peek works on any running session regardless of state.

2. **Multi-template overlays.** Could a session combine fields from
   multiple templates? Current answer: no. One template per session. If
   needed, create a new template that inherits from others.

## Appendix: Current vs Target Comparison

### Creating a Pool Member

**Current (7 steps, in-memory):**
1. `evaluatePool()` → desired count
2. `poolAgents()` → deep copy config per instance
3. `buildOneAgent()` → resolve provider, build command, create agent.Agent
4. `syncSessionBeads()` → create bead to match agent
5. `reconcileSessionBeads()` → decide to wake
6. `agent.Agent.Start()` → runtime session
7. Agent picks up work via `bd ready --label=pool:{template}`

**Target (5 steps, bead-driven):**
1. `evaluatePool()` → desired count
2. `resolveTemplate()` → session.CreateParams from config
3. `session.Manager.Create()` → bead (state=creating, no pool: label)
4. Runtime starts, liveness confirmed → state=active, pool: label added
5. Session picks up work via pool label on bead

### Stopping a Pool Member

**Current (destroyed):**
1. Controller sees excess instances
2. `agent.Agent.Stop()` → tmux session killed
3. `syncSessionBeads()` → bead closed
4. History lost

**Target (drained + archived):**
1. Controller sees excess active sessions
2. `session.Manager.Drain()` → state=draining, pool label removed
3. Wait for in-flight work (or timeout)
4. `session.Manager.Archive()` → state=archived, runtime killed
5. Queryable via `gc session list --state=archived --template=worker`
6. Reactivatable if pool grows again
