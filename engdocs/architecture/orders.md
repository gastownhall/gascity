---
title: "Orders"
---


> Last verified against code: 2026-03-01

## Summary

Orders are Gas City's derived mechanism (Layer 2-4, part of Formulas
& Molecules) for scheduled and event-driven work dispatch without human
intervention. Each order pairs a trigger condition (when to fire) with
an action (a shell script or formula wisp), living as an
`order.toml` file inside formula directories. The controller
evaluates all non-manual triggers on every patrol tick and dispatches due
orders -- exec orders run shell scripts directly with no LLM
involvement, while formula orders instantiate wisps dispatched to
agent pools.

## Key Concepts

- **Order**: A parsed definition from an `order.toml` file with
  a Name (derived from subdirectory name), a dispatch action (Formula or
  Exec, mutually exclusive), a Trigger type, trigger-specific parameters, and
  optional Pool routing. Defined in the `Order` struct at
  `internal/orders/order.go`.

- **Trigger**: The trigger condition that controls when an order fires.
  Five types exist: `cooldown` (minimum interval since last run), `cron`
  (5-field schedule matching), `condition` (shell command exits 0),
  `event` (matching events after a cursor position), and `manual`
  (explicit invocation only, never auto-fires). See
  `internal/orders/triggers.go`.

- **Exec Order**: An order whose action is a shell command
  (`exec` field) run directly by the controller. No LLM, no agent, no
  wisp. The script receives `ORDER_DIR` in its environment, set to
  the directory containing the `order.toml` file. Default timeout:
  60 seconds.

- **Formula Order**: An order whose action is a formula name
  (`formula` field). When the trigger opens, the controller calls `MolCook`
  to instantiate a wisp and labels it for pool dispatch. Default timeout:
  30 seconds.

- **ScopedName**: A rig-qualified key that creates unique identity for
  orders across rigs. City-level orders use the plain name
  (e.g., `dolt-health`). Rig-level orders append `:rig:<rigName>`
  (e.g., `dolt-health:rig:demo-repo`). ScopedName drives independent
  cooldown tracking, event cursors, and label scoping.

- **Formula Layer**: A directory scanned for `orders/*/order.toml`.
  Layers are ordered lowest to highest priority; a higher-priority layer's
  order definition overrides a lower-priority one with the same
  subdirectory name (last-wins semantics).

- **Tracking Bead**: A bead created synchronously before each dispatch
  goroutine launches, labeled `order-run:<scopedName>`. Serves dual
  purpose: prevents the cooldown trigger from re-firing on the next tick,
  and provides execution history for `gc order history`.

## Architecture

The order subsystem spans two packages:

- **`internal/orders/`** -- parsing, validation, scanning, and trigger
  evaluation. Pure library code with no side effects beyond shell command
  execution for condition triggers.
- **`cmd/gc/`** -- controller-side dispatch (`order_dispatch.go`)
  and CLI commands (`cmd_order.go`). Wires the library into the
  controller loop and the `gc order` command tree.

```
                ┌──────────────────────────────────────────────────┐
                │           Controller Tick                        │
                │           cmd/gc/controller.go                   │
                │                                                  │
                │  ┌────────────────────────────────────────────┐  │
                │  │  orderDispatcher.dispatch(ctx, now)    │  │
                │  │  cmd/gc/order_dispatch.go              │  │
                │  │                                             │  │
                │  │  for each order:                       │  │
                │  │    ┌───────────────────────────────────┐    │  │
                │  │    │ CheckTrigger(a, now, lastRunFn,      │    │  │
                │  │    │          ep, cursorFn)             │    │  │
                │  │    │ internal/orders/triggers.go      │    │  │
                │  │    └───────────┬───────────────────────┘    │  │
                │  │                │                             │  │
                │  │        ┌───────▼────────┐                   │  │
                │  │        │ TriggerResult.Due? │                   │  │
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
                │          Order Discovery (Startup)          │
                │                                                  │
                │  buildOrderDispatcher()                     │
                │    ├─ cityFormulaLayers(cfg)                     │
                │    ├─ orders.Scan(cityLayers)               │
                │    ├─ for each rig:                              │
                │    │   ├─ rigExclusiveLayers(rigLayers, city)    │
                │    │   ├─ orders.Scan(exclusive)            │
                │    │   └─ stamp Rig field on each order     │
                │    ├─ filter out manual-trigger orders          │
                │    └─ return memoryOrderDispatcher           │
                └──────────────────────────────────────────────────┘
```

### Data Flow

**Discovery (on controller start and config reload):**

1. `buildOrderDispatcher()` resolves city-level formula layers via
   `cityFormulaLayers()` and calls `orders.Scan()` to find city
   orders.
2. For each rig, `rigExclusiveLayers()` strips the city prefix from the
   rig's formula layers to avoid double-scanning city orders. The
   remaining rig-exclusive layers are scanned separately.
3. Rig orders get their `Rig` field stamped with the rig name.
4. Manual-trigger orders are filtered out (they never auto-dispatch).
5. If no auto-dispatchable orders remain, the dispatcher is nil
   (nil-guard pattern -- callers check before use).

**Trigger evaluation and dispatch (on each controller tick):**

1. `dispatch()` iterates all non-manual orders.
2. `CheckTrigger()` evaluates the trigger condition against current time,
   last-run history (from bead store), and event state (from event bus).
3. For each due order, a tracking bead is created **synchronously**
   with label `order-run:<scopedName>`. This is critical: it
   prevents the cooldown trigger from re-firing on the next tick.
4. A goroutine calls `dispatchOne()` with a context timeout derived from
   `effectiveTimeout()` (per-order timeout capped by global
   `max_timeout`).
5. `dispatchOne()` records an `order.fired` event, then branches:
   - **Exec**: `dispatchExec()` runs the shell command via `ExecRunner`,
     labels the tracking bead with `exec` (or `exec-failed`), and
     records `order.completed` or `order.failed`.
   - **Formula**: `dispatchWisp()` calls `instantiateWisp()` (which
     delegates to `store.MolCook()`), labels the wisp root bead with
     `order-run:<scopedName>` and `pool:<qualifiedPool>`, and
     records `order.completed` or `order.failed`.

**Scanning (`orders.Scan()`):**

1. For each formula layer (ordered lowest to highest priority), read
   `<layer>/orders/*/order.toml`.
2. Parse each TOML file into an `Order` struct. Set `Name` from the
   subdirectory name, `Source` from the absolute file path.
3. Higher-priority layers overwrite lower ones by name (map keyed on
   subdirectory name).
4. Exclude disabled orders (`enabled = false`) and those in the
   `skip` list.
5. Return the result slice preserving discovery order.

### Key Types

- **`Order`** (`internal/orders/order.go`): The parsed
  order definition. Fields: Name, Description, Formula, Exec, Trigger,
  Interval, Schedule, Check, On, Pool, Timeout, Enabled, Source, Rig.

- **`TriggerResult`** (`internal/orders/triggers.go`): The outcome of a
  trigger check. Fields: Due (bool), Reason (human-readable), LastRun
  (time.Time).

- **`orderDispatcher`** (`cmd/gc/order_dispatch.go`): Interface
  with a single method `dispatch(ctx, cityPath, now)`. Production
  implementation is `memoryOrderDispatcher`.

- **`memoryOrderDispatcher`** (`cmd/gc/order_dispatch.go`):
  Holds the scanned order list, bead store, events provider, command
  runner, exec runner, events recorder, stderr writer, and max timeout.

- **`ExecRunner`** (`cmd/gc/order_dispatch.go`): Function type
  `func(ctx, command, dir string, env []string) ([]byte, error)` for
  running shell commands. Production implementation `shellExecRunner`
  uses `os/exec`.

## Invariants

These properties must hold for the order subsystem to be correct.
Violations indicate bugs.

- **Formula XOR Exec**: Every order has exactly one of `formula` or
  `exec` set. `Validate()` rejects orders with both or neither.

- **Exec orders have no pool**: An exec order runs a shell
  script directly on the controller. It has no agent pipeline and
  therefore no pool. `Validate()` rejects `exec` + `pool` combinations.

- **Trigger type requires matching parameters**: A `cooldown` trigger requires
  `interval`, a `cron` trigger requires `schedule`, a `condition` trigger
  requires `check`, an `event` trigger requires `on`. `Validate()` enforces
  these per-trigger-type constraints.

- **Tracking beads are created before dispatch goroutines**: The tracking
  bead (labeled `order-run:<scopedName>`) is created synchronously
  in the main dispatch loop. This prevents the cooldown trigger from
  re-firing on the next controller tick while the dispatch goroutine is
  still running.

- **ScopedName provides rig isolation**: The same order name
  deployed to multiple rigs produces independent scoped names (e.g.,
  `dolt-health:rig:rig-a` vs `dolt-health:rig:rig-b`). Cooldown
  tracking, event cursors, and history queries all use ScopedName.
  Firing one rig's order does not affect another rig's trigger
  evaluation.

- **Higher-priority layers override lower by name**: When the same
  order subdirectory name exists in multiple formula layers,
  `Scan()` uses the definition from the highest-priority layer (last in
  the layers slice). The override is total (the entire TOML definition
  replaces the lower one).

- **Manual triggers never auto-fire**: `CheckTrigger()` for a `manual` trigger
  always returns `Due: false`. Manual orders are filtered out of the
  dispatcher entirely during build. They can only be triggered via
  `gc order run`.

- **Disabled orders are excluded from scan results**: `Scan()`
  filters out orders with `enabled = false`. They do not appear in
  any CLI command output or dispatch evaluation.

- **Cron trigger fires at most once per minute**: After matching the 5-field
  schedule, `checkCron()` verifies the last run was not in the same
  truncated minute. This prevents duplicate fires within a single cron
  window.

- **Event trigger uses cursor-based deduplication**: Event orders track
  the highest processed event sequence number via `seq:<N>` labels on
  wisp beads. Subsequent trigger checks use `AfterSeq` filtering to avoid
  reprocessing already-handled events.

- **Dispatch is fire-and-forget**: Once a goroutine is launched, the
  controller does not track its completion. Failed orders emit
  `order.failed` events but do not retry. The tracking bead
  prevents re-fire within the same cooldown window.

- **No role names in Go code**: The order subsystem operates on
  config-driven pool names and formula references. No line of Go
  references a specific role name.

## Interactions

| Depends on | How |
|---|---|
| `internal/config` | `OrdersConfig` for skip list and max timeout. `FormulaLayers` for formula directory resolution. `City` struct for config access. |
| `internal/events` | `Recorder` for emitting `order.fired`, `order.completed`, `order.failed` events. `Provider` for event trigger queries (`List` with `AfterSeq` filtering). |
| `internal/beads` | `Store` for creating tracking beads, querying last-run history (`ListByLabel`), and instantiating wisps (`MolCook`). `CommandRunner` for bd CLI invocation. |
| `internal/fsys` | `FS` interface for filesystem abstraction in `Scan()` (enables fake filesystem in tests). `OSFS` for production. |

| Depended on by | How |
|---|---|
| `cmd/gc/controller.go` | The controller loop calls `buildOrderDispatcher()` on startup and config reload, then calls `dispatch()` on each tick. |
| `cmd/gc/cmd_order.go` | CLI commands (`gc order list/show/run/check/history`) use `orders.Scan()` and `orders.CheckTrigger()` for user-facing operations. |
| Health Patrol (`cmd/gc/`) | Order dispatch is one phase of the Health Patrol tick cycle, running after agent reconciliation and wisp GC. |

## Code Map

| File | Responsibility |
|---|---|
| `internal/orders/order.go` | `Order` struct, `Parse()`, `Validate()`, `IsEnabled()`, `IsExec()`, `TimeoutOrDefault()`, `ScopedName()` |
| `internal/orders/triggers.go` | `TriggerResult`, `CheckTrigger()`, `checkCooldown()`, `checkCron()`, `checkCondition()`, `checkEvent()`, `cronFieldMatches()`, `MaxSeqFromLabels()` |
| `internal/orders/scanner.go` | `Scan()` -- discovers orders across formula layers with priority override |
| `cmd/gc/order_dispatch.go` | `orderDispatcher` interface, `memoryOrderDispatcher`, `buildOrderDispatcher()`, `dispatch()`, `dispatchOne()`, `dispatchExec()`, `dispatchWisp()`, `effectiveTimeout()`, `rigExclusiveLayers()`, `qualifyPool()`, `ExecRunner`, `shellExecRunner` |
| `cmd/gc/cmd_order.go` | CLI commands: `gc order list`, `show`, `run`, `check`, `history`. Helper functions: `loadOrders()`, `loadAllOrders()`, `cityFormulaLayers()`, `findOrder()`, `orderLastRunFn()`, `bdCursorFunc()` |

## Configuration

Orders are defined as `order.toml` files inside formula
directories following the structure
`<formulaDir>/orders/<name>/order.toml`. The `[orders]`
section in `city.toml` controls global order behavior.

### order.toml (per-order definition)

```toml
[order]
description = "Check database health"
formula = "mol-db-health"        # dispatch action (XOR with exec)
# exec = "scripts/check-db.sh"   # alternative: shell script dispatch
trigger = "cooldown"                # cooldown | cron | condition | event | manual
interval = "5m"                  # required for cooldown trigger
# schedule = "0 3 * * *"         # required for cron trigger (5-field)
# check = "test -f /tmp/flag"    # required for condition trigger
# on = "bead.closed"             # required for event trigger
pool = "worker"                  # target pool for formula dispatch (optional)
timeout = "90s"                  # per-order timeout (optional)
enabled = true                   # default: true
```

### city.toml (global settings)

```toml
[orders]
skip = ["noisy-order"]      # order names to exclude from scanning
max_timeout = "120s"             # hard cap on per-order timeout (default: uncapped)
```

### Order layering (override priority, lowest to highest)

The formula layer order determines which `order.toml` wins when the
same order name exists in multiple layers:

1. **City pack formulas** -- from pack referenced in `city.toml`
2. **City local formulas** -- from `[formulas]` section or `.gc/formulas/`
3. **Rig pack formulas** -- from pack applied to a specific rig
4. **Rig local formulas** -- from rig's `formulas_dir`

A higher-numbered layer completely replaces a lower-numbered layer's
definition for the same order name. This enables packs to
define defaults that operators override locally.

### Rig-scoped orders

When a rig has rig-exclusive formula layers (layers beyond the city
prefix), orders found in those layers are stamped with the rig
name. This produces independent scoped tracking:

- Same order deployed to rigs `rig-a` and `rig-b` tracks
  independently as `db-health:rig:rig-a` and `db-health:rig:rig-b`.
- Pool names are auto-qualified: `pool = "worker"` in rig `demo-repo`
  becomes `pool:demo-repo/worker` on the wisp label. Already-qualified
  names (containing `/`) are left unchanged.

## Testing

The order subsystem has comprehensive unit tests across three test
files in the library and two in the CLI:

| Test file | Coverage |
|---|---|
| `internal/orders/automation_test.go` | Parse (formula, exec, event orders), Validate (all trigger types, mutual exclusion, missing fields, timeout validation), IsEnabled default/explicit, IsExec, TimeoutOrDefault (defaults and custom), ScopedName (city and rig) |
| `internal/orders/triggers_test.go` | CheckTrigger for all five trigger types: cooldown (never run, due, not due), cron (matched, not matched, already run this minute), condition (pass, fail), event (due, with cursor, cursor past all, not due, nil provider), rig-scoped triggers (cooldown, cron, event use ScopedName), MaxSeqFromLabels (various label configurations) |
| `internal/orders/scanner_test.go` | Scan (basic discovery, empty layers, layer override priority, skip list, disabled filtering, source path recording) |
| `cmd/gc/order_dispatch_test.go` | Dispatcher nil-guard (no orders, manual-only), cooldown dispatch (due, not due, multiple), exec dispatch (due, failure, cooldown, ORDER_DIR env, timeout), rig-scoped dispatch (rig stamping, independent cooldown, qualified pool), rigExclusiveLayers, qualifyPool, effectiveTimeout (default, custom, capped) |
| `cmd/gc/cmd_order_test.go` | CLI commands: list (empty, with data, exec type), show (found, not found), check (due, not due), history, findOrder |

All tests use in-memory fakes (`fsys.NewFake()`, `beads.NewMemStore()`,
stubbed `ExecRunner`, `memRecorder`) with no external infrastructure
dependencies. Condition trigger tests use real `sh -c true` and `sh -c false`
commands. See `TESTING.md` for the overall testing philosophy and tier
boundaries.

## Known Limitations

- **No retry on dispatch failure**: Failed orders emit events but
  are not retried. The tracking bead prevents re-fire within the same
  cooldown window, so a failed order must wait for the next trigger
  opening.

- **Cron granularity is minutes**: The cron trigger operates at
  minute-level granularity with simple field matching (`*`, exact
  integer, comma-separated values). It does not support ranges (`1-5`),
  steps (`*/5`), or sub-minute scheduling.

- **Condition trigger blocks the dispatch loop**: `checkCondition()` runs
  `sh -c <check>` synchronously during trigger evaluation. A slow check
  command blocks evaluation of subsequent orders on that tick.

- **Event trigger cursor is per-wisp, not per-dispatch**: The cursor
  position is computed from `seq:<N>` labels on existing wisp beads via
  `MaxSeqFromLabels()`. If wisp creation fails, the cursor is not
  advanced, which may cause duplicate event processing on retry.

- **No hot-add of orders**: Order discovery runs on controller
  start and config reload (via fsnotify). Adding a new
  `order.toml` file requires the config directory watcher to
  trigger a reload; adding a new formula layer directory requires a
  `city.toml` change.

- **Fire-and-forget goroutines**: Dispatch goroutines are not tracked
  by the controller. On shutdown, in-flight dispatches may be
  interrupted mid-execution if the context is canceled.

## See Also

- [Architecture glossary](glossary.md) -- authoritative definitions
  of order, trigger, wisp, formula, and other terms used in this
  document
- [Health Patrol architecture](health-patrol.md) -- the controller
  loop that drives order dispatch on each tick
- [Beads architecture](beads.md) -- the bead store used for tracking
  beads, wisp instantiation via MolCook, and label-based queries
- [Config architecture](config.md) -- FormulaLayers resolution,
  pack expansion, and OrdersConfig
- [Trigger evaluation logic](https://github.com/gastownhall/gascity/blob/main/internal/orders/triggers.go) --
  CheckTrigger implementation for all five trigger types
- [Order discovery](https://github.com/gastownhall/gascity/blob/main/internal/orders/scanner.go) --
  Scan function for formula layer traversal
- [Controller dispatch](https://github.com/gastownhall/gascity/blob/main/cmd/gc/order_dispatch.go) --
  production dispatcher wiring exec and formula orders
- [Event type constants](https://github.com/gastownhall/gascity/blob/main/internal/events/events.go) --
  order.fired, order.completed, order.failed event types
