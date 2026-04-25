---
title: "Exec Beads Provider"
---

Gas City's bead store is the universal persistence substrate for work units
(tasks, messages, molecules, convoys). Today it has two providers: `bd`
(shells out to the `bd` CLI backed by Dolt) and `file` (JSON persistence
for tutorials). This document designs a third: `exec`, which delegates each
store operation to a user-supplied script — the same pattern used by the
exec session provider.

## Motivation

The `bd` provider couples Gas City to a specific technology stack: the Go
`bd` CLI wrapping a Dolt SQL database. Users may want:

- **beads_rust (`br`)** — SQLite + JSONL hybrid with different performance
  characteristics and no JVM/Dolt dependency
- **Custom backup semantics** — bead operations that trigger S3 snapshots,
  git commits, or other persistence strategies
- **Alternative databases** — PostgreSQL, SQLite, flat files, or any
  storage backend accessible via CLI

The exec beads provider makes the bead store a pluggable boundary. If we
got the layering right, a user can change one config line and point Gas City
at their own implementation.

## Current Architecture

### Store Interface (9 methods)

`internal/beads/beads.go` defines the `Store` interface — the SDK's
contract for bead persistence:

```go
type Store interface {
    Create(b Bead) (Bead, error)       // persist new bead → fills ID, Status, CreatedAt
    Get(id string) (Bead, error)       // retrieve by ID
    Update(id string, opts UpdateOpts) error  // modify fields (Description, ParentID, Labels)
    Close(id string) error             // set status to "closed"
    List() ([]Bead, error)             // all beads
    Ready() ([]Bead, error)            // all open beads
    Children(parentID string) ([]Bead, error)  // beads with matching ParentID
    SetMetadata(id, key, value string) error   // key-value metadata on a bead
    MolCook(formula, title string, vars []string) (string, error)  // instantiate molecule
}
```

### Three Implementations

| Provider | Backing | Used By |
|----------|---------|---------|
| `BdStore` | `bd` CLI → Dolt SQL | Production (default) |
| `FileStore` | JSON file, wraps MemStore | Tutorials, lightweight setups |
| `MemStore` | In-memory map | Unit tests |

### BdStore-Only Methods (Not in Store Interface)

BdStore exposes methods that other subsystems use directly via `*BdStore`:

| Method | Used By | Purpose |
|--------|---------|---------|
| `Init(prefix)` | `cmd/gc/beads_provider_lifecycle.go` | Initialize `.beads/` database |
| `ConfigSet(key, value)` | `cmd/gc/beads_provider_lifecycle.go` | Set bd configuration |
| `ListByLabel(label, limit)` | `cmd/gc/cmd_order.go` | Query beads by label (order history, cursors) |
| `Purge(beadsDir, dryRun)` | `cmd/gc/wisp_gc.go` and admin flows | Remove closed ephemeral beads |
| `SetPurgeRunner(fn)` | Tests only | Test injection |

### Provider Selection

`cmd/gc/providers.go` selects the bead store at runtime:

```go
func beadsProvider(cityPath string) string {
    if v := os.Getenv("GC_BEADS"); v != "" {
        return v
    }
    cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
    if err == nil && cfg.Beads.Provider != "" {
        return cfg.Beads.Provider
    }
    return "bd"
}
```

Priority: `GC_BEADS` env var → `city.toml [beads].provider` → `"bd"`.

Config:
```toml
[beads]
provider = "bd"    # or "file", or "exec:/path/to/script"
```

## What Must Change

### 1. Promote ListByLabel to the Store Interface

`ListByLabel` is used by the order subsystem for:
- **Order history** — list all wisps for a order
- **Last run time** — find most recent wisp for a order
- **Event cursor** — find max `seq:` label across order wisps

This is a core query pattern, not a bd-specific feature. Any bead store
can filter by label. The interface should include it:

```go
type Store interface {
    // ... existing 9 methods ...

    // ListByLabel returns beads matching an exact label string.
    // Limit controls max results (0 = unlimited). Results ordered
    // newest first.
    ListByLabel(label string, limit int) ([]Bead, error)
}
```

**Impact:** MemStore and FileStore need `ListByLabel` implementations
(trivial filter over existing data).

### 2. Keep Admin Operations Outside the Store Interface

`Init`, `ConfigSet`, `Purge`, and `SetPurgeRunner` are lifecycle/admin
operations, not bead CRUD. They belong to the provider implementation,
not the SDK interface. The exec beads provider handles them as optional
operations (exit 2 = unsupported).

### 3. Add Exec Beads Provider

New package: `internal/beads/exec/` (mirrors `internal/runtime/exec/`).

## Exec Beads Protocol

### Calling Convention

```
<script> <operation> [args...]
```

Data on stdin (JSON). Results on stdout (JSON). Follows the session exec
provider pattern exactly.

### Exit Codes

| Code | Meaning |
|------|---------|
| 0 | Success |
| 1 | Failure (stderr contains error message) |
| 2 | Unknown operation (treated as success — forward compatible) |

### Operations

#### Core Store Operations (10 methods)

| Operation | Invocation | Stdin | Stdout |
|-----------|-----------|-------|--------|
| `create` | `script create` | Bead JSON | Bead JSON (with ID, status, created_at) |
| `get` | `script get <id>` | — | Bead JSON |
| `update` | `script update <id>` | UpdateOpts JSON | — |
| `close` | `script close <id>` | — | — |
| `list` | `script list` | — | Bead JSON array |
| `ready` | `script ready` | — | Bead JSON array |
| `children` | `script children <parent-id>` | — | Bead JSON array |
| `set-metadata` | `script set-metadata <id> <key>` | value on stdin | — |
| `mol-cook` | `script mol-cook` | MolCookRequest JSON | root bead ID (plain text) |
| `list-by-label` | `script list-by-label <label> <limit>` | — | Bead JSON array |

#### Admin Operations (Optional)

| Operation | Invocation | Stdin | Stdout |
|-----------|-----------|-------|--------|
| `init` | `script init <dir> <prefix>` | — | — |
| `config-set` | `script config-set <key> <value>` | — | — |
| `purge` | `script purge <beads-dir>` | PurgeOpts JSON | PurgeResult JSON |

Scripts that don't support admin operations return exit 2 (unknown
operation). Gas City treats this as success — admin ops are only called
during `gc init` and `gc dolt sync`, not during normal operation.

#### Lifecycle Operations (Optional)

| Operation | Invocation | Stdin | Stdout | Purpose |
|-----------|-----------|-------|--------|---------|
| `ensure-ready` | `script ensure-ready` | — | — | Make backing service usable |
| `start` | `script start` | — | — | Enhanced start with backoff/health tracking |
| `stop` | `script stop` | — | — | Enhanced stop with graceful shutdown |
| `shutdown` | `script shutdown` | — | — | Legacy graceful stop |
| `init` | `script init <dir> <prefix>` | — | — | First-time setup for a directory |
| `health` | `script health` | — | — | Check provider health (probe only, no side effects) |
| `recover` | `script recover` | — | — | Stop, restart, verify health after failure |
| `probe` | `script probe` | — | — | Check if backing service is available (exit 0 = yes, 2 = not running) |

These operations are called by `gc start` and `gc stop` to manage the
bead store's backing service — analogous to Docker Compose starting and
stopping database containers. They are convenience operations, not part
of the Store interface contract.

Exit code semantics follow the same convention as other operations:
0 = success, 1 = error, 2 = not needed/not running. Scripts that have
no backing service (e.g., `br` which uses an embedded SQLite database)
return exit 2 for all lifecycle operations.

The `health` operation is a read-only probe — it MUST NOT attempt
recovery or restarts. The SDK calls `recover` separately on health
failure. The `probe` operation is a lightweight availability check used
during `gc init` to decide whether bead initialization can proceed now
or must be deferred to `gc start`.

### Wire Format

#### Bead JSON

The wire format matches `beads.Bead` JSON tags — the same shape that
`bd` already produces:

```json
{
  "id": "WP-42",
  "title": "digest wisp",
  "status": "open",
  "type": "task",
  "created_at": "2026-02-27T10:00:00Z",
  "assignee": "",
  "parent_id": "",
  "ref": "",
  "needs": [],
  "description": "",
  "labels": ["order-run:digest", "pool:dog"]
}
```

Fields omitted from the JSON are treated as zero values. The `id` field
on `create` input is ignored (the script assigns IDs).

#### Create Request

```json
{
  "title": "my task",
  "type": "task",
  "labels": ["pool:dog"],
  "parent_id": "WP-1"
}
```

#### UpdateOpts JSON

```json
{
  "description": "updated description",
  "parent_id": "WP-1",
  "labels": ["new-label"]
}
```

Null/missing fields are not applied. `labels` appends (does not replace).

#### MolCookRequest JSON

```json
{
  "formula": "mol-digest",
  "title": "digest run",
  "vars": ["key=value"]
}
```

Stdout: the root bead ID as plain text (e.g., `WP-42\n`).

#### PurgeOpts JSON

```json
{
  "dry_run": true
}
```

#### PurgeResult JSON

```json
{
  "purged_count": 5
}
```

### Conventions

- **JSON on stdin for mutations** — avoids shell quoting issues with
  descriptions, titles, and label values
- **JSON on stdout for reads** — consistent with bd's `--json` output
- **Plain text for simple results** — `mol-cook` returns just the ID
- **Empty array for no results** — `list`, `ready`, `children`,
  `list-by-label` return `[]`, never null
- **Idempotent close** — closing an already-closed bead returns exit 0
- **ErrNotFound → exit 1** — `get`, `update`, `close`, `set-metadata`
  with unknown ID print error to stderr and exit 1

### Status Mapping

Gas City's `beads.Store` surface uses the SDK's three-state vocabulary:
`open`, `in_progress`, and `closed`. Backends that expose a richer status
set must map it onto those three values. The built-in `BdStore`, for
example, maps bd's `blocked`, `review`, and `testing` states to `open`.
An empty status is also treated as `open`.

## Implementation Plan

### Package Structure

```
internal/beads/exec/
├── exec.go          # ExecStore implementing Store interface
├── exec_test.go     # unit tests with fake script
└── json.go          # wire format types (like session/exec/json.go)
```

### ExecStore

```go
// ExecStore implements beads.Store by delegating each operation to a
// user-supplied script via fork/exec.
type ExecStore struct {
    script  string
    timeout time.Duration
}

func NewExecStore(script string) *ExecStore {
    return &ExecStore{script: script, timeout: 30 * time.Second}
}
```

The `run` method mirrors `session/exec`'s pattern exactly:

```go
func (s *ExecStore) run(stdinData []byte, args ...string) (string, error) {
    ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
    defer cancel()
    cmd := exec.CommandContext(ctx, s.script, args...)
    cmd.WaitDelay = 2 * time.Second
    // ... same exit code 2 handling as session exec ...
}
```

### Provider Selection Update

`cmd/gc/providers.go` adds the exec case:

```go
func newBeadStore(cityPath, cmdName string, stderr io.Writer) (beads.Store, int) {
    provider := beadsProvider(cityPath)
    if strings.HasPrefix(provider, "exec:") {
        script := strings.TrimPrefix(provider, "exec:")
        return beadsexec.NewExecStore(script), 0
    }
    switch provider {
    case "file":
        // ... existing ...
    default:
        // ... existing bd ...
    }
}
```

### Config Update

```toml
[beads]
provider = "exec:/path/to/gc-beads-br"
```

Or via environment:

```bash
export GC_BEADS=exec:gc-beads-br
```

## Dependency Map: SDK Primitives vs. Provider Operations

This table maps every Gas City subsystem to the bead store operations it
requires. This is how we verify the layering: if every operation in the
"Uses" column is in the Store interface (or exec protocol), the subsystem
works with any provider.

| Subsystem | Layer | Uses (Store Interface) | Uses (*BdStore Only) |
|-----------|-------|----------------------|---------------------|
| Dispatch (sling) | L3 | Create, Get, Update, Close, MolCook | — |
| Task loop | L2 | Ready, Get, Update, Close | — |
| Molecules | L2 | Create, Children, Update, Close, MolCook | — |
| Messaging | L2 | Create (type=message), List | — |
| Order check | L3 | — | ListByLabel (→ promote) |
| Order run | L3 | MolCook | ListByLabel (→ promote) |
| Order history | L3 | — | ListByLabel (→ promote) |
| Health patrol | L2 | Ready, SetMetadata | — |
| Convoy | L3 | Create, Children, Close, Update | — |
| Rig init | L0 | — | Init, ConfigSet |
| Dolt sync | L0 | — | Purge |
| Event cursor | L3 | — | ListByLabel (→ promote) |

**After promoting ListByLabel:** Only `Init`, `ConfigSet`, and `Purge`
remain outside the Store interface. These are all admin/lifecycle
operations called during `gc init` and `gc dolt sync` — not during
normal agent work loops. The exec protocol handles them as optional
operations (exit 2).

## beads_rust (br) Gap Analysis

[beads_rust](https://github.com/Dicklesworthstone/beads_rust) is a Rust
reimplementation of the beads concept using SQLite + JSONL. Here's how it
maps to Gas City's requirements:

### Supported (Direct Mapping)

| Store Method | br Command | Notes |
|-------------|------------|-------|
| `Create` | `br create --json <title>` | Has `--type`, `--label` |
| `Get` | `br show --json <id>` | Returns JSON |
| `Update` | `br update --json <id>` | Has `--description`, `--label` |
| `Close` | `br close --json <id>` | Direct mapping |
| `List` | `br list --json` | Has `--limit`, `--all` |
| `Ready` | `br ready --json` | Open beads |
| `ListByLabel` | `br list --json --label=X` | Has `--label` filter |

### Gaps (Script Must Bridge)

| Store Method | Gap | Workaround |
|-------------|-----|------------|
| `Children(parentID)` | No `--parent` on create | Script tracks parent→child in sidecar or labels |
| `SetMetadata(id, key, value)` | No `--set-metadata` | Script uses labels (`meta:key=value`) or sidecar file |
| `MolCook(formula, title, vars)` | No molecule concept | Script creates root bead + step beads from formula TOML |

### Not Needed by Store Interface

| br Feature | Relevance |
|-----------|-----------|
| `br comment` | Not in Store interface — could be future extension |
| `br search` | Not in Store interface — search is done via List + filter |
| `br dep-tree` | Interesting for molecules but not required |
| `br blocked` | Subset of Ready with dependency tracking |
| `br priority` | Not in Gas City's bead model |

### Feasibility Assessment

A `gc-beads-br` script wrapping `br` is feasible for **basic bead CRUD**
(7 of 10 operations map directly). The three gaps (Children, SetMetadata,
MolCook) require the script to implement bridging logic:

- **Children**: Use `br list --label=parent:<id>` (script adds parent
  label on create)
- **SetMetadata**: Use `br update --label=meta:key=value` (script
  convention)
- **MolCook**: Parse formula TOML, create root + step beads, wire
  parent links. This is the hardest gap — it requires the script to
  understand Gas City's formula format.

A more practical approach: implement `MolCook` in Go within Gas City
(it already knows formula TOML) and decompose it into `Create` + `Update`
calls against the Store interface. This makes MolCook a **composed
operation** rather than a primitive the script must implement.

## Design Decision: MolCook as Composed vs. Primitive

**Option A: MolCook is a primitive in the exec protocol.**
The script must understand formulas and create molecule bead trees.
Simple for bd (has `bd mol cook`), hard for custom backends.

**Option B: MolCook is composed from Create + Update in Go.**
Gas City reads the formula TOML, creates the root bead via `Create`,
creates step beads with ParentID via `Create`, wires dependencies via
`Update`. The script only needs CRUD primitives.

**Recommendation: Option B.** MolCook is a *mechanism* (Layer 2),
not a *primitive*. It's composed from Task Store operations + Config
parsing. Pushing formula knowledge into every backend script violates
the Bitter Lesson — the SDK should handle composition, scripts handle
storage.

This means the Store interface becomes:

```go
type Store interface {
    Create(b Bead) (Bead, error)
    Get(id string) (Bead, error)
    Update(id string, opts UpdateOpts) error
    Close(id string) error
    List() ([]Bead, error)
    Ready() ([]Bead, error)
    Children(parentID string) ([]Bead, error)
    SetMetadata(id, key, value string) error
    ListByLabel(label string, limit int) ([]Bead, error)
    MolCook(formula, title string, vars []string) (string, error)  // composed internally for exec
}
```

For the exec provider, `MolCook` is implemented in Go by the ExecStore
itself using its own `Create` and `Update` methods + formula parsing.
BdStore continues to delegate to `bd mol cook`. FileStore/MemStore
get their own Go implementation.

## Migration Path

### Phase 1: Interface Promotion (This PR)
1. Add `ListByLabel(label string, limit int) ([]Bead, error)` to Store
2. Implement on MemStore and FileStore (filter existing data)
3. Change `cmd/gc/cmd_order.go` functions from `*BdStore` to `Store`

### Phase 2: Exec Provider
1. Create `internal/beads/exec/` package
2. Implement ExecStore with all Store interface methods
3. Add `exec:` prefix handling in `beadsProvider()`
4. Write protocol documentation

### Phase 3: MolCook Decomposition
1. Extract formula→bead-tree logic from `bd mol cook` into Go
2. Implement composed MolCook on ExecStore using Create + Update
3. Optionally add composed MolCook to FileStore/MemStore

### Phase 4: Reference Script
1. Write `gc-beads-br` script wrapping beads_rust
2. Verify all Gas City operations work end-to-end
3. Document gaps and workarounds

## Comparison: Session vs. Beads Exec Pattern

| Aspect | Session Exec | Beads Exec |
|--------|-------------|------------|
| Interface | `runtime.Provider` (14+ methods) | `beads.Store` (10 methods) |
| Data format | Mixed (JSON for start, text for others) | JSON for all mutations and reads |
| Selection | `GC_SESSION=exec:<script>` | `GC_BEADS=exec:<script>` |
| Config | N/A (env var only) | `[beads] provider = "exec:..."` |
| Forward compat | Exit 2 = unknown op | Exit 2 = unknown op |
| Wire types | `startConfig` (stable subset) | `beads.Bead` JSON tags (stable) |
| Timeout | 30s | 30s |
| Composed ops | None (all primitive) | MolCook (composed from Create+Update) |

## Open Questions

1. **Should `Children` use a label convention or a first-class parent
   field?** If we use labels (`parent:<id>`), the script doesn't need
   native parent support. But `bd` has native parent support. Decision:
   keep ParentID as a first-class field in the wire format; scripts that
   don't support it natively use labels internally.

2. **Should `ListByLabel` support multiple labels (AND)?** Current
   BdStore only supports a single label. Keep it simple for now — single
   label. Multiple-label queries can be composed from single-label
   results.

3. **Purge semantics for exec provider.** Purge is dolt-specific
   (removes closed ephemeral beads from the Dolt database). For exec
   providers, should this be delegated or composed? Recommendation:
   delegate as optional (exit 2 = no-op). The script can implement its
   own cleanup strategy.

## Shipped Scripts

See `contrib/beads-scripts/` for maintained implementations:

- **gc-beads-br** — beads_rust (`br`) backend. Wraps the `br` CLI with
  SQLite + JSONL backing. Dependencies: `br`, `jq`, `bash`.
- **gc-beads-k8s** — Kubernetes backend. Runs `bd` inside a lightweight
  "beads runner" pod via `kubectl exec`. The pod connects to Dolt running
  as a StatefulSet inside the cluster. Dependencies: `kubectl`, `jq`, `bash`.
