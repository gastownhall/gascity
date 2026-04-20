---
title: "Life of a Bead"
---

> Last verified against code: 2026-03-01

This document traces a single bead through its entire lifecycle in Gas
City, from creation to garbage collection. It names every function, file,
and state transition along the way -- Gas City's analog to CockroachDB's
"Life of a SQL Query."

**Who this is for.** Contributors debugging a stuck bead, a broken hook,
or a molecule that never completes.

**What we trace.** A task bead dispatched to a pool agent, discovered
through the hook mechanism, claimed, executed, and closed. Variant paths
(mail, molecules, convoys) are noted at each phase.

```
 Creation       Discovery      Claiming       Execution       Completion    Afterlife
    |               |              |              |               |             |
    v               v              v              v               v             v
 ┌──────┐     ┌──────────┐   ┌──────────┐   ┌──────────┐   ┌──────────┐  ┌──────────┐
 │ open │────>│  Ready() │──>│in_progress│──>│ metadata │──>│  closed  │─>│  purge / │
 │      │     │  hook    │   │ assignee  │   │  updates │   │          │  │  archive │
 └──────┘     └──────────┘   └──────────┘   └──────────┘   └──────────┘  └──────────┘
```

## Phase 1: Creation

Every bead enters the world through `Store.Create()`. The Store interface
is defined in `internal/beads/beads.go`. Regardless of which implementation
handles the call, the contract is the same: the store assigns a unique
non-empty ID, forces Status to `"open"`, defaults Type to `"task"` if
empty, and stamps CreatedAt.

### Path A: Direct CLI creation (bd create)

The most common path. There is no `gc create` -- Gas City delegates to bd
directly. `BdStore.Create()` in `internal/beads/bdstore.go` shells out:

```
bd create --json "Implement the frobulator" -t task --label pool:worker
```

It calls `s.runner(s.dir, "bd", args...)` and parses the JSON response
through `bdIssue.toBead()`. The bd CLI returns the new bead's ID,
timestamps, and status from its embedded Dolt database.

### Path B: Sling with --formula (wisp instantiation)

`gc sling` does not create beads -- they already exist. But with
`--formula`, sling instantiates a wisp first. `cmdSling()` in
`cmd/gc/cmd_sling.go` calls `instantiateWisp()`, which delegates to
`Store.MolCook()`.

- For `BdStore`, that becomes `bd mol wisp <formula> --json`
- For attached molecules, `Store.MolCookOn()` becomes
  `bd mol bond <formula> <bead> --json`
- For `exec.Store`, the configured script handles `mol-cook` and
  `mol-cook-on`
- For `MemStore` and `FileStore`, tests and tutorials get a simplified
  molecule root bead

The root bead ID is returned to sling for routing.

### Path C: Mail send

Inter-agent messaging composes on top of beads. `beadmail.Provider.Send()`
in `internal/mail/beadmail/beadmail.go` calls:

```go
p.store.Create(beads.Bead{
    Title:    body,
    Type:     "message",
    Assignee: to,
    From:     from,
})
```

A mail message is just a bead with Type `"message"`. The Assignee field
doubles as the recipient address. No special storage -- the same Store, the
same invariants.

### Path D: Convoy creation

`doConvoyCreate()` in `cmd/gc/cmd_convoy.go` creates a bead with Type
`"convoy"`, then links child beads to it via `Store.Update()` setting
their ParentID. Sling also creates auto-convoys (line 199 of
`cmd/gc/cmd_sling.go`) to track individual bead routing.

### Path E: Order dispatch

The controller's order dispatcher (`cmd/gc/order_dispatch.go`)
fires due orders. Formula orders call `Store.MolCook()` to
instantiate wisps, then route the root bead via `buildSlingCommand()`.
Exec orders run shell scripts directly and may create beads as a
side effect. Both paths record a tracking bead with an
`order-run:<name>` label for cooldown gating.

### The exec.Store variant

With `exec:<script>` provider, `exec.Store.Create()` in
`internal/beads/exec/exec.go` marshals a `createRequest` JSON object
(`internal/beads/exec/json.go`), pipes it to the script's stdin, and
parses the JSON bead response from stdout. Exit code 2 means "unknown
operation" (forward compatible).

## Phase 2: Discovery

A bead exists, but no agent knows about it yet. Discovery is how agents
find work. Gas City uses the **pull model**: agents poll for available
work rather than being pushed assignments.

### The hook mechanism (gc hook)

Every agent has a `work_query` config field. When the agent's session
provider fires a hook (e.g., Claude's Stop hook), it runs `gc hook`
(`cmd/gc/cmd_hook.go`). The flow:

1. `cmdHook()` resolves the agent from `$GC_AGENT` or a positional arg
2. Loads city config, checks suspension status
3. Calls `a.EffectiveWorkQuery()` (`internal/config/config.go`, line 630)
4. Delegates to `doHook()` which runs the query via `shellWorkQuery()`

The default work queries (from `EffectiveWorkQuery()`) are:

- **Fixed agents**: `bd ready --assignee=<qualified-name>`
- **Pool agents**: `bd ready --label=pool:<qualified-name> --unassigned --limit=1`

Both ultimately call `BdStore.Ready()` (`internal/beads/bdstore.go`, line
385), which shells out to `bd ready --json --limit=0`. For pool agents,
the bd CLI filters by label server-side.

### The --inject mode

With `--inject`, `gc hook` wraps output in a `<system-reminder>` XML block
for LLM context injection. Hook-enabled agents discover work automatically
between turns. If no work exists, `--inject` emits nothing and exits 0.

### Ready() and GUPP

`Store.Ready()` returns all beads with status `"open"` -- the fundamental
discovery primitive. For BdStore: `bd ready --json --limit=0`. For
exec.Store: the script receives `ready` as its operation argument.

Discovery feeds into GUPP: "If you find work on your hook, YOU RUN IT."
No confirmation, no waiting. This principle lives in prompt templates, not
Go code. Gas City ensures the work is visible; the prompt tells the agent
what to do.

## Phase 3: Claiming

An agent has discovered a bead through its hook. Now it claims ownership.
This is a status transition and assignee update.

### Sling as the routing mechanism

Before claiming, the bead must be routed to the agent. `doSling()` in
`cmd/gc/cmd_sling.go` calls `buildSlingCommand(a.EffectiveSlingQuery(),
beadID)`. The default sling queries (`EffectiveSlingQuery()` in
`internal/config/config.go`) are:

- **Fixed agents**: `bd update <bead-id> --assignee=<qualified-name>`
- **Pool agents**: `bd update <bead-id> --label=pool:<qualified-name>`

Fixed agents claim by assignee. Pool agents claim by label -- any member
matching `pool:<name>` can pick it up.

### The claiming act

For pool agents, claiming happens at the prompt level. The agent runs
`bd update <id> --claim` (or equivalent) to set itself as assignee and
transition the status from `open` to `in_progress`. This is not enforced
by Gas City Go code -- it is prescribed in the agent's prompt template.
The bd CLI handles the atomic compare-and-swap.

For fixed agents, the sling command already sets the assignee. The agent
transitions status by running `bd update <id> --status=in_progress` (or
the agent's session tool equivalent).

Under the hood, both paths flow through `BdStore.Update()` (line 293 of
`internal/beads/bdstore.go`):

```
bd update --json <id> --description "..." --label "..."
```

The `Store.Update()` contract: only non-nil fields in `UpdateOpts` are
applied. Labels append, never replace. This is invariant 9 from the bead
store specification.

### Container expansion

When `gc sling` receives a convoy ID, `doSlingBatch()` expands it. It
calls `querier.Children(b.ID)` to get child beads, filters to open ones,
and routes each child individually. Epics are no longer first-class
containers and are rejected by `gc sling`. The container itself is the
convoy -- no auto-convoy is created.

## Phase 4: Execution

The bead is now `in_progress` with an assignee. The agent works on it.
Gas City's infrastructure is mostly hands-off during this phase -- ZFC
(Zero Framework Cognition) means Go code does not make decisions about
work execution.

### Status updates and metadata

The agent may update metadata via `Store.SetMetadata()`. For BdStore,
`BdStore.SetMetadata()` in `internal/beads/bdstore.go` shells out:
`bd update --json <id> --set-metadata key=value`. Merge strategy metadata
(set by `gc sling --merge`) uses this path.

### Molecule step progression

For molecule beads (wisps), the agent works through steps sequentially.
step ordering is handled by the configured bead backend, primarily `bd`
in production. Gas City routes and labels the molecule root; agents then
work through the resulting step beads and close them through normal bead
operations.

### Health patrol during execution

While the agent works, the controller's reconciliation loop
(`doReconcileAgents()` in `cmd/gc/reconcile.go`) monitors agent health.
If an agent crashes mid-execution, the bead persists in its current state
(NDI -- Nondeterministic Idempotence). When the agent restarts, it
rediscovers the in-progress bead through its hook and resumes. The bead
is the durable record; sessions are ephemeral.

## Phase 5: Completion

The agent finishes work and closes the bead.

### Store.Close()

The agent calls `bd close <id>`, which flows through `BdStore.Close()`
(line 322 of `internal/beads/bdstore.go`):

```
bd close --json <id>
```

The Store contract: Close sets status to `"closed"`. It is idempotent --
closing an already-closed bead is a no-op (invariant 6). After Close, the
bead no longer appears in `Ready()` results (invariant 7).

For exec.Store, the script receives `close <id>` as arguments
(`exec.Store.Close()`, line 200 of `internal/beads/exec/exec.go`).

### Event emission

Bead lifecycle events are recorded on the event bus. The event types
(defined in `internal/events/events.go`) include:

- `bead.created` -- emitted when a bead is created
- `bead.closed` -- emitted when a bead is closed
- `bead.updated` -- emitted on updates
- `convoy.closed` -- emitted when a convoy auto-closes

These events feed into order triggers. An `event` trigger type fires
when a specific event type occurs, enabling reactive order chains.

### Convoy auto-close

When a child bead closes, convoy tracking kicks in.
`doConvoyAutocloseWith()` in `cmd/gc/cmd_convoy.go` (line 579) checks:

1. Does the closed bead have a ParentID?
2. Is the parent a convoy (not closed, not "owned")?
3. Are ALL sibling children now closed?

If yes, it closes the parent convoy and records a `convoy.closed` event.
This is best-effort infrastructure called from a bd hook script.

The batch version, `doConvoyCheck()` (line 427), scans all open convoys
and auto-closes any where all children are resolved. It skips convoys
with the `"owned"` label -- their lifecycle is managed manually.

### Molecule completion

For molecules, completion means all step beads are closed.
The root molecule bead is then closed, marking the entire formula run as
complete. For wisps (ephemeral molecules), this triggers eventual garbage
collection (Phase 6).

## Phase 6: Afterlife

Closed beads are not immediately deleted. They persist for querying,
audit, and progress tracking. But they do eventually get cleaned up.

### Query exclusion

The most immediate effect of closing: `Store.Ready()` no longer returns
the bead. This is invariant 7. The bead still appears in `Store.List()`
and `Store.Get()` -- it is findable but no longer "work."

For mail beads, `beadmail.Provider.Inbox()` (line 38 of
`internal/mail/beadmail/beadmail.go`) filters on `b.Status == "open"`, so
closed messages vanish from the inbox. `beadmail.Provider.Read()` closes
the bead as a side effect of reading (marking it "read").
`beadmail.Provider.Archive()` closes without reading.

### Wisp garbage collection

The controller runs a periodic wisp GC for closed molecules.
`memoryWispGC.runGC()` in `cmd/gc/wisp_gc.go` (line 58):

1. Lists closed molecules: `bd list --json --limit=0 --status=closed --type=molecule`
2. Compares each molecule's CreatedAt against a TTL cutoff
3. Deletes expired ones: `bd delete <id> --force`

The GC interval and TTL are configured via `[daemon]` config
(`wisp_gc_interval` and `wisp_ttl`). `newWispGC()` returns nil if either
is zero (disabled). The controller nil-guards before calling
`shouldRun()`.

### BdStore.Purge()

For bulk cleanup, `BdStore.Purge()` (line 125 of
`internal/beads/bdstore.go`) runs `bd purge --json` with a 60-second
timeout. This is an admin operation outside the Store interface, used by
the controller for periodic database maintenance. It removes closed
ephemeral beads from the Dolt database entirely.

### Order cooldown tracking

Closed order-tracking beads persist for cooldown gating. When an
order fires, a bead is created with label `order-run:<name>`.
On the next tick, `Store.ListByLabel("order-run:<name>", 1)` finds
the most recent run. If it is younger than the cooldown period, the
order is suppressed. The tracking bead's afterlife IS the cooldown
mechanism.

## State Transition Summary

```
  Store.Create()          sling/claim            Store.Close()
 ┌────────────┐       ┌────────────────┐       ┌──────────────┐
 │   open     │──────>│  in_progress   │──────>│    closed    │
 └────────────┘       └────────────────┘       └──────────────┘
       │  Ready() includes                            │  Ready() excludes
       │  Inbox() includes (messages)                 │  Inbox() excludes
       │                                              │  wisp GC eligible
       └── direct close (simple tasks) ───────────────┘
```

**Status mapping.** The bd CLI uses six statuses (open, in_progress,
blocked, review, testing, closed). `mapBdStatus()` in
`internal/beads/bdstore.go` collapses these to Gas City's three: closed
maps to closed, in_progress maps to in_progress, everything else to open.

## Code Map

| Phase | Key function | File |
|---|---|---|
| Create | `BdStore.Create()` | `internal/beads/bdstore.go` |
| Create | `exec.Store.Create()` | `internal/beads/exec/exec.go` |
| Create (molecule) | `Store.MolCook()` / `Store.MolCookOn()` | `internal/beads/beads.go` |
| Create (mail) | `beadmail.Provider.Send()` | `internal/mail/beadmail/beadmail.go` |
| Create (convoy) | `doConvoyCreate()` | `cmd/gc/cmd_convoy.go` |
| Discovery | `cmdHook()` / `doHook()` | `cmd/gc/cmd_hook.go` |
| Discovery | `EffectiveWorkQuery()` | `internal/config/config.go` |
| Discovery | `BdStore.Ready()` | `internal/beads/bdstore.go` |
| Routing | `doSling()` / `doSlingBatch()` | `cmd/gc/cmd_sling.go` |
| Routing | `EffectiveSlingQuery()` | `internal/config/config.go` |
| Routing | `instantiateWisp()` | `cmd/gc/cmd_sling.go` |
| Execution | `BdStore.Update()` | `internal/beads/bdstore.go` |
| Execution | `BdStore.SetMetadata()` | `internal/beads/bdstore.go` |
| Execution | provider-managed molecule step beads | `bd` or the configured beads backend |
| Completion | `BdStore.Close()` | `internal/beads/bdstore.go` |
| Completion | `doConvoyAutocloseWith()` | `cmd/gc/cmd_convoy.go` |
| Completion | `doConvoyCheck()` | `cmd/gc/cmd_convoy.go` |
| Afterlife | `memoryWispGC.runGC()` | `cmd/gc/wisp_gc.go` |
| Afterlife | `BdStore.Purge()` | `internal/beads/bdstore.go` |
| Afterlife | `beadmail.Provider.Archive()` | `internal/mail/beadmail/beadmail.go` |

## See Also

- [Bead Store architecture](beads.md) -- Store interface, invariants, and
  implementation details for all four store backends
- [Dispatch architecture](dispatch.md) -- how sling routes beads to agents
  and pools, including container expansion
- [Formulas architecture](formulas.md) -- formula parsing, molecule
  instantiation, and step dependency resolution
- [Orders architecture](orders.md) -- trigger conditions, cooldown
  tracking via order-run labels, and wisp dispatch
- [Messaging architecture](messaging.md) -- how mail composes on top of
  beads (messages are beads with type "message")
- [Glossary](glossary.md) -- authoritative definitions of bead, molecule,
  convoy, wisp, GUPP, NDI, and other terms used in this document
