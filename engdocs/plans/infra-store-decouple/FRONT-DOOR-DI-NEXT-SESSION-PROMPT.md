# Next-session prompt — front-door dependency injection (copy-paste)

Paste the block below into a fresh session.

---

Continue the object-model front-door migration on PR **#3800**. The front doors exist and
all ops route through them, but functions still take the raw `beads.Store` and wrap it
inline per call (`sessionFrontDoor(store)`, `orders.NewStore(...)`, `workAssignment{store}`),
so the no-raw-bead boundary is discipline-enforced. **Make it type-enforced:** construct each
front door once at the composition root (CityRuntime/controllerState) and pass it in place of
the store, so session/order/nudge call-tree functions have NO `beads.Store` in scope.

**Read first:** `engdocs/plans/infra-store-decouple/FRONT-DOOR-DI-HANDOFF.md` (full plan: the
exact call sites, the composition-root accessors, mixed-class functions, the raw-by-design
exceptions to keep on the store, the phasing, and the CI guard to add). Also skim
`OBJECT-MODEL-FRONT-DOOR-DESIGN.md` for context.

**Worktree / branch:** `/data/projects/gascity/.claude/worktrees/object-front-doors`, branch
`upstream/object-front-doors`, PR #3800 (base `upstream/store-interfaces`, stacked on #3773),
HEAD `aadeb34b4`, currently CI-green. Work only here. Run `git -C <worktree> log --oneline -5`
and `go build ./...` to confirm a green baseline before starting.

**How:** TDD (the refactor is byte-identical — existing reconciler/session/order suites +
the recording-fake parity tests are the oracle), **red-team between every slice**,
build-green per commit, halt-on-block. Drive it with a Workflow using **flattened `[]string`
schemas** (nested-object schemas cap the StructuredOutput tool). Phasing: (1) add the
`cr.sessions()/orders()/nudges()/workAssignment()` root accessors; (2) flip the session call
tree to take `*session.InfoStore`; (3) order; (4) nudge + work-assignment; (5) add the arch
guard that forbids `beads.Store`/`beads.SessionStore`/`beads.OrdersStore` params (and the
inline wrappers) in those files. Keep `beads.Store` only at the root, by-id/federation, graph
(`ApplyGraphPlan`), the work substrate, and the documented raw-by-design reads
(`session_reconciler.go:342`, `:3844`, `:3889`).

**Invariants:** wire byte-identical (empty `openapi.json`/`docs/reference/schema/`/generated-TS
diff; the two wire golden-oracle tests pass), runtime byte-identical, projection-invariance,
no typed-nil traps.

**Gates before push:** `go build ./...`, `go vet ./...`, `make lint`, `make fmt-check`,
`make check-docs`, full `make test-fast-parallel` (all 8 shards). Then push
`upstream/object-front-doors` and watch CI: `gh pr checks 3800 --watch`.

**Gotchas:** commit `--no-verify` (stale absolute `core.hooksPath`); `git checkout go.sum`
after builds (never commit the spurious charm.land/cloud.google lines); `gh pr
edit/create/ready` abort on the projectCards GraphQL deprecation — use REST/GraphQL mutations
(`gh api --method PATCH/POST repos/gastownhall/gascity/...`); never `tmux kill-server`; never
`go clean -cache` (`-testcache` ok). gascity Dolt is LOCAL-ONLY — never `bd dolt push/pull`.

**Done =** every session/order/nudge call-tree function takes its front door (no `beads.Store`
param), inline wrappers gone, arch guard green, full gates + #3800 CI green, wire byte-identical.
Update `memory/infra-beads-decoupling-plan.md` when complete.

---
