---
title: "Bead Store"
---

> Last verified against code: 2026-03-01

## Summary

The Bead Store is the universal persistence substrate for Gas City -- a
Layer 0-1 primitive that provides CRUD, parent-child relationships, labels,
and molecule instantiation over work units called beads. Everything in Gas
City is a bead: tasks, mail, molecules, convoys, and epics. The `Store`
interface abstracts over four implementations (BdStore, FileStore, MemStore,
and exec Store) so that higher-layer mechanisms like dispatch, orders,
and messaging compose purely against the interface without knowing the
storage backend.

## Key Concepts

- **Bead**: A single unit of work. Struct with ID, Title, Status (`open` /
  `in_progress` / `closed`), Type (default `task`), CreatedAt, Assignee,
  ParentID, Ref, Needs, Description, and Labels. Defined in
  `internal/beads/beads.go`.

- **Store**: The persistence interface. Provides Create, Get, Update,
  Close, List, Ready, Children, ListByLabel, SetMetadata, and MolCook.
  All domain persistence in Gas City flows through this interface. Defined
  in `internal/beads/beads.go`.

- **Container Type**: A bead type that groups child beads for batch
  expansion during dispatch. Currently only `convoy`. Queried via
  `IsContainerType()`. Children link to their container via ParentID.

- **Molecule**: A formula instantiated at runtime as one root bead
  (type `molecule`, Ref = formula name) plus one child step bead per
  formula step (type `task`, ParentID = root, Ref = step ID). Created
  by `MolCook`.

- **Label**: A string tag on a bead's `Labels` field. Labels enable pool
  dispatch (e.g., `pool:worker`), rig scoping (e.g., `rig:frontend`),
  order tracking (e.g., `order-run:lint`), and arbitrary
  categorization. Beads are queryable by exact label match via
  `ListByLabel`.

- **CommandRunner**: A function type `func(dir, name string, args ...string) ([]byte, error)`
  used by BdStore to execute bd CLI commands. Injectable for testing.

## Architecture

The bead store is a single interface with four implementations, selected
at startup by the `[beads].provider` config key or `GC_BEADS` env var.

```
                        beads.Store (interface)
                       /       |        \         \
                      /        |         \         \
               BdStore    FileStore   MemStore   exec.Store
             (bd CLI)   (JSON file)  (in-mem)   (user script)
                 |            |
                 |        embeds MemStore
                 |
          ExecCommandRunner
           (with telemetry)
```

**Provider resolution** (in `cmd/gc/main.go:openCityStore`):

1. `GC_BEADS` env var (highest priority)
2. `[beads].provider` in `city.toml`
3. Default: `"bd"`

Valid provider values: `"bd"` (BdStore), `"file"` (FileStore),
`"exec:<script-path>"` (exec.Store).

### Data Flow

The most common lifecycle of a bead:

```
Create(Bead{Title, Type, Labels})
  --> store assigns ID, sets Status="open", Type defaults to "task", sets CreatedAt
  --> returns complete Bead

Get(id)
  --> returns Bead or ErrNotFound

Update(id, UpdateOpts{Description, ParentID, Labels})
  --> applies non-nil fields; Labels appends (does not replace)

Close(id)
  --> sets Status="closed"; idempotent (closing already-closed is no-op)

Ready()
  --> returns all beads with Status="open"

Children(parentID)
  --> returns all beads whose ParentID matches

ListByLabel(label, limit)
  --> returns beads matching exact label; newest first; 0 = unlimited
```

Molecule instantiation via `MolCook`:

```
MolCook(formulaName, title, vars)
  --> BdStore: shells out to "bd mol cook --formula=<name>"
  --> MemStore: creates a bead with Type="molecule", Ref=formulaName
  --> exec.Store (with resolver): calls formula.ComposeMolCook which:
      1. Resolves the formula by name
      2. Substitutes {{key}} vars in step descriptions
      3. Creates root bead (type="molecule", Ref=formulaName)
      4. Creates one child bead per step (type="task", ParentID=root, Ref=stepID)
      --> returns root bead ID
```

### Key Types

- **`Bead`** (`internal/beads/beads.go`) -- The work unit struct. All
  fields are JSON-tagged for serialization.

- **`Store`** (`internal/beads/beads.go`) -- The persistence interface.
  Ten methods covering CRUD, querying, metadata, and molecule creation.

- **`UpdateOpts`** (`internal/beads/beads.go`) -- Partial update
  descriptor. Nil pointer fields are skipped; Labels appends.

- **`BdStore`** (`internal/beads/bdstore.go`) -- Production store backed
  by the bd CLI and Dolt database. Also provides Init, ConfigSet, Purge,
  and SetPurgeRunner methods not on the Store interface.

- **`exec.Store`** (`internal/beads/exec/exec.go`) -- Script-delegating
  store. Each operation is a fork/exec of a user-supplied script with
  operation name as arg and JSON on stdin/stdout. Exit code 2 = unknown
  operation (forward compatible).

## Invariants

These properties must hold for any correct Store implementation. They are
enforced by the conformance suite in `internal/beads/beadstest/conformance.go`.

1. **Create assigns a unique, non-empty ID.** No two calls to Create on
   the same store instance may return the same ID.

2. **Create sets Status to "open".** Regardless of any Status value on
   the input Bead, the returned Bead has Status `"open"`.

3. **Create defaults Type to "task".** When the input Type is empty, the
   returned Bead has Type `"task"`. An explicit Type is preserved.

4. **Create sets CreatedAt.** The returned Bead has a CreatedAt within a
   reasonable window of the current time.

5. **Get returns ErrNotFound for missing IDs.** The error must wrap
   `beads.ErrNotFound` so callers can use `errors.Is`.

6. **Close is idempotent.** Closing an already-closed bead succeeds
   without error. The bead remains closed.

7. **Close removes beads from Ready results.** After Close(id), Ready()
   must not include that bead.

8. **Update with nil fields is a no-op.** Passing an empty UpdateOpts
   does not modify any bead fields.

9. **Labels append, never replace.** UpdateOpts.Labels adds to the
   existing label set.

10. **Children filters by ParentID.** Only beads whose ParentID matches
    the given ID are returned.

11. **ListByLabel matches exact strings.** Partial or prefix matches do
    not count. Limit 0 means unlimited.

12. **Container types are case-sensitive.** `"convoy"` is a container;
    `"epic"` is an ordinary bead type, and `"CONVOY"` is not a container.

13. **All bd subprocess calls live in `internal/beads/`.** The boundary
    test `TestNoBdExecOutsideBeads` enforces that no Go code outside
    `internal/beads/` (and `test/integration/`) directly invokes the bd
    binary.

14. **BdStore maps bd's extended statuses to Gas City's three.** bd uses
    open, in_progress, blocked, review, testing, closed. Gas City maps
    closed to closed, in_progress to in_progress, and everything else
    to open.

15. **FileStore uses atomic writes.** Persistence writes go to a temp
    file first, then `os.Rename` to the target path -- never partial
    writes.

## Interactions

| Depends on | How |
|---|---|
| `internal/fsys` | FileStore uses `fsys.FS` for all file I/O (testable via `fsys.Fake`) |
| `internal/telemetry` | BdStore's `ExecCommandRunner` calls `telemetry.RecordBDCall` for every bd subprocess invocation |
| Formula-aware backends | `BdStore.MolCook` delegates to `bd mol wisp`; `exec.Store` delegates to script operations; in-memory stores provide simplified molecule roots for tests and tutorials |

| Depended on by | How |
|---|---|
| `cmd/gc/` (CLI commands) | `openCityStore` creates the appropriate Store; used by convoy, sling, order, handoff, and hook commands |
| `internal/mail/beadmail` | Implements mail.Provider backed by beads.Store -- mail messages are beads with type `"message"` |
| Formula-aware backends | Molecule creation and step materialization are delegated to the configured store backend |
| `internal/orders` | Order dispatch uses Store for cooldown tracking (`ListByLabel` with `order-run:` labels) and cursor-based event gates |
| `internal/doctor` | Health checks verify Store accessibility for both city-level and per-rig bead databases |
| `cmd/gc/cmd_convoy.go` | Convoy operations (create, list, status, add, close, check, stranded) all operate through Store |
| `cmd/gc/cmd_handoff.go` | Work handoff between agents reads and writes beads through Store |

## Code Map

| Path | Description |
|---|---|
| `internal/beads/beads.go` | Bead struct, Store interface, UpdateOpts, ErrNotFound, IsContainerType |
| `internal/beads/bdstore.go` | BdStore: production store shelling out to bd CLI; includes Init, ConfigSet, Purge, CommandRunner, ExecCommandRunner, status mapping |
| `internal/beads/memstore.go` | MemStore: in-memory store with mutex-guarded slice; exported for use as test double |
| `internal/beads/filestore.go` | FileStore: embeds MemStore, adds JSON persistence via fsys.FS with atomic writes |
| `internal/beads/exec/exec.go` | exec.Store: delegates all operations to a user-supplied script via fork/exec |
| `internal/beads/exec/json.go` | Wire format types (createRequest, updateRequest, molCookRequest, beadWire) for exec.Store's JSON protocol |
| `internal/beads/beadstest/conformance.go` | RunStoreTests: the conformance suite that all Store implementations must pass |
| `internal/beads/boundary_test.go` | TestNoBdExecOutsideBeads: architectural boundary enforcement |
| `internal/beads/bdstore_test.go` | BdStore unit tests with fake CommandRunner |
| `internal/beads/memstore_test.go` | MemStore tests including conformance suite, MolCook, and ListByLabel |
| `internal/beads/filestore_test.go` | FileStore tests including persistence, corruption, and fsys.Fake failure paths |
| `internal/beads/exec/exec_test.go` | exec.Store tests including conformance suite, composed MolCook with resolver, timeout, and error handling |
| `internal/beads/exec/br_test.go` | Integration test for beads_rust (br) provider via exec.Store |
| `cmd/gc/main.go` | openCityStore: factory that selects and creates the appropriate Store |
| `cmd/gc/providers.go` | beadsProvider: resolves provider name from GC_BEADS env var or city.toml |

## Configuration

The bead store backend is selected via the `[beads]` section in `city.toml`:

```toml
[beads]
provider = "bd"        # "bd" (default), "file", or "exec:/path/to/script"
```

The `GC_BEADS` environment variable overrides the config file. Related
env vars:

- `GC_DOLT=skip` -- bypasses dolt server lifecycle in init/start/stop
  (used by tests)
- `GC_LOG_BD_OUTPUT=true` -- includes bd stdout/stderr in telemetry log
  events

BdStore-specific admin operations (not on the Store interface):

- `Init(prefix, host, port)` -- runs `bd init --server` to create the
  Dolt-backed beads database
- `ConfigSet(key, value)` -- runs `bd config set` to configure the
  beads database
- `Purge(beadsDir, dryRun)` -- runs `bd purge` to garbage-collect
  closed ephemeral beads (60-second timeout)

## Testing

The bead store has a layered testing strategy aligned with
[TESTING.md](https://github.com/gastownhall/gascity/blob/main/TESTING.md):

**Conformance suite** (`internal/beads/beadstest/conformance.go`):
`RunStoreTests` runs 25+ subtests against any Store implementation,
covering Create semantics, Get/Close error paths, List/Ready filtering,
Children parent matching, ListByLabel exactness and limits, and Update
partial application. Additional suites `RunSequentialIDTests` and
`RunCreationOrderTests` cover implementation-specific guarantees for
in-process stores.

**Unit tests per implementation**: Each store has its own `*_test.go`
file testing implementation-specific behavior -- BdStore tests fake
CommandRunner responses and argument construction; FileStore tests
persistence across reopens, corruption handling, and fsys.Fake failure
injection; MemStore tests MolCook and ListByLabel ordering.

**Boundary test** (`internal/beads/boundary_test.go`):
`TestNoBdExecOutsideBeads` walks the entire repo to enforce that all bd
subprocess calls are confined to `internal/beads/`.

**exec.Store conformance** (`internal/beads/exec/exec_test.go`):
`TestExecStoreConformance` runs the full conformance suite against a
jq-based reference implementation in `testdata/conformance.sh`.

**Integration tests** (`internal/beads/exec/br_test.go`):
`TestBrProviderConformance` runs the conformance suite against the
beads_rust (br) binary as a real external provider (build tag:
`integration`).

## Known Limitations

- **BdStore.Children is client-side filtered.** The bd CLI does not
  support parent-child queries natively, so Children fetches all beads
  via List and filters in Go. This is acceptable at current scale but
  will not perform well with very large bead counts.

- **MemStore.SetMetadata is a no-op.** MemStore has no metadata storage;
  it only verifies the bead exists. Callers that need to verify metadata
  values must use BdStore or a recording wrapper.

- **BdStore timestamps are second-precision.** Dolt stores timestamps at
  second granularity. Sub-second precision from bd create may be
  truncated on bd show, causing minor CreatedAt drift.

- **No in-process BdStore conformance.** BdStore requires a real bd
  binary and Dolt database, so it cannot run the conformance suite in
  unit tests. Its correctness relies on unit tests with faked
  CommandRunner plus real bd in integration tests.

- **Ordering varies by implementation.** In-process stores (MemStore,
  FileStore) guarantee creation order for List and Ready. BdStore
  returns bd's default sort order (which may differ for beads sharing
  the same second-precision timestamp). ListByLabel returns newest-first
  across all implementations.

## See Also

- [Architecture glossary](/architecture/glossary) -- authoritative definitions of
  bead, molecule, convoy, label, and other terms used in this document
- [Formula file reference](/reference/formula) -- formula file layout,
  layer resolution, and how stores instantiate molecules from formulas
- [Beadmail provider](https://github.com/gastownhall/gascity/tree/main/internal/mail/beadmail/) -- how inter-agent
  messaging composes on top of bead store (mail = beads with type
  `"message"`)
- [TESTING.md](https://github.com/gastownhall/gascity/blob/main/TESTING.md) -- testing philosophy and tier
  boundaries for the conformance suite approach
- [CLAUDE.md](https://github.com/gastownhall/gascity/blob/main/CLAUDE.md) -- design principles including "Beads is
  the universal persistence substrate" (layering invariant 2)
