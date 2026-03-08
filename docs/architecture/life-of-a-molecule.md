# Life of a Molecule

<!--
End-to-end trace of a molecule through its entire lifecycle. Modeled after
CockroachDB's "Life of a SQL Query." Names every function, file, and state
transition.

Audience: Gas City contributors (human and LLM agent).
Update this document when the implementation changes.
-->

> Last verified against code: 2026-03-01

## Introduction

A molecule is a formula instantiated at runtime as a bead tree. This
document traces a single molecule from the `*.formula.toml` file on disk,
through parsing, resolution, instantiation as beads, assignment to an
agent, step-by-step execution, completion, and garbage collection. It also
traces the parallel automation path where a gate fires and the controller
creates a wisp without any human invocation.

```
Phase:    1 Def   2 Resolve   3 Cook   4 Assign   5 Steps   6 Done   7 GC
          ─────── ────────── ──────── ────────── ──────── ──────── ────────
Root         .         .       open     open      open    closed   deleted
Step 1       .         .       open     open     closed   closed   deleted
Step 2       .         .       open     open      open    closed   deleted
Step 3       .         .       open     open      open    closed   deleted
```

## Phase 1: Definition

Every molecule begins as a `*.formula.toml` file declaring a name and
steps with dependency edges:

```toml
formula = "code-review"
description = "Multi-step code review workflow"

[[steps]]
id = "analyze"
title = "Analyze changes"
description = "Review the diff for {{repo}}"

[[steps]]
id = "test"
title = "Run tests"
needs = ["analyze"]

[[steps]]
id = "report"
title = "Write report"
needs = ["test"]
```

**Parsing:** `formula.Parse()` (`internal/formula/formula.go`) decodes
TOML bytes into a `Formula` struct with `Name`, `Description`, and
`Steps []Step` (each Step has ID, Title, Description, Needs).

**Validation:** `formula.Validate()` (`internal/formula/validate.go`)
runs five checks: non-empty name, at least one step, no duplicate step
IDs, all Needs references resolve, and no dependency cycles (Kahn's
topological sort).

## Phase 2: Resolution

Formulas are discovered through a 4-layer priority system computed by
`config.ComputeFormulaLayers()` (`internal/config/pack.go`):

| Priority | Layer | Source |
|----------|-------|--------|
| 1 (lowest) | City pack | `formulas/` from city-level packs |
| 2 | City local | `[formulas] dir` in `city.toml` |
| 3 | Rig pack | `formulas/` from rig-level packs |
| 4 (highest) | Rig local | `formulas_dir` in `[[rigs]]` entry |

City-scoped agents see layers 1-2. Rig-scoped agents see all four.

**Symlink materialization:** `ResolveFormulas()` (`cmd/gc/formula_resolve.go`)
iterates layers lowest-to-highest, building a filename-to-path winner map
where later layers overwrite earlier entries. Winners are symlinked into
`<targetDir>/.beads/formulas/` so bd finds them natively. Stale symlinks
are cleaned; real files are never overwritten.

## Phase 3: Instantiation (Cooking)

Cooking turns a static formula into a live bead tree. Two entry points
converge on the same function.

**CLI path:** `gc sling agent-1 code-review --formula --var repo=gascity`
enters `cmdSling()` (`cmd/gc/cmd_sling.go`), which resolves the city,
loads config, finds the agent, creates a BdStore, and calls
`instantiateWisp()` -- a thin wrapper over `store.MolCook()`.

**BdStore.MolCook()** (`internal/beads/bdstore.go`) shells out:

```sh
bd mol cook --formula=code-review --title="Sprint work" --var repo=gascity
```

bd reads the formula via the symlink, substitutes `{{repo}}`, creates a
root bead (Type=`molecule`, Ref=formula name, Status=`open`) and one
child bead per step (Type=`task`, ParentID=root, Ref=step ID, Needs
preserved). Returns the root bead ID on stdout.

**ComposeMolCook()** (`internal/formula/compose.go`) is the alternative
for stores without native formula support: resolves the formula via a
`Resolver`, applies `SubstituteVars()` (shallow copy with `{{key}}`
replacement), then issues `store.Create()` calls for root + steps.

**State after cooking:**

| Bead | Type | Status | Ref | ParentID |
|------|------|--------|-----|----------|
| BD-1 (root) | molecule | open | code-review | -- |
| BD-2 | task | open | analyze | BD-1 |
| BD-3 | task | open | test | BD-1 |
| BD-4 | task | open | report | BD-1 |

## Phase 4: Assignment

Back in `doSling()`, after cooking returns root ID `BD-1`:

1. `buildSlingCommand()` replaces `{}` in the agent's `sling_query`
   template with the bead ID. Default for fixed agents:
   `bd update BD-1 --assignee=agent-1`. Default for pools:
   `bd update BD-1 --label=pool:my-rig/worker`.
2. `shellSlingRunner()` executes the command, stamping the root bead.
3. Optional: `doSlingNudge()` sends text to the agent's tmux session.

**Hook discovery:** When the agent checks its hook (`gc hook`), it
queries the store (e.g., `bd list --assignee=<name> --status=open` or
`bd ready --label=pool:<name>`). It finds the molecule root bead. Per
GUPP: "If you find work on your hook, YOU RUN IT."

## Phase 5: Step Execution

**CurrentStep()** (`internal/formula/formula.go`): builds a set of closed
step Refs, then returns the first open step whose Needs are all in that
set. For our example, `analyze` (no Needs) is returned first; `test` and
`report` are blocked.

The agent executes the step's instructions, then closes it:
`bd close BD-2` (via `BdStore.Close()`: `bd close --json BD-2`).

**Progression:** After closing `analyze`, `CurrentStep()` returns `test`
(its Needs are now met). After closing `test`, returns `report`. After
closing `report`, returns nil (all done).

**Helpers:** `CompletedCount(steps)` counts closed step beads (for "2/3
steps complete"). `StepIndex(steps, ref)` returns the 1-based position.

**Burning:** Some workflows use `bd mol burn <wisp-id> --force` to close
the root and all step beads in one operation, regardless of step progress.

## Phase 6: Completion

When all step beads are closed, `CurrentStep()` returns nil. The agent
closes the root bead: `bd close BD-1`. The molecule is now fully closed.

The event bus records `bead.closed` events in `.gc/events.jsonl`. These
can trigger downstream automations with `gate = "event"`.

## Phase 7: Garbage Collection

Wisp GC purges closed molecules past their TTL, configured in
`city.toml`:

```toml
[daemon]
wisp_gc_interval = "5m"
wisp_ttl = "24h"
```

Both must be non-zero. At startup, `startController()` (`cmd/gc/controller.go`)
creates a `memoryWispGC` via `newWispGC()` (`cmd/gc/wisp_gc.go`).

On each tick: `shouldRun(now)` checks elapsed time against the interval.
When due, `runGC(cityPath, now)`:

1. Lists closed molecules: `bd list --json --limit=0 --status=closed --type=molecule`
2. Computes cutoff: `now - wisp_ttl`
3. Deletes each entry older than cutoff: `bd delete BD-1 --force`
4. Returns purged count (individual failures are best-effort)

## The Automation Path

Automations create wisps without `gc sling`. An automation lives at
`<formula-layer>/automations/<name>/automation.toml`:

```toml
[automation]
formula = "code-review"
gate = "cooldown"
interval = "1h"
pool = "reviewer"
```

**Scanning:** `automations.Scan()` (`internal/automations/scanner.go`)
walks `<layer>/automations/*/automation.toml` across formula layers.
Higher-priority layers override by name. Disabled and skipped automations
are excluded.

**Dispatcher build:** `buildAutomationDispatcher()` (`cmd/gc/automation_dispatch.go`)
scans city and per-rig layers, stamps rig automations with their `Rig`
field, and filters out manual-gate automations. Returns nil if none.

**Gate evaluation:** On each tick, `CheckGate()` (`internal/automations/gates.go`)
evaluates each automation: cooldown (elapsed time via
`store.ListByLabel("automation-run:<name>", 1)`), cron (schedule match),
condition (shell exit 0), event (new events past cursor), or manual
(always false).

**Dispatch:** When due, `dispatch()` creates a tracking bead synchronously
(preventing re-fire), then launches `dispatchOne()` in a goroutine:

1. Records `AutomationFired` event
2. Cooks wisp: `instantiateWisp()` -> `BdStore.MolCook()`
   (`bd mol cook --formula=code-review`)
3. Labels root bead: `bd update BD-1 --label=automation-run:<name> --label=pool:reviewer`
   (rig-scoped: `pool:my-rig/reviewer`)
4. Records `AutomationCompleted` event

The labeled wisp is discoverable by pool agents via their work query.
From here, lifecycle continues at Phase 5.

**Exec automations** bypass the molecule pipeline. The controller runs the
script directly; no wisp or agent is involved.

## Function Reference

| Phase | Function | File |
|-------|----------|------|
| Parse | `formula.Parse()` | `internal/formula/formula.go` |
| Validate | `formula.Validate()` | `internal/formula/validate.go` |
| Layer computation | `config.ComputeFormulaLayers()` | `internal/config/pack.go` |
| Symlink resolution | `ResolveFormulas()` | `cmd/gc/formula_resolve.go` |
| CLI entry | `cmdSling()` | `cmd/gc/cmd_sling.go` |
| Wisp creation | `instantiateWisp()` | `cmd/gc/cmd_sling.go` |
| BdStore cook | `BdStore.MolCook()` | `internal/beads/bdstore.go` |
| Composed cook | `formula.ComposeMolCook()` | `internal/formula/compose.go` |
| Var substitution | `formula.SubstituteVars()` | `internal/formula/compose.go` |
| Sling routing | `buildSlingCommand()` | `cmd/gc/cmd_sling.go` |
| Current step | `formula.CurrentStep()` | `internal/formula/formula.go` |
| Step progress | `formula.CompletedCount()` | `internal/formula/formula.go` |
| GC creation | `newWispGC()` | `cmd/gc/wisp_gc.go` |
| GC execution | `memoryWispGC.runGC()` | `cmd/gc/wisp_gc.go` |
| Automation scan | `automations.Scan()` | `internal/automations/scanner.go` |
| Gate evaluation | `automations.CheckGate()` | `internal/automations/gates.go` |
| Dispatcher build | `buildAutomationDispatcher()` | `cmd/gc/automation_dispatch.go` |
| Wisp dispatch | `dispatchWisp()` | `cmd/gc/automation_dispatch.go` |

## See Also

- [Formulas & Molecules](./formulas.md) -- reference doc for the formula
  pipeline, invariants, and configuration
- [Bead Store](./beads.md) -- MolCook across store backends
- [Dispatch](./dispatch.md) -- sling routing and container expansion
- [Automations](./automations.md) -- gate types, scanning, dispatch paths
- [Glossary](./glossary.md) -- authoritative definitions of all terms
