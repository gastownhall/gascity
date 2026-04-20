---
title: "Health Patrol"
---


> Last verified against code: 2026-03-18

## Summary

Health Patrol is Gas City's Layer 2-4 derived mechanism for agent
supervision. It is the subsystem within the controller that monitors
agent liveness, detects configuration drift, enforces crash loop
quarantine, kills idle agents, and dispatches orders on a periodic
tick. Health Patrol follows the Erlang/OTP supervision model: the
controller is the supervisor, agents are workers, `[[agent]]` entries
are child specs, and "let it crash" is realized through GUPP + beads
(agents die, hooks persist, fresh sessions resume the work).

## Key Concepts

- **Reconciliation**: The declarative process that makes running sessions
  match the desired agent list from config. Each tick compares the "want"
  set (from `city.toml`) against the "have" set (from `ListRunning()`)
  and takes corrective actions: start missing agents, stop orphans,
  restart drifted agents.

- **Config Drift**: A state where a running agent's stored config
  fingerprint (SHA-256 of command + env + fingerprint extras) differs
  from the current config. Detected via `runtime.ConfigFingerprint()` and
  resolved by stop + start.

- **Crash Loop Quarantine**: When an agent exceeds `max_restarts` within
  `restart_window`, it enters quarantine. The controller stops attempting
  to restart it until the window expires. In-memory only -- intentionally
  lost on controller restart (counter reset, same as Erlang/OTP).

- **Idle Timeout**: An opt-in per-agent duration after which an agent
  with no session I/O activity is killed and restarted. Queries
  `runtime.Provider.GetLastActivity()` on each tick.

- **Order Dispatch**: The controller evaluates trigger conditions
  (cooldown, cron, condition, event, manual) on every tick and fires
  due orders. Exec orders run shell scripts directly. Formula
  orders instantiate wisps dispatched to agent pools.

- **Patrol Interval**: The tick frequency for the controller loop.
  Defaults to 30 seconds. Configured via `[daemon] patrol_interval`.

- **Zombie Capture**: When a session exists but the agent process inside
  it is dead, the controller captures pane output for crash forensics
  (via `Peek()`) before restarting the agent.

## Architecture

The Health Patrol is not a standalone subsystem with its own package. It
is composed from several collaborating components wired together inside
the controller loop in `cmd/gc/controller.go`. The controller
instantiates and holds instances of four tracker interfaces, each
following a nil-guard pattern (nil means disabled, callers check before
use):

```
                     ┌─────────────────────────────────────┐
                     │         controllerLoop()            │
                     │   cmd/gc/controller.go:226          │
                     │                                     │
                     │  ┌───────────┐   ┌───────────────┐  │
  fsnotify ─────────►│  │  dirty    │   │ ticker (30s)  │  │
  (config dirs)      │  │  atomic   │   │               │  │
                     │  └─────┬─────┘   └───────┬───────┘  │
                     │        │                 │          │
                     │        ▼                 ▼          │
                     │  ┌─────────────────────────────┐   │
                     │  │  if dirty: tryReloadConfig() │   │
                     │  │  rebuild trackers             │   │
                     │  └──────────────┬──────────────┘   │
                     │                 ▼                   │
                     │  ┌─────────────────────────────┐   │
                     │  │ doReconcileAgents()          │   │
                     │  │ (reconcile.go)               │   │
                     │  │   ├─ crashTracker            │   │
                     │  │   ├─ idleTracker             │   │
                     │  │   ├─ reconcileOps (drift)    │   │
                     │  │   └─ drainOps (pool scaling) │   │
                     │  └──────────────┬──────────────┘   │
                     │                 ▼                   │
                     │  ┌─────────────────────────────┐   │
                     │  │ wispGC.runGC()              │   │
                     │  └──────────────┬──────────────┘   │
                     │                 ▼                   │
                     │  ┌─────────────────────────────┐   │
                     │  │ orderDispatcher         │   │
                     │  │   .dispatch()                │   │
                     │  └─────────────────────────────┘   │
                     └─────────────────────────────────────┘
```

### Data Flow

A single controller tick proceeds as follows:

1. **Config reload** (conditional). If the `dirty` atomic flag is set
   (via fsnotify debounce on config directory changes),
   `tryReloadConfig()` re-parses `city.toml` with includes and patches.
   If the reload succeeds, the crash tracker, idle tracker, wisp GC, and
   order dispatcher are all rebuilt from the new config.

2. **Agent list build**. `buildFn(cfg)` re-evaluates the desired agent
   set, including pool `check` commands for elastic scaling.

3. **Reconciliation** (`doReconcileAgents()`). The core state machine.
   For each desired agent, determines the correct action. See the
   Reconciliation State Machine below.

4. **Wisp GC**. If enabled, purges expired closed molecules older than
   `wisp_ttl`.

5. **Order dispatch** (`ad.dispatch()`). Evaluates all non-manual
   order gates. For each due order, creates a tracking bead
   synchronously (to prevent re-fire), then dispatches in a goroutine.

### Reconciliation State Machine

`doReconcileAgents()` in `cmd/gc/reconcile.go` classifies each agent
into one of four states and takes action:

```
┌──────────────────────────────────────────────────────────┐
│ State              │ Condition         │ Action          │
├──────────────────────────────────────────────────────────┤
│ Not running        │ !IsRunning()      │ Start           │
│ Healthy            │ hash matches      │ Skip            │
│ Orphan             │ running, not in   │ Stop            │
│                    │ desired set       │                 │
│ Drifted            │ hash differs      │ Stop + Start    │
└──────────────────────────────────────────────────────────┘
```

Additional sub-states within "running" are checked in order:

1. **Restart requested**: Agent self-requested restart (context
   exhaustion). Stop + start.
2. **Idle timeout exceeded**: `idleTracker.checkIdle()` returns true.
   Stop + start, emit `agent.idle_killed` event.
3. **Config drift**: Stored hash differs from current. Stop + start.

Agents not running are subject to **crash loop quarantine**: if
`crashTracker.isQuarantined()` returns true, the agent is skipped
silently (the quarantine event was emitted when the threshold was first
hit).

**Orphan cleanup** (Phase 2) handles sessions with the city prefix that
are not in the desired set:
- Pool excess members are drained gracefully via `drainOps`.
- Suspended agents are stopped with an `agent.suspended` event.
- True orphans are killed immediately.

**Dependency-aware bounded parallel starts** (Phase 1b): The bead-driven
session reconciler plans starts serially, groups them into dependency
waves, runs each wave with bounded parallelism, then applies
success/failure side effects serially in stable plan order.

**Dependency-aware bounded force-stops**: Bulk stop paths (`gc stop`,
controller shutdown, provider swap, `gc rig restart`) send interrupts to
all sessions first, then force-stop any survivors in reverse dependency
waves with bounded parallelism.

### Key Types

- **`crashTracker`** (`cmd/gc/crash_tracker.go`): Interface for crash
  loop detection. Production impl `memoryCrashTracker` holds an in-memory
  map of session name to recent start timestamps. Prunes entries older
  than `restart_window` on every call.

- **`idleTracker`** (`cmd/gc/idle_tracker.go`): Interface for agent
  inactivity detection. Production impl `memoryIdleTracker` queries
  `runtime.Provider.GetLastActivity()` and compares against per-agent
  timeout durations.

- **`reconcileOps`** (`cmd/gc/reconcile.go`): Interface for
  session-level operations needed by reconciliation: `listRunning()`,
  `storeConfigHash()`, `configHash()`. Backed by
  `runtime.Provider.SetMeta()`/`GetMeta()` for hash persistence.

- **`orderDispatcher`** (`cmd/gc/order_dispatch.go`): Interface
  for order trigger evaluation and dispatch. Production impl
  `memoryOrderDispatcher` holds the scanned order list, a bead
  store for tracking, an events provider for event triggers, and an exec
  runner for shell commands.

- **`DaemonConfig`** (`internal/config/config.go`): Configuration struct
  holding patrol interval, max restarts, restart window, shutdown timeout,
  wisp GC settings.

## Invariants

These properties must hold for Health Patrol to be correct. Violations
indicate bugs.

- **Single controller**: At most one controller runs per city. Enforced
  by `flock(LOCK_EX|LOCK_NB)` on `.gc/controller.lock`. A second
  `gc start` fails immediately.

- **Reconciliation is idempotent**: Running `doReconcileAgents()` with
  the same config and same running set produces no side effects. A
  healthy running agent with a matching hash is always skipped.

- **Crash tracking is bounded**: `memoryCrashTracker.prune()` removes
  entries older than `restart_window` on every `recordStart()` and
  `isQuarantined()` call. Memory grows at most O(max_restarts *
  num_agents).

- **Quarantine auto-expires**: Once all start timestamps within the
  sliding window have aged past `restart_window`, `isQuarantined()`
  returns false and the agent is restarted on the next tick.

- **Crash tracking resets on controller restart**: The crash tracker is
  in-memory only. Controller restart clears all quarantine state. This is
  intentional (Erlang/OTP parallel: supervisor restart clears child
  restart counts).

- **Config drift uses content hashing, not timestamps**:
  `runtime.ConfigFingerprint()` hashes command + env + fingerprint
  extras. Two configs with identical content always produce the same hash
  regardless of when they were loaded.

- **Order tracking beads are created synchronously before dispatch
  goroutines**: This prevents the cooldown trigger from re-firing on the
  next tick while the dispatch is still running.

- **No PID files for liveness**: Agent liveness is determined by querying
  `runtime.Provider.IsRunning()` and `ProcessAlive()`, which inspect the
  live process tree. Controller discovery uses Unix socket ping probes,
  not PID files, and liveness decisions still come from the live process
  tree.

- **No role names in Go code**: Health Patrol operates on resolved config,
  runtime session names, and provider state. No line of Go references a
  specific role name.

- **SDK self-sufficiency**: All Health Patrol operations (reconciliation,
  crash tracking, idle detection, order dispatch) function with only
  the controller running. No user-configured agent role is required.

## Interactions

### Erlang/OTP Supervision Model

Health Patrol follows Erlang/OTP patterns mapped to Gas City:

| Erlang/OTP concept       | Gas City equivalent                       |
|--------------------------|-------------------------------------------|
| Supervisor               | Controller (`controllerLoop`)             |
| Worker                   | Agent (any role)                          |
| Child spec               | `[[agent]]` entry in `city.toml`         |
| one_for_one restart      | Restart dead agent only (no cascade)      |
| max_restarts/max_seconds | `max_restarts` / `restart_window`         |
| Links (death propagates) | Not implemented (no `depends_on` yet)     |
| "Let it crash"           | GUPP + beads: agent dies, hook persists,  |
|                          | fresh session picks up persisted work     |
| Process mailbox          | Mail inbox (beads with type=message)      |
| GenServer loop           | Agent loop: check hook -> run -> repeat   |

### Package Dependencies

| Depends on | How |
|---|---|
| `internal/config` | Parses `DaemonConfig` for patrol interval, max restarts, restart window, shutdown timeout. Provides `Revision()` for config reload detection. |
| `internal/runtime` | `Provider` interface for Start/Stop/IsRunning/ListRunning/GetLastActivity/SetMeta/GetMeta. `ConfigFingerprint()` for drift detection. |
| `internal/events` | `Recorder` interface for emitting lifecycle events (`agent.started`, `agent.stopped`, `agent.crashed`, `agent.quarantined`, `agent.idle_killed`, `agent.suspended`, `controller.started`, `controller.stopped`, `order.fired`, `order.completed`, `order.failed`). `Provider` interface for event trigger queries. |
| `internal/beads` | `Store` interface for order tracking beads (create, update, list by label). `CommandRunner` for bd CLI invocation. |
| `internal/orders` | `Scan()` to discover orders from formula layers. `CheckTrigger()` to evaluate trigger conditions. `Order` struct for dispatch metadata. |
| `internal/agent` | `Agent` interface wrapping config + session provider for `Start()`/`Stop()`/`IsRunning()`/`SessionName()` operations. |
| `github.com/fsnotify/fsnotify` | File system watcher for config directory change detection. |

| Depended on by | How |
|---|---|
| `cmd/gc/cmd_supervisor.go` | Starts and manages one `CityRuntime` per registered city under the machine-wide supervisor. |
| `cmd/gc/cmd_start.go` | Hidden standalone compatibility path via `gc start --foreground`, which still calls `runController()` after building the initial agent list and config. |

## Code Map

All Health Patrol implementation lives in `cmd/gc/`:

| File | Responsibility |
|---|---|
| `cmd/gc/controller.go` | Controller lock, Unix socket, fsnotify config watcher, `controllerLoop()`, `tryReloadConfig()`, `runController()`, `gracefulStopAll()` |
| `cmd/gc/reconcile.go` | `reconcileOps` interface, `doReconcileAgents()` (4-state reconciliation + parallel starts + orphan cleanup), `doStopOrphans()` |
| `cmd/gc/crash_tracker.go` | `crashTracker` interface, `memoryCrashTracker` (in-memory restart history with sliding window pruning) |
| `cmd/gc/idle_tracker.go` | `idleTracker` interface, `memoryIdleTracker` (per-agent timeout + GetLastActivity query) |
| `cmd/gc/order_dispatch.go` | `orderDispatcher` interface, `memoryOrderDispatcher` (trigger evaluation, exec dispatch, wisp dispatch, tracking bead lifecycle) |
| `internal/config/config.go` | `DaemonConfig` struct with `PatrolIntervalDuration()`, `MaxRestartsOrDefault()`, `RestartWindowDuration()`, `ShutdownTimeoutDuration()` |
| `internal/config/revision.go` | `Revision()` (SHA-256 bundle hash of all config sources + pack dirs), `WatchDirs()` |
| `internal/runtime/fingerprint.go` | `ConfigFingerprint()` (SHA-256 of command + env + extras for drift detection) |
| `internal/orders/triggers.go` | `CheckTrigger()` with cooldown, cron, condition, event, and manual trigger evaluators |
| `internal/orders/order.go` | `Order` struct definition, `Scan()` for discovery |

## Configuration

Health Patrol is configured via the `[daemon]` and `[orders]`
sections of `city.toml`:

```toml
[daemon]
patrol_interval = "30s"     # reconciliation tick frequency (default: 30s)
max_restarts = 5            # crash loop threshold (default: 5, 0 = unlimited)
restart_window = "1h"       # sliding window for restart counting (default: 1h)
shutdown_timeout = "5s"     # grace period before force-kill on shutdown (default: 5s)
wisp_gc_interval = "5m"     # how often to purge expired wisps (disabled if unset)
wisp_ttl = "24h"            # how long closed wisps survive (disabled if unset)

[orders]
skip = ["noisy-order"] # order names to exclude from dispatch
max_timeout = "120s"        # hard cap on per-order timeout (default: uncapped)
```

Per-agent idle timeout is configured on individual `[[agent]]` entries:

```toml
[[agent]]
name = "worker"
idle_timeout = "30m"        # restart if no I/O activity for 30 minutes
```

## Testing

Each Health Patrol component has dedicated unit tests:

| Test file | Coverage |
|---|---|
| `cmd/gc/controller_test.go` | Controller loop tick behavior, config reload, dirty flag, fsnotify debounce, order dispatch integration |
| `cmd/gc/reconcile_test.go` | All four reconciliation states (not running/healthy/orphan/drifted), parallel starts, zombie capture, crash loop quarantine integration, idle restart, pool drain, suspended agent handling |
| `cmd/gc/crash_tracker_test.go` | Sliding window pruning, quarantine threshold, clear history, nil-guard (disabled tracker) |
| `cmd/gc/idle_tracker_test.go` | Timeout detection, zero time handling, per-agent timeout configuration, nil-guard |
| `cmd/gc/order_dispatch_test.go` | Trigger evaluation (cooldown, cron, condition, event, manual), exec dispatch, wisp dispatch, tracking bead creation, timeout capping, rig-scoped orders |

All tests use in-memory fakes (`runtime.Fake`, `events.Discard`,
stubbed `ExecRunner`) with no external infrastructure dependencies. See
`TESTING.md` for the overall testing philosophy and tier boundaries.

## Known Limitations

- **No cascading restarts**: Erlang/OTP supports `one_for_all` and
  `rest_for_one` restart strategies. Gas City currently implements only
  `one_for_one` (restart the dead agent, nothing else). There is no
  `depends_on` mechanism for agent dependency ordering.

- **Crash tracker is in-memory only**: Crash history is lost on
  controller restart. An agent that crash-looped before a controller
  restart will be retried immediately. This is intentional (matches
  Erlang/OTP behavior) but may surprise operators.

- **Idle detection depends on provider support**: `GetLastActivity()`
  returns zero time if the session provider does not support activity
  tracking. In that case, idle detection silently does nothing (no
  false positives, but also no idle kills).

- **Order dispatch is fire-and-forget**: Once a goroutine is
  launched for a due order, the controller does not track its
  completion. Failed orders emit events but do not retry. The
  tracking bead prevents re-fire within the same cooldown window.

- **No hot-reload for structural changes**: Changing `workspace.name`
  requires a full controller restart. `tryReloadConfig()` rejects name
  changes and keeps the old config.

## See Also

- [Architecture glossary](glossary.md) -- authoritative definitions
  of all Gas City terms used in this document
- [Config struct definitions](https://github.com/gastownhall/gascity/blob/main/internal/config/config.go) --
  `DaemonConfig`, `Agent`, and `PoolConfig` struct fields and defaults
- [Runtime Provider interface](https://github.com/gastownhall/gascity/blob/main/internal/runtime/runtime.go) --
  the provider interface that Health Patrol queries for liveness, metadata,
  and activity
- [Order trigger evaluation](https://github.com/gastownhall/gascity/blob/main/internal/orders/triggers.go) --
  trigger types (cooldown, cron, condition, event, manual) and their
  check logic
- [Event type constants](https://github.com/gastownhall/gascity/blob/main/internal/events/events.go) -- all event
  types emitted by Health Patrol
- [Config revision hashing](https://github.com/gastownhall/gascity/blob/main/internal/config/revision.go) --
  SHA-256 bundle hash for config reload detection
- [Session config fingerprinting](https://github.com/gastownhall/gascity/blob/main/internal/runtime/fingerprint.go)
  -- per-agent SHA-256 hash for drift detection
