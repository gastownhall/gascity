---
title: "Controller"
---


> Last verified against code: 2026-04-14

## Summary

The Controller is Gas City's per-city reconciliation runtime. The canonical
long-running process is `gc supervisor run`, which hosts one controller
runtime per registered city. A hidden `gc start --foreground` compatibility
mode still launches the same per-city runtime directly. The controller loop
watches `city.toml` for changes (via fsnotify), periodically reconciles
running agents against the desired config, evaluates pool scaling, dispatches
automations, and garbage-collects expired wisps.

## Key Concepts

- **Controller Loop**: The persistent `select` loop in `controllerLoop()`
  that fires on a configurable ticker (default 30s) and on config file
  changes. Each tick runs the full reconciliation, wisp GC, and order
  dispatch pipeline. Implemented in `cmd/gc/controller.go`.

- **Config Reload**: The debounced mechanism by which filesystem changes
  to `city.toml` and pack directories trigger a full config re-parse.
  An `atomic.Bool` dirty flag is set by fsnotify; at the top of each tick,
  if dirty is true, `tryReloadConfig()` re-parses and validates the config.
  Rejects workspace name changes (requires controller restart).

- **Graceful Shutdown**: The two-pass agent termination sequence:
  (1) send Interrupt (Ctrl-C) to all sessions, (2) wait `shutdown_timeout`,
  (3) force-kill survivors via `Stop()`. Implemented in `gracefulStopAll()`.

- **Supervisor vs Standalone Compatibility**: `gc start` now validates an
  existing city, registers it with the machine-wide supervisor, ensures the
  supervisor is running, and waits for the city to become active.
  `gc supervisor run` is the canonical foreground control loop. Hidden
  `gc start --foreground` remains as a compatibility path for the legacy
  per-city controller.

- **Pool Evaluation**: The process of running each pool agent's `check`
  shell command, parsing the output as an integer, clamping to `[min, max]`,
  and building the corresponding agent instances. Pool checks run in
  parallel via goroutines.

- **Nil-Guard Tracker Pattern**: Optional subsystems (crash tracker, idle
  tracker, wisp GC, stuck tracker, order dispatcher) follow a nil-means-disabled
  convention. Callers check `if tracker != nil` before use. This avoids
  conditional plumbing and keeps the loop body clean.

## Architecture

The controller is implemented entirely in `cmd/gc/` as a set of
collaborating functions and interfaces -- not as a standalone package.
It composes primitives (Agent Protocol, Config, Event Bus, Beads, Prompts)
into the runtime orchestration loop.

### Data Flow

The hidden standalone compatibility flow still proceeds as follows:

```
gc start --foreground
  │
  ├─ 1. Require an initialized city
  ├─ 2. Fetch remote packs
  ├─ 3. LoadWithIncludes(city.toml)  →  *config.City + Provenance
  ├─ 4. ensureBeadsProvider()        →  start dolt server if bd backend
  ├─ 5. ValidateRigs() + resolve paths
  ├─ 6. initAllRigBeads()           →  per-rig .beads/ databases + routes
  ├─ 7. MaterializeSystemFormulas()  →  embed system formulas as Layer 0
  ├─ 8. ResolveFormulas()            →  symlinks in .beads/formulas/
  ├─ 9. ValidateAgents() + hooks
  ├─10. newSessionProvider()         →  tmux / exec / k8s / subprocess
  ├─11. runController()
  │     ├─ acquireControllerLock()   →  flock LOCK_EX|LOCK_NB
  │     ├─ startControllerSocket()   →  Unix socket for IPC
  │     ├─ build trackers (crash, idle, wisp GC, order)
  │     └─ controllerLoop()
  │           ├─ watchConfigDirs()   →  fsnotify on config + pack dirs
  │           ├─ initial reconciliation
  │           └─ ticker loop:
  │                 ├─ if dirty: tryReloadConfig() + rebuild trackers
  │                 ├─ buildAgents(cfg)  →  evaluate pools in parallel
  │                 ├─ doReconcileAgents()
  │                 ├─ wispGC.runGC()
  │                 ├─ runStuckSweep()   ← controller-level stuck detection
  │                 └─ orderDispatcher.dispatch()
  │
  └─ shutdown:
        ├─ gracefulStopAll()         →  interrupt → wait → kill
        ├─ record controller.stopped event
        └─ release lock + remove socket + pid
```

### Main Loop Detail

Each tick of `controllerLoop()` (`cmd/gc/controller.go:268-320`) performs:

1. **Dirty check** (`dirty.Swap(false)`): If config files changed since
   the last tick, `tryReloadConfig()` re-parses `city.toml` with includes
   and patches. On success, all four trackers are rebuilt from the new
   config. On failure (parse error, validation error, name change), the
   old config is kept and an error is logged.

2. **Agent list build** (`buildAgents(cfg)`): Re-evaluates the desired
   agent set. For pool agents, `evaluatePool()` runs `check` commands in
   parallel via goroutines (`cmd/gc/pool.go:43`). Suspended agents and
   agents in suspended rigs are excluded. Fixed agents are resolved
   individually. Each agent gets its environment, prompt, hooks, overlay,
   and session setup expanded.

3. **Reconciliation** (`doReconcileAgents()`): Declarative convergence --
   make running sessions match the desired list. See
   [Health Patrol](health-patrol.md) for the reconciliation state machine,
   crash loop quarantine, and idle tracking details.

4. **Wisp GC** (`wg.runGC()`): If enabled (`wisp_gc_interval` and
   `wisp_ttl` both set), queries closed molecules via `bd list` and
   deletes those older than the TTL cutoff.

5. **Stuck sweep** (`runStuckSweep()`): If `[daemon].stuck_sweep` is
   enabled, inspects each running session for wedge signals (stale
   in-progress wisp + pane error pattern or no-progress-since-wisp-open)
   and files a warrant bead (`type=warrant`, configurable label) on
   detection. Emits `agent.stuck` on successful Create. Idempotent per
   target. See [Health Patrol](health-patrol.md) for signal composition
   and the known-limitation on `updated_at` parsing.

6. **Order dispatch** (`ad.dispatch()`): Evaluates gate conditions
   for all non-manual orders. See
   [Health Patrol](health-patrol.md) for gate evaluation and dispatch
   details.

### Key Types

- **`controllerLoop()`** (`cmd/gc/controller.go:226`): The main loop
  function. Accepts all dependencies as parameters for testability:
  config, build function, session provider, reconcile/drain ops, all four
  trackers, event recorder, and I/O writers.

- **`runController()`** (`cmd/gc/controller.go:335`): The top-level
  orchestrator. Acquires the flock, opens the Unix socket, builds
  trackers, enters the loop, and performs graceful shutdown on exit.

- **`tryReloadConfig()`** (`cmd/gc/controller.go:137`): Config reload
  with validation. Rejects workspace name changes. Returns the new
  config, provenance, and revision hash on success.

- **`gracefulStopAll()`** (`cmd/gc/controller.go:169`): Two-pass shutdown.
  Interrupt all sessions, wait `shutdown_timeout`, then force-kill
  survivors.

- **`DaemonConfig`** (`internal/config/config.go:377`): Configuration
  struct holding `patrol_interval`, `max_restarts`, `restart_window`,
  `shutdown_timeout`, `wisp_gc_interval`, `wisp_ttl`. All durations
  have `*Duration()` accessor methods with sensible defaults.

## Invariants

These properties must hold for the controller to be correct. Violations
indicate bugs.

- **Single standalone controller per city**: At most one standalone
  controller runs per city
  directory. Enforced by `flock(LOCK_EX|LOCK_NB)` on
  `.gc/controller.lock`. A second `gc start --foreground` fails
  immediately with "controller already running."

- **Config reload preserves city identity**: `tryReloadConfig()` rejects
  any reload where `workspace.name` changes. The city name is locked at
  startup; changing it requires a controller restart.

- **Tracker rebuild is atomic per tick**: When config reloads, all four
  trackers (crash, idle, wisp GC, order) are rebuilt in the same tick
  before reconciliation runs. No tick ever uses a mix of old and new
  tracker state.

- **Dirty flag is edge-triggered, not level-triggered**: The `atomic.Bool`
  is set by the fsnotify goroutine and cleared by `dirty.Swap(false)` at
  the top of each tick. Multiple filesystem events within a single tick
  coalesce into a single reload.

- **Pool check commands run in parallel**: `evaluatePool()` calls for all
  pool agents in a single `buildAgents()` invocation run concurrently via
  goroutines. Results are processed sequentially after `wg.Wait()`.

- **Supervisor-managed and standalone runtimes share reconciliation code**:
  `CityRuntime.run()` and `doReconcileAgents()` power both the
  machine-wide supervisor path and the hidden standalone
  `gc start --foreground` path.

- **Graceful shutdown sends Interrupt before Stop**: `gracefulStopAll()`
  always sends `Interrupt()` to all sessions before sleeping
  `shutdown_timeout` and calling `Stop()` on survivors. Zero timeout
  skips the grace period entirely.

- **Socket cleanup is best-effort**: The Unix socket at
  `.gc/controller.sock` is removed on startup (stale cleanup) and on
  shutdown. Crash-orphaned sockets are cleaned up by the next controller
  start.

- **No role names in Go code**: The controller operates on resolved config,
  runtime session names, and provider state. No line of Go references a
  specific role name.

- **SDK self-sufficiency**: All controller operations (config watch,
  reconciliation, pool scaling, order dispatch, wisp GC, graceful
  shutdown) function with only the controller process running. No user-
  configured agent role is required for any infrastructure operation.

## Interactions

| Depends on | How |
|---|---|
| `internal/config` | `LoadWithIncludes()` for config parsing, `DaemonConfig` for loop timing, `Revision()` for reload detection, `WatchDirs()` for fsnotify targets, `ValidateAgents()`/`ValidateRigs()` for validation, `ResolveProvider()` for agent commands. |
| `internal/runtime` | `Provider` interface for Start/Stop/IsRunning/ListRunning/Interrupt/Peek/SetMeta/GetMeta/ClearScrollback. `ConfigFingerprint()` drives drift detection. |
| `internal/agent` | `SessionNameFor()` computes session names and `StartupHints` feeds runtime config assembly. |
| `internal/events` | `Recorder` for emitting lifecycle events. `Provider` for event gate queries in order dispatch. `NewFileRecorder()` for JSONL persistence. |
| `internal/beads` | `Store` for order tracking beads. `CommandRunner` for bd CLI invocation. `NewBdStore()` for rig-scoped stores. |
| `internal/orders` | `Scan()` for order discovery. `CheckGate()` for gate evaluation. |
| `internal/hooks` | `Install()` for provider-specific agent hooks. `Validate()` for hook name validation. |
| `cmd/gc/beads_provider_lifecycle.go` | Starts, initializes, health-checks, and shuts down the configured beads backend. |
| `internal/fsys` | `OSFS{}` filesystem abstraction for testability. |
| `github.com/fsnotify/fsnotify` | File system watcher for config directory change detection. |

| Depended on by | How |
|---|---|
| `cmd/gc/cmd_start.go` | Hidden compatibility entry point: `doStartStandalone()` calls `runController()` in foreground mode. |
| `cmd/gc/cmd_supervisor.go` | Canonical machine-wide entry point: starts and reconciles one `CityRuntime` per registered city. |
| `cmd/gc/cmd_stop.go` | `tryStopController()` connects to the Unix socket and sends "stop". |

## Code Map

All controller implementation lives in `cmd/gc/`:

| File | Responsibility |
|---|---|
| `cmd/gc/controller.go` | `acquireControllerLock()`, `startControllerSocket()`, `watchConfigDirs()`, `tryReloadConfig()`, `gracefulStopAll()`, `controllerLoop()` compatibility shim, `runController()` |
| `cmd/gc/city_runtime.go` | `CityRuntime` shared per-city runtime used by both supervisor-managed and standalone controller paths |
| `cmd/gc/cmd_start.go` | `doStart()` supervisor registration path, `doStartStandalone()` hidden compatibility path, `buildAgents()` closure, `computeSuspendedNames()`, `computePoolSessions()`, `buildIdleTracker()` |
| `cmd/gc/cmd_supervisor.go` | Machine-wide supervisor lifecycle, registry reconciliation, API hosting, and child `CityRuntime` management |
| `cmd/gc/cmd_stop.go` | `cmdStop()`, `tryStopController()` (Unix socket IPC), `doStop()`, `gracefulStopAll()` |
| `cmd/gc/cmd_suspend.go` | `doSuspendCity()` (sets `workspace.suspended` in TOML), `citySuspended()`, `isAgentEffectivelySuspended()` |
| `cmd/gc/reconcile.go` | `reconcileOps` interface, `doReconcileAgents()` (4-state reconciliation + parallel starts + orphan cleanup) |
| `cmd/gc/pool.go` | `evaluatePool()`, `poolAgents()`, `expandSessionSetup()`, `expandDirTemplate()` |
| `cmd/gc/providers.go` | `newSessionProvider()`, `beadsProvider()`, `newMailProvider()`, `newEventsProvider()` |
| `cmd/gc/beads_provider_lifecycle.go` | `ensureBeadsProvider()`, `shutdownBeadsProvider()`, `initBeadsForDir()` |
| `cmd/gc/formula_resolve.go` | `ResolveFormulas()` (layered symlink materialization) |
| `cmd/gc/wisp_gc.go` | `wispGC` interface, `memoryWispGC` (TTL-based closed molecule purging) |
| `cmd/gc/order_dispatch.go` | `orderDispatcher` interface, `memoryOrderDispatcher`, `buildOrderDispatcher()` |
| `cmd/gc/crash_tracker.go` | `crashTracker` interface, `memoryCrashTracker` |
| `cmd/gc/idle_tracker.go` | `idleTracker` interface, `memoryIdleTracker` |
| `cmd/gc/stuck_tracker.go` | `stuckTracker` interface, `memoryStuckTracker` (pure convergent-evidence predicate) |
| `cmd/gc/stuck_sweep.go` | `CityRuntime.runStuckSweep()` — tick wrapper, warrant filing, idempotence |
| `cmd/gc/wisp_freshness.go` | `wispFreshness` opaque snapshot, `sweepWispFreshness()` bd-list parser |
| `cmd/gc/cmd_agent_drain.go` | `drainOps` interface, `providerDrainOps` (session metadata-backed drain signals) |

Supporting packages:

| Package | Role |
|---|---|
| `internal/config/config.go` | `DaemonConfig` struct and duration accessors |
| `internal/config/revision.go` | `Revision()` SHA-256 bundle hash, `WatchDirs()` |
| `internal/config/pack.go` | Pack expansion during `LoadWithIncludes()` |
| `internal/runtime/fingerprint.go` | `ConfigFingerprint()` for drift detection |

## Configuration

The controller is configured via the `[daemon]` section of `city.toml`:

```toml
[daemon]
patrol_interval = "30s"     # reconciliation tick frequency (default: 30s)
max_restarts = 5            # crash loop threshold (default: 5, 0 = unlimited)
restart_window = "1h"       # sliding window for restart counting (default: 1h)
shutdown_timeout = "5s"     # grace period before force-kill (default: 5s)
wisp_gc_interval = "5m"     # wisp GC run frequency (disabled if unset)
wisp_ttl = "24h"            # how long closed wisps survive (disabled if unset)
stuck_sweep           = false        # opt-in controller-level stuck detection
stuck_wisp_threshold  = "10m"        # wisp staleness threshold (default 10m)
stuck_error_patterns  = []           # Go regexp sources; empty disables pattern axis
stuck_peek_lines      = 50           # pane lines inspected (clamped [1,2000])
stuck_warrant_label   = "pool:dog"   # label applied to warrant beads
```

Session provider selection (affects all controller session operations):

```toml
[session]
provider = ""               # "", "fake", "fail", "subprocess", "exec:<script>", "k8s"
```

Beads provider selection (affects order tracking, wisp GC):

```toml
[beads]
provider = "bd"             # "bd" (default), "file", "exec:<script>"
```

Order filtering:

```toml
[orders]
skip = ["noisy-order"] # orders to exclude from dispatch
max_timeout = "120s"        # hard cap on per-order timeout
```

City-level suspension:

```toml
[workspace]
suspended = false           # when true, no agents are started
```

## Testing

Controller tests use in-memory fakes and require no external infrastructure:

| Test file | Coverage |
|---|---|
| `cmd/gc/controller_test.go` | Controller loop tick behavior, config reload, dirty flag, fsnotify debounce, tracker rebuild on reload, order dispatch integration |
| `cmd/gc/reconcile_test.go` | All reconciliation states, parallel starts, zombie capture, crash quarantine integration, idle restart, pool drain, suspended agent handling, orphan cleanup |
| `cmd/gc/pool_test.go` | `evaluatePool()` (clamping, error handling), `poolAgents()` (naming, deep-copy), `expandSessionSetup()`, `expandDirTemplate()` |
| `cmd/gc/formula_resolve_test.go` | Layer priority, symlink creation/update/cleanup, idempotence, real file preservation |
| `cmd/gc/wisp_gc_test.go` | TTL-based purging, `shouldRun()` interval, empty list handling |
| `cmd/gc/order_dispatch_test.go` | Gate evaluation, exec dispatch, wisp dispatch, tracking bead lifecycle, timeout capping, rig-scoped orders |
| `cmd/gc/cmd_start_test.go` | Supervisor registration path, hidden foreground compatibility mode, existing-city validation, provider resolution |
| `cmd/gc/cmd_supervisor_test.go` | Supervisor lifecycle, status reporting, service file generation |
| `cmd/gc/cmd_suspend_test.go` | Suspend/resume TOML mutation, inheritance hierarchy |
| `cmd/gc/beads_provider_lifecycle_test.go` | Provider ensure/shutdown/init lifecycle |

All tests use `session.NewFake()`, `events.Discard`, and stubbed
`ExecRunner`/`CommandRunner` functions. See `TESTING.md` for the overall
testing philosophy and tier boundaries.

## Known Limitations

- **No hot-reload for structural changes**: Changing `workspace.name`
  requires a full controller restart. `tryReloadConfig()` rejects name
  changes and keeps the old config.

- **Debounce window is global**: The 200ms fsnotify debounce applies to
  all watched directories. A burst of changes across multiple pack
  dirs produces a single reload, which is correct but may delay detection
  of a single file change by up to 200ms.

- **Pool check commands can stall the tick**: Although pool checks run in
  parallel with each other, the controller tick blocks on `wg.Wait()`
  until all checks complete. A hung `check` command blocks the entire
  reconciliation cycle. There is no per-check timeout.

- **Socket probes are for discovery, not liveness**: Per-city controller
  status uses `controller.sock` ping responses, and supervisor status uses
  `supervisor.sock`. Liveness still comes from `flock` for singleton
  control loops and `runtime.Provider.IsRunning()` for agents.

- **Unix socket has no authentication**: Any local process with filesystem
  access to `.gc/controller.sock` can send "stop" to shut down the
  controller. File permissions (0o755 on `.gc/`) provide the only access
  control.

- **Tracker state is in-memory only**: Crash tracker history, idle
  tracker timestamps, and order dispatch state are all lost on
  controller restart. This is intentional (matches Erlang/OTP supervisor
  restart semantics) but may surprise operators expecting persistence
  across restarts.

## See Also

- [Health Patrol](health-patrol.md) -- reconciliation state machine,
  crash loop quarantine, idle tracking, and order dispatch details
- [Architecture glossary](glossary.md) -- authoritative definitions
  of controller, pool, provider, rig, and other terms used in this doc
- [Config struct definitions](https://github.com/gastownhall/gascity/blob/main/internal/config/config.go) --
  `DaemonConfig`, `City`, `Agent`, `PoolConfig` struct fields and defaults
- [Runtime Provider interface](https://github.com/gastownhall/gascity/blob/main/internal/runtime/runtime.go) --
  the provider interface that the controller uses for all session operations
- [Orders architecture](orders.md) -- gate types, dispatch
  model, and order configuration
- [Formulas architecture](formulas.md) -- formula resolution, layering,
  and symlink materialization
- [Nine Concepts overview](nine-concepts.md) -- how the controller relates
  to the five primitives and four derived mechanisms
