---
title: "Life of a Bead"
---

> Last verified against code: 2026-11-08 for Phase 1 paths B, D and E,
> Phase 2 (Discovery), Phase 3 (Claiming) and the Code Map; 2026-03-01 for
> the rest. File line numbers are omitted because they drift; search for
> the function name.

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
`--formula`, sling instantiates a wisp first. `sling.DoSling()`
(`internal/sling/sling_core.go`) compiles the formula once and hands the
recipe to `InstantiateCompiledSlingFormula()` (`internal/sling/sling.go`),
which stamps the recipe's routes (see Phase 3) and calls
`molecule.Instantiate()` (`internal/molecule/molecule.go`). Every sling
launch instantiates through that function; `InstantiateSlingFormula()` is
its compile-first wrapper for `--on` and default-formula launches. Gas City
writes the root and step beads itself through the store; it does not shell
out to `bd mol`. `molecule.Cook()` and `molecule.CookOn()` are the
compile-and-instantiate wrappers other callers use.

The root bead ID is returned to sling, which routes it with
`finalizeInStore()`. A graph.v2 workflow skips that step:
`doStartGraphWorkflow()` (`internal/sling/sling_core.go`) marks the root
`in_progress`, and the routes stamped at instantiation stand.

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
`"convoy"`, then records each child bead as a member with a `tracks`
dependency from the convoy (`convoy.TrackItemIn()` in
`internal/convoy/membership.go`), leaving the child's ParentID alone.
`convoy.MembersIn()` reads those members plus any legacy parent-child
children. Sling also creates auto-convoys (`finalizeInStore()` in
`internal/sling/sling_core.go`) to track individual bead routing.

### Path E: Order dispatch

The controller's order dispatcher (`cmd/gc/order_dispatch.go`)
fires due orders. Formula orders call `molecule.Instantiate()` to
instantiate wisps, then stamp the root bead through `Store.Update()` with
its `order-run:<name>` label and, for an order with a pool, `gc.routed_to`.
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
work rather than being pushed assignments. Routed agents normally discover
work through the claim protocol rendered into the session startup prompt:
the protocol runs `gc hook --claim --drain-ack --json`, which finds existing
assigned work or atomically claims exactly one routed bead (or acknowledges a
runtime drain when nothing is available), and then the agent works that
claimed bead. Plain `gc hook` only lists routed work; it claims nothing.

### The hook mechanism (gc hook)

Every agent has a `work_query` config field. `gc hook`
(`cmd/gc/cmd_hook.go`) runs that query for plain discovery, and
`gc hook --claim` runs the same query to pick its candidates. The flow:

1. `cmdHookWithOptions()` resolves the agent from a positional arg or,
   without one, from `$GC_TEMPLATE` inside a session, else `$GC_ALIAS` or
   `$GC_AGENT`
2. Loads city config, checks suspension status
3. Calls `hookWorkQuery()`, which builds the query with
   `a.EffectiveWorkQueryFor(topo)` (`internal/config/workquery.go`) for the
   city's query topology and expands `{{.Rig}}`-style templates
4. Plain discovery runs the query through `doHookDiscovery()`, which calls
   `doHook()`; `--claim` hands it to `claimHookWork()`. Both run the shell
   command with `shellWorkQueryWithEnv()` against each store the agent can
   see, or only against the agent's own store when the generated query
   already reads city-wide through `gc ready` (see below)

A custom `work_query` is run verbatim. Otherwise the default is the shell
script built by `buildWorkQuery()` in `internal/config/workquery.go`. It is
the same for every agent, fixed or pooled (the control dispatcher also
probes its legacy `workflow-control` identity), and runs these tiers in
order, stopping at the first one that returns work:

1. **Assigned, in progress** (crash recovery): `bd list --status in_progress`
   for beads whose assignee is one of the session's identities. The tier
   loops over `$GC_SESSION_ID`, `$GC_SESSION_NAME` and `$GC_ALIAS` in that
   order, skipping empty and repeated values, so work is found whichever
   identifier assigned it. A candidate that is blocked or parked on a
   dispatch hold is skipped. An assigned graph.v2 workflow root found here
   is remembered as a fallback instead of being returned at once.
2. **Assigned, ready**: `bd ready --assignee=<identity> --limit=1` over the
   same identities, skipping dispatch-held rows.
3. **Origin gate**: only an ephemeral (pool) session, a session with no
   `$GC_SESSION_ORIGIN`, or a session whose `$GC_ALIAS` equals the route
   target goes on to the routed tiers. Any other session ends the query here
   (returning the remembered workflow root, if any), so a named session
   cannot claim its pool's routed work.
4. **Graph anchor probe**: routed, unassigned ready steps whose
   `gc.root_bead_id` is the remembered workflow root. If none are ready, the
   remembered root itself is returned.
5. **Pool demand**: `bd ready --metadata-field "gc.routed_to=<route>"
   --unassigned --exclude-type=epic` plus `--exclude-label` for the dispatch
   holds and the human label, reading up to `RoutedReadyClaimWindow` (20) rows
   in bd's priority order. A fallback follows for graph.v2 workflow roots
   created before roots carried `gc.routed_to`, which are routed only by
   `gc.run_target`. `<route>` is the agent's pool name when it has one, else
   its qualified name.

The script prints `[]` when no tier has work, or nothing at all when the
origin gate ends it; the hook reads both as no work. Mail
(`issue_type=message`) is withheld from every tier. Parent epics are
excluded from the routed tiers only, because an assigned epic wisp must
still be resumable. Wisps (ephemeral beads) need reads of their own: the
in-progress tier always adds a `bd query` read of them, and the
assigned-ready and pool-demand tiers add one only in the default
`bd_compatibility = "bd-1.0.4"` mode; under `"bd-1.0.5"` the ready reads
pass `bd ready --include-ephemeral` instead. On a city that serves a
coordination class from a store `bd` cannot reach from the work directory,
the topology swaps the `bd ready` and `bd list` reads for the federated
reader `gc ready`. When the reconciler runs the query with no session
identity, the assigned tiers skip and only pool demand fires.
`EffectivePoolDemandQuery()` shares the pool-demand tier's predicates, so
the reconciler's spawn decision and the worker's claim decision read the
same demand.

The default query shells out to `bd` (or `gc ready`) directly; it does
not go through `BdStore.Ready()`.

An empty queue and a failed query are different answers. A default work
query whose store read fails (bd missing from PATH, a store bd cannot read,
output jq cannot parse) exits with that read's non-zero status and names the
failure on stderr, instead of falling through to `[]`. One failed tier fails
the whole query: a later tier's answer is only correct given the earlier
ones. Plain `gc hook` then exits 2 rather than the no-work exit 1, and
`gc hook --claim` reports an error rather than a `no_work` drain, so a
worker whose tools are broken does not drain itself as idle.

The same holds across stores. When the hook runs the query against several
stores (an agent's own store plus rig or city stores) and one of them cannot
be read, the hook still serves work that another store returned and names
the unread store on stderr. If no store returned work, the result is the
failed-query result above, naming the store, not "no work". Only the agent's
own store's failure is escalated as the session's work-query failure event.

### The --inject mode

`gc hook --inject` is legacy Stop-hook compatibility. It exits 0 without
running the work query and emits no output. Routed discovery belongs in the
session startup claim protocol or an explicit `gc hook --claim` invocation.

### Ready() and the run-what-you-find rule

`Store.Ready()` returns the open beads that nothing blocks -- the
fundamental discovery primitive for Go callers. For BdStore:
`bd ready --json --limit 0`, filtered again in Go. For
exec.Store: the script receives `ready` as its operation argument.

Discovery feeds into the run-what-you-find rule: "If you find work on your
hook, YOU RUN IT." No confirmation, no waiting. This principle lives in
prompt templates, not Go code. Gas City ensures the work is visible; the
prompt tells the agent what to do.

## Phase 3: Claiming

An agent has discovered a bead through its hook. Now it claims ownership.
This is a status transition and assignee update.

### Sling as the routing mechanism

Before claiming, the bead must be routed to the agent. `doSling()` in
`cmd/gc/cmd_sling.go` calls `sling.DoSling()`, and the route is written in
`finalizeInStore()` (`internal/sling/sling_core.go`); for a convoy,
`sling.DoSlingBatch()` routes each open child the same way. Default
routing stamps metadata; it sets no assignee and no label.
`DefaultSlingQuery()` (`internal/config/workquery.go`) is the same for
every agent:

```
bd update {} --set-metadata gc.routed_to=<route>
```

`<route>` is the agent's pool name when it has one, else its qualified
name. `EffectiveSlingQuery()` returns the agent's `sling_query` verbatim
when one is configured, else this default.

The CLI and the API route through a typed `BeadRouter` (`cliBeadRouter`
in `cmd/gc/cmd_sling.go`, `apiBeadRouter` in
`internal/api/handler_sling.go`). For an agent on the default query it
writes `gc.routed_to` through the store's `SetMetadata()` instead of
running the shell command. A custom `sling_query` is expanded with
`sling.BuildSlingCommandForAgent()` (`internal/sling/sling.go`) and run
through the shell runner.

Formula steps get their routes before they are written:
`graphroute.ApplyGraphRouting()` (`internal/graphroute/graphroute.go`)
stamps `gc.routed_to` on the compiled recipe, so the step beads
`molecule.Instantiate()` creates come out already routed (a legacy formula
attached to a bead routes only that bead). A graph.v2 step whose formula
sets `assignee` to a session (a session bead ID, a named session, or a
live session's name) is bound to that session's bead instead of routed.

A routed bead stays open and unassigned. Any session whose work query
reaches the pool-demand tier for that route can claim it; the reconciler
counts it as demand and starts or wakes a session to serve it, within the
agent's session limits.

### The claiming act

`gc hook --claim`, which the startup claim protocol runs, makes the
claim. `claimHookWork()` (`cmd/gc/cmd_hook.go`) runs the work query, and
`tryHookClaim()` (`cmd/gc/cmd_hook_claim.go`) takes the first of these
that applies:

1. An `in_progress` bead already assigned to one of the session's
   identities is returned as `existing_assignment` without a new claim
   (`hookClaimExistingAssignment()`), so a restarted session resumes it.
2. An open bead assigned to one of the session's identities is claimed
   (`claimFirstReadyHookAssignment()`).
3. Otherwise one routed, unassigned candidate is claimed
   (`claimFirstEligibleHookCandidate()`).

Both claims go through the store's `Claim()`. On a bd store that runs
`bd update <id> --claim` with the session as the actor, which sets the
session as assignee and moves the bead from `open` to `in_progress`. The
bd CLI handles the atomic compare-and-swap, so when two sessions race for
one bead only one claim wins. An agent that claims a bead by hand runs
`bd update <id> --claim` itself. That claim fails when another agent
already holds the bead, and the agent must then not work it.

Sling assigns no one, for fixed and pooled agents alike. A bead gets an
assignee before the claim only when something sets one explicitly: for
example `bd update <id> --assignee=<session>`, graph routing that binds a
step to the session its formula names, or `gc hook --claim` pinning the
open siblings in a claimed step's continuation group to the same session
(`preassignHookContinuationGroup()`). The assigned tiers then find it and
step 2 above claims it.

`BdStore.Claim()` (`internal/beads/bdstore.go`) runs
`bd update <id> --claim --json` and reports a lost race as "not claimed"
rather than an error. Other bead edits go through `BdStore.Update()`:

```
bd update --json <id> --description "..." --add-label "..."
```

The `Store.Update()` contract: only non-nil fields in `UpdateOpts` are
applied. Labels append, never replace. This is invariant 9 from the bead
store specification.

### Container expansion

When `gc sling` receives a convoy ID, `doSlingBatch()` expands it through
`sling.DoSlingBatch()` (`internal/sling/sling_core.go`). That reads the
convoy's members with `convoy.MembersIn()`, filters to open ones, and
routes each child individually. Epics are no longer first-class
containers and are rejected by `gc sling`. The container itself is the
convoy -- no auto-convoy is created.

## Phase 4: Execution

The bead is now `in_progress` with an assignee. The agent works on it.
Gas City's infrastructure is mostly hands-off during this phase -- the
framework moves work but does not reason about it, so Go code does not
make decisions about work execution.

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

While the agent works, the controller's bead-driven session reconciler
(`reconcileSessionBeadsTracedWithNamedDemand()` in
`cmd/gc/session_reconciler.go`) monitors
session health.
If an agent crashes mid-execution, the bead persists in its current state,
so the system converges because the work itself survives. When the agent
restarts, it rediscovers the in-progress bead through its hook and resumes.
The bead is the durable record; sessions are ephemeral.

## Phase 5: Completion

The agent finishes work and closes the bead.

### Store.Close()

The agent calls `bd close <id>`, which flows through `BdStore.Close()`
(`internal/beads/bdstore.go`):

```
bd close --json <id>
```

The Store contract: Close sets status to `"closed"`. It is idempotent --
closing an already-closed bead is a no-op (invariant 6). After Close, the
bead no longer appears in `Ready()` results (invariant 7).

For exec.Store, the script receives `close <id>` as arguments
(`exec.Store.Close()` in `internal/beads/exec/exec.go`).

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
`doConvoyAutocloseWith()` in `cmd/gc/cmd_convoy.go` checks:

1. Does the closed bead have a ParentID?
2. Is the parent a convoy (not closed, not "owned")?
3. Are ALL sibling children now closed?

If yes, it closes the parent convoy and records a `convoy.closed` event.
This is best-effort infrastructure called from a bd hook script.

The batch version, `doConvoyCheck()`, scans all open convoys
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

For mail beads, `beadmail.Provider.Inbox()` (in
`internal/mail/beadmail/beadmail.go`) filters on `b.Status == "open"`, so
closed messages vanish from the inbox. `beadmail.Provider.Read()` closes
the bead as a side effect of reading (marking it "read").
`beadmail.Provider.Archive()` closes without reading.

### Wisp garbage collection

The controller runs a periodic wisp GC for closed molecules.
`memoryWispGC.runGC()` in `cmd/gc/wisp_gc.go`:

1. Lists closed molecules: `bd list --json --limit=0 --status=closed --type=molecule`
2. Compares each molecule's CreatedAt against a TTL cutoff
3. Deletes expired ones: `bd delete <id> --force`

The GC interval and TTL are configured via `[daemon]` config
(`wisp_gc_interval` and `wisp_ttl`). `newWispGC()` returns nil if either
is zero (disabled). The controller nil-guards before calling
`shouldRun()`.

### BdStore.Purge()

For bulk cleanup, `BdStore.Purge()` (in
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
mechanism. Closed tracking history beyond
`[beads.policies.order_tracking].delete_after_close` is pruned by
`gc order sweep-tracking` and the maintenance exec order, defaulting
to 7d while always keeping at least the latest 10 closed tracking
beads per order.

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
blocked, review, testing, closed), but the `beads.Store` contract stays
three-state: open, in_progress, closed. `BdStore` therefore maps bd's
blocked/review/testing values to open. An empty status from a backend is
also normalized to open.

## Code Map

| Phase | Key function | File |
|---|---|---|
| Create | `BdStore.Create()` | `internal/beads/bdstore.go` |
| Create | `exec.Store.Create()` | `internal/beads/exec/exec.go` |
| Create (molecule) | `InstantiateCompiledSlingFormula()` / `InstantiateSlingFormula()` | `internal/sling/sling.go` |
| Create (molecule) | `molecule.Instantiate()` / `molecule.Cook()` / `molecule.CookOn()` | `internal/molecule/molecule.go` |
| Create (mail) | `beadmail.Provider.Send()` | `internal/mail/beadmail/beadmail.go` |
| Create (convoy) | `doConvoyCreate()` | `cmd/gc/cmd_convoy.go` |
| Create (convoy) | `convoy.TrackItemIn()` / `convoy.MembersIn()` | `internal/convoy/membership.go` |
| Discovery | `cmdHookWithOptions()` / `hookWorkQuery()` / `doHook()` | `cmd/gc/cmd_hook.go` |
| Discovery | `EffectiveWorkQueryFor()` / `buildWorkQuery()` | `internal/config/workquery.go` |
| Discovery | `EffectivePoolDemandQuery()` | `internal/config/workquery.go` |
| Discovery | `BdStore.Ready()` | `internal/beads/bdstore.go` |
| Routing | `doSling()` / `doSlingBatch()` / `cliBeadRouter.Route()` | `cmd/gc/cmd_sling.go` |
| Routing | `apiBeadRouter.Route()` | `internal/api/handler_sling.go` |
| Routing | `DoSling()` / `DoSlingBatch()` / `finalizeInStore()` / `doStartGraphWorkflow()` | `internal/sling/sling_core.go` |
| Routing | `EffectiveSlingQuery()` / `DefaultSlingQuery()` | `internal/config/workquery.go` |
| Routing | `BuildSlingCommandForAgent()` | `internal/sling/sling.go` |
| Routing | `ApplyGraphRouting()` | `internal/graphroute/graphroute.go` |
| Claim | `claimHookWork()` | `cmd/gc/cmd_hook.go` |
| Claim | `tryHookClaim()` / `hookClaimExistingAssignment()` / `claimFirstReadyHookAssignment()` / `claimFirstEligibleHookCandidate()` | `cmd/gc/cmd_hook_claim.go` |
| Claim | `preassignHookContinuationGroup()` | `cmd/gc/cmd_hook_claim.go` |
| Claim | `BdStore.Claim()` | `internal/beads/bdstore.go` |
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
  convoy, wisp, the run-what-you-find rule, convergence through persistent
  work, and other terms used in this document
