# Automations

<!--
Current-state architecture document. Describes how Automations work
TODAY. For proposed changes, write a design doc in docs/design/ instead.

Audience: Gas City contributors (human and LLM agent).
Update this document when the implementation changes.
-->

> Last verified against code: 2026-03-01

## Summary

Automations are Gas City's derived mechanism (Layer 2-4, part of Formulas
& Molecules) for scheduled and event-driven work dispatch without human
intervention. Each automation pairs a gate condition (when to fire) with
an action (a shell script or formula wisp), living as an
`automation.toml` file inside formula directories. The controller
evaluates all non-manual gates on every patrol tick and dispatches due
automations -- exec automations run shell scripts directly with no LLM
involvement, while formula automations instantiate wisps dispatched to
agent pools.

## Key Concepts

- **Automation**: A parsed definition from an `automation.toml` file with
  a Name (derived from subdirectory name), a dispatch action (Formula or
  Exec, mutually exclusive), a Gate type, gate-specific parameters, and
  optional Pool routing. Defined in the `Automation` struct at
  `internal/automations/automation.go`.

- **Gate**: The trigger condition that controls when an automation fires.
  Five types exist: `cooldown` (minimum interval since last run), `cron`
  (5-field schedule matching), `condition` (shell command exits 0),
  `event` (matching events after a cursor position), and `manual`
  (explicit invocation only, never auto-fires). See
  `internal/automations/gates.go`.

- **Exec Automation**: An automation whose action is a shell command
  (`exec` field) run directly by the controller. No LLM, no agent, no
  wisp. The script receives `AUTOMATION_DIR` in its environment, set to
  the directory containing the `automation.toml` file. Default timeout:
  60 seconds.

- **Formula Automation**: An automation whose action is a formula name
  (`formula` field). When the gate opens, the controller calls `MolCook`
  to instantiate a wisp and labels it for pool dispatch. Default timeout:
  30 seconds.

- **ScopedName**: A rig-qualified key that creates unique identity for
  automations across rigs. City-level automations use the plain name
  (e.g., `dolt-health`). Rig-level automations append `:rig:<rigName>`
  (e.g., `dolt-health:rig:demo-repo`). ScopedName drives independent
  cooldown tracking, event cursors, and label scoping.

- **Formula Layer**: A directory scanned for `automations/*/automation.toml`.
  Layers are ordered lowest to highest priority; a higher-priority layer's
  automation definition overrides a lower-priority one with the same
  subdirectory name (last-wins semantics).

- **Tracking Bead**: A bead created synchronously before each dispatch
  goroutine launches, labeled `automation-run:<scopedName>`. Serves dual
  purpose: prevents the cooldown gate from re-firing on the next tick,
  and provides execution history for `gc automation history`.

## Architecture

The automation subsystem spans two packages:

- **`internal/automations/`** -- parsing, validation, scanning, and gate
  evaluation. Pure library code with no side effects beyond shell command
  execution for condition gates.
- **`cmd/gc/`** -- controller-side dispatch (`automation_dispatch.go`)
  and CLI commands (`cmd_automation.go`). Wires the library into the
  controller loop and the `gc automation` command tree.

```
                ┌──────────────────────────────────────────────────┐
                │           Controller Tick                        │
                │           cmd/gc/controller.go                   │
                │                                                  │
                │  ┌────────────────────────────────────────────┐  │
                │  │  automationDispatcher.dispatch(ctx, now)    │  │
                │  │  cmd/gc/automation_dispatch.go              │  │
                │  │                                             │  │
                │  │  for each automation:                       │  │
                │  │    ┌───────────────────────────────────┐    │  │
                │  │    │ CheckGate(a, now, lastRunFn,      │    │  │
                │  │    │          ep, cursorFn)             │    │  │
                │  │    │ internal/automations/gates.go      │    │  │
                │  │    └───────────┬───────────────────────┘    │  │
                │  │                │                             │  │
                │  │        ┌───────▼────────┐                   │  │
                │  │        │ GateResult.Due? │                   │  │
                │  │        └──┬──────────┬──┘                   │  │
                │  │       no  │          │ yes                   │  │
                │  │       skip│          ▼                       │  │
                │  │           │  Create tracking bead (sync)    │  │
                │  │           │          │                       │  │
                │  │           │          ▼                       │  │
                │  │           │  go dispatchOne(a) ──────────┐  │  │
                │  │           │     ┌────────┬────────┐      │  │  │
                │  │           │     │ IsExec │Formula │      │  │  │
                │  │           │     ▼        ▼        │      │  │  │
                │  │           │  shell    MolCook     │      │  │  │
                │  │           │  script   + label     │      │  │  │
                │  │           │     │        │        │      │  │  │
                │  │           │     └────┬───┘        │      │  │  │
                │  │           │          ▼            │      │  │  │
                │  │           │   Record event       │      │  │  │
                │  │           │   (fired/completed/  │      │  │  │
                │  │           │    failed)            │      │  │  │
                │  │           │                       │      │  │  │
                │  └───────────┴───────────────────────┴──────┘  │
                └──────────────────────────────────────────────────┘

                ┌──────────────────────────────────────────────────┐
                │          Automation Discovery (Startup)          │
                │                                                  │
                │  buildAutomationDispatcher()                     │
                │    ├─ cityFormulaLayers(cfg)                     │
                │    ├─ automations.Scan(cityLayers)               │
                │    ├─ for each rig:                              │
                │    │   ├─ rigExclusiveLayers(rigLayers, city)    │
                │    │   ├─ automations.Scan(exclusive)            │
                │    │   └─ stamp Rig field on each automation     │
                │    ├─ filter out manual-gate automations          │
                │    └─ return memoryAutomationDispatcher           │
                └──────────────────────────────────────────────────┘
```

### Data Flow

**Discovery (on controller start and config reload):**

1. `buildAutomationDispatcher()` resolves city-level formula layers via
   `cityFormulaLayers()` and calls `automations.Scan()` to find city
   automations.
2. For each rig, `rigExclusiveLayers()` strips the city prefix from the
   rig's formula layers to avoid double-scanning city automations. The
   remaining rig-exclusive layers are scanned separately.
3. Rig automations get their `Rig` field stamped with the rig name.
4. Manual-gate automations are filtered out (they never auto-dispatch).
5. If no auto-dispatchable automations remain, the dispatcher is nil
   (nil-guard pattern -- callers check before use).

**Gate evaluation and dispatch (on each controller tick):**

1. `dispatch()` iterates all non-manual automations.
2. `CheckGate()` evaluates the gate condition against current time,
   last-run history (from bead store), and event state (from event bus).
3. For each due automation, a tracking bead is created **synchronously**
   with label `automation-run:<scopedName>`. This is critical: it
   prevents the cooldown gate from re-firing on the next tick.
4. A goroutine calls `dispatchOne()` with a context timeout derived from
   `effectiveTimeout()` (per-automation timeout capped by global
   `max_timeout`).
5. `dispatchOne()` records an `automation.fired` event, then branches:
   - **Exec**: `dispatchExec()` runs the shell command via `ExecRunner`,
     labels the tracking bead with `exec` (or `exec-failed`), and
     records `automation.completed` or `automation.failed`.
   - **Formula**: `dispatchWisp()` calls `instantiateWisp()` (which
     delegates to `store.MolCook()`), labels the wisp root bead with
     `automation-run:<scopedName>` and `pool:<qualifiedPool>`, and
     records `automation.completed` or `automation.failed`.

**Scanning (`automations.Scan()`):**

1. For each formula layer (ordered lowest to highest priority), read
   `<layer>/automations/*/automation.toml`.
2. Parse each TOML file into an `Automation` struct. Set `Name` from the
   subdirectory name, `Source` from the absolute file path.
3. Higher-priority layers overwrite lower ones by name (map keyed on
   subdirectory name).
4. Exclude disabled automations (`enabled = false`) and those in the
   `skip` list.
5. Return the result slice preserving discovery order.

### Key Types

- **`Automation`** (`internal/automations/automation.go`): The parsed
  automation definition. Fields: Name, Description, Formula, Exec, Gate,
  Interval, Schedule, Check, On, Pool, Timeout, Enabled, Source, Rig.

- **`GateResult`** (`internal/automations/gates.go`): The outcome of a
  gate check. Fields: Due (bool), Reason (human-readable), LastRun
  (time.Time).

- **`automationDispatcher`** (`cmd/gc/automation_dispatch.go`): Interface
  with a single method `dispatch(ctx, cityPath, now)`. Production
  implementation is `memoryAutomationDispatcher`.

- **`memoryAutomationDispatcher`** (`cmd/gc/automation_dispatch.go`):
  Holds the scanned automation list, bead store, events provider, command
  runner, exec runner, events recorder, stderr writer, and max timeout.

- **`ExecRunner`** (`cmd/gc/automation_dispatch.go`): Function type
  `func(ctx, command, dir string, env []string) ([]byte, error)` for
  running shell commands. Production implementation `shellExecRunner`
  uses `os/exec`.

## Invariants

These properties must hold for the automation subsystem to be correct.
Violations indicate bugs.

- **Formula XOR Exec**: Every automation has exactly one of `formula` or
  `exec` set. `Validate()` rejects automations with both or neither.

- **Exec automations have no pool**: An exec automation runs a shell
  script directly on the controller. It has no agent pipeline and
  therefore no pool. `Validate()` rejects `exec` + `pool` combinations.

- **Gate type requires matching parameters**: A `cooldown` gate requires
  `interval`, a `cron` gate requires `schedule`, a `condition` gate
  requires `check`, an `event` gate requires `on`. `Validate()` enforces
  these per-gate-type constraints.

- **Tracking beads are created before dispatch goroutines**: The tracking
  bead (labeled `automation-run:<scopedName>`) is created synchronously
  in the main dispatch loop. This prevents the cooldown gate from
  re-firing on the next controller tick while the dispatch goroutine is
  still running.

- **ScopedName provides rig isolation**: The same automation name
  deployed to multiple rigs produces independent scoped names (e.g.,
  `dolt-health:rig:rig-a` vs `dolt-health:rig:rig-b`). Cooldown
  tracking, event cursors, and history queries all use ScopedName.
  Firing one rig's automation does not affect another rig's gate
  evaluation.

- **Higher-priority layers override lower by name**: When the same
  automation subdirectory name exists in multiple formula layers,
  `Scan()` uses the definition from the highest-priority layer (last in
  the layers slice). The override is total (the entire TOML definition
  replaces the lower one).

- **Manual gates never auto-fire**: `CheckGate()` for a `manual` gate
  always returns `Due: false`. Manual automations are filtered out of the
  dispatcher entirely during build. They can only be triggered via
  `gc automation run`.

- **Disabled automations are excluded from scan results**: `Scan()`
  filters out automations with `enabled = false`. They do not appear in
  any CLI command output or dispatch evaluation.

- **Cron gate fires at most once per minute**: After matching the 5-field
  schedule, `checkCron()` verifies the last run was not in the same
  truncated minute. This prevents duplicate fires within a single cron
  window.

- **Event gate uses cursor-based deduplication**: Event automations track
  the highest processed event sequence number via `seq:<N>` labels on
  wisp beads. Subsequent gate checks use `AfterSeq` filtering to avoid
  reprocessing already-handled events.

- **Dispatch is fire-and-forget**: Once a goroutine is launched, the
  controller does not track its completion. Failed automations emit
  `automation.failed` events but do not retry. The tracking bead
  prevents re-fire within the same cooldown window.

- **No role names in Go code**: The automation subsystem operates on
  config-driven pool names and formula references. No line of Go
  references a specific role name.

## Interactions

| Depends on | How |
|---|---|
| `internal/config` | `AutomationsConfig` for skip list and max timeout. `FormulaLayers` for formula directory resolution. `City` struct for config access. |
| `internal/events` | `Recorder` for emitting `automation.fired`, `automation.completed`, `automation.failed` events. `Provider` for event gate queries (`List` with `AfterSeq` filtering). |
| `internal/beads` | `Store` for creating tracking beads, querying last-run history (`ListByLabel`), and instantiating wisps (`MolCook`). `CommandRunner` for bd CLI invocation. |
| `internal/fsys` | `FS` interface for filesystem abstraction in `Scan()` (enables fake filesystem in tests). `OSFS` for production. |

| Depended on by | How |
|---|---|
| `cmd/gc/controller.go` | The controller loop calls `buildAutomationDispatcher()` on startup and config reload, then calls `dispatch()` on each tick. |
| `cmd/gc/cmd_automation.go` | CLI commands (`gc automation list/show/run/check/history`) use `automations.Scan()` and `automations.CheckGate()` for user-facing operations. |
| Health Patrol (`cmd/gc/`) | Automation dispatch is one phase of the Health Patrol tick cycle, running after agent reconciliation and wisp GC. |

## Code Map

| File | Responsibility |
|---|---|
| `internal/automations/automation.go` | `Automation` struct, `Parse()`, `Validate()`, `IsEnabled()`, `IsExec()`, `TimeoutOrDefault()`, `ScopedName()` |
| `internal/automations/gates.go` | `GateResult`, `CheckGate()`, `checkCooldown()`, `checkCron()`, `checkCondition()`, `checkEvent()`, `cronFieldMatches()`, `MaxSeqFromLabels()` |
| `internal/automations/scanner.go` | `Scan()` -- discovers automations across formula layers with priority override |
| `cmd/gc/automation_dispatch.go` | `automationDispatcher` interface, `memoryAutomationDispatcher`, `buildAutomationDispatcher()`, `dispatch()`, `dispatchOne()`, `dispatchExec()`, `dispatchWisp()`, `effectiveTimeout()`, `rigExclusiveLayers()`, `qualifyPool()`, `ExecRunner`, `shellExecRunner` |
| `cmd/gc/cmd_automation.go` | CLI commands: `gc automation list`, `show`, `run`, `check`, `history`. Helper functions: `loadAutomations()`, `loadAllAutomations()`, `cityFormulaLayers()`, `findAutomation()`, `automationLastRunFn()`, `bdCursorFunc()` |

## Configuration

Automations are defined as `automation.toml` files inside formula
directories following the structure
`<formulaDir>/automations/<name>/automation.toml`. The `[automations]`
section in `city.toml` controls global automation behavior.

### automation.toml (per-automation definition)

```toml
[automation]
description = "Check database health"
formula = "mol-db-health"        # dispatch action (XOR with exec)
# exec = "scripts/check-db.sh"   # alternative: shell script dispatch
gate = "cooldown"                # cooldown | cron | condition | event | manual
interval = "5m"                  # required for cooldown gate
# schedule = "0 3 * * *"         # required for cron gate (5-field)
# check = "test -f /tmp/flag"    # required for condition gate
# on = "bead.closed"             # required for event gate
pool = "worker"                  # target pool for formula dispatch (optional)
timeout = "90s"                  # per-automation timeout (optional)
enabled = true                   # default: true
```

### city.toml (global settings)

```toml
[automations]
skip = ["noisy-automation"]      # automation names to exclude from scanning
max_timeout = "120s"             # hard cap on per-automation timeout (default: uncapped)
```

### Automation layering (override priority, lowest to highest)

The formula layer order determines which `automation.toml` wins when the
same automation name exists in multiple layers:

1. **City pack formulas** -- from pack referenced in `city.toml`
2. **City local formulas** -- from `[formulas]` section or `.gc/formulas/`
3. **Rig pack formulas** -- from pack applied to a specific rig
4. **Rig local formulas** -- from rig's `formulas_dir`

A higher-numbered layer completely replaces a lower-numbered layer's
definition for the same automation name. This enables packs to
define defaults that operators override locally.

### Rig-scoped automations

When a rig has rig-exclusive formula layers (layers beyond the city
prefix), automations found in those layers are stamped with the rig
name. This produces independent scoped tracking:

- Same automation deployed to rigs `rig-a` and `rig-b` tracks
  independently as `db-health:rig:rig-a` and `db-health:rig:rig-b`.
- Pool names are auto-qualified: `pool = "worker"` in rig `demo-repo`
  becomes `pool:demo-repo/worker` on the wisp label. Already-qualified
  names (containing `/`) are left unchanged.

## Testing

The automation subsystem has comprehensive unit tests across three test
files in the library and two in the CLI:

| Test file | Coverage |
|---|---|
| `internal/automations/automation_test.go` | Parse (formula, exec, event automations), Validate (all gate types, mutual exclusion, missing fields, timeout validation), IsEnabled default/explicit, IsExec, TimeoutOrDefault (defaults and custom), ScopedName (city and rig) |
| `internal/automations/gates_test.go` | CheckGate for all five gate types: cooldown (never run, due, not due), cron (matched, not matched, already run this minute), condition (pass, fail), event (due, with cursor, cursor past all, not due, nil provider), rig-scoped gates (cooldown, cron, event use ScopedName), MaxSeqFromLabels (various label configurations) |
| `internal/automations/scanner_test.go` | Scan (basic discovery, empty layers, layer override priority, skip list, disabled filtering, source path recording) |
| `cmd/gc/automation_dispatch_test.go` | Dispatcher nil-guard (no automations, manual-only), cooldown dispatch (due, not due, multiple), exec dispatch (due, failure, cooldown, AUTOMATION_DIR env, timeout), rig-scoped dispatch (rig stamping, independent cooldown, qualified pool), rigExclusiveLayers, qualifyPool, effectiveTimeout (default, custom, capped) |
| `cmd/gc/cmd_automation_test.go` | CLI commands: list (empty, with data, exec type), show (found, not found), check (due, not due), history, findAutomation |

All tests use in-memory fakes (`fsys.NewFake()`, `beads.NewMemStore()`,
stubbed `ExecRunner`, `memRecorder`) with no external infrastructure
dependencies. Condition gate tests use real `sh -c true` and `sh -c false`
commands. See `TESTING.md` for the overall testing philosophy and tier
boundaries.

## Known Limitations

- **No retry on dispatch failure**: Failed automations emit events but
  are not retried. The tracking bead prevents re-fire within the same
  cooldown window, so a failed automation must wait for the next gate
  opening.

- **Cron granularity is minutes**: The cron gate operates at
  minute-level granularity with simple field matching (`*`, exact
  integer, comma-separated values). It does not support ranges (`1-5`),
  steps (`*/5`), or sub-minute scheduling.

- **Condition gate blocks the dispatch loop**: `checkCondition()` runs
  `sh -c <check>` synchronously during gate evaluation. A slow check
  command blocks evaluation of subsequent automations on that tick.

- **Event gate cursor is per-wisp, not per-dispatch**: The cursor
  position is computed from `seq:<N>` labels on existing wisp beads via
  `MaxSeqFromLabels()`. If wisp creation fails, the cursor is not
  advanced, which may cause duplicate event processing on retry.

- **No hot-add of automations**: Automation discovery runs on controller
  start and config reload (via fsnotify). Adding a new
  `automation.toml` file requires the config directory watcher to
  trigger a reload; adding a new formula layer directory requires a
  `city.toml` change.

- **Fire-and-forget goroutines**: Dispatch goroutines are not tracked
  by the controller. On shutdown, in-flight dispatches may be
  interrupted mid-execution if the context is canceled.

## See Also

- [Architecture glossary](./glossary.md) -- authoritative definitions
  of automation, gate, wisp, formula, and other terms used in this
  document
- [Health Patrol architecture](./health-patrol.md) -- the controller
  loop that drives automation dispatch on each tick
- [Beads architecture](./beads.md) -- the bead store used for tracking
  beads, wisp instantiation via MolCook, and label-based queries
- [Config architecture](./config.md) -- FormulaLayers resolution,
  pack expansion, and AutomationsConfig
- [Gate evaluation logic](../../internal/automations/gates.go) --
  CheckGate implementation for all five gate types
- [Automation discovery](../../internal/automations/scanner.go) --
  Scan function for formula layer traversal
- [Controller dispatch](../../cmd/gc/automation_dispatch.go) --
  production dispatcher wiring exec and formula automations
- [Event type constants](../../internal/events/events.go) --
  automation.fired, automation.completed, automation.failed event types
