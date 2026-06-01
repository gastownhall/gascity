---
title: "Dispatch (Sling)"
---

> Last verified against code: 2026-05-21

## Summary

Dispatch is Gas City's work routing mechanism -- a Layer 2-4 derived
mechanism that composes primitives (Session, Bead Store, Event Bus,
Config) to route work to agents. The `gc sling` command resolves a target
agent or pool, optionally instantiates a formula as a wisp, executes the
agent's sling query to route each bead, optionally wraps single beads in
a tracking convoy, records telemetry, and nudges the target. Convoys are
expanded to their open children before routing.

## Key Concepts

- **Sling**: The act of routing a bead to an agent or pool by executing
  the target's sling query. The sling query is a shell command template
  with `{}` as a placeholder for the bead ID. Implemented in
  `cmd/gc/cmd_sling.go`.

- **Sling Query**: A shell command template on each agent config
  (`sling_query`) that routes a bead to that agent. Defaults to
  `bd update {} --assignee=<qualified-name>` for fixed agents and
  `bd update {} --label=pool:<qualified-name>` for pool agents. The `{}`
  placeholder is replaced with the actual bead ID at dispatch time.
  Defined in `internal/config/config.go:EffectiveSlingQuery`.

- **Container Expansion**: When a convoy is slung, dispatch expands it
  to its open children and routes each child individually. Non-open
  children are skipped. Epics are ordinary beads and are not expanded.
  The container itself becomes the convoy -- no auto-convoy is created.

- **Auto-Convoy**: When slinging a single bead (not a formula, not a
  container), dispatch automatically wraps it in a new convoy bead for
  batch tracking. Suppressed with `--no-convoy`.

- **Wisp Instantiation**: When `--formula` is set, dispatch creates an
  ephemeral molecule (wisp) from the named formula via `Store.MolCook`
  and routes the wisp's root bead to the target. Variable substitution
  and custom titles are supported.

- **Target Resolution**: The 2-step resolution of agent names -- first
  literal match against qualified names, then contextual match using the
  current rig directory. Implemented in
  `cmd/gc/cmd_agent.go:resolveAgentIdentity`.

- **System Formula**: A formula embedded in the `gc` binary that is
  materialized to `.gc/system-formulas/` at startup. System formulas
  are always overwritten to stay in sync with the binary version. Stale
  files are cleaned up. Implemented in `cmd/gc/system_formulas.go`.

- **Review Quorum Formula**: `mol-review-quorum` is a core pack
  `graph.v2` formula that dispatches exactly two read-only reviewer
  lanes using formula-supplied lane IDs, providers, models, and targets,
  and then routes a synthesis step. Dispatch treats it like any other
  formula-backed wisp; it does not give
  `dx-review` lifecycle ownership.

## Architecture

Dispatch is not a separate Go package. It is a composition of primitives
orchestrated by `cmd/gc/cmd_sling.go`. The dispatch pipeline has three
layers:

```
CLI layer (cmd/gc/cmd_sling.go)
  |
  +-- cmdSling()          resolve city, config, agent, store
  |     |
  |     v
  +-- doSlingBatch()      container expansion (convoy -> children)
        |
        +-- doSling()     single-bead dispatch pipeline:
              |
              +-- instantiateWisp()     [if --formula] MolCook
              +-- checkBeadState()      [pre-flight] warn re-route
              +-- buildSlingCommand()   {} -> bead ID substitution
              +-- runner(slingCmd)      execute shell command
              +-- telemetry.RecordSling()
              +-- store.SetMetadata()   [if --merge] merge strategy
              +-- store.Create(convoy)  [if auto-convoy] tracking wrapper
              +-- doSlingNudge()        [if --nudge] wake the agent
```

### Data Flow

**Single bead dispatch** (`gc sling <agent> <bead-id>`):

1. `cmdSling` resolves the city path, loads config, and resolves the
   target agent via `resolveAgentIdentity`.
2. `doSlingBatch` checks if the bead is a container type. If not, falls
   through to `doSling`.
3. `doSling` warns about suspended agents or empty pools (unless
   `--force`).
4. If `--formula`, calls `instantiateWisp` which delegates to
   `Store.MolCook` to create the wisp and uses the root bead ID.
5. Pre-flight: warns if the bead already has an assignee or pool labels
   (unless `--force`).
6. Builds the sling command by replacing `{}` in the agent's
   `EffectiveSlingQuery()` with the bead ID.
7. Executes the sling command via `SlingRunner` (shell `sh -c`).
8. Records telemetry via `telemetry.RecordSling`.
9. If `--merge` is set, writes the merge strategy as bead metadata.
10. If auto-convoy is enabled (not `--no-convoy`, not `--formula`),
    creates a convoy bead and sets the routed bead's ParentID to the
    convoy.
11. If `--nudge`, sends a nudge to the target agent.

**Container expansion** (`gc sling <agent> <convoy-id>`):

1. `doSlingBatch` looks up the bead and checks `IsContainerType`.
2. Lists all children via `querier.Children`.
3. Partitions children into open (routable) and skipped (non-open).
4. Routes each open child individually through `buildSlingCommand` +
   `runner`. No auto-convoy is created -- the container IS the convoy.
5. Reports per-child success/failure and a summary line.
6. Nudges once after all children are routed (if `--nudge` and at
   least one succeeded).

### Key Types

- **`SlingOpts`** (`cmd/gc/cmd_sling.go`) -- All flags for the sling
  command: `IsFormula`, `DoNudge`, `Force`, `Title`, `Vars`, `Merge`,
  `NoConvoy`, `Owned`.

- **`SlingRunner`** (`cmd/gc/cmd_sling.go`) -- Function type
  `func(command string) (string, error)` that executes the sling shell
  command. Injectable for testing.

- **`BeadQuerier`** (`cmd/gc/cmd_sling.go`) -- Interface for retrieving
  a single bead by ID. Used for pre-flight checks.

- **`BeadChildQuerier`** (`cmd/gc/cmd_sling.go`) -- Extends `BeadQuerier`
  with `Children(parentID)` for container expansion.

- **`config.Agent`** (`internal/config/config.go`) -- Carries the
  `SlingQuery` field and `EffectiveSlingQuery()` method that determines
  how beads are routed to this agent.

## scale_check ↔ work_query correspondence

Dispatch has two read sides that must stay symmetric:

- **Reconciler (spawn side)**: when deciding whether a pool template
  needs another session, the controller invokes
  `Agent.EffectivePoolDemandQuery` (a thin pass-through that
  `EffectiveScaleCheck` also returns) and parses the count.
- **Worker (claim side)**: when an ephemeral session boots, `gc hook`
  invokes `Agent.EffectiveWorkQuery`, whose Tier 3 fires for unassigned
  routed work after the assignee tiers fall through.

Both forms answer the same question — "is there ready, unassigned,
non-epic work routed to this pool-demand target?" — and must therefore
observe the bead store through the same filter. They share target
resolution and the predicate: `bdReadyPoolDemandShell(limitFlag)`
returns `bd ready --include-ephemeral --metadata-field
"$key=$target" --unassigned --exclude-type=epic --json <limitFlag>`.
The caller provides `key` and `target` as shell variables. The work-query
form checks `gc.run_target` first, then the compatibility fallback
`gc.routed_to`; each tier appends `--sort oldest --limit=1`.
That is an intentional routed-queue policy: unassigned routed pool work is
FIFO before priority, so newer high-priority work does not jump ahead of
older ready work already queued for the same target. The count form uses
the same key precedence and predicate, appends `--limit 0`, and pipes
through `jq 'length'`. Targets resolve to `Agent.PoolName` when set and
`Agent.QualifiedName()` otherwise, so pool instances and pool templates
land on the same routed queue.

Supported handoff forms are intentionally distinct. Generic pool demand is
ready work with `assignee=""` and `gc.routed_to=<target>`; assigning the
pool template itself is not pool demand. Direct named-session delivery is
ready work with `assignee=<named-session-identity>` and no generic route
metadata, so the reconciler does not also treat the handoff as generic pool
demand.

The shared predicate is the agreement substrate. Failure envelopes
intentionally differ: the worker path suppresses `bd ready` stderr and
returns `[]` so a session exits cleanly, while the count form propagates
the failure to `evaluatePool`, which records telemetry and falls back to
the pool minimum.

Diverging the two — for example, by adding a state filter to the
work-query without updating the count form — re-introduces the
protocol-mismatch class. Pre-PR #1516 symptom: the reconciler counted
molecule-typed beads as demand while the worker's `bd ready` skipped
them, so spawned sessions exited immediately and the reconciler
re-spawned, producing spawn storms.

PR #1516 retired the old molecule-counting tier from the count form. A
later gc-udx change added `--exclude-type=epic` to the worker claim
path, leaving the default count form one filter behind. This refactor
does two things: routes both paths through `bdReadyPoolDemandShell` and
adds `--exclude-type=epic` to the default count form. Custom
`scale_check` overrides are unchanged. Future predicate changes should
be single-helper changes; tests `TestPoolDemandPredicateSharedWithWorkQuery`
(structural) and `TestPoolDemandAndWorkQueryAgreeOnRoutedSemantics`
(behavioral) guard against regressions.

## Invariants

1. **Sling query placeholder is always `{}`.** The `buildSlingCommand`
   function performs literal string replacement of all `{}` occurrences
   with the bead ID. No other placeholder syntax is supported.

2. **Container expansion routes only open children.** Children with
   status other than `"open"` are skipped and reported, never routed.

3. **Auto-convoy is suppressed for formulas and containers.** When
   `--formula` is set or the target bead is a container type, no
   auto-convoy is created. Formulas have their own molecule structure;
   containers are their own convoy.

4. **`--owned` requires a convoy.** The `--owned` and `--no-convoy`
   flags are mutually exclusive. The CLI rejects the combination before
   dispatch begins.

5. **Merge strategy is one of three values.** `--merge` accepts only
   `"direct"`, `"mr"`, or `"local"`. The CLI validates before dispatch.

6. **Pre-flight warnings are best-effort.** If the bead store query
   fails, dispatch proceeds silently. Warnings never block routing.

7. **Telemetry records every dispatch attempt.** `RecordSling` is called
   on both success and failure paths with the target name, target type
   (`"agent"` or `"pool"`), method (`"bead"`, `"formula"`, or `"batch"`),
   and error status.

8. **Pool nudge targets the first running instance.** When nudging a
   pool, dispatch iterates pool instances in order and nudges the first
   one with a running session. If none are running, a warning is emitted.

9. **System formulas are idempotent.** `MaterializeSystemFormulas`
   always overwrites files to match the binary version and removes stale
   formula files that are no longer embedded. Non-formula files in the
   directory are left alone.

10. **Default sling queries differ by agent type.** Fixed agents default
    to `bd update {} --assignee=<name>`; pool agents default to
    `bd update {} --label=pool:<name>`. Custom `sling_query` overrides
    the default entirely.

11. **scale_check ↔ work_query correspondence.** The reconciler's
    pool-demand-detection path (`Agent.EffectivePoolDemandQuery`, count
    form) and the worker's claim path (`Agent.EffectiveWorkQuery`, Tier
    3 first-row form) MUST derive their `bd ready --include-ephemeral
    --metadata-field <key>=<target> --unassigned --exclude-type=epic
    --json` predicate from the same target-resolution helper and
    `bdReadyPoolDemandShell` helper in `internal/config/config.go`. The
    worker and reconciler must use the same `gc.run_target` then
    `gc.routed_to` key precedence; only the worker's first-row form adds
    native `bd ready --sort oldest --limit=1` selection. Any pool-demand
    predicate change to one (added filter, modified target resolution, new
    state) MUST be reflected in the other. Diverging the two re-introduces
    the protocol-mismatch class — the reconciler
    spawning sessions for work the worker can't claim, or the worker idle
    while new demand sits unspawned. The legacy `workflow-control` fallback is
    worker-only for pre-rename `control-dispatcher` graphs and intentionally
    lives outside the shared primary pool-demand predicate. Enforced by
    `TestPoolDemandPredicateSharedWithWorkQuery` and
    `TestPoolDemandAndWorkQueryAgreeOnRoutedSemantics` in
    `internal/config/config_test.go`. Historical context: PR #1516
    removed the old molecule-counting tier from the count form; a later
    gc-udx change added `--exclude-type=epic` to the worker path; this
    refactor adds that filter to the default count form and makes the
    equivalence structural rather than coincidental.

## Interactions

| Depends on | How |
|---|---|
| `internal/beads` (Store) | `MolCook` for wisp instantiation, `Create` for auto-convoy, `Get`/`Children` for container expansion, `Update` for ParentID linking, `SetMetadata` for merge strategy |
| `internal/config` | Agent resolution, `EffectiveSlingQuery`, pool detection via `IsPool`, `PoolConfig` for sizing, `Suspended` flag |
| `internal/runtime` | `Provider.IsRunning` and `Provider.Nudge` for agent nudging via `doSlingNudge` |
| `internal/agent` | `SessionNameFor` to compute session names |
| `internal/worker` | `Handle.Nudge` at the worker boundary for direct nudge delivery |
| `internal/telemetry` | `RecordSling` for metrics and log events on every dispatch |
| `cmd/gc/cmd_agent.go` | `resolveAgentIdentity` for 2-step target resolution (literal then contextual) |

| Depended on by | How |
|---|---|
| `cmd/gc/cmd_convoy.go` | Convoys are the batch tracking containers that dispatch creates and expands |
| `internal/orders` | Order dispatch creates wisps and routes them through the same formula instantiation path (`Store.MolCook`) |
| `cmd/gc/cmd_handoff.go` | Work handoff between agents uses similar agent resolution and bead routing patterns |
| Controller | The controller's reconciliation loop drives pool sizing via `evaluatePool` which determines how many pool instances exist to receive slung work |

## Code Map

| Path | Description |
|---|---|
| `cmd/gc/cmd_sling.go` | CLI command, `SlingOpts`, `doSling`, `doSlingBatch`, `buildSlingCommand`, `instantiateWisp`, `checkBeadState`, `doSlingNudge` |
| `cmd/gc/cmd_sling_test.go` | Unit tests: command building, single-bead dispatch, formula dispatch, container expansion, nudge behavior, merge strategy, auto-convoy, pre-flight warnings |
| `cmd/gc/cmd_convoy.go` | Convoy CRUD: create, list, status, add, close, check (auto-close), stranded, autoclose (hidden hook) |
| `cmd/gc/system_formulas.go` | `MaterializeSystemFormulas`, `ListEmbeddedSystemFormulas`, stale file cleanup |
| `cmd/gc/system_formulas_test.go` | Tests for materialization: empty FS, write, overwrite, stale cleanup, idempotency, orders |
| `cmd/gc/pool.go` | `evaluatePool` (scale check), `poolAgents` (instance expansion), `expandSessionSetup` (template context) |
| `internal/config/config.go` | `Agent.SlingQuery`, `Agent.EffectiveSlingQuery()`, `Agent.EffectiveWorkQuery()`, `Agent.EffectivePoolDemandQuery()`, `Agent.EffectiveScaleCheck()`, `bdReadyPoolDemandShell()`, `Agent.IsPool()` |
| `internal/beads/beads.go` | `IsContainerType`, `Store.MolCook`, `Store.Children`, `Store.SetMetadata` |
| `internal/beads/bdstore.go` | `BdStore.MolCook` and `BdStore.MolCookOn` -- formula-backed wisp instantiation via `bd mol wisp` / `bd mol bond` |
| `internal/telemetry/recorder.go` | `RecordSling` -- metrics counter + structured log event for each dispatch |
| `cmd/gc/cmd_agent.go` | `resolveAgentIdentity` -- 2-step agent name resolution |

## Configuration

The dispatch mechanism is configured through agent-level fields in
`city.toml`:

```toml
[[agent]]
name = "worker"

# Custom sling query (optional -- has sensible defaults).
# {} is replaced with the bead ID at dispatch time.
sling_query = "bd update {} --assignee=worker"

# Custom work query (the read side of dispatch).
# Pool agents must set both sling_query and work_query, or neither.
work_query = "bd ready --assignee=worker --limit=1"

# Nudge text sent to wake the agent after routing.
nudge = "Work slung. Check your hook."
```

Pool agents with default queries:

```toml
[[agent]]
name = "coder"
pool = { min = 1, max = 3, check = "echo 2" }
# Default sling_query: bd update {} --set-metadata gc.routed_to=coder
# Default work_query: bd ready --include-ephemeral --metadata-field gc.run_target=coder
#   --unassigned --exclude-type=epic --json --sort oldest --limit=1,
#   then gc.routed_to fallback
```

System formulas are embedded in the `gc` binary and materialized to
`.gc/system-formulas/` at startup. They form the lowest-priority formula
layer (Layer 0) in the formula resolution stack. Pack and city-level
formulas override system formulas by name.

`mol-review-quorum` is provided by the core pack formula layer. Its reviewer
lane IDs, providers, models, and dispatch targets are all supplied through
formula vars; the synthesis target is configured separately with
`synthesis_target`. Each reviewer lane is expected to produce durable structured
output with verdict, findings, evidence, usage, failure classification, and
read-only mutation-baseline delta.
The synthesis step writes the combined `review-quorum.summary.v1` state for
future consumers such as `dx-review summarize`. The `internal/reviewquorum`
Go finalizer defines the durable contract but is not invoked by formula
synthesis yet.

Read-only mutation checks are baseline-relative. Dispatch and review consumers
must compare reviewer after-state to the reviewer-recorded before baseline, not
to an absolute clean-worktree expectation; pre-existing untracked files are not
reviewer-created mutations.

## Testing

Dispatch testing follows the philosophy in
[TESTING.md](https://github.com/gastownhall/gascity/blob/main/TESTING.md), relying heavily on injected fakes:

**Unit tests** (`cmd/gc/cmd_sling_test.go`): All dispatch logic is tested
through `doSling` and `doSlingBatch` with injected `fakeRunner` (records
shell commands), `session.NewFake()` (fake session provider), and
`beads.NewMemStore()` (in-memory bead store). Tests cover:

- `buildSlingCommand` placeholder substitution including multiple `{}`
- Single-bead dispatch to fixed agents and pools
- Formula dispatch with `--formula` flag (wisp instantiation)
- Container expansion: convoy beads expand to open children; epics are rejected
- Merge strategy metadata (`--merge=direct`, `--merge=mr`, `--merge=local`)
- Auto-convoy creation and suppression (`--no-convoy`)
- Owned convoy labeling (`--owned`)
- Pre-flight warnings for already-assigned beads and pool-labeled beads
- Suspended agent and empty pool warnings
- Nudge delivery to fixed agents and first running pool member
- Error paths: runner failure, MolCook failure, store failure

**System formula tests** (`cmd/gc/system_formulas_test.go`): Cover
materialization from embedded FS including empty FS (no-op), file
writing, overwrite semantics, stale file cleanup, idempotency, and
order subdirectory support.

**Config tests** (`internal/config/config_test.go`):
`TestEffectiveSlingQuery*` tests verify default sling query generation
for fixed agents, rig-scoped agents, pool agents, and custom overrides.
`TestValidatePoolWorkQueryMismatch` verifies that pool agents must set
both `sling_query` and `work_query` together or neither.

## Known Limitations

- **Sling query is a shell command, not a Go function call.** Every
  dispatch forks a shell process via `sh -c`. This is simple and
  flexible (any CLI tool can be a routing backend) but adds per-bead
  fork overhead during batch expansion of large containers.

- **Container expansion is serial.** When expanding a convoy,
  each child is slung sequentially. A single slow or failing sling
  command blocks subsequent children. Partial success is reported but
  not retried.

- **No built-in load balancing across pool instances.** Sling routes to
  the pool as a whole (via label), not to a specific instance. Work
  distribution depends on the pool's work query and claim semantics
  (`bd ready --label=pool:<name> --unassigned --limit=1`), which is first-come
  first-served.

- **Nudge targets only one pool instance.** After slinging to a pool,
  `--nudge` wakes the first running instance found. Other instances
  discover work on their next poll cycle.

- **No dry-run mode.** There is no way to preview what a sling command
  would do without actually executing it. The pre-flight `checkBeadState`
  only warns; it does not prevent routing.

## See Also

- [Architecture glossary](glossary.md) -- authoritative definitions of
  sling, convoy, wisp, formula, and other terms used in this document
- [Bead Store architecture](beads.md) -- the persistence substrate that
  dispatch reads and writes through, including MolCook molecule
  instantiation
- [Health Patrol architecture](health-patrol.md) -- the supervision
  model that keeps pool agents alive to receive dispatched work
- [Config architecture](config.md) -- how agent configuration
  (sling_query, pool, suspended) drives dispatch behavior
- [CLAUDE.md](https://github.com/gastownhall/gascity/blob/main/CLAUDE.md) -- design principles including "the
  controller drives all SDK infrastructure operations" (layering
  invariant 6)
- [Formula file reference](../../docs/reference/formula.md) -- formula structure,
  layer resolution, and wisp instantiation inputs
- [TESTING.md](https://github.com/gastownhall/gascity/blob/main/TESTING.md) -- testing philosophy and tier
  boundaries for the fake-injection approach used in dispatch tests
