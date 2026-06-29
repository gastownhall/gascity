# Front-door dependency-injection — handoff

**Goal:** make the no-raw-bead-poking-of-non-work-objects boundary **type-enforced**
instead of discipline-enforced, by constructing each domain front door **once** at the
composition root and **passing it in place of the raw store** to the functions that
operate on that object. Then a function in the session/order/nudge call tree has **no
`beads.Store` in scope** — a raw bead op on a non-work object becomes untypeable, not
just absent.

This is the completion of the object-model front-door migration (`OBJECT-MODEL-FRONT-DOOR-DESIGN.md`).
It goes **on the same PR** (#3800, branch `upstream/object-front-doors`).

## Where things stand (starting point)

- **Worktree:** `/data/projects/gascity/.claude/worktrees/object-front-doors`
  (branch `upstream/object-front-doors`). Do all work here; do NOT touch
  `.claude/worktrees/infra-store-plan` or `.../upstream-store-pr`.
- **PR #3800** (base `upstream/store-interfaces`, stacked on #3773). HEAD `aadeb34b4`,
  19 commits (`4bd5631cb..`), **CI green**, `mergeable: CLEAN`.
- The front doors already exist and every op already routes through them — but the
  functions still take the **raw store** and wrap it **inline per call**:
  - session: `sessionFrontDoor(store) *session.InfoStore` (a free helper in cmd/gc),
    called inline at every site.
  - order: `orders.NewStore(beads.OrdersStore{Store: store})` inline.
  - work-assignment: `workAssignment{store: store}` (cmd/gc/work_assignment.go) inline.
  - nudge: the nudge front door, wrapped inline.
  - mail: `mail.Provider` (already injected — the reference pattern).

## The change

**From** (discipline-enforced — `store` still in scope, raw ops still compile):
```go
func healSomething(store beads.Store, ...) {
    sessionFrontDoor(store).ApplyPatch(id, batch)
}
```
**To** (type-enforced — no bead store in scope):
```go
func healSomething(sessions *session.InfoStore, ...) {
    sessions.ApplyPatch(id, batch)
}
// built ONCE at the root and threaded down:
//   cr.sessions()        -> *session.InfoStore  (session.NewInfoStore(cr.sessionsBeadStore()))
//   cr.orders()          -> *orders.Store
//   cr.workAssignment()  -> workAssignment
//   cr.nudges()          -> the nudge front door
```

## Call sites to convert (from the inline-wrap grep)

- **session** — `sessionFrontDoor(store)` at: `session_reconciler.go` (66,378,1818,2136,2162,2293,…),
  `session_wake.go` (68,611), `session_sleep.go` (156,171,181,295,310,332),
  `session_circuit_breaker.go` (628,670,739,760), `soft_reload.go` (146),
  `cmd_handoff.go` (377), `cmd_session_pin.go` (125), `cmd_prime.go` (588),
  `session_name_lookup.go` (219,234,241,242), `cmd_nudge.go` (1281),
  plus `session.NewInfoStore(beads.SessionStore{Store: store})` in `cmd_mail.go` (934,955,1151).
- **order** — `orders.NewStore(...)` at `order_dispatch.go` (557,617,1155,1286,1410),
  `cmd_order.go` (752,800,1387).
- **work** — `workAssignment{store: ...}` construction (cmd/gc/work_assignment.go + callers).
- **nudge** — the nudge front-door inline wraps.

## Rules / scope

1. **Construct once at the composition root** (CityRuntime / controllerState / the tick
   or run entry point), from the already-resolved class store; thread the front door
   value down. Delete the per-call inline wrappers (`sessionFrontDoor(store)` becomes
   the injected `sessions`).
2. **Mixed-class functions take multiple typed params** (e.g. a function that closes a
   session AND releases work takes `sessions *session.InfoStore, work workAssignment`).
3. **`beads.Store` survives ONLY at:** the composition root; by-id/federation
   (`storeref`); graph (`ApplyGraphPlan`); the work substrate; and the documented
   **raw-by-design** exceptions — `session_reconciler.go:342` (full status/metadata
   resync, not an attribute read) and the session-START work-dir/opt reads
   (`session_reconciler.go:3844/3889`). Keep those on the raw store; comment why.
4. **Byte-identical** — this is a signature/wiring refactor, behavior unchanged. The
   existing reconciler/session/order suites + the recording-fake parity tests are the
   oracle. No new metadata/op semantics.
5. **Add the regression guard** — an arch test (mirror
   `cmd/gc/worker_boundary_import_test.go` / `TestGCNonTestFilesStayOnWorkerBoundary`)
   that forbids non-test `beads.Store` / `beads.SessionStore` / `beads.OrdersStore`
   **parameters** (and the inline `sessionFrontDoor(store)` / `orders.NewStore(` /
   `workAssignment{` wrap) in the session/order/nudge call-tree files, so the boundary
   cannot regress. List the raw-by-design files as explicit allowances.

## Suggested phasing (each ≤5 files where possible, build-green, red-team between)

1. **Roots** — add `cr.sessions()/orders()/nudges()/workAssignment()` accessors (and the
   controllerState equivalents); no call-site change yet. Build-green.
2. **Session call tree** — flip the reconciler/tick/lifecycle/CLI session functions to take
   `*session.InfoStore`; construct at the tick/run entry; delete `sessionFrontDoor`. Biggest.
3. **Order** — flip the dispatch/cmd_order functions to take `*orders.Store`.
4. **Nudge + work-assignment** — flip those call trees.
5. **CI guard** — add the arch test + make it pass.

## Process (owner-directed)

- **TDD** (the refactor is byte-identical; existing tests + recording-fake prove it),
  **red-team between every slice**, build-green per commit, halt-on-block.
- Use a **Workflow** with **flattened `[]string` schemas** (nested-object schemas cap the
  `StructuredOutput` tool — that killed an earlier run).
- Commit `--no-verify` (stale absolute `core.hooksPath`); trailer
  `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`.
- After a build: `git checkout go.sum` (builds spuriously re-add charm.land/cloud.google
  module lines — never commit them).
- **Verify before push:** `go build ./...` · `go vet ./...` · `make lint` · `make fmt-check`
  · `make check-docs` · full `make test-fast-parallel` (all 8 shards — narrow `-run`
  filters MISS reload/tick tests) · empty diff on `internal/api/openapi.json` /
  `docs/reference/schema/` / `cmd/gc/dashboard/web/src/generated/` (wire byte-identical).
- **Push** to `upstream/object-front-doors` (the pre-push hook re-runs the fast suite),
  then watch CI on **#3800** to green: `gh pr checks 3800 --watch`.
- `gh pr edit/create/ready` ABORT on the projectCards GraphQL deprecation — use REST/GraphQL
  mutations directly (see the parent memory for the exact incantations).

## Done =

Every session/order/nudge call-tree function takes its front door (no `beads.Store` param);
the inline wrappers are gone; the arch guard is green; full gates + #3800 CI green; wire
byte-identical. The leak is then a **compiler invariant**, not a convention.
