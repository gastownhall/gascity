# Unified Session Model

| Field | Value |
|---|---|
| Status | Draft |
| Date | 2026-03-07 |
| Author(s) | Chris Sells, Claude |
| Issue | — |
| Supersedes | [docs/design/chat-sessions.md](chat-sessions.md) |

## Summary

Unify Gas City's two independent agent lifecycle systems — config-driven
`[[agents]]` managed by the reconciler, and on-demand sessions managed via
CLI/API — into a single primitive: the **session**. Every agent instance
becomes a session backed by a bead. Sessions don't have restart policies;
they have **wake reasons**. The reconciler becomes a simple loop: "does
this session have a reason to be awake? if yes and no process, wake it."
This eliminates the restart/policy/lifecycle state machine, reduces the
codebase's concept count, and directly implements NDI — the process is
ephemeral, the work is permanent.

## Motivation

Today Gas City has two parallel systems for managing agent processes:

**System 1: Config agents** (`[[agents]]` in city.toml)
- Reconciler starts them at boot, restarts on crash, stops orphans
- Config fingerprinting detects drift -> drain-then-restart
- Crash-loop backoff, idle timeout, pool scaling
- State lives in session metadata (tmux env vars)
- No bead persistence — agent identity exists only while the city runs

**System 2: On-demand sessions** (`gc session new`)
- User creates/suspends/resumes/closes via CLI/API
- Bead-backed — session state survives controller restart
- No health monitoring, no crash recovery, no work dispatch
- Completely invisible to the reconciler

This creates three concrete problems:

**1. Sessions can't receive work autonomously.** When a user creates a
session via `gc session new helper`, that session can chat interactively
but can't be slung work via the dispatch system. If work is slung to it,
nothing monitors whether it's alive to process it. The reconciler doesn't
know it exists.

**2. Config agents lose state on controller restart.** A config agent's
identity is ephemeral — it exists as a tmux session name and some
metadata keys. If the controller crashes, the agent is restarted from
scratch. There's no persistent record of what it was doing, what work
was hooked, or what its conversation history contained.

**3. Two lifecycle systems means doubled complexity.** The reconciler has
a 573-line state machine with crash tracking, drain signaling, idle
timeouts, config fingerprinting, and pool scaling. The session manager
has its own 300-line state machine with suspend/resume/close/prune. Both
manage the same underlying thing — a process running in a runtime
provider — but with completely different mechanics and no shared code.

The root insight: **a config agent is just a session with a standing
reason to be awake** ("the city config says I should exist"). An
interactive session's reason is "the user is attached." A slung-work
session's reason is "I have hooked beads." There's no need for restart
policies, crash-loop backoff, or separate lifecycle management — just
one question: **should this be awake right now?**

This follows directly from NDI (Nondeterministic Idempotence): the
session is a bead. The bead persists. The process is a transient
materialization of that bead's liveness. Multiple independent observers
(reconciler ticks, user actions, work dispatch) can check the same
state idempotently and converge to the correct outcome.

## Guide-Level Explanation

### Everything is a session

After this change, there is one concept for "a running agent": a
**session**. Sessions are beads. They persist across controller
restarts. They can be awake (has a runtime process) or asleep (bead
exists, no process).

Config agents in city.toml become session templates with a standing wake
reason:

```toml
[[agents]]
name = "overseer"
provider = "claude"
prompt_template = "prompts/overseer.md"
# This agent is always awake while the city runs.
# Equivalent to the old always-on behavior.
```

Interactive sessions are created on demand:

```bash
$ gc session new claude --title "debugging auth"
# Uses provider preset "claude" (no matching [[agents]] entry).
# Session created, tmux attached.
# When you detach + go idle, the session sleeps.
# Work persists in the bead. Resume anytime:
$ gc session attach gc-42
```

Both kinds of session can receive slung work:

```bash
$ gc sling gc-42 --formula investigate
# If gc-42 is asleep, the reconciler wakes it.
# If gc-42 is awake, it picks up the work on its next hook check.
```

### Wake and sleep, not start and stop

Sessions don't "start" and "stop" — they **wake** and **sleep**. The
distinction matters: stopping implies destruction, but a sleeping session
retains its full identity, conversation history, and work state. It just
doesn't have a process.

The controller's reconciliation loop becomes:

```
for each session bead:
    reasons := wakeReasons(session)   // pure — no side effects
    alive := isAlive(session)         // ProcessAlive, not just IsRunning

    if held(session):
        skip                          // user override suppresses wake

    if quarantined(session):
        skip                          // crash-loop protection

    if len(reasons) > 0 && !alive:
        wake(session)

    if len(reasons) == 0 && alive && pastIdleTimeout:
        beginDrain(session)           // async — returns immediately
```

Wake reasons are:
- **Config presence** — `[[agents]]` entry exists in city.toml
  (pool instances: slot within desired count)
- **User attached** — terminal connected to session
  (providers that support it)
- **Hooked work** — session has open beads assigned to it

Sleep triggers are:
- **Idle timeout** — no I/O activity for configured duration
- **Explicit suspend** — user ran `gc session sleep` (sets user hold)
- **Config removal** — `[[agents]]` entry removed from city.toml
- **Work complete** — all hooked beads closed, no other wake reason

### User hold (explicit sleep override)

When a user explicitly sleeps a session via `gc session sleep`, a
**user hold** is set on the session bead (`held_until` metadata field).
This suppresses all computed wake reasons until the hold is released.
Without this, `gc session sleep` on a config agent would be immediately
undone by the next reconciler tick seeing `WakeConfig`.

```bash
$ gc session sleep gc-1                # holds indefinitely
$ gc session sleep gc-1 --for 30m      # holds for 30 minutes
$ gc session wake gc-1                 # releases hold AND quarantine, wakes
```

The `gc session sleep` handler sets both `held_until` and
`sleep_reason = "user-hold"` in one batched write, then begins
an async drain if the session is alive.

The hold is a timestamp, not a boolean. `held_until = "9999-12-31T..."`
means indefinite hold. A past timestamp means the hold has expired and
wake reasons apply normally. The reconciler checks `held_until` before
computing wake reasons.

User holds are distinct from pool suppression — `gc session wake` only
clears user holds and quarantine, not pool-computed desiredness. See
[Pool integration](#pool-integration).

<!-- REVIEW: added per Blocker 7 — gc session wake clears BOTH hold AND quarantine consistently -->

### What the user sees

```bash
# List all sessions (config agents + interactive)
$ gc session list
ID       TEMPLATE   STATE    REASON     TITLE              AGE    LAST ACTIVE
gc-1     overseer   awake    config     —                  2d     3s ago
gc-2     worker     awake    config     —                  2d     1m ago
gc-3     worker     awake    config     —                  2d     45s ago
gc-42    helper     asleep   idle       debugging auth     4h     4h ago
gc-51    helper     asleep   user-hold  refactor config    1d     1d ago
gc-7     worker     asleep   quarantine —                  1d     1d ago
gc-8     worker     awake    config (draining)  —          2d     5s ago

# STATE is always awake|asleep (draining is a REASON suffix)
# REASON shows: primary wake reason when awake, sleep reason when asleep
# Draining is shown as a suffix: "config (draining)"

$ gc session peek gc-1
# ... last 50 lines of overseer's output ...

$ gc session attach gc-42
# Releases user hold, wakes the session, resumes conversation.
```

<!-- REVIEW: added per Blocker 7 — draining is a modifier/suffix, not a state -->

### Pool scaling

Pools still work, but pool instances are sessions:

```toml
[[agents]]
name = "worker"
provider = "claude"
prompt_template = "prompts/worker.md"
wake_mode = "fresh"        # "resume" (default) | "fresh"

[agents.pool]
min = 1
max = 5
check = "bd ready --label=role:worker --json | jq length"
```

The pool evaluator determines desired count. Each instance is a session
bead (`worker-1` through `worker-5`). Excess instances sleep instead of
being destroyed — they can be woken again if demand increases. The
`wake_mode` controls whether the woken session resumes its previous
conversation or starts with a clean context window:

- **`resume`** (default) — reuse the provider session key. The agent
  picks up where it left off. Best for overseers, monitors, and roles
  that need continuity.
- **`fresh`** — start a new provider session on every wake. The bead
  persists (identity, crash tracking, pool slot stability), but the
  runtime process gets a clean context window. Best for coding workers
  that should approach each task without stale assumptions.

This directly supports the Gas Town polecat pattern: ephemeral coding
agents that get fresh context for each task. The slot identity is
stable (worker-3 is always worker-3), but the conversation is not
carried forward.

**Polecat lifecycle:** A fresh-mode worker wakes, processes its hooked
work, completes, goes idle, and sleeps via idle timeout. The next
dispatch wakes it with a clean context. The worker does NOT need to
self-terminate after each task — the normal idle-timeout → sleep →
fresh-wake cycle handles context rotation. This avoids false crash-loop
dampening triggers. If a worker processes multiple tasks within a single
waking period, they share context; fresh context resets only across
sleep/wake boundaries. For strict per-task isolation, configure a short
`idle_timeout` so the worker sleeps promptly after completing work.

Pool desiredness is purely computed: `wakeReasons()` checks whether this
instance's `pool_slot` is within the desired count. There is no stored
"pool hold" — the slot number and desired count are the ground truth.
See [Pool integration](#pool-integration).

## Reference-Level Explanation

### Session bead schema

Every session is a bead with `type: "session"` and label `gc:session`:

```
Bead {
    ID:        "gc-1"
    Title:     "overseer" (or user-provided title)
    Type:      "session"
    Status:    "open" | "closed"
    Labels:    ["gc:session", "template:overseer"]
    CreatedAt: timestamp
    Metadata: {
        // Identity (5 fields — controller-owned, immutable after creation)
        "template":      "overseer"           // agent template name
        "provider":      "claude"             // provider preset
        "session_name":  "s-gc-1"             // runtime session name
        "pool_slot":     "2"                  // pool slot (0 for non-pool)
        "pool_template": "worker"             // parent template (pool only)

        // Wake behavior (1 field — synced from config each tick, mutable)
        "wake_mode":     "resume"             // "resume" | "fresh"

        // State — advisory, healed each tick, never branched on (3 fields)
        "state":         "awake" | "asleep"
        "slept_at":      RFC3339
        "sleep_reason":  "idle" | "user-hold" | "quarantine" | ...

        // User hold — distinct from pool desiredness (1 field)
        "held_until":    RFC3339 | ""         // empty = no hold

        // Crash-loop dampening (3 fields)
        "wake_attempts":      "3"             // consecutive failed/unstable wakes
        "last_woke_at":       RFC3339         // when last successfully woken
        "quarantined_until":  RFC3339         // skip wakes until

        // Drain — ephemeral, not persisted across crashes (2 fields)
        // Tracked in-memory by the controller, NOT in bead metadata.
        // Listed here for documentation; see Drain section.

        // Runtime (1 field — controller-owned, not user-mutable)
        "work_dir":      "/home/user/project" // validated at wake time

        // Config fingerprinting (2 fields)
        "config_hash":   "abc123"             // hash of restart-requiring config fields
        "live_hash":     "def456"             // hash of hot-reloadable config fields

        // Concurrency control (2 fields)
        "generation":    "4"                  // incremented on each wake
        "instance_token": "nonce-..."         // controller-owned nonce for runtime binding
    }
}
```

<!-- REVIEW: added per Blocker 6 — session_key removed from bead metadata (moved to secrets file), instance_token added, identity fields marked immutable -->

**Session key storage:** `session_key` is NOT stored in bead metadata.
It is stored in `.gc/secrets/<session-id>.key` with `0600` permissions,
read only by the controller at wake time. It is redacted from API
responses and event payloads. See [Runtime targeting](#runtime-targeting-and-execution-guarantees).

<!-- REVIEW: added per Blocker 6 — session_key in secrets file -->

**Field count comparison** (old system -> new):

| Old System | Fields |
|---|---|
| tmux env vars per agent | GC_AGENT, GC_CONFIG_HASH, GC_LIVE_HASH, GC_DRAIN (4) |
| in-memory reconciler state per agent | crashCount, lastCrash, quarantineUntil, drainSignaled, idleTimeout, configFingerprint (6) |
| session manager per session | bead ID, title, state, suspended_at (4) |
| **Total (two systems combined)** | **14 fields across two disjoint systems** |

| New System | Fields |
|---|---|
| session bead metadata | 18 fields in one unified store |
| secrets file | session_key (1 field, external) |
| in-memory reconciler state | drainState map (2 fields per draining session), worker pool state |
| **Total (one system)** | **~21 fields in one system** |

The new system has ~50% more fields but eliminates the *second system
entirely*. The additional fields are: pool identity (2), user hold (1),
generation (1), instance_token (1), sleep_reason (1), last_woke_at (1),
wake_mode (1).
The win is model unification, not field-count reduction.

<!-- REVIEW: updated per Blocker 6 — field count adjusted for session_key move and instance_token addition -->

### State model

<!-- REVIEW: added per Blocker 7 — stable public status schema -->

Sessions have two **public states** with orthogonal modifiers:

**Public states** (always one of these in API/CLI output):
- `awake` — has a live runtime process
- `asleep` — bead exists, no process

**Orthogonal modifiers** (can combine with either state, shown as suffixes):
- `held` — user hold suppresses wake reasons
- `quarantined` — crash-loop dampening suppresses wake reasons
- `draining` — graceful shutdown in progress (ephemeral, in-memory only)

**Stable public status schema** (returned by API and CLI):

```go
type SessionStatus struct {
    State          string      `json:"state"`           // always "awake" or "asleep"
    DesiredAwake   bool        `json:"desired_awake"`   // true if wake reasons exist
    WakeReasons    []string    `json:"wake_reasons"`    // e.g., ["config"], ["work","attached"]
    BlockedReasons []string    `json:"blocked_reasons"` // e.g., ["pool","dependency"]
    SleepReason    string      `json:"sleep_reason"`    // why asleep: "idle","user-hold","quarantine",...
    HeldUntil      *time.Time  `json:"held_until"`      // nil if no hold
    QuarantinedUntil *time.Time `json:"quarantined_until"` // nil if no quarantine
    WakeAttempts   int         `json:"wake_attempts"`   // consecutive failed wakes
    Draining       bool        `json:"draining"`        // true if drain in progress
    DrainReason    string      `json:"drain_reason"`    // "idle","pool-excess","config-drift","user-sleep" (empty if not draining)
}
// <!-- REVIEW: Round 2 fix — drain_reason added to public status -->
```

<!-- REVIEW: added per Blocker 7 — draining is a modifier, desired_awake/blocked_reasons exposed -->

```
                ┌──────────┐
   create ──────>  awake   |
                | (has     |<──── wake (reasons exist, not held/quarantined)
                | process) |
                └────┬─────┘
                     |
        drain (async) -> process exits -> asleep
                     |
                     v
                ┌──────────┐
                |  asleep  |
                | (bead    |──── close (permanent end)
                |  only)   |         |
                └──────────┘         v
                                ┌──────────┐
                                |  closed  |
                                | (bead    |
                                | status=  |
                                | closed)  |
                                └──────────┘
```

`closed` is not a session state — it's a bead status. A closed session's
bead still exists for historical queries but can never be woken.

"Awake" and "asleep" replace the current "active" and "suspended"
terminology. This reinforces that the session is always there — it's
just sleeping, not gone.

### Wake reasons (computed, not stored)

Wake reasons are **computed fresh each reconciler tick**, not stored.
This is critical for NDI — stale stored state can't cause incorrect
behavior if the reasons are always recomputed from ground truth.

`wakeReasons()` is a **pure function** — it reads bead metadata and
config but never writes. Expiry clearing is handled by a separate
healing pass.

```go
type WakeReason string

const (
    WakeConfig   WakeReason = "config"    // [[agents]] entry exists (per-instance for pools)
    WakeAttached WakeReason = "attached"  // user terminal connected
    WakeWork     WakeReason = "work"      // has hooked/open beads
)

// wakeReasons computes why a session should be awake.
// PURE FUNCTION — reads only, never writes metadata.
// poolDesired is the per-tick snapshot from evaluatePoolCached().
// Returns nil if the session should be asleep.
func wakeReasons(session bead, cfg config.City, sp runtime.Provider, poolDesired map[string]int) []WakeReason {
    // User hold suppresses all reasons
    if held := session.Metadata["held_until"]; held != "" {
        if t, err := time.Parse(time.RFC3339, held); err == nil && time.Now().Before(t) {
            return nil
        }
        // Hold expired — treated as no hold. Cleared by healExpiredTimers().
    }

    // Quarantine suppresses all reasons
    if q := session.Metadata["quarantined_until"]; q != "" {
        if t, err := time.Parse(time.RFC3339, q); err == nil && time.Now().Before(t) {
            return nil
        }
        // Quarantine expired — treated as no quarantine. Cleared by healExpiredTimers().
    }

    var reasons []WakeReason

    // Config presence — per-instance for pools
    template := session.Metadata["template"]
    if agent, ok := cfg.FindAgent(template); ok {
        if agent.Pool == nil {
            reasons = append(reasons, WakeConfig)
        } else {
            // Pool: only wake if slot is within desired count (cached per tick)
            slot, _ := strconv.Atoi(session.Metadata["pool_slot"])
            desired := poolDesired[template]  // from per-tick snapshot
            if slot > 0 && slot <= desired {
                reasons = append(reasons, WakeConfig)
            }
        }
    }

    // User attached — only if provider supports it.
    // IsAttached uses context deadline (2s) and tri-state return.
    // ProbeUnknown is treated as "not attached" (conservative).
    if sp.Capabilities().CanReportAttachment {
        ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
        defer cancel()
        if sp.IsAttached(ctx, session.Metadata["session_name"]) == ProbeAlive {
            reasons = append(reasons, WakeAttached)
        }
    }
    // <!-- REVIEW: Round 4 fix — IsAttached uses context + tri-state -->

    // Phase 4: Hooked work — deferred until work-driven wake ships.
    // When enabled, sessions with open hooked beads stay awake even
    // if they lose WakeConfig (e.g., pool scale-down). Until Phase 4,
    // pool-excess instances drain even with hooked work — the sling
    // handler emits EventWorkSlungToBlockedSession for operator mitigation.
    //
    // if hasHookedWork(session.ID) {
    //     reasons = append(reasons, WakeWork)
    // }

    return reasons
}
```

### Liveness predicate

The reconciler uses `ProcessAlive`, not `IsRunning`, to determine if a
session has a live workload process. Provider probe calls return a
tri-state result to distinguish timeout from negative:

<!-- REVIEW: Round 3 fix — probe APIs use tri-state (alive/dead/unknown) with context deadlines -->

```go
// ProbeResult represents a bounded probe outcome.
type ProbeResult int
const (
    ProbeAlive   ProbeResult = iota  // process confirmed alive
    ProbeDead                        // process confirmed dead or absent
    ProbeUnknown                     // timeout or error — cannot determine
)

// ProcessAlive checks both carrier existence AND workload liveness.
// Uses context deadline (default: 2s). Returns ProbeUnknown on timeout.
func (sp *TmuxProvider) ProcessAlive(ctx context.Context, name string) ProbeResult

func isAlive(ctx context.Context, session bead, sp runtime.Provider) ProbeResult {
    name := session.Metadata["session_name"]
    probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
    defer cancel()
    return sp.ProcessAlive(probeCtx, name)
}
```

The reconciler handles `ProbeUnknown` by skipping the session for that
tick — no wake decision, no sleep decision, no crash counting. The
session is retried on the next tick. This prevents timeouts from being
misinterpreted as process death.

```go
// In the reconciler wake pass:
    probeResult := isAlive(tickCtx, session, sp)
    if probeResult == ProbeUnknown {
        emit(EventProbeTimeout, session.ID, session.Metadata["session_name"])
        continue  // skip this session entirely — retry next tick
    }
    alive := probeResult == ProbeAlive
```

`IsAttached` and `GetLastActivity` follow the same pattern: context
deadline, tri-state return, skip-on-unknown.

<!-- REVIEW: added per Major 2 — dead pane handling specified -->

### Provider capability tiers

Not all runtime providers support all wake-reason inputs:

```go
type ProviderCapabilities struct {
    CanReportAttachment bool  // IsAttached() returns meaningful results
    CanReportActivity   bool  // GetLastActivity() returns meaningful results
}
```

| Capability | tmux | subprocess | (future) k8s |
|---|---|---|---|
| ProcessAlive check | session exists AND pane process running (not `pane_dead`) | `os.Process.Signal(0)` succeeds AND not exited | pod phase = Running AND container ready |
| CanReportAttachment | yes | no | no |
| CanReportActivity | yes | no | yes (via metrics) |

When `CanReportAttachment` is false, `WakeAttached` is never produced.
When `CanReportActivity` is false, idle timeout is skipped — the session
stays awake until wake reasons change. This is a known behavioral
difference: providers without activity reporting produce longer-lived
sessions. Document this in operator guides.

<!-- REVIEW: added per Major 2 — explicit behavioral difference documentation -->

### Provider state table

<!-- REVIEW: added per Major 2 — concrete provider behavior specification -->

Each provider operation has defined behavior per runtime:

| Operation | tmux | subprocess |
|---|---|---|
| **ProcessAlive** | `tmux has-session` succeeds AND `pane_dead != 1`. If dead pane detected: `tmux kill-session`, return false. Next wake recreates. | `os.Process.Signal(0)` succeeds AND `cmd.ProcessState == nil` (not exited). |
| **Start** | `tmux new-session -d -s <name> <cmd>`. Verify session exists after return. | `exec.CommandContext(ctx, path, args...).Start()`. Record PID. |
| **Stop** | `tmux kill-session -t <name>`. | `SIGTERM` to process group. Wait 5s. If still alive, `SIGKILL`. |
| **Interrupt** | `tmux send-keys -t <name> C-c`. | `SIGINT` to process group. |
| **Activity** | Last pane output timestamp via `tmux display -p '#{pane_last_output}'`. | Not available — returns zero time. |
| **IsAttached** | `tmux display -p '#{session_attached}'` returns > 0. | Not available — returns false. |

**Dead pane recovery (tmux):** When `remain-on-exit` is enabled, a crashed
process leaves a dead pane. `ProcessAlive` detects this via `pane_dead`
flag. The dead session is killed immediately. On the next reconciler tick,
the session has wake reasons but no process, so it is re-woken with a
fresh pane.

### Reconciler

The reconciler is a phased loop running in the controller. Intent
computation is synchronous; provider I/O executes in a bounded worker
pool. See [Concurrency architecture](#concurrency-architecture).

<!-- REVIEW: updated per Blocker 3 — reconciler description now references concurrency architecture -->

```go
func reconcile(sessions []bead, cfg config.City, sp runtime.Provider) {
    tickStart := clock.Now()

    // Pre-check: abort entire tick if bead store is unhealthy
    if err := store.Ping(); err != nil {
        emit(MetricBeadStoreHealthy, 0)
        emit(EventBeadStoreUnavailable, err, true)
        consecutiveStoreFailures++
        if consecutiveStoreFailures >= 3 {
            emit(EventBeadStoreCritical, consecutiveStoreFailures)
        }
        return
    }
    emit(MetricBeadStoreHealthy, 1)
    consecutiveStoreFailures = 0

    // Phase 0: Heal expired timers (pure cleanup, no decisions)
    for _, session := range allSessionBeads() {
        healExpiredTimers(session)
    }

    // Phase 1: Ensure sessions exist for all config agents.
    // session_name is validated at creation time (not just wake time).
    for _, agent := range cfg.Agents {
        if !hasSessionBead(agent.Name) {
            name := sessionNameFor(agent.Name)
            if !sessionNamePattern.MatchString(name) {
                emit(EventConfigValidationError, "session_name", name)
                continue
            }
            bead := createSessionBead(agent, name)
            // Seed wake_mode from config (validated at config load)
            bead.SetMetadata("wake_mode", agent.WakeMode) // "" defaults to "resume"
        }
    }

    // Phase 1a: Sync mutable config fields to ALL config-backed sessions.
    // This runs unconditionally — before liveness probes, quarantine checks,
    // or budget limits — so config changes propagate even to asleep or
    // quarantined sessions. Takes effect on next wake.
    //
    // Write failure semantics: if SetMetadata fails, the in-memory snapshot
    // retains the stale value for this tick. The write retries next tick.
    // One-tick staleness is safe in both directions:
    //   resume→fresh: session resumes once more, then starts fresh next tick.
    //   fresh→resume: session starts fresh once more, key file is preserved
    //     (fresh mode never deletes keys), so resume works on next tick.
    // This matches the general metadata write failure model used by all
    // other fields (config_hash, held_until, etc.).
    for _, session := range allSessions {
        template := session.Metadata["template"]
        if agent, ok := cfg.FindAgent(template); ok {
            wm := agent.WakeMode
            if wm == "" { wm = "resume" }
            if wm != session.Metadata["wake_mode"] {
                session.SetMetadata("wake_mode", wm)
            }
        }
    }

    // Phase 1b: Pool scaling — snapshot desired counts, then reconcile.
    // evaluatePool is called ONCE per template here and cached for the tick.
    // wakeReasons() uses the cached value — never re-evaluates.
    // reconcilePool returns the POST-HYSTERESIS desired count, which is
    // written back to poolDesired. All downstream consumers (wakeReasons,
    // isPoolExcess, sling) use poolDesired — the authoritative applied count.
    poolDesired := map[string]int{}  // template name -> applied desired count
    for _, agent := range cfg.Agents {
        if agent.Pool != nil {
            rawDesired := evaluatePoolCached(agent, poolDesired)
            poolDesired[agent.Name] = reconcilePool(agent, rawDesired, cfg, sp)
            // poolDesired now contains the post-hysteresis value
        }
    }
    // <!-- REVIEW: Round 4 fix — poolDesired stores post-hysteresis applied count -->

    // Phase 2a: Wake pass (forward dependency order — dependencies first).
    // Compute intent synchronously, dispatch provider I/O to worker pool.
    // All probe calls use context deadlines and tri-state results.
    wakeOrder := topoOrder(allSessionBeads(), cfg.DependsOn)
    wakesBudget := maxWakesPerTick  // default: 5
    tickCtx, tickCancel := context.WithTimeout(context.Background(), tickBudget)
    defer tickCancel()

    for _, session := range wakeOrder {
        // Wall-clock budget: defer remaining work to next tick
        if clock.Since(tickStart) > tickBudget {  // default: 5s
            emit(EventTickBudgetExhausted, clock.Since(tickStart))
            break
        }

        reasons := wakeReasons(session, cfg, sp, poolDesired)
        probeResult := isAlive(tickCtx, session, sp)
        if probeResult == ProbeUnknown {
            emit(EventProbeTimeout, session.ID, session.Metadata["session_name"])
            continue  // skip — cannot make decisions without liveness truth
        }
        alive := probeResult == ProbeAlive
        // <!-- REVIEW: Round 4 fix — probe tri-state wired into reconciler loop -->

        // Stability check: detect rapid exits before making decisions.
        // If session was recently woken and is already dead, count as crash.
        // Returns true if a failure was recorded this tick (suppresses
        // duplicate recordWakeFailure in the switch below).
        stabilityFailed := checkStability(session, alive)

        // Clear crash counter for sessions that have been stable.
        if alive && stableLongEnough(session) && wakeAttempts(session) > 0 {
            clearWakeFailures(session)
        }

        // Re-read quarantine state — checkStability may have set it.
        if isQuarantined(session) {
            healState(session, alive)
            continue
        }

        switch {
        case len(reasons) > 0 && !alive && !isDraining(session):
            // Check dependency gate: all dependencies must be alive and not draining.
            if !allDependenciesAlive(tickCtx, session, cfg, sp) {
                emit(EventSessionWakeDeferred, session.ID, "dependency")
                continue  // defer to next tick — don't consume budget
            }
            if wakesBudget <= 0 {
                emit(EventSessionWakeDeferred, session.ID, "budget-exhausted")
                continue  // defer to next tick
            }
            if err := wake(session, cfg, sp); err != nil {
                // Only count as failure if checkStability didn't already count
                if !stabilityFailed {
                    recordWakeFailure(session)
                }
            } else {
                gen, _ := strconv.Atoi(session.Metadata["generation"])
                emit(EventSessionWoke, session.ID, session.Metadata["template"],
                    reasons, gen)
            }
            wakesBudget--

        case len(reasons) > 0 && alive:
            if needsRestart(session, cfg) {
                // Config drift: update config_hash NOW (at drain start),
                // not at wake. This prevents restart loops.
                beginDrain(session, sp, "config-drift")
                session.SetMetadata("config_hash", computeConfigHash(session, cfg))
                emit(EventConfigDrift, session.ID, session.Metadata["config_hash"])
            }

        case len(reasons) > 0 && !alive && isDraining(session):
            // Draining but already dead — advanceDrains() will clean up.

        case len(reasons) == 0 && !alive:
            // Correct state, skip.
        }

        // Heal advisory state (dirty-check: only write if changed)
        healState(session, alive)
    }

    // Phase 2b: Sleep pass (reverse dependency order — dependents first).
    // Uses same tri-state probe pattern as Phase 2a.
    sleepOrder := reverse(wakeOrder)
    for _, session := range sleepOrder {
        // Wall-clock budget check
        if clock.Since(tickStart) > tickBudget {
            emit(EventTickBudgetExhausted, clock.Since(tickStart))
            break
        }

        reasons := wakeReasons(session, cfg, sp, poolDesired)
        probeResult := isAlive(tickCtx, session, sp)
        if probeResult == ProbeUnknown {
            continue  // skip — cannot determine liveness
        }
        alive := probeResult == ProbeAlive

        if len(reasons) == 0 && alive && !isDraining(session) {
            // Pool excess: drain immediately, don't wait for idle timeout.
            if isPoolExcess(session, cfg, poolDesired) {
                beginDrain(session, sp, "pool-excess")
            } else if pastIdleTimeout(session, sp) {
                beginDrain(session, sp, "idle")
            }
        }

        healState(session, alive)
    }
    // <!-- REVIEW: Round 5 fix — Phase 2b uses tri-state probes -->

    // Phase 2c: Advance in-progress drains.
    advanceDrains(sp)

    // Phase 3: Orphan cleanup (grace period).
    // Requires adoption barrier — see Migration protocol.
    if !adoptionBarrierPassed {
        // Skip orphan cleanup until all live sessions have matching beads.
        return
    }
    for _, name := range sp.ListRunning(prefix) {
        if !hasMatchingBead(name) {
            ticks := incrementOrphanTickCount(name)
            if ticks >= orphanGraceTicks {  // default: 3
                emit(EventOrphanKill, name, ticks)
                sp.Stop(name)
                clearOrphanTickCount(name)
            }
        } else {
            clearOrphanTickCount(name)
        }
    }

    emit(MetricReconcileDuration, clock.Since(tickStart))
}

// healExpiredTimers clears expired held_until and quarantined_until.
// Separate from wakeReasons() to keep that function pure.
func healExpiredTimers(session bead) {
    if h := session.Metadata["held_until"]; h != "" {
        if t, _ := time.Parse(time.RFC3339, h); !t.IsZero() && clock.Now().After(t) {
            // Clear hold AND stale sleep_reason so CLI/API don't show "user-hold"
            // after the hold has expired.
            batch := map[string]string{"held_until": ""}
            if session.Metadata["sleep_reason"] == "user-hold" {
                batch["sleep_reason"] = ""
            }
            session.SetMetadataBatch(batch)
        }
    }
    if q := session.Metadata["quarantined_until"]; q != "" {
        if t, _ := time.Parse(time.RFC3339, q); !t.IsZero() && clock.Now().After(t) {
            // Clear quarantine, reset attempts, AND stale sleep_reason.
            batch := map[string]string{
                "quarantined_until": "",
                "wake_attempts":     "0",
            }
            if session.Metadata["sleep_reason"] == "quarantine" {
                batch["sleep_reason"] = ""
            }
            session.SetMetadataBatch(batch)
        }
    }
}
// <!-- REVIEW: Round 2 fix — healExpiredTimers now clears stale sleep_reason -->

// healState updates advisory metadata only when changed (dirty check).
func healState(session bead, alive bool) {
    target := "asleep"
    if alive {
        target = "awake"
    }
    if session.Metadata["state"] != target {
        session.SetMetadata("state", target)
    }
}
```

<!-- REVIEW: updated per Blocker 1 — config_hash advances at drain start, not wake -->
<!-- REVIEW: updated per Blocker 2 — consecutive store failure tracking, RTO alert -->
<!-- REVIEW: updated per Blocker 3 — wall-clock tick budget -->
<!-- REVIEW: updated per Blocker 4 — adoption barrier gates orphan cleanup -->
<!-- REVIEW: updated per Blocker 5 — allDependenciesAlive checks draining -->

What disappears:
- Restart policy enum -> gone entirely
- Separate code paths for "initial start" vs "crash recovery" vs
  "config drift" -> all are just "should be awake but isn't"

What remains but moves into the session bead:
- Config fingerprint (for drift detection)
- Crash-loop tracking (wake_attempts, quarantine)
- User hold

What is ephemeral (in-memory only):
- Drain state (drain_started_at, drain_deadline, drain_reason)
- Orphan tick counters
- Pool desired count cache (per-tick snapshot)
- Last-known-good pool counts (persisted across ticks)

### Crash-loop dampening

When `wake()` fails or the woken process dies quickly, the reconciler
tracks consecutive failures:

```go
func recordWakeFailure(session bead) {
    attempts, _ := strconv.Atoi(session.Metadata["wake_attempts"])
    attempts++

    maxAttempts := configuredMaxRestarts(session)  // default: 5, from config via template
    if attempts >= maxAttempts {
        qUntil := clock.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339)
        session.SetMetadataBatch(map[string]string{
            "wake_attempts":     strconv.Itoa(attempts),
            "quarantined_until": qUntil,
            "sleep_reason":      "quarantine",
        })
        emit(EventSessionQuarantined, session.ID, session.Metadata["template"],
            attempts, qUntil)
    } else {
        session.SetMetadata("wake_attempts", strconv.Itoa(attempts))
    }
}

// Rapid-exit detection: if the session was woken within stabilityThreshold
// and is already dead, count as a failed wake (not a successful one).
// Returns true if a failure was recorded (caller should skip recordWakeFailure).
// Edge-triggered: clears last_woke_at after recording so the same crash
// is counted exactly once, not once per tick.
// Drain-aware: draining sessions died by request, not by crash.
func checkStability(session bead, alive bool) bool {
    if alive {
        return false
    }
    // Don't count intentional drains as crashes
    if isDraining(session) {
        return false
    }
    lastWoke := session.Metadata["last_woke_at"]
    if lastWoke == "" {
        return false
    }
    t, err := time.Parse(time.RFC3339, lastWoke)
    if err != nil {
        return false
    }
    if clock.Since(t) < stabilityThreshold {  // default: 30s
        recordWakeFailure(session)  // died too fast — count as crash
        // Clear last_woke_at so this crash is not re-counted next tick
        session.SetMetadata("last_woke_at", "")
        return true
    }
    return false
}

func clearWakeFailures(session bead) {
    session.SetMetadata("wake_attempts", "0")
    session.SetMetadata("quarantined_until", "")
}

// stableLongEnough returns true if the session has been alive past stabilityThreshold.
func stableLongEnough(session bead) bool {
    lastWoke := session.Metadata["last_woke_at"]
    if lastWoke == "" {
        return false
    }
    t, err := time.Parse(time.RFC3339, lastWoke)
    if err != nil {
        return false
    }
    return clock.Since(t) >= stabilityThreshold
}

func wakeAttempts(session bead) int {
    n, _ := strconv.Atoi(session.Metadata["wake_attempts"])
    return n
}

func isQuarantined(session bead) bool {
    q := session.Metadata["quarantined_until"]
    if q == "" { return false }
    t, err := time.Parse(time.RFC3339, q)
    if err != nil { return false }
    return clock.Now().Before(t)
}

// allDependenciesAlive checks that every session this one depends on
// has a live process AND is not draining. Uses tri-state probes.
// ProbeUnknown is treated as "not alive" (conservative — defer wake).
func allDependenciesAlive(ctx context.Context, session bead, cfg config.City, sp runtime.Provider) bool {
    deps := cfg.DependsOn[session.Metadata["template"]]
    for _, dep := range deps {
        depSessions := sessionBeadsForTemplate(dep)
        anyAlive := false
        for _, ds := range depSessions {
            probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
            result := sp.ProcessAlive(probeCtx, ds.Metadata["session_name"])
            cancel()
            if result == ProbeAlive && !isDraining(ds) {
                anyAlive = true
                break
            }
        }
        if !anyAlive {
            return false
        }
    }
    return true
}
// <!-- REVIEW: Round 4 fix — allDependenciesAlive uses tri-state probes -->

// isPoolExcess returns true if this session is a pool instance whose slot
// exceeds the current desired count (from the per-tick snapshot).
func isPoolExcess(session bead, cfg config.City, poolDesired map[string]int) bool {
    template := session.Metadata["template"]
    agent, ok := cfg.FindAgent(template)
    if !ok || agent.Pool == nil {
        return false
    }
    slot, _ := strconv.Atoi(session.Metadata["pool_slot"])
    desired := poolDesired[template]  // from per-tick snapshot
    return slot > 0 && slot > desired
}
```

<!-- REVIEW: updated per Blocker 5 — allDependenciesAlive returns false for draining dependencies -->

The reconciler calls `checkStability()` during the wake/sleep decision
phase for sessions that have `last_woke_at` set but are no longer alive.
A process that starts and dies within `stabilityThreshold` increments
the crash counter. Only processes that survive past the threshold get
their counter cleared.

Quarantined sessions show in `gc session list` as `asleep` with
`sleep_reason: "quarantine"`. The quarantine expires automatically;
`gc session wake` clears both quarantine AND user hold early.

<!-- REVIEW: updated per Blocker 7 — gc session wake clears quarantine too -->

### Concurrency architecture

<!-- REVIEW: added per Blocker 3 — full concurrency specification -->

The reconciler computes intent synchronously, but provider I/O and pool
evaluation execute in a bounded worker pool with context deadlines. This
prevents blocking provider calls from freezing the control plane.

```
┌─────────────────────┐
│  Reconciler Tick     │
│  (single goroutine)  │
│                      │
│  1. Compute intent   │──── pure: which sessions to wake/sleep/drain
│  2. Dispatch I/O     │──── fan out to worker pool
│  3. Collect results  │──── bounded wait with tick budget
│  4. Apply results    │──── update bead metadata
└─────────────────────┘
         │
         ├──── Worker 1: sp.Start(session-A)     [ctx timeout: 10s]
         ├──── Worker 2: sp.Stop(session-B)      [ctx timeout: 10s]
         └──── Worker 3: sp.Interrupt(session-C)  [ctx timeout: 5s]

Max concurrent provider calls per tick: 3 (configurable)
Wall-clock budget per tick: 5s (configurable) — defer remaining to next tick
```

**Invariants:**

1. **Intent computation is synchronous.** `wakeReasons()`, dependency
   ordering, and drain decisions run in the reconciler goroutine. No
   concurrent access to the session snapshot.

2. **Provider I/O is bounded.** `sp.Start()`, `sp.Stop()`, and
   `sp.Interrupt()` execute in a worker pool of size `maxProviderWorkers`
   (default: 3). Each call uses `context.WithTimeout` (default: 10s for
   Start/Stop, 5s for Interrupt). **Hot-path probes** (`ProcessAlive`,
   `IsAttached`, `GetLastActivity`, `ListRunning`) also use context
   deadlines (default: 2s). Probes are batched at tick start: a single
   `ListRunning` call provides carrier existence, then per-session
   `ProcessAlive` checks run with a 2s timeout. A timed-out probe
   treats the session as "unknown" — it is skipped for that tick (no
   wake, no sleep decision) and retried next tick.
   <!-- REVIEW: Round 2 fix — hot-path probes bounded with deadlines -->

3. **Read-only requests bypass the event loop.** `gc session list`,
   `gc session peek`, and `GET /v0/sessions` read the bead store snapshot
   directly. They do not queue through the reconciler. The snapshot is
   taken at tick start and is immutable for the tick's duration.

4. **Mutating requests are queued.** `gc session wake`, `gc session sleep`,
   `POST /v0/session/new`, and other mutations are queued and processed
   between reconciler phases. The queue is bounded (default: 100). Overflow
   returns `503 Service Unavailable`.

5. **Pool evaluation is async.** `evaluatePoolCached` dispatches pool check
   commands to the worker pool with a 5s timeout. Results are collected
   before Phase 2a begins.

6. **SetMetadataBatch failure semantics.** On failed commit, no visible
   state change occurs. The in-memory snapshot is NOT updated for a failed
   write. The reconciler continues with remaining sessions; the failed
   session retries on the next tick.

<!-- REVIEW: added per Blocker 2 — SetMetadataBatch failure semantics -->

7. Each session bead has a `generation` counter, incremented on each
   wake. Drain operations verify the generation hasn't changed before
   stopping a process.

8. `gc session new` creates the bead AND starts the process atomically
   within the event loop — preventing orphan cleanup races. For
   non-pool config-backed templates, if an open session bead already
   exists for that template, the command attaches to the existing
   session instead of creating a duplicate. Pool templates always
   create a new slot (up to `pool.max`).

9. `SetMetadataBatch(map[string]string)` coalesces per-bead metadata
   updates into a single atomic temp-file-rename. All mutation sites
   use batch writes for multi-field updates. `SetMetadata(k, v)` is
   shorthand for single-field updates. Target: at most 1 write per
   bead per phase, at most 3N+D total file writes per tick for N sessions
   and D completing drains.

10. **Session provenance is dynamic.** Whether a session is config-managed
    or provider-preset is determined by `cfg.FindAgent(template)` each tick,
    not by a stored flag. Adding an `[[agents]]` entry makes a matching
    session config-managed (gains `WakeConfig`, Phase 1a sync); removing
    it makes the session provider-preset (loses `WakeConfig`, always
    resumes). This is intentional: the unified model means config is the
    authority for behavior, and beads are the authority for persistence.
    On deconfiguration, stored `wake_mode` in bead metadata becomes inert:
    `wake()` defaults to `"resume"` for provider-preset sessions regardless
    of the stored value. The stale metadata is harmless and cleaned up if
    the session is later closed and a new bead created.

11. Session list is loaded ONCE per tick (snapshot model). The snapshot
    is taken AFTER Phase 1b completes, so newly created pool beads are
    visible to Phase 2a in the same tick. `createSessionBead` appends
    to the in-memory snapshot. All subsequent phases operate on this
    snapshot; metadata writes within a tick update the in-memory objects
    and are visible to later phases without disk re-reads.

### Dependency-ordered wake and sleep

When `depends_on` is configured, wake and sleep respect topological
ordering:

- **Wake (Phase 2a, forward topo order):** dependencies wake before
  their dependents. A must be awake before B wakes. "Awake" means
  `ProcessAlive` returns true AND not draining — not merely that
  `sp.Start()` returned. The `allDependenciesAlive()` gate defers
  dependent wakes to subsequent ticks until the dependency is confirmed
  alive and stable. Deferred sessions do not consume wake budget.
- **Sleep/drain (Phase 2b, reverse topo order):** dependents receive
  drain signals before their dependencies. The sleep pass iterates in
  reverse topological order so B's `beginDrain` is called before A's.
  **Note:** This is signal ordering, not exit ordering. Since drain is
  async, a dependency may exit before its dependent finishes graceful
  shutdown. Agents must tolerate dependency disappearance during drain
  (per ZFC — the prompt instructs graceful handling, not the framework).
  For strict exit ordering, use `depends_on` for wake ordering only and
  handle drain-time dependency loss in prompt templates.

<!-- REVIEW: updated per Blocker 5 — allDependenciesAlive checks draining status -->

**`depends_on` configuration example:**

<!-- REVIEW: added per Major 1 — concrete depends_on TOML example -->

```toml
[[agents]]
name = "database"
provider = "claude"
prompt_template = "prompts/database.md"

[[agents]]
name = "api-server"
provider = "claude"
prompt_template = "prompts/api-server.md"
depends_on = ["database"]

[[agents]]
name = "frontend"
provider = "claude"
prompt_template = "prompts/frontend.md"
depends_on = ["api-server"]
# Transitive: frontend depends on api-server depends on database.
# Wake order: database -> api-server -> frontend
# Drain order: frontend -> api-server -> database
```

**Validation rules for `depends_on`:**
1. All targets must reference existing `[[agents]]` entries (no dangling refs).
2. No cycles — validated via topological sort at config load time.
3. Self-references are rejected.
4. Pool templates can depend on non-pool templates (and vice versa).
   All instances of a pool template share the same dependency set.

**Cycle and dangling reference detection:** `depends_on` is validated at
config load time by `config.Validate()`. Cycles are rejected with a
clear error naming the cycle members. Dangling references (dependency
targets not in `[[agents]]`) are also rejected. If an invalid graph reaches the reconciler despite
validation (e.g., hot-reloaded config), it falls back to unordered
processing and emits a `ConfigValidationError` event.

```go
func topoOrder(sessions []bead, deps map[string][]string) []bead {
    // Returns sessions in dependency order (dependencies first).
    // For sleep/drain, callers use reverse(wakeOrder).
    // If a cycle is detected (should not happen — validated at config load,
    // but possible via hot-reload race), falls back to unordered processing
    // and emits ConfigValidationError. Does NOT panic.
    ...
}
```

If `depends_on` is not configured, sessions wake and sleep independently
(the common case for simple cities).

### Crash recovery via NDI

When the controller crashes and restarts, the normal reconciler loop
runs without special recovery logic. This works because:

1. `wakeReasons()` is pure and recomputes from ground truth (config +
   provider state + bead store).
2. `isAlive()` queries the provider directly — no advisory state needed.
3. Sessions that should be awake but aren't (process died during
   controller downtime) naturally match `reasons > 0 && !alive -> wake`.
4. Sessions that are running but shouldn't be (config changed during
   downtime) match `reasons == 0 && alive -> drain`.

In-progress drains are lost on crash (they are ephemeral in-memory
state). This is safe: a session with `drain` state will either have
exited (and gets evaluated normally) or still be running (and gets
re-drained if it has no wake reasons, or left alone if it does).

Wake operations on restart are subject to `maxWakesPerTick` (default: 5)
to prevent thundering herd.

### Config agent lifecycle

A config `[[agents]]` entry creates a session bead on first reconciler
tick if one doesn't exist. The bead persists across controller restarts.
The `[[agents]]` entry is a **standing wake reason**, not a session
definition.

**Adding an agent to city.toml:**
1. Reconciler sees agent template with no matching session bead
2. Creates session bead (state=asleep)
3. Next tick: wake reasons include `WakeConfig` -> wakes the session

**Removing an agent from city.toml:**
1. Session bead still exists (beads are persistent)
2. `WakeConfig` reason disappears
3. If no other wake reasons -> session drains and sleeps
4. Bead remains for history; auto-closed after grace period (default: 7d)
   if no wake reasons re-appear. `gc session close` for immediate cleanup.

<!-- REVIEW: updated per Major 1 — session retention policy with configurable grace period -->

**Config drift (hash advancement rules):**

Two hashes track config state:
- `config_hash` — hash of restart-requiring fields (provider, command,
  prompt_template). Changes require drain-then-restart.
- `live_hash` — hash of hot-reloadable fields (labels, env overrides).
  Changes are applied without drain.

Advancement timing:
1. Reconciler computes both hashes from current config each tick.
2. **`config_hash` drift:** Begin async drain. Advance `config_hash` in
   bead metadata immediately at drain start (not at wake). This prevents
   re-detecting the same drift on subsequent ticks. On wake, the session
   starts with the new config. Steady state: `needsRestart()` returns
   false because stored hash matches current config.
3. **`live_hash` drift:** Advance `live_hash` in bead metadata immediately.
   Apply hot-reloadable changes without drain (e.g., update env vars via
   `sp.SetEnv()` if supported, or just update metadata for next prompt
   render). No restart needed.
4. **`wake_mode` sync:** `wake_mode` is synced directly from config to
   bead metadata in Phase 1a (unconditionally, before liveness probes).
   It does not participate in `live_hash` or `config_hash` computation
   because it only affects wake behavior, not the running process.
   Changes take effect on the next wake — no drain needed.

<!-- REVIEW: updated per Blocker 1 + Round 2 — config_hash and live_hash advancement rules fully specified -->

### Template identity

<!-- REVIEW: added per Major 1 — qualified template identity -->

Template names are bare strings in single-rig cities (the common case).
When multiple rigs exist with potentially colliding template names,
templates are qualified by rig path:

```toml
# Single rig — bare names work:
[[agents]]
name = "worker"

# Multi-rig — qualified names resolve ambiguity:
[[agents]]
name = "frontend/worker"    # worker template from the "frontend" rig
```

The `template` field in session bead metadata always stores the qualified
name. `config.FindAgent()` accepts both bare and qualified names,
preferring exact match. If a bare name is ambiguous (matches templates in
multiple rigs), `config.Validate()` rejects the config with an error
listing the conflicting rigs. `wake_mode` is validated as a closed enum:
only `"resume"` and `"fresh"` are accepted; any other value (including
typos like `"freh"`) causes `config.Validate()` to fail with a clear
error message. An empty/omitted value defaults to `"resume"`.

### Waking a session

Waking materializes a runtime process from the config template (not
from stored metadata). This prevents command injection via bead store
mutation.

**Security invariant:** No value read from bead metadata or API input is
passed to process creation without validation against a defined format
or allowlist.

<!-- REVIEW: updated per Blocker 1 — two-phase wake with durable incarnation token -->
<!-- REVIEW: updated per Blocker 6 — ExecSpec, instance_token, immutable fields -->

```go
// ExecSpec defines a validated command for process creation.
// Command is NEVER a shell string — always structured argv.
type ExecSpec struct {
    Path    string            // absolute path to executable
    Args    []string          // arguments (no shell interpolation)
    Env     map[string]string // environment variables
    WorkDir string            // validated working directory
}

func wake(session bead, cfg config.City, sp runtime.Provider) error {
    name := session.Metadata["session_name"]
    if !sessionNamePattern.MatchString(name) {
        return fmt.Errorf("invalid session_name %q", name)
    }
    template := session.Metadata["template"]

    // === Phase 1: Persist new incarnation BEFORE starting process ===
    gen, _ := strconv.Atoi(session.Metadata["generation"])
    newGen := gen + 1
    token := generateInstanceToken()  // crypto/rand nonce

    if err := session.SetMetadataBatch(map[string]string{
        "generation":     strconv.Itoa(newGen),
        "instance_token": token,
        "last_woke_at":   clock.Now().UTC().Format(time.RFC3339),
        "sleep_reason":   "",
    }); err != nil {
        // Metadata commit failed — do NOT start the process.
        // Next tick will retry.
        return fmt.Errorf("pre-wake metadata commit: %w", err)
    }

    // === Phase 2: Resolve wake_mode and build command ===
    // Resolve wake_mode ONCE from bead metadata (Phase 1a already synced
    // config -> metadata). Provider-preset sessions default to "resume".
    // This single resolved value is used for BOTH ExecSpec construction
    // AND post-start key cleanup — one authority, no divergence.
    wakeMode := "resume"
    if _, ok := cfg.FindAgent(template); ok {
        wakeMode = session.Metadata["wake_mode"] // Phase 1a already synced from config
        if wakeMode == "" { wakeMode = "resume" }
    }

    var spec ExecSpec
    if agent, ok := cfg.FindAgent(template); ok {
        spec = buildExecSpecFromConfig(agent, session, wakeMode)
    } else {
        // Provider-preset session: always resume (wakeMode is "resume" above).
        preset, err := providers.Lookup(session.Metadata["provider"])
        if err != nil {
            return fmt.Errorf("unknown provider preset %q: %w",
                session.Metadata["provider"], err)
        }
        spec = preset.BuildExecSpec(session)
    }

    // Validate work_dir: must be canonical, must exist, must be a directory.
    if err := validateWorkDir(spec.WorkDir); err != nil {
        return fmt.Errorf("invalid work_dir %q: %w", spec.WorkDir, err)
    }

    // Inject instance token into carrier env for runtime binding verification
    spec.Env["GC_INSTANCE_TOKEN"] = token

    // === Phase 3: Start the process ===
    rtCfg := runtime.Config{
        ExecSpec: spec,
    }

    ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()
    if err := sp.Start(ctx, name, rtCfg); err != nil {
        // Start failed — process is NOT running.
        // The pre-committed generation is "unregistered": no process matches it.
        // Next tick: isAlive() returns false, session has wake reasons,
        // checkStability() sees last_woke_at is recent -> counts as crash.
        // This self-heals without special recovery logic.
        return err
    }

    // === Phase 4: Post-start confirmation ===
    // Verify the process actually appeared with our instance token.
    // If verification fails, the process is "unregistered" — next tick
    // will see it as an orphan (no matching token) or reconcile it.
    confirmCtx, confirmCancel := context.WithTimeout(context.Background(), 2*time.Second)
    defer confirmCancel()
    probeResult := sp.ProcessAlive(confirmCtx, name)
    if probeResult == ProbeDead {
        // Process vanished immediately — treat as wake failure
        return fmt.Errorf("process not alive after start")
    }
    // ProbeUnknown is OK here — we just started it, can't confirm yet.
    // The next reconciler tick will check via normal liveness path.
    // <!-- REVIEW: Round 5 fix — wake() post-start uses tri-state probe -->

    // Fresh mode does NOT delete session key files. The key is simply
    // ignored by buildExecSpecFromConfig() when wakeMode is "fresh".
    // Preserving keys is important: if config changes from fresh→resume,
    // the existing key enables seamless resume without data loss. Stale
    // key files are harmless (small, on-disk only, cleaned up on close).

    return nil
}

// generateInstanceToken returns a cryptographically random nonce.
func generateInstanceToken() string {
    b := make([]byte, 16)
    _, _ = rand.Read(b)
    return hex.EncodeToString(b)
}

// readSessionKey reads the session key from the secrets directory.
func readSessionKey(sessionID string) (string, error) {
    path := filepath.Join(cityDir, ".gc", "secrets", sessionID+".key")
    data, err := os.ReadFile(path)
    if err != nil {
        return "", err
    }
    return strings.TrimSpace(string(data)), nil
}

// sessionNamePattern validates session names: starts with letter/digit, then alphanum/dash/underscore.
var sessionNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

// validateWorkDir ensures the path is safe to use as a working directory.
func validateWorkDir(dir string) error {
    abs, err := filepath.Abs(dir)
    if err != nil {
        return err
    }
    if abs != filepath.Clean(abs) {
        return fmt.Errorf("non-canonical path")
    }
    info, err := os.Stat(abs)
    if err != nil {
        return err
    }
    if !info.IsDir() {
        return fmt.Errorf("not a directory")
    }
    return nil
}

// buildEnv constructs environment variables from validated config fields.
// No arbitrary metadata flows into env — only explicit, known keys.
// Emits BOTH new GC_SESSION_* vars and legacy GC_AGENT/GC_CITY/GC_DIR
// vars during the migration transition period.
func buildEnv(session bead) map[string]string {
    env := map[string]string{
        // New canonical vars
        "GC_SESSION_ID":   session.ID,
        "GC_SESSION_NAME": session.Metadata["session_name"],
        "GC_TEMPLATE":     session.Metadata["template"],
        // Legacy compat vars (emitted during transition, removed in Phase 5)
        "GC_AGENT":        session.Metadata["template"],
        "GC_CITY":         cityDir,
        "GC_DIR":          session.Metadata["work_dir"],
    }
    return env
}

func buildExecSpecFromConfig(agent config.Agent, session bead, wakeMode string) ExecSpec {
    env := buildEnv(session)
    workDir := session.Metadata["work_dir"]
    if workDir == "" {
        workDir = cityDir
    }

    // wakeMode is resolved once by wake() and passed in — no re-resolution here.

    // Fresh mode: always start clean — no session key lookup
    if wakeMode == "fresh" {
        return BuildStartExecSpec(agent, env, workDir)
    }

    // Resume mode: resume if provider supports it and session has a key
    if key, err := readSessionKey(session.ID); err == nil && key != "" {
        return BuildResumeExecSpec(agent, key, env, workDir)
    }
    return BuildStartExecSpec(agent, env, workDir)
}
```

<!-- REVIEW: added per Blocker 4 — buildEnv emits legacy env vars -->
<!-- REVIEW: added per Blocker 6 — ExecSpec replaces string Command -->

**Two-phase wake protocol:** The generation and instance token are
persisted BEFORE the process starts (Phase 1). If `sp.Start()` fails
(Phase 3), the pre-committed generation creates a "claimed but
unoccupied" slot. The next tick sees `last_woke_at` is recent and the
process is dead, triggering `checkStability()` which counts it as a
crash. No special recovery path is needed — the existing crash-loop
dampening handles it.

If the post-start confirmation write fails (Phase 4 detects process
vanished), the same self-healing applies. If the Phase 1 metadata
commit itself fails, the process is never started — the next tick
retries from scratch.

<!-- REVIEW: added per Blocker 1 — repair/abort path documentation -->

**Command construction invariant:** `BuildResumeExecSpec` and
`BuildStartExecSpec` return `ExecSpec` with discrete `Path` and `Args`.
Arguments are passed as array elements, never concatenated into a shell
string. The provider's `Start` method executes via `exec.Command(spec.Path, spec.Args...)`,
not `sh -c`.

### Runtime targeting and execution guarantees

<!-- REVIEW: added per Blocker 6 — runtime targeting section -->

**Authenticated runtime binding:** On wake, the controller generates a
cryptographically random `instance_token` and writes it to both the bead
metadata and the carrier environment (`GC_INSTANCE_TOKEN`). Subsequent
provider operations (Stop, Interrupt) verify the token matches before
acting. This prevents stale drain operations from targeting a re-woken
session.

```go
func verifiedStop(session bead, sp runtime.Provider) error {
    name := session.Metadata["session_name"]
    expectedToken := session.Metadata["instance_token"]
    actualToken := sp.GetEnv(name, "GC_INSTANCE_TOKEN")
    if expectedToken != "" && actualToken != expectedToken {
        // Stale reference — the process was replaced since we last knew it.
        // Do NOT include token values in error (they are binding material).
        return fmt.Errorf("instance token mismatch for session %s", session.ID)
    }
    return sp.Stop(name)
}

// verifiedInterrupt sends an interrupt signal after verifying instance_token.
// Used by beginDrain to prevent signaling a re-woken process.
func verifiedInterrupt(session bead, sp runtime.Provider) error {
    name := session.Metadata["session_name"]
    expectedToken := session.Metadata["instance_token"]
    actualToken := sp.GetEnv(name, "GC_INSTANCE_TOKEN")
    if expectedToken != "" && actualToken != expectedToken {
        return fmt.Errorf("instance token mismatch for session %s", session.ID)
    }
    return sp.Interrupt(name)
}
// <!-- REVIEW: Round 2 fix — verifiedInterrupt added, token values redacted from errors -->
```

**Immutable controller-owned fields:** The following metadata fields are
set at session creation time and are immutable thereafter. `PATCH /v0/session/{id}`
and API writes that attempt to modify these fields are rejected with 403:
- `template`
- `provider`
- `pool_slot`
- `session_name`

**Session key storage:** Session keys (provider resume tokens) are stored
in `.gc/secrets/<session-id>.key` with `0600` permissions. They are:
- Read only by the controller at wake time
- Never written to bead metadata
- Redacted from API responses (replaced with `"[redacted]"` if present)
- Redacted from event payloads

**File permissions:**
- `.gc/` directory: `0700`
- `.gc/secrets/` directory: `0700`
- `.gc/secrets/*.key` files: `0600`
- Bead store files: `0600`
- Controller socket: `0600`

### Sleeping a session (async drain)

Drain is **asynchronous** — it sets a signal and returns immediately.
The reconciler advances drains on subsequent ticks. This prevents
blocking the serial event loop.

**Drain state is ephemeral** (in-memory map, not bead metadata). If the
controller crashes mid-drain, the state is lost. This is safe: a
half-drained session will either have exited (normal) or still be
running (re-evaluated by reconciler on restart).

**Drains are cancelable:** If wake reasons reappear for a session that
is draining (same generation), the drain is canceled and the session
remains awake. This prevents unnecessary restarts when transient
conditions (e.g., brief pool scale-down) reverse before the drain
completes.

<!-- REVIEW: added per Blocker 5 — cancelable drains -->

```go
// In-memory drain tracking (not persisted in beads)
type drainState struct {
    startedAt time.Time
    deadline  time.Time
    reason    string       // "config-drift", "idle", "scale-down", "user-sleep"
    generation int         // generation at drain start — stale check
}
var activeDrains = map[string]*drainState{}  // keyed by session ID

// beginDrain initiates an async drain. Returns immediately.
func beginDrain(session bead, sp runtime.Provider, reason string) {
    if isDraining(session) {
        return  // already draining
    }
    name := session.Metadata["session_name"]
    gen, _ := strconv.Atoi(session.Metadata["generation"])

    activeDrains[session.ID] = &drainState{
        startedAt:  clock.Now(),
        deadline:   clock.Now().Add(drainTimeout),  // default: 30s
        reason:     reason,
        generation: gen,
    }

    // Best-effort drain signal: provider-specific interrupt.
    // For tmux: send-keys C-c. For subprocess: SIGINT to process group.
    // Agents check for interrupt in their prompt loop.
    // Verify instance_token before signaling to prevent targeting wrong process.
    if err := verifiedInterrupt(session, sp); err != nil {
        emit(EventDrainInterruptFailed, session.ID, err)
    }
}

// cancelDrain removes a drain if wake reasons reappeared for the same generation.
func cancelDrain(session bead) {
    if ds, ok := activeDrains[session.ID]; ok {
        gen, _ := strconv.Atoi(session.Metadata["generation"])
        if gen == ds.generation {
            emit(EventDrainCanceled, session.ID, ds.reason)
            delete(activeDrains, session.ID)
        }
    }
}

// advanceDrains checks all in-progress drains. Called once per tick.
func advanceDrains(sp runtime.Provider) {
    for id, ds := range activeDrains {
        session := loadSessionBead(id)
        if session == nil {
            delete(activeDrains, id)
            continue
        }

        // Stale check: if session was re-woken (generation changed), cancel drain
        gen, _ := strconv.Atoi(session.Metadata["generation"])
        if gen != ds.generation {
            delete(activeDrains, id)
            continue
        }

        // Cancelation check: if wake reasons reappeared, cancel drain
        // (config-drift drains are NOT cancelable — the config changed)
        if ds.reason != "config-drift" {
            reasons := wakeReasons(session, cfg, sp, poolDesired)
            if len(reasons) > 0 {
                emit(EventDrainCanceled, session.ID, ds.reason)
                delete(activeDrains, id)
                continue
            }
        }

        name := session.Metadata["session_name"]

        // Tri-state probe: ProbeUnknown → skip, check again next tick
        probeCtx, probeCancel := context.WithTimeout(context.Background(), 2*time.Second)
        probeResult := sp.ProcessAlive(probeCtx, name)
        probeCancel()
        if probeResult == ProbeUnknown {
            continue  // can't determine — retry next tick
        }

        if probeResult == ProbeDead {
            // Process exited — drain complete
            session.SetMetadataBatch(map[string]string{
                "slept_at":     clock.Now().UTC().Format(time.RFC3339),
                "sleep_reason": ds.reason,
                "state":        "asleep",
            })
            emit(EventSessionSlept, session.ID, session.Metadata["template"],
                ds.reason, ds.reason)
            delete(activeDrains, id)
            continue
        }

        if clock.Now().After(ds.deadline) {
            // Drain timed out — force stop (token-verified)
            emit(EventDrainTimeout, session.ID, session.Metadata["template"],
                ds.reason, clock.Since(ds.startedAt))
            if err := verifiedStop(session, sp); err != nil {
                emit(EventDrainStopFailed, session.ID, err)
            }
            session.SetMetadataBatch(map[string]string{
                "slept_at":     clock.Now().UTC().Format(time.RFC3339),
                "sleep_reason": ds.reason,
                "state":        "asleep",
            })
            emit(EventSessionSlept, session.ID, session.Metadata["template"],
                ds.reason, ds.reason)
            delete(activeDrains, id)
        }
        // Else: still draining, check again next tick
    }
}

func isDraining(session bead) bool {
    _, ok := activeDrains[session.ID]
    return ok
}
```

**Drain signaling contract:** The primary drain signal is provider-specific:
for tmux, `send-keys C-c`; for subprocess, `SIGINT` to the process group.
Agent prompt templates should include instructions to handle interrupt
gracefully (finish current work, save state). This is a prompt-level
concern (per ZFC), not a framework mechanism.

<!-- REVIEW: updated per Major 2 — provider-specific interrupt details -->

### SIGINT handling contract

<!-- REVIEW: added per Major 1 — SIGINT handling contract -->

Agent prompt templates SHOULD include an interrupt handling stanza. The
recommended pattern:

```markdown
## Interrupt Handling

When you receive a SIGINT (Ctrl-C):
1. Finish the current atomic unit of work (do not leave partial state).
2. Save your progress to the hook (update the bead with current status).
3. Exit cleanly.

Do NOT abort mid-operation. Do NOT ignore the signal indefinitely.
```

`gc config validate` emits a warning for templates that do not contain
the string "interrupt" or "SIGINT" (case-insensitive). This is advisory,
not blocking — templates without interrupt guidance still pass validation.

### Pool integration

Pool instances become session beads with `pool_slot` metadata. Pool
desiredness is **purely computed** from `slot <= desired` — no stored
hold or suppression flag is needed.

```go
// evaluatePoolCached runs the pool check command ONCE per template per tick.
// Results are cached in poolDesired map to ensure consistency across
// reconcilePool and all wakeReasons calls within the same tick.
// Timeout: 5 seconds. On timeout or error, falls back to last-known-good
// desired count. Only falls back to pool.min if no previous count exists.
func evaluatePoolCached(template config.Agent, cache map[string]int) int {
    if d, ok := cache[template.Name]; ok {
        return d  // already evaluated this tick
    }
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    d, err := runPoolCheck(ctx, template.Pool.Check)
    if err != nil {
        emit(EventPoolCheckError, template.Name, err)
        // Fallback: use last-known-good desired count if not expired.
        // Only fall back to pool.min if no previous count or entry expired.
        if entry, ok := lastKnownPoolDesired[template.Name]; ok {
            if clock.Since(entry.updatedAt) < lastKnownPoolExpiry {
                return entry.count
            }
        }
        return template.Pool.Min
    }
    // Clamp to configured bounds
    if d < template.Pool.Min { d = template.Pool.Min }
    if d > template.Pool.Max { d = template.Pool.Max }
    // Update last-known-good with timestamp
    lastKnownPoolDesired[template.Name] = poolDesiredEntry{count: d, updatedAt: clock.Now()}
    return d
}

// lastKnownPoolDesired tracks successful pool evaluations across ticks.
// Persisted in-memory only (lost on controller restart, falls back to min).
// Expiry: entries older than lastKnownPoolExpiry are treated as stale
// and fall back to pool.min (prevents indefinitely stale counts).
var lastKnownPoolDesired = map[string]poolDesiredEntry{}

type poolDesiredEntry struct {
    count     int
    updatedAt time.Time
}
const lastKnownPoolExpiry = 5 * time.Minute  // stale after 5 minutes
// <!-- REVIEW: Round 2 fix — lastKnownPoolDesired has expiry policy -->

// poolScaleDownHysteresis tracks consecutive ticks at a new lower desired count.
// Scale-down only executes after 2 consecutive ticks at the lower value.
var poolScaleDownHysteresis = map[string]int{}  // template -> consecutive ticks at lower count

// lastAppliedDesired tracks the desired count that was ACTUALLY APPLIED
// (after hysteresis) on the previous tick. This is distinct from
// lastKnownPoolDesired, which tracks the raw evaluation result.
// Hysteresis compares against lastAppliedDesired to detect scale-down.
var lastAppliedDesired = map[string]int{}  // template -> last applied count
// <!-- REVIEW: Round 3 fix — separate lastAppliedDesired from lastKnownPoolDesired for hysteresis -->

// reconcilePool returns the post-hysteresis desired count (the applied value).
// Callers write this back to poolDesired for use by wakeReasons/isPoolExcess.
func reconcilePool(template config.Agent, desired int, cfg config.City, sp runtime.Provider) int {
    current := sessionBeadsForTemplate(template.Name)  // excludes status=closed
    // Sort by pool_slot for deterministic scaling
    sort.Slice(current, func(i, j int) bool {
        si, _ := strconv.Atoi(current[i].Metadata["pool_slot"])
        sj, _ := strconv.Atoi(current[j].Metadata["pool_slot"])
        return si < sj
    })

    // Hysteresis: require 2 consecutive ticks at a lower desired before scaling down.
    // Compare against lastAppliedDesired (what we actually used last tick),
    // NOT lastKnownPoolDesired (the raw evaluation result). This prevents
    // the update-then-read ordering bug where evaluatePoolCached updates
    // lastKnownPoolDesired before reconcilePool reads it.
    prevApplied := template.Pool.Min
    if prev, ok := lastAppliedDesired[template.Name]; ok {
        prevApplied = prev
    }
    if desired < prevApplied {
        poolScaleDownHysteresis[template.Name]++
        if poolScaleDownHysteresis[template.Name] < 2 {
            desired = prevApplied  // hold at previous level this tick
        }
    } else {
        poolScaleDownHysteresis[template.Name] = 0  // reset on equal or increase
    }

    // Reconcile by slot set (1..desired), not count.
    // Fill gaps from closed instances.
    existingSlots := map[int]bool{}
    for _, s := range current {
        slot, _ := strconv.Atoi(s.Metadata["pool_slot"])
        existingSlots[slot] = true
    }

    for slot := 1; slot <= desired; slot++ {
        if !existingSlots[slot] {
            // Gap or new slot — create a bead
            name := fmt.Sprintf("%s-%d", template.Name, slot)
            bead := createSessionBead(name, template)
            bead.SetMetadata("pool_slot", strconv.Itoa(slot))
            bead.SetMetadata("pool_template", template.Name)
            bead.SetMetadata("wake_mode", template.WakeMode)
        }
    }

    // Scale-down: excess instances (slot > desired) lose WakeConfig
    // on the next wakeReasons() call. No hold needed — the computed
    // check `slot > 0 && slot <= desired` returns false for excess slots.
    // The sleep pass drains excess instances immediately (reason: "pool-excess"),
    // bypassing idle timeout. If the excess instance has hooked work,
    // it stays awake via WakeWork until the work completes.

    // Record the APPLIED desired count for next tick's hysteresis comparison.
    lastAppliedDesired[template.Name] = desired
    return desired  // post-hysteresis value for poolDesired map
}

// sessionBeadsForTemplate returns open session beads for the given template.
// Excludes beads with status=closed.
func sessionBeadsForTemplate(template string) []bead {
    return store.ListByLabel("template:"+template,
        beads.FilterType("session"),
        beads.FilterStatusNot("closed"))
}
```

<!-- REVIEW: updated per Blocker 5 — last-known-good fallback, hysteresis for scale-down -->

**Scale-down behavior:** When `desired` decreases, the pool evaluator
applies hysteresis: 2 consecutive ticks at the lower desired count are
required before acting. This prevents flapping from transient check
results. Once stable, excess instances lose their `WakeConfig` reason
because `slot > desired`. The sleep pass drains them immediately with
reason `"pool-excess"` (bypassing idle timeout). **Phase 4 interaction:**
Once `WakeWork` ships, excess instances with hooked work stay awake via
`WakeWork` until the work completes — GUPP is preserved. Before Phase 4,
the `sling` handler emits `EventWorkSlungToBlockedSession{reason: "pool-suppressed"}`
for operator-driven reassignment.

**Scale-up behavior:** When `desired` increases, previously excess
instances regain `WakeConfig` because `slot <= desired` becomes true
again. No hold to clear — the pure computation handles it.

**User override:** `gc session wake worker-3` only affects user holds
and quarantine. It cannot override pool desiredness — if `worker-3` is
slot 3 and desired is 2, waking it clears any hold/quarantine but
`WakeConfig` still evaluates to false. The session stays asleep unless
it has other wake reasons.

### Work dispatch interaction

When work is slung to a session that cannot wake (held, quarantined, or
pool-suppressed), the sling **rejects** the operation with a typed error.
This prevents silent work stranding during Phase 2-3 (before WakeWork).

<!-- REVIEW: updated per Blocker 5 — sling rejects (not just emits event) for blocked sessions -->

```go
func sling(sessionID string, workBead bead, cfg config.City) error {
    session := loadSessionBead(sessionID)
    if session == nil {
        return fmt.Errorf("session %q not found", sessionID)
    }

    // Gate: reject dispatch to sessions that cannot wake (unless WakeWork is implemented)
    blockReason := ""
    if h := session.Metadata["held_until"]; h != "" {
        if t, err := time.Parse(time.RFC3339, h); err == nil && clock.Now().Before(t) {
            blockReason = "user-hold"
        }
    }
    if q := session.Metadata["quarantined_until"]; q != "" {
        if t, err := time.Parse(time.RFC3339, q); err == nil && clock.Now().Before(t) {
            blockReason = "quarantine"
        }
    }
    // Pool suppression: check if session's slot exceeds desired count.
    // Uses lastAppliedDesired (the post-hysteresis authoritative count)
    // to ensure dispatch admission agrees with reconciler wake/sleep decisions.
    template := session.Metadata["template"]
    if agent, ok := cfg.FindAgent(template); ok && agent.Pool != nil {
        slot, _ := strconv.Atoi(session.Metadata["pool_slot"])
        desired := agent.Pool.Min  // fallback if no applied count exists
        if applied, ok := lastAppliedDesired[template]; ok {
            desired = applied
        }
        if slot > 0 && slot > desired {
            blockReason = "pool-suppressed"
        }
    }
    // <!-- REVIEW: Round 5 fix — sling uses lastAppliedDesired (post-hysteresis) -->

    if blockReason != "" {
        // Phase 2-3: reject. Phase 4+: WakeWork will handle this — remove gate.
        emit(EventWorkSlungToBlockedSession, sessionID, workBead.ID, blockReason)
        return fmt.Errorf("session %q cannot wake: %s (use --force to override)", sessionID, blockReason)
    }

    if err := hookBead(workBead, sessionID); err != nil {
        return err
    }
    return nil
}
```

**Phase gap:** `WakeWork` (automatic wake on hooked work) is deferred to
Phase 4. During Phases 2-3, sessions that cannot wake reject slung work
with a typed error and structured event. **Important interaction:** pool
instances scaled down (losing `WakeConfig`) will reject work until
Phase 4 ships `WakeWork`. Current dispatch behavior (targeting only
config agents, which always have `WakeConfig`) is unaffected. The
regression only affects:
(a) interactive sessions that are asleep and receive slung work, and
(b) pool instances with hooked work that are scaled down.
Neither flow exists today.

### Bead store requirements

<!-- REVIEW: added per Blocker 2 — bead store requirements section -->

The bead store is the authoritative control plane for session lifecycle.
The current `FileStore` implementation rewrites the entire database on
each write, which is insufficient for Phase 2. The following requirements
MUST be met before Phase 2 ships:

**Record-level corruption isolation:** Malformed records must be skipped
(with a warning event), not cause the entire store to fail. A corrupt
bead for session gc-42 must not prevent reading session gc-1.

**Atomic writes:** All writes use temp-file-then-rename. `fsync` the temp
file before rename. Handle `ENOSPC` by aborting the write and emitting
`EventBeadStoreWriteError{error: "ENOSPC"}`. The bead store must never
leave a half-written file as the active store.

**Record-level operations:** The store MUST support appending or updating
individual records without rewriting the entire database. This is
required for write-budget scalability. **Chosen implementation:**
append-only JSONL with periodic compaction. Each write appends a
record; reads merge records by bead ID (last-writer-wins). Compaction
rewrites the file periodically (e.g., every 100 appends or on startup).
This is the simplest approach that meets all requirements and is
consistent with the existing event log format.
<!-- REVIEW: Round 2 fix — authoritative store backend chosen (append-only JSONL) -->

**Degraded-mode policy:** When the bead store is unavailable:
- Running processes continue undisturbed (no stop, no drain).
- No new wakes, no new sleeps, no orphan cleanup.
- The reconciler aborts the tick immediately.
- After 3 consecutive failed ticks, emit `EventBeadStoreCritical` for
  operator alerting.
- **RTO target:** When the store recovers, the next tick resumes normal
  operation with no special recovery logic (NDI handles convergence).

**SetMetadataBatch failure semantics:** A failed batch commit produces
no visible state change. The in-memory snapshot retains the pre-write
values. No partial writes are visible to subsequent phases in the same
tick. Cleanup rules: the failed temp file is removed; the original file
is untouched.

### Migration protocol

Upgrading from the pre-unified reconciler to session beads requires a
rerunnable adoption barrier.

<!-- REVIEW: updated per Blocker 4 — rerunnable adoption barrier replaces sticky migration_complete -->

**Adoption barrier (EVERY startup, not just first):**

On every controller startup, before the reconciler enters its normal
loop, the adoption barrier runs. This replaces the previous sticky
`migration_complete` flag with a rerunnable check.

1. List all running runtime sessions via `sp.ListRunning(prefix)`.
2. For each running session, check if an **open** session bead already
   exists by matching on `session_name` metadata against the running
   process name. This is the per-instance dedup key. **Closed beads are
   excluded from the dedup query** — a closed bead with the same
   `session_name` does not prevent re-adoption. This handles the
   rollback/re-upgrade case where a session was adopted, closed, and
   then a new process starts with the same name.
   <!-- REVIEW: Round 2 fix — adoption dedup excludes closed beads -->
3. If no **open** bead exists, **adopt permissively**: create a bead for it.
   - Set `session_name` to the legacy `SessionNameFor()` result.
   - Set `state = "awake"`, `generation = "1"`.
   - Set `config_hash` and `live_hash` from the current config.
   - Set `wake_mode` from the current config (defaults to `"resume"`).
   - **For pool instances:** derive `pool_slot` from the legacy name
     suffix (e.g., `s-worker-3` -> slot 3). Validate that the parsed
     slot is a positive integer within `[1, pool.Max]`. **If parsing
     fails or slot is out of bounds:** adopt anyway with the parsed slot
     (adopt-then-drain, not skip-then-orphan-kill). Emit
     `MigrationSlotOutOfBounds {name, slot, max}`. The reconciler will
     drain the out-of-bounds instance on the next tick if it has no
     wake reasons.
   - **Session name validation:** Validate the legacy name against
     `sessionNamePattern` before creating the bead. Skip with a
     warning event if validation fails.
4. **Barrier gate:** Verify that ALL running sessions now have matching
   beads. If any running session still lacks a bead (e.g., name
   validation failure), orphan cleanup remains disabled for this tick.
   The barrier is re-evaluated every startup.
5. Once the barrier passes (all running sessions have beads), orphan
   cleanup is enabled for the remainder of this controller lifetime.

**Worked example -- pool of 5 workers:**
```
Running tmux sessions: s-worker-1, s-worker-2, s-worker-3, s-worker-4, s-worker-5
Step 2: Query beads for session_name=s-worker-1 -> not found
Step 3: Create bead {session_name: "s-worker-1", pool_slot: "1", pool_template: "worker", ...}
Step 2: Query beads for session_name=s-worker-2 -> not found
Step 3: Create bead {session_name: "s-worker-2", pool_slot: "2", ...}
... (repeat for 3, 4, 5)
Step 4: All 5 running sessions have beads -> barrier passes
Result: 5 distinct beads, one per instance, correct slot assignments.
```

**Out-of-bounds pool slot example:**
```
Running tmux sessions: s-worker-1, ..., s-worker-7  (but pool.Max = 5)
Step 3: Adopt s-worker-7 with pool_slot: "7" (out of bounds)
        Emit MigrationSlotOutOfBounds {name: "s-worker-7", slot: 7, max: 5}
Step 4: All running sessions have beads -> barrier passes
Next tick: wakeReasons(worker-7) -> slot 7 > desired -> no WakeConfig -> drain
```

**Interactive sessions:** Pre-existing interactive session beads (from
the old session manager) are identified by `type=session` in the bead
store. They are not re-adopted — they already have beads. The migration
only creates beads for running processes that lack them.

**Legacy environment variable compatibility:** During the transition
period, `buildEnv()` emits both new `GC_SESSION_*` variables and legacy
`GC_AGENT`, `GC_CITY`, `GC_DIR` variables. This preserves compatibility
with existing prompt templates and agent scripts that reference legacy
env vars. Legacy vars are removed in Phase 5.

<!-- REVIEW: added per Blocker 4 — legacy env var compat -->

**Legacy session key migration:** Pre-existing interactive session beads
may store resume tokens in bead metadata. On first wake after upgrade,
the controller checks for a metadata-stored session key, writes it to
`.gc/secrets/<session-id>.key` with `0600` permissions, then removes
the metadata copy. This is a one-time migration — subsequent wakes use
the secrets file exclusively.

**Crash safety:** If the controller crashes mid-adoption, the next
restart re-runs the adoption barrier. The per-instance dedup key query
prevents duplicate beads. The barrier is inherently rerunnable.

**Rollback:** Reverting to the pre-unified binary is safe. The old
reconciler ignores session beads (different data path). Running
processes continue. Sleeping sessions become invisible to the old
reconciler but are not harmed. The legacy env vars (`GC_AGENT`, etc.)
ensure running processes still have the environment they expect.

**Rollback + re-upgrade:** If the old reconciler creates new tmux
sessions for config agents during rollback, re-upgrading runs the
adoption barrier again. The per-instance dedup key (matching on
`session_name`) prevents duplicates.

**Dry-run command:**

```bash
$ gc migration plan
Would adopt 5 running sessions:
  s-worker-1  -> create bead {template: "worker", pool_slot: 1}
  s-worker-2  -> create bead {template: "worker", pool_slot: 2}
  s-worker-3  -> create bead {template: "worker", pool_slot: 3}
  s-overseer  -> create bead {template: "overseer"}
  s-helper-1  -> create bead {template: "helper"}

Already have beads: 0
Orphan cleanup: would be enabled after adoption

No changes made. Run `gc up` to execute.
```

<!-- REVIEW: added per Blocker 4 — gc migration plan dry-run command -->

### Package changes

| Current | After | Notes |
|---|---|---|
| `internal/session/` (manager.go) | `internal/session/` | Expanded: becomes the universal session manager |
| `internal/runtime/` (Provider) | `internal/runtime/` | Updated: `Config.Command` becomes `Config.ExecSpec` |
| `internal/agent/` (Handle, Agent) | Removed | Absorbed into session manager |
| `cmd/gc/reconcile.go` | Simplified | Wake/sleep loop replaces state machine |
| `cmd/gc/build_agent.go` | `cmd/gc/build_session.go` | Config->session bead translation, ExecSpec construction |
| `cmd/gc/pool.go` | Simplified | Pool instances are session beads |
| `cmd/gc/api_state.go` | Simplified | Session beads are the state |
| `internal/api/handler_agents.go` | Merged into handler_sessions.go | One API surface |
| `cmd/gc/cmd_agent.go` | Merged into cmd_session.go | One CLI surface |
| `cmd/gc/cmd_migration.go` | New | `gc migration plan` dry-run command |

### API surface (unified)

```
GET    /v0/sessions                  # all sessions (config + interactive)
GET    /v0/sessions?wake=config      # only config-managed sessions
GET    /v0/sessions?wake=none        # only sleeping sessions
GET    /v0/sessions?template=worker  # filter by template
GET    /v0/session/{id}              # single session detail
POST   /v0/session/{id}/wake         # explicit wake (clears hold AND quarantine)
POST   /v0/session/{id}/sleep        # explicit sleep (sets user hold)
POST   /v0/session/{id}/close        # permanent end
POST   /v0/session/new               # create interactive session
PATCH  /v0/session/{id}              # rename only (title field)
GET    /v0/session/{id}/peek         # last N lines of output
```

<!-- REVIEW: updated per Blocker 7 — wake clears hold AND quarantine -->

**`POST /v0/session/{id}/wake` response:**

<!-- REVIEW: added per Blocker 7 — typed wake outcomes -->

Returns a typed outcome:
```json
{
    "outcome": "woken",
    "session_id": "gc-1",
    "wake_reasons": ["config"]
}
```

Possible outcomes:
- `"woken"` — session was asleep, now waking
- `"hold_cleared"` — user hold was removed, session will wake on next tick
- `"quarantine_cleared"` — quarantine was removed, session will wake on next tick
- `"already_awake"` — session was already awake, no action taken
- `"blocked:pool"` — pool desiredness prevents wake (slot > desired)
- `"blocked:dependency"` — dependency is not alive
- `"blocked:no_reason"` — no wake reasons exist (not a config agent, no work)

**`PATCH /v0/session/{id}`** accepts only: `title` (string, max 200
chars, no control characters). Rejects writes to `template`, `provider`,
`pool_slot`, `session_name`, and all other fields with 403.

<!-- REVIEW: updated per Blocker 6 — immutable fields listed -->

**`POST /v0/session/new`** accepted fields and validation:
- `template` (required): must match an existing `[[agents]]` entry or a
  registered provider preset. Validated against config.
- `title` (optional): string, max 200 chars, no control characters.
- `work_dir` (optional): validated by `validateWorkDir()` — must be
  canonical, must exist, must be a directory. Defaults to city root.
- `provider`, `command`, `session_key`, and all other fields: rejected.
  These are derived from the template at wake time.
- `wake_mode`: set from the matching `[[agents]]` config entry if one
  exists (inherits the config's `wake_mode`); defaults to `"resume"` for
  provider-preset sessions (those not matching any `[[agents]]` entry).
  Template-derived sessions that match a config entry behave as
  config-managed sessions: they receive `WakeConfig`, participate in
  Phase 1a sync, and inherit `wake_mode` from config.

The current `/v0/agents` endpoints become aliases during transition,
then are deprecated.

### CLI surface (unified)

```bash
gc session list [--state awake|asleep|closed|all] [--template NAME]
gc session new <template> [--title TITLE] [--no-attach]
gc session attach <id|name>         # supports name-based lookup
gc session peek <id|name> [--lines N]
gc session sleep <id|name> [--for DURATION]   # sets user hold
gc session wake <id|name>           # clears hold AND quarantine, wakes
gc session close <id|name>
gc session rename <id> <title>
gc session prune [--before 7d]
gc migration plan                   # dry-run adoption preview
```

<!-- REVIEW: updated per Blocker 7 — wake clears hold AND quarantine -->

**Backward-compatible aliases** (Phase 3, deprecated in Phase 5):

```bash
gc session list                # session list (replaces gc agent list)
gc session peek <name>         # session peek (replaces gc agent status)
gc session kill <name>         # force-kill + reconciler restarts
```

<!-- REVIEW: updated per Blocker 7 — gc agent restart preserves sync behavior -->

### CLI compatibility contract

Aliases must provide **exact fidelity** during the transition:

1. **Name-based addressing.** `gc session` commands accept both session
   IDs (`gc-42`) and template names (`worker`). For pool templates with
   multiple instances, name-based lookup returns all matches with a
   hint to use IDs for disambiguation.
2. **Deprecation shims.** Legacy `gc agent` subcommands (list, attach,
   peek, etc.) print a stderr message with the replacement command and
   exit with an error. `gc agent` retains only config-level operations
   (add, suspend, resume).

<!-- REVIEW: updated per Blocker 7 — gc agent restart preserves sync semantics -->

### REASON column specification

<!-- REVIEW: added per Blocker 7 — REASON column semantics -->

The `REASON` column in `gc session list` has defined semantics:

| State | REASON value | Meaning |
|---|---|---|
| awake | `config` | Awake due to config presence |
| awake | `work` | Awake due to hooked work (Phase 4+) |
| awake | `attached` | Awake because user is attached |
| awake | `config (draining)` | Awake but drain in progress |
| awake | `work (draining)` | Awake with work, drain in progress |
| asleep | `idle` | Slept due to idle timeout |
| asleep | `user-hold` | User explicitly held the session |
| asleep | `quarantine` | Crash-loop quarantine active |
| asleep | `config-drift` | Slept for config drift restart |
| asleep | `pool-excess` | Pool scaled down past this slot |
| asleep | `work-complete` | All hooked work finished, no other reason |

When draining, the drain is shown as a suffix to the primary reason:
`"config (draining)"`. The `--json` output provides structured fields
(`wake_reasons[]`, `draining: bool`, `sleep_reason: string`) for
programmatic consumers.

### Metrics and events

**Counters:**
- `gc_session_wake_total{reason}` — wakes by reason
- `gc_session_sleep_total{trigger}` — sleeps by trigger
- `gc_session_wake_failure_total` — failed wake attempts
- `gc_session_wake_deferred_total{reason}` — wakes deferred (budget, dependency)
- `gc_orphan_kill_total` — orphan processes killed
- `gc_pool_check_error_total{template}` — pool check failures/timeouts
- `gc_drain_canceled_total{reason}` — drains canceled by returning wake reasons

**Gauges:**
- `gc_sessions_by_state{state}` — current count per state
- `gc_bead_store_healthy` — 1 if readable, 0 if not
- `gc_sessions_quarantined` — count of quarantined sessions
- `gc_sessions_draining` — count of sessions in async drain
- `gc_bead_store_consecutive_failures` — consecutive failed ticks

**Histograms:**
- `gc_reconcile_duration_seconds` — time per reconciler tick
- `gc_drain_duration_seconds` — time from drain start to process exit

**Structured events** (appended to `.gc/events.jsonl`):
- `SessionWoke {id, template, reasons[], generation}`
- `SessionSlept {id, template, trigger, sleep_reason}`
- `SessionQuarantined {id, template, attempts, quarantine_until}`
- `SessionClosed {id, template}`
- `OrphanKilled {session_name, unmatched_ticks}`
- `BeadStoreUnavailable {error, tick_skipped: true}`
- `BeadStoreCritical {consecutive_failures}`
- `BeadStoreWriteError {error, session_id}`
- `MigrationAdoption {adopted_count}`
- `MigrationSlotOutOfBounds {name, slot, max}`
- `ConfigDrift {id, template, old_hash, new_hash}`
- `WorkSlungToBlockedSession {session_id, bead_id, block_reason}`
- `ConfigValidationError {field, error}`
- `DrainTimeout {id, template, reason, elapsed}`
- `DrainCanceled {id, reason}`
- `SessionWakeDeferred {id, reason}` — wake skipped due to budget or dependency
- `PoolCheckError {template, error}` — pool check command failed or timed out
- `PoolCheckTimeout {template, elapsed}` — pool check exceeded 5s deadline
- `MigrationSlotParseError {name, raw_suffix}` — legacy pool name unparseable
- `TickBudgetExhausted {elapsed}` — tick exceeded wall-clock budget

<!-- REVIEW: updated per various blockers — added new events -->

## Primitive Test

Not applicable — this proposal does not add a primitive or derived
mechanism. It **reduces** the system's concept count by unifying two
implementations of the same underlying primitive (agent process
lifecycle) into one.

The session-as-bead pattern composes from existing primitives:
- **Agent Protocol** (Layer 0) -> runtime.Provider handles process
  lifecycle
- **Task Store** (Layer 0) -> beads store session identity and state
- **Event Bus** (Layer 1) -> wake signals propagated via events
- **Config** (Layer 0) -> `[[agents]]` provides standing wake reasons

No new primitive is introduced.

## Drawbacks

**1. Migration complexity.** Existing cities have config agents with no
session beads. The adoption barrier runs on every startup, creating beads
for all running processes that lack them. This is rerunnable and
crash-safe (per-instance dedup key prevents duplicates). The `gc migration plan`
dry-run command allows operators to preview adoption before upgrading.
See [Migration protocol](#migration-protocol).

<!-- REVIEW: updated — reflects rerunnable adoption barrier and dry-run command -->

**2. Bead store becomes load-bearing for agent lifecycle.** Currently,
config agents can run even if the bead store is broken — they're just
tmux sessions with env vars. After this change, a corrupt or unavailable
bead store means no agent can wake. This increases the bead store's
criticality. Mitigation: the bead store must meet the requirements in
[Bead store requirements](#bead-store-requirements) before Phase 2 ships,
including record-level corruption isolation, fsync-before-rename, and
ENOSPC handling. On bead store failure, the reconciler aborts
the tick — running processes are left undisturbed. After 3 consecutive
failures, an operator alert is emitted. See
[Bead store failure behavior](#bead-store-requirements).

<!-- REVIEW: updated — references bead store requirements section -->

**3. Session beads accumulate.** Config agents that are removed from
city.toml leave behind session beads. Mitigation: config-managed sessions
whose template is removed from config are auto-closed after a
configurable grace period (default: 7d). `gc session prune` provides
manual cleanup. See [Config agent lifecycle](#config-agent-lifecycle).

<!-- REVIEW: updated — reflects session retention policy -->

**4. Wake-reason computation cost.** Every reconciler tick must compute
wake reasons for every session bead, which involves querying bead stores
for hooked work. Mitigation: wake reasons can be cached within a tick;
bead queries can be batched; hooked-work check can be event-driven.
Provider I/O runs in a bounded worker pool to prevent computation stalls.

<!-- REVIEW: updated — references bounded worker pool -->

**5. Terminology change.** "Agent" is deeply embedded in the current
CLI, API, config, and documentation. `gc agent restart` preserves
synchronous semantics during the alias period (blocks until re-wake or
30s timeout). See [CLI compatibility contract](#cli-compatibility-contract)
for the transition plan.

<!-- REVIEW: updated — notes gc agent restart sync behavior -->

**6. Model unification, not simplification.** The unified system has
~50% more metadata fields than either individual system. The win is
eliminating the second system entirely and making all sessions
first-class citizens of one lifecycle model. See the
[field count comparison](#session-bead-schema) for honest accounting.

## Alternatives

### Alternative 1: Keep two systems, add bridging

Keep the reconciler managing config agents separately. Add a "promote"
operation that converts an interactive session into a reconciler-managed
agent, and a "demote" operation for the reverse.

**Why rejected:** Doubles complexity. Every future feature must be
implemented twice or bridged.

### Alternative 2: Sessions managed by reconciler with restart policies

Add `restart_policy: always | on-failure | manual` to sessions.

**Why rejected:** Restart policies are accidental complexity. Every
policy can be derived from wake reasons: `always` = config, `on-failure`
= work, `manual` = no standing reason. Wake reasons are strictly more
expressive.

### Alternative 3: Do nothing

**Why rejected:** Blocks the core Gas City vision — sessions as
persistent, resumable, work-receiving agent instances.

## Resolved Questions

1. **Should `[[agents]]` config key change?** No. Keep `[[agents]]`.

2. ~~**Crash backoff.**~~ **Resolved.** Added crash-loop dampening with
   quarantine and rapid-exit detection. See
   [Crash-loop dampening](#crash-loop-dampening).

3. **Bead store bootstrapping.** Controller's responsibility. `gc up`
   creates the bead store if missing.

4. **Pool instance identity.** Wake the same sleeping session beads.
   Pool slots are stable. `wake_mode` controls context freshness:
   `"resume"` (default) preserves conversation via session key;
   `"fresh"` skips session key use on wake (key file is preserved
   for seamless `fresh→resume` transitions), giving each activation
   a clean context (the polecat pattern).

5. **Event schema.** See [Metrics and events](#metrics-and-events).

6. ~~**Migration mechanics.**~~ **Resolved.** Rerunnable adoption
   barrier with permissive out-of-bounds adoption. Replaces sticky
   `migration_complete` flag. See [Migration protocol](#migration-protocol).

7. **Backward-compat timeline.** Two minor releases after Phase 3.
   See [CLI compatibility contract](#cli-compatibility-contract).

8. ~~**`gc session wake` semantics.**~~ **Resolved.** `gc session wake`
   clears BOTH user hold AND quarantine. Returns typed outcome
   (`"woken"`, `"hold_cleared"`, `"quarantine_cleared"`, `"blocked:pool"`,
   etc.). See [API surface](#api-surface-unified).

<!-- REVIEW: added per Blocker 7 — resolved wake semantics -->

9. ~~**`gc agent restart` sync vs async.**~~ **Resolved.** Preserves
   synchronous behavior (blocks up to 30s) as the default during alias
   period. `--async` flag for non-blocking behavior. No `--wait` flag
   needed. See [CLI compatibility contract](#cli-compatibility-contract).

<!-- REVIEW: added per Blocker 7 — resolved restart semantics -->

10. ~~**Config hash advancement timing.**~~ **Resolved.** `config_hash`
    advances at drain start (when drift is detected), not at wake. This
    prevents restart loops where the new process sees the old hash and
    triggers another drift detection. See
    [Waking a session](#waking-a-session).

<!-- REVIEW: added per Blocker 1 — resolved config hash timing -->

11. ~~**Pool check fallback.**~~ **Resolved.** Falls back to
    last-known-good desired count, not pool.min. Only uses pool.min if
    no previous count exists. Scale-down requires 2 consecutive ticks
    at the lower value (hysteresis). See
    [Pool integration](#pool-integration).

<!-- REVIEW: added per Blocker 5 — resolved pool fallback -->

12. ~~**Session key storage.**~~ **Resolved.** Stored in
    `.gc/secrets/<session-id>.key` with `0600` permissions, not in bead
    metadata. Redacted from API and events. See
    [Runtime targeting](#runtime-targeting-and-execution-guarantees).

<!-- REVIEW: added per Blocker 6 — resolved session key storage -->

## Known Limitations and Future Work

- **Residual session beads after config removal.** Auto-closed after
  configurable grace period (default: 7d). See
  [Config agent lifecycle](#config-agent-lifecycle).

- **Authorization model.** Not specified. Gas City is currently
  single-user. Address when API is exposed beyond localhost.

- **Orphan tick counter resets on restart.** This is intentionally
  conservative — a restarted controller should be cautious about
  killing unknown processes. The adoption barrier re-verifies all
  running sessions on each startup.

- **TOCTOU on `validateWorkDir`.** `filepath.Abs` does not resolve
  symlinks; a path could be swapped between `os.Stat` and `sp.Start`.
  Academic in single-user localhost context. Future: use
  `filepath.EvalSymlinks` if authorization boundary enforcement is
  needed.

- **Double SIGINT on crash-restart.** If the controller crashes after
  sending `sp.Interrupt()` during drain, drain state is lost and a
  second interrupt may be sent on restart. Agent prompt templates
  should handle repeated SIGINT idempotently (finish current work,
  save state, no double-abort).

- **Last-known-good pool counts lost on restart.** The `lastKnownPoolDesired`
  map is in-memory only. After controller restart, pool check failures
  fall back to `pool.min` until the first successful check. This is
  acceptable for the single-machine deployment model.

## Testing Strategy

<!-- REVIEW: added per Major 3 — testing strategy section -->

### Clock abstraction

All time-dependent logic uses an injected `Clock` interface, never
`time.Now()` directly:

```go
type Clock interface {
    Now() time.Time
    Since(time.Time) time.Duration
    NewTimer(time.Duration) Timer
}

type Timer interface {
    C() <-chan time.Time
    Stop() bool
    Reset(time.Duration) bool
}

// realClock wraps the standard library (production).
type realClock struct{}

// fakeClock allows deterministic time advancement in tests.
type fakeClock struct {
    mu  sync.Mutex
    now time.Time
}
func (c *fakeClock) Advance(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }
```

### Multi-tick test harness

A `TestHarness` provides deterministic multi-tick simulation:

```go
type TestHarness struct {
    Clock    *fakeClock
    Provider *fakeProvider
    Store    *fakeBeadStore
    Config   config.City
    Reconciler *Reconciler
}

// Tick advances the clock and runs one reconciler tick.
func (h *TestHarness) Tick() { h.Reconciler.reconcile() }

// Restart simulates a controller crash and restart (clears in-memory state).
func (h *TestHarness) Restart() {
    h.Reconciler = newReconciler(h.Clock, h.Provider, h.Store, h.Config)
}

// SetPoolCheckResult pre-programs the pool check command output.
func (h *TestHarness) SetPoolCheckResult(template string, desired int, err error)

// AssertSessionState checks that a session is in the expected state.
func (h *TestHarness) AssertSessionState(t *testing.T, id string, state string)
```

### Fake provider

```go
type fakeProvider struct {
    sessions map[string]*fakeSession  // name -> state
    caps     ProviderCapabilities
}

type fakeSession struct {
    alive     bool
    attached  bool
    env       map[string]string
    startTime time.Time
}

func (p *fakeProvider) Start(ctx context.Context, name string, cfg runtime.ExecSpec) error
func (p *fakeProvider) Stop(name string) error
func (p *fakeProvider) Interrupt(name string) error
func (p *fakeProvider) ProcessAlive(ctx context.Context, name string) ProbeResult
func (p *fakeProvider) IsAttached(ctx context.Context, name string) ProbeResult
func (p *fakeProvider) GetEnv(name, key string) string
func (p *fakeProvider) ListRunning(prefix string) []string

// Test helpers
func (p *fakeProvider) Kill(name string)                     // simulate crash
func (p *fakeProvider) FailStart(name string, err error)     // inject start failure
func (p *fakeProvider) SetProbeTimeout(name string)          // make probes return ProbeUnknown
// <!-- REVIEW: Round 4 fix — fake provider uses tri-state probes and ExecSpec -->
```

### Fake bead store

```go
type fakeBeadStore struct {
    beads      map[string]*bead
    failNext   error         // inject next operation failure
    failCount  int           // fail this many operations
    writeCount int           // track write budget
}
```

### Pool evaluator interface

```go
type PoolEvaluator interface {
    Evaluate(ctx context.Context, template config.Agent) (int, error)
}

// In production: runs the check command.
// In tests: returns pre-programmed values.
type fakePoolEvaluator struct {
    results map[string]int
    errors  map[string]error
}
```

### Mandatory scenario matrix

The following scenarios MUST have test coverage before Phase 2 ships:

| # | Scenario | Verifies |
|---|---|---|
| 1 | Config agent wake on first tick | Phase 1 bead creation + Phase 2a wake |
| 2 | Process crash -> rapid-exit detection -> quarantine | checkStability, recordWakeFailure |
| 3 | Quarantine expiry -> auto-wake | healExpiredTimers, wakeReasons |
| 4 | User hold -> gc session wake clears hold AND quarantine | hold/quarantine clearing |
| 5 | Config drift -> drain -> re-wake with new config | config_hash at drain start |
| 6 | Pool scale-up: 2->4 instances | evaluatePoolCached, reconcilePool |
| 7 | Pool scale-down with hysteresis | 2-tick hysteresis, pool-excess drain |
| 8 | Pool check failure -> last-known-good fallback | evaluatePoolCached error path |
| 9 | Dependency-ordered wake (A depends on B) | topoOrder, allDependenciesAlive |
| 10 | Dependency draining blocks dependent wake | allDependenciesAlive + isDraining |
| 11 | Controller crash + restart -> NDI convergence | TestHarness.Restart(), adoption barrier |
| 12 | Bead store unavailable -> tick abort, processes undisturbed | store.Ping failure |
| 13 | Bead store unavailable for 3 ticks -> critical alert | consecutiveStoreFailures |
| 14 | Orphan grace period (3 ticks) | incrementOrphanTickCount |
| 15 | Drain cancelation on returning wake reasons | cancelDrain, advanceDrains |
| 16 | Tick budget exhaustion -> defer remaining work | tickBudget, EventTickBudgetExhausted |
| 17 | Concurrent provider I/O bounded to maxProviderWorkers | worker pool bounds |
| 18 | Instance token mismatch -> reject stale Stop | verifiedStop, token check |
| 19 | SetMetadataBatch failure -> no visible state change | fakeBeadStore.failNext |
| 20 | Migration adoption of out-of-bounds pool slot | adopt-then-drain path |
| 21 | Sling to blocked session -> rejection (Phase 2-3) | sling gate |
| 22 | Fresh-mode wake -> no session key, clean context | buildExecSpecFromConfig fresh path |
| 23 | Phase 1a sync reaches asleep/quarantined sessions | wake_mode sync unconditional |
| 24 | Provider-preset session always resumes (ignores bead wake_mode) | wake() preset path |
| 25 | wake_mode config change -> no hash drift/restart | Phase 1a sync, no drain |
| 26 | Legacy metadata session key migrated to secrets file | One-time key migration on wake |

## Implementation Plan

### Phase 1: Session bead for config agents (medium)

The existing reconciler behavior is unchanged in Phase 1. Bead creation
is additive and write-only — a side effect alongside the current
lifecycle management. Phase 2 switches to the new reconciler.

- Create session bead for each `[[agents]]` entry if not exists
- Store config hash, live hash, session name in bead metadata
- Rerunnable adoption barrier with permissive out-of-bounds slot handling
- Add `generation` counter, `instance_token`, `gc_bead_store_healthy` metric
- Validate `depends_on` for cycles at config load time
- `gc migration plan` dry-run command
- Emit legacy env vars alongside new `GC_SESSION_*` vars
- Session key storage in `.gc/secrets/` with `0600` permissions
- File permission enforcement: `.gc/` at `0700`, store at `0600`
- Clock interface injection (preparation for testability)

<!-- REVIEW: updated per Blockers 1, 4, 6, Major 3 -->

### Phase 2: Wake/sleep reconciler (large)

**Prerequisite:** Bead store must meet [requirements](#bead-store-requirements)
(record-level operations, corruption isolation, fsync-before-rename).

<!-- REVIEW: added per Blocker 2 — bead store prerequisite -->

- Pure `wakeReasons()` with separate `healExpiredTimers()` pass
- Two-phase wake protocol: persist generation BEFORE sp.Start()
- Per-instance pool `WakeConfig` via cached per-tick slot computation
- Pool check fallback to last-known-good, with hysteresis for scale-down
- Async drain (non-blocking, in-memory state, generation-fenced)
- Cancelable drains (wake reasons reappear -> cancel)
- Split wake/sleep passes: forward topo for wake, reverse for drain
- `allDependenciesAlive()` gate (checks draining status too)
- `checkStability()` and `clearWakeFailures()` wired into Phase 2a
- Crash-loop dampening with rapid-exit detection
- `ProcessAlive` for liveness, provider `Capabilities()`
- `maxWakesPerTick` budget with `SessionWakeDeferred` event
- Wall-clock tick budget (default: 5s) with deferral
- Bounded provider I/O worker pool (default: 3 concurrent calls)
- Read-only CLI/API requests bypass event loop (snapshot reads)
- Mutating CLI/API requests queued between reconciler phases
- Immediate pool-excess drain (bypasses idle timeout)
- `SetMetadataBatch` for coalesced per-bead writes; failure = no visible change
- `ExecSpec` replaces string Command in runtime.Config
- Authenticated runtime binding via instance_token
- Immutable controller-owned metadata fields (template, provider, etc.)
- Orphan grace period (3 ticks), gated by adoption barrier
- `session_name` validation at creation and wake
- Command reconstruction from config with ExecSpec validation
- Config hash advancement at drain start (not wake)
- Session retention policy: auto-close after grace period (default: 7d)
- Metrics and structured events (all defined events have emit sites)
- `WorkSlungToBlockedSession` event + sling rejection for blocked targets
- Full test coverage per [mandatory scenario matrix](#mandatory-scenario-matrix)

<!-- REVIEW: updated per all blockers and majors -->

### Phase 3: Unified CLI and API (medium)

- Merge agent endpoints into session endpoints
- Backward-compat aliases with translation layer
- Stable public status schema: `SessionStatus` struct in API responses
- `REASON` column in `gc session list` (draining as suffix)
- Typed wake outcomes from `POST /v0/session/{id}/wake`
- `gc agent restart` preserves synchronous behavior (30s timeout default)
- `gc agent restart --async` for non-blocking behavior
- Name-based addressing
- `PATCH` restricted to `title`; immutable fields rejected with 403
- `POST /v0/session/new` validated
- Deprecation warnings on stderr
- `gc session wake` clears hold AND quarantine
- Qualified template identity for multi-rig cities
- `gc config validate` warns for templates missing interrupt guidance

<!-- REVIEW: updated per Blockers 6, 7, Majors 1 -->

### Phase 4: Work-driven wake (medium)

- Implement `WakeWork` reason (query bead store for hooked beads)
- Wire dispatch/sling to wake target session if sleeping
- Remove sling rejection gate (WakeWork handles blocked sessions)
- Add event-driven wake trigger (don't wait for next tick)

### Phase 5: Deprecation cleanup (small)

Targets next major version, at least two minor releases after Phase 3.

- Remove `/v0/agents` endpoints and `gc agent` CLI aliases
- Remove `internal/agent/` package
- Remove legacy env vars (`GC_AGENT`, `GC_CITY`, `GC_DIR`) from buildEnv
- Update documentation and tutorials
