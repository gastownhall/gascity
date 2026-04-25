# Dependency Cycle Detection at Sling Time

Gas City rejects sling operations that would create a deadlock in the bead
dependency graph. The check runs as part of the standard preflight pass — before
any work is dispatched, before any session is woken, and before any bead state
changes.

## Why This Matters

Every bead can declare that it depends on other beads (via `bd dep add`). When an
agent picks up bead **A** and waits for bead **B** to close before proceeding, and
bead **B** waits for **A**, neither bead ever closes — a deadlock. With multi-bead
workflows this can be indirect: A→B→C→A. The cycle may span many hops and not be
obvious to the person slinging the work.

Catching cycles at sling time costs a single graph traversal against the bead
store. Discovering them at runtime costs human attention and session recovery.

## The Algorithm

Cycle detection uses a **three-color depth-first search** (white/gray/black) over
the `"down"` (depends-on) edges of the bead dependency graph:

1. Start at the bead being slung.
2. Mark it gray (in the current DFS stack).
3. For each scheduling dependency it declares, recurse.
4. If a gray node is encountered during traversal, a back-edge is found — that
   node is on the current stack, so following the edge would return to an
   ancestor. This is a cycle.
5. When all outgoing edges of a node are exhausted, mark it black (done).
   Black nodes are never revisited.

The DFS runs in O(V + E) time where V is the number of reachable beads and E is
the number of scheduling dependency edges between them.

## Which Dependency Types Are Cycle-Sensitive

Not all dependency types create scheduling obligations. Only these are traversed
during cycle detection:

| Type | Cycle-sensitive | Reason |
|------|:---:|---------|
| `blocks` | ✓ | Downstream work cannot start until this resolves |
| `waits-for` | ✓ | Explicit ordering constraint |
| `conditional-blocks` | ✓ | May create a scheduling gate |
| `parent-child` | ✓ | Parent waits for all children |
| *(empty)* | ✓ | Default type, treated as blocks |
| `relates-to` | ✗ | Informational only |
| `tracks` | ✗ | Informational only |

## When Cycle Detection Runs

Cycle detection is active when **all** of these conditions hold:

- The sling target is a plain bead (not a formula or on-formula attachment)
- `--force` is not set
- `--dry-run` is not set
- The bead is not inline text (newly created in the same sling call)

Formula slinging creates molecules whose molecule-step dependencies are managed
separately by the formula execution engine, not by the bead dependency graph, so
cycle detection is skipped for those paths.

## Error Output

When a cycle is detected, sling exits before dispatching with a descriptive error
showing the full cycle path:

```
dependency cycle detected: bead-a → bead-b → bead-c → bead-a
```

The path always starts and ends with the same bead ID so the cycle boundary is
unambiguous.

## Bypassing Detection

Pass `--force` to skip cycle detection when you know what you are doing (e.g.
breaking a cycle by hand, or testing recovery from a corrupted dependency state).

```
gc sling my-agent bead-xyz --force
```

## Implementation

| Artifact | Purpose |
|---|---|
| `internal/sling/cycle.go` | `DetectCycle`, `CycleError`, `DepLister` interface |
| `internal/sling/cycle_test.go` | Unit tests: no-cycle, direct, indirect, self-loop, diamond, type filtering |
| `internal/sling/sling_core.go` | `preflight()` calls `DetectCycle` via `shouldCheckDepCycle` predicate |

The `DepLister` interface has a single method:

```go
type DepLister interface {
    DepList(id, direction string) ([]beads.Dep, error)
}
```

`beads.Store` satisfies this interface, so no adapter is needed in the production
path. Tests use a lightweight `fakeDepGraph` map type.
