# Gas City

Gas City is an orchestration-builder SDK — a Go toolkit for composing
multi-agent coding workflows. It extracts the battle-tested subsystems from
Steve Yegge's Gas Town (github.com/steveyegge/gastown) into a configurable
SDK where **all role behavior is user-supplied configuration** and the SDK
provides only infrastructure. The core principle: **ZERO hardcoded roles.**
The SDK has no built-in Mayor, Deacon, Polecat, or any other role. If a
line of Go references a specific role name, it's a bug.

You can build Gas Town in Gas City, or Ralph, or Claude Code Agent Teams,
or any other orchestration pack — via specific configurations.

**Why Gas City exists:** Gas Town proved multi-agent orchestration works,
but all its roles are hardwired in Go code. Steve realized the way work is
expressed — as beads composed into formulas — was powerful enough to abstract
roles into configuration. Gas City extracts that insight into an SDK where Gas
Town becomes one configuration among many.

## Current integration mission

This fork is integrating **Gas City + T3 Code + a DoltLite-backed beads
store**. The target is not a permanent divergence from upstream Gas City.
The target is a maintainable integration branch whose useful changes can
track upstream easily and whose fork-specific behavior is isolated behind
small, obvious ownership boundaries.

When working here, assume three codebases matter:

- **This repo** (`/data/projects/gascity`): the Gas City SDK and the fork
  integration layer.
- **T3 Code** (`/data/projects/t3code` when present): the UI/runtime that
  hosts visible agent threads through the `t3bridge` runtime provider.
- **Beads / bd with DoltLite**: the work ledger backend. Gas City should use
  the normal beads abstractions and keep DoltLite-specific read/write behavior
  contained in beads/provider boundaries.

### Upstream alignment rules

- Keep `upstream/main` easy to merge. Prefer new files, small adapters,
  and fork-owned packages over broad edits to upstream-owned code.
- If upstream code must change, make the patch minimal and idiomatic so it
  can be rebased, dropped, or proposed upstream cleanly.
- Do not bury T3 Code or DoltLite assumptions in generic SDK paths. Put
  provider-specific behavior behind the existing runtime, config, or beads
  backend boundaries.
- Before rebuilding a missing feature from scratch, search history. This
  fork has repeatedly lost working code during branch churn; older branches
  and commits often already contain the fix.
- Treat archived plans and audits as evidence, not gospel. Confirm against
  current code and current upstream before porting.

### Feature archaeology workflow

Use git history deliberately when a feature appears missing or regressed:

```bash
git remote -v
git fetch upstream
git log --all --oneline --decorate --grep '<keyword>'
git log --all --oneline --decorate -- <path>
git show <commit>:<path>
git diff upstream/main...HEAD -- <path>
git range-diff upstream/main...HEAD
```

Useful search targets:

- T3 bridge/runtime: `t3bridge`, `T3Bridge`, `internal/runtime/t3bridge`,
  `cmd/gc/template_resolve_t3bridge.go`
- DoltLite/beads backend: `doltlite`, `DoltLite`, `internal/beads`,
  `providers.go`, `beads_provider_lifecycle`
- Prior parity work: `engdocs/archive/analysis/gastown-upstream-audit.md`,
  `engdocs/archive/analysis/feature-parity.md`,
  `engdocs/contributors/dolt-regression-audit.md`

If history contains working code, prefer porting the smallest proven slice
instead of inventing a parallel mechanism.

## Development approach

**TDD.** Write the test first, watch it fail, make it pass. Every package
has `*_test.go` files next to the code. Integration tests that need real
infrastructure (tmux, filesystem) go in `test/` with build tags.

**The architecture docs are a reference, not a blueprint.** When the DX
conflicts with the docs, DX wins. We update the docs to match.

## Architecture

**Orchestration is the value.** A formula is a method for how a job gets
done, and the controller (`engdocs/architecture/controller.md`) runs it as a
graph — decomposing the job into beads, fanning the ready ones out to many
agents at once, gating each step on its dependencies, retrying failures, and
draining convoys in parallel, driving the work to completion outside the
user's session. The control dispatcher (`internal/dispatch`) executes control
beads — check, retry, fan-out, tally, drain, scope-check, workflow-finalize
(`docs/reference/specs/formula-spec-v2.md` sec 0) — and that dispatcher is
the engine. Orders trigger formulas on a schedule or event; health patrol
keeps the fleet alive.

**This orchestration is composed from primitives, with ZERO hardcoded
roles.** Beads are the universal persistence substrate — work survives
sessions — and every orchestration mechanism is provably composable from the
five primitives. That composability is what lets the same SDK be configured
as Gas Town, Ralph, or any other pack; no role name appears in Go. (A single
agent running a formula's steps in sequence in the user's own session —
formula v1 — is still supported as a peer shape, but graph orchestration is
why Gas City exists.)

### Code-layering view (implements the six primitives)

> **Authoritative user model:** `docs/getting-started/how-gas-city-works.md` defines the
> six primitives (**Agent** = WHO, **Bead** = WHAT, **Formula** = HOW,
> **Rig** = WHERE, **Pack** = CONFIGURES, **Event** = OBSERVE). That is the
> canonical conceptual model. The view below is the *code-layering lens*: it
> decomposes the Go substrate by layer so contributors can reason about
> imports, side-effect confinement, and the CI invariants below. It is a
> finer-grained projection of the same six primitives, not a competing
> taxonomy.

How the code substrate maps onto the six user-facing primitives:

| Code substrate (this view)                          | User-facing primitive |
| --------------------------------------------------- | --------------------- |
| Session + Prompt Templates                          | **Agent** (WHO)       |
| Task Store (Beads)                                  | **Bead** (WHAT)       |
| Formulas + Molecules + Dispatch (Sling) + Orders + Health Patrol | **Formula** (HOW) |
| Rigs (project/repo registered with the city)        | **Rig** (WHERE)       |
| Config (`pack.toml` / `city.toml`; the City is the local (root) pack — it imports shared packs) | **Pack** (CONFIGURES) |
| Event Bus                                            | **Event** (OBSERVE)   |

**Layer 0-1 substrate:**

1. **Session** — start/stop/prompt/observe sessions regardless of
   provider. Identity (via `agent.SessionNameFor`), pools, sandboxes,
   resume, crash adoption. Lifecycle is a bead-backed projection
   (`internal/session/lifecycle_projection.go`). Runtime providers
   (tmux, subprocess, exec, k8s, fake) plus routing layers (acp,
   auto, hybrid) live under `internal/runtime/` and plug in behind
   the Session surface. (Under the **Agent** primitive.)
2. **Task Store (Beads)** — CRUD + Hook + Dependencies + Labels + Query
   over work units. Everything is a bead: tasks, mail, convoy members.
   (The **Bead** primitive.)
3. **Event Bus** — append-only pub/sub log of all system activity. Two
   tiers: critical (bounded queue) and optional (fire-and-forget).
   Events are fired by activity as outbound notifications so humans and
   agents can watch; the bus is the delivery machinery. (Under the
   **Event** primitive.)
4. **Config** — TOML parsing with progressive activation (Levels 0-8 from
   section presence) and multi-layer override resolution. This is the
   machinery beneath the **Pack** primitive: `pack.toml`/`city.toml`
   declare agents, formulas, and orders, and the City is the local (root)
   pack that imports shared packs.
5. **Prompt Templates** — Go `text/template` in Markdown defining what
   each role does. The behavioral specification, supplied by a Pack and
   rendered into a running Agent.

**Layer 2-4 substrate:**

6. **Messaging** — Mail = `TaskStore.Create(bead{type:"message"})`.
   Nudge = a session-layer operation implemented via
   `runtime.Provider.Nudge()` (and exposed through
   `worker.Handle.Nudge()` at the worker boundary). No new
   primitive needed.
7. **Formulas** — a Formula is the reusable method (TOML parsed by
   Config) applied *over* a convoy of beads, looping/fanning each to an
   Agent. (When a formula runs, it materializes as a molecule — a root
   bead plus child step beads in the Task Store; wisps are the ephemeral
   variant. That materialization is a v1 implementation detail, not part
   of what a formula *is*.) Orders = formulas with gate conditions on the
   Event Bus that automate *when* a formula runs (Health Patrol is one
   kind of order). All of this is the **Formula** primitive.
8. **Dispatch (Sling)** — composed: find/spawn agent → select formula →
   materialize work as beads → hook to agent → nudge → create convoy →
   fire event. (Under the **Formula** primitive.)
9. **Health Patrol** — probe sessions (Session), compare thresholds
   (Config), publish stalls (Event Bus), restart with backoff. (One kind
   of order, under the **Formula** primitive.)

### Layering invariants

1. **No upward dependencies.** Layer N never imports Layer N+1.
2. **Beads is the universal persistence substrate** for domain state.
3. **Events are the universal outbound-notification mechanism** — fired by
   activity so humans and agents can watch; the bus is delivery machinery.
4. **Config is the universal activation mechanism.**
5. **Side effects (I/O, process spawning) are confined to Layer 0.**
6. **The controller drives all SDK infrastructure operations.**
   No SDK mechanism may require a specific user-configured agent role.

### Progressive capability model

Capabilities activate progressively via config presence.

| Level | Adds                   |
| ----- | ---------------------- |
| 0-1   | Session + tasks        |
| 2     | Task loop              |
| 3     | Multiple agents + pool |
| 4     | Messaging              |
| 5     | Formulas               |
| 6     | Health monitoring      |
| 7     | Orders                 |
| 8     | Full orchestration     |

## Architecture docs

Read **`engdocs/architecture/api-control-plane.md`** and
**`engdocs/contributors/huma-usage.md`** before touching:

- `internal/api/` (HTTP + SSE API layer)
- `cmd/gc/` (CLI) — especially anything that constructs events,
  calls `apiroute.go:apiClient()`, or uses
  `internal/api/genclient`
- `internal/events/` (event bus, registry)
- `internal/extmsg/` (external-messaging emitters)
- Anything that affects `internal/api/openapi.json`,
  `docs/reference/schema/openapi.json`, or the generated TS types under
  `cmd/gc/dashboard/web/src/generated/`

Load-bearing invariants enforced by CI (violating any fails the
build; full rationale is in the architecture docs):

- **Object model at the center.** `internal/{beads, mail, convoy,
  formula, events, session, worker, sling, ...}` is the canonical
  domain. The CLI (`cmd/gc/`) and the HTTP+SSE API
  (`internal/api/`) are projections over it. Neither re-implements
  domain logic. `internal/agent/` is a small helper package
  (session-name utilities, startup hints) — not a primitive.
- **Typed wire.** No hand-written JSON on any HTTP or SSE wire
  path; no `map[string]any` or `json.RawMessage` on wire types
  (documented exceptions live in the API control-plane doc). All
  endpoints are Huma-registered; the OpenAPI spec is generated,
  never hand-written (`TestOpenAPISpecInSync`).
- **Typed events.** Every constant in `events.KnownEventTypes`
  must have a registered payload via
  `events.RegisterPayload(constant, sample)`. Use
  `events.NoPayload` for events whose envelope fields alone
  capture the semantics. Enforced by
  `TestEveryKnownEventTypeHasRegisteredPayload`.

## Active migrations

These migrations are in flight. New code on affected paths must take
the canonical route, not the legacy route.

- **Worker boundary (started `12a0a848` on Apr 17 2026, in progress).**
  `internal/worker/handle.go` is the canonical boundary for session
  creation and lifecycle operations. Production `cmd/gc/*.go` files
  must route through `worker.Handle` — enforced by
  `TestGCNonTestFilesStayOnWorkerBoundary` in
  `cmd/gc/worker_boundary_import_test.go`, which forbids non-test
  files from importing `session.NewManager(`, `worker.SessionHandle`,
  `sessionlog`, and similar bypass paths in `cmd/gc`. The remaining
  manager-construction/direct-create bypasses are split by category:
  `internal/api/session_manager.go` constructs `session.Manager` values
  for API handlers, and `internal/api/session_resolution.go` still calls
  `mgr.CreateAliasedNamedWithTransportAndMetadata(...)` directly. This
  list is not a sessionlog read-site inventory; stream and transcript
  readers in `internal/api/` and `internal/session/` still read
  session logs directly. Package-internal helpers in `internal/session/`
  may construct and use `session.Manager`; tests may construct it
  directly. Do not add new non-test direct `session.Manager.Create*` call
  sites outside the worker boundary.
- **Session-first (completed `dd90ac0a` on Mar 8 2026).** The former
  Agent Protocol primitive was removed; responsibilities moved to
  `internal/session/` (lifecycle) and `internal/runtime/` (providers).
  `internal/agent/` is now a helper package with session-name utilities
  and startup hints — not a primitive. Do not reconstruct the
  `Agent` / `Handle` interfaces.

## Design decisions (settled)

These decisions are final. Do not revisit them.

- **City-as-directory model.** A city is a directory on disk containing
  `city.toml`, `.gc/` runtime state, and `rigs/` infrastructure.
- **Fresh binary, not a Gas Town fork.** We build `gc` from scratch.
- **TOML for config.** `pack.toml` (definition) and `city.toml` (deployment) are the config files.
- **Tutorials win over architecture docs.** When the docs disagree, we update the docs.
- **No premature abstraction.** Don't build interfaces until two
  implementations exist.
- **Mayor is overseer, not worker.** The mayor plans; coding agents work.
- **`internal/` packages for now.** SDK exports (`pkg/`) are future work.
  Everything is private to the `gc` binary until the API stabilizes.
- **ZERO hardcoded roles.** Roles are pure configuration. No role name
  appears in Go source code.

## Decision frameworks

- **`engdocs/contributors/primitive-test.md`** — The Primitive Test: three necessary
  conditions (Atomicity + becomes more useful as models improve + keeps
  judgment out of Go) for whether a capability belongs in the SDK vs the
  consumer layer. Apply this before adding any new primitive.
- **`engdocs/archive/backlogs/worktree-roadmap.md`** — Worktree isolation roadmap, polecat
  lifecycle analysis, and Gas Town cleanup bug lessons.

## Key design principles

- **Keep judgment out of Go.** Go handles transport, not reasoning. The
  framework moves work; it doesn't reason about it. If a line of Go contains
  a judgment call, it's a violation. **The test:** does any line of Go contain
  a judgment call? An `if stuck then restart` is framework intelligence. Move
  the decision to the prompt.
- **A primitive must become more useful as models improve.** Every primitive
  should grow MORE useful as models improve, not less. Don't build heuristics
  or decision trees.
- **If you find work on your hook, you run it.** No confirmation, no waiting.
  The hook having work IS the assignment. This is rendered into agent prompts
  via templates, not enforced by Go code.
- **The system converges because work persists.** The system converges to
  correct outcomes because work (beads), hooks, and molecules are all
  persistent. Sessions come and go; the work survives. Multiple independent
  observers check the same state idempotently and converge on it. Redundancy
  is the reliability mechanism.
- **No status files — query live state.** Never write PID files, lock files,
  or state files to track running processes. Always discover state by querying
  the system directly (process table, port scans, `ps`, `lsof`). Status files
  go stale on crash and create false positives. The process table is the
  single source of truth for "what is running."
- **SDK self-sufficiency.** Every SDK infrastructure operation (gate
  evaluation, health patrol, bead lifecycle, order dispatch) must
  function with only the controller running. No SDK operation may
  depend on a specific user-configured agent role existing. The
  controller drives infrastructure; user agents execute work. Test:
  if removing a `[[agent]]` entry breaks an SDK feature, it's a
  violation.

## What Gas City does NOT contain

These are permanent exclusions, not "not yet." Each fails the test of
becoming more useful as models improve — it becomes LESS useful instead.

- **No skills system** — the model IS the skill system
- **No capability flags** — a sentence in the prompt is sufficient
- **No MCP/tool registration** — if a tool has a CLI, the agent uses it
- **No decision logic in Go** — the agent decides from prompt and reality
- **No hardcoded role names** — roles are pure configuration

## Code conventions

- Unit tests next to code: `config.go` → `config_test.go`
- `t.TempDir()` for filesystem tests
- Integration tests use `//go:build integration`
- `cobra` for CLI, `github.com/BurntSushi/toml` for config
- Atomic file writes: temp file → `os.Rename`
- No panics in library code — return errors
- Error messages include context: `fmt.Errorf("adding rig %q: %w", name, err)`
- Role names never appear in Go code. If you're writing `if role == "mayor"`,
  it's a design error.
- **Tmux safety:** Never run bare `tmux kill-server` as cleanup. Never kill the
  default tmux server. If tmux cleanup is required, target only the known
  city/test socket explicitly with `tmux -L <socket> ...`, or prefer `gc stop`
  for city shutdown. Treat personal tmux servers as out of bounds.
- **Adding agent config fields:** When adding a field to `config.Agent`,
  also add it to `AgentPatch`, `AgentOverride`, their apply functions
  (`applyAgentPatch`, `applyAgentOverride`), and the `poolAgents` deep-copy
  in `cmd/gc/pool.go`. `TestAgentFieldSync` enforces this for the struct
  definitions; the apply functions and pool deep-copy must be checked
  manually.
- **Adding rig config fields:** When adding a field to `config.Rig`, also
  add the corresponding optional field to `RigPatch` and wire the merge
  into `applyRigPatch` so layered configs (fragments, patches) can
  override it. No field-sync test exists for Rig today; the patch path
  must be checked manually.

- `TESTING.md` — testing philosophy, tier boundaries, and sharded local
  runners. Read before writing any test. For broad local sweeps, prefer the
  documented shard targets (`make test-fast-parallel`,
  `make test-cmd-gc-process-parallel`, `make test-integration-shards-parallel`,
  `make test-local-full-parallel`) over raw `go test`.

## Build Cache Conventions

**Hard ban: never run `go clean -cache`** in any script, hook, or agent session.

Running `go clean -cache` against a shared `GOCACHE` (the default when
`$GOCACHE` is not overridden) corrupts the fleet-wide build cache for every
concurrent executor. Each executor that hits a missing cache entry then runs a
full rebuild, and any that calls `go clean -cache` mid-flight invalidates all
the others' in-progress caches. The incident (vp-g96b, 2026-06-13) produced
~10 cascading cache-miss errors across the executor pool.

**Safe alternative for cold builds:**

```bash
GOCACHE=$(mktemp -d) go build ./cmd/gc/
```

This isolates the cache to a throwaway directory without touching the shared
pool. Clean up with `rm -rf` after if disk space matters.

**Exception:** `go clean -testcache` is explicitly allowed. It clears only the
test-result cache, not the compiled-object cache, and does not corrupt
concurrent builds.

## Code quality gates

Before considering any task complete:

- Fast unit baseline passes (`make test`, or `make test-fast-parallel` on
  machines where sharding is useful)
- Broader process/integration coverage uses the sharded targets documented in
  `TESTING.md` instead of one monolithic `go test ./...` sweep
- `go vet ./...` clean
- `.githooks/pre-commit` is active locally (`git config core.hooksPath`
  prints `.githooks`) and has run for the staged change
- `make dashboard-check` passes for any change touching `internal/api/`,
  `internal/api/openapi.json`, `docs/reference/schema/openapi.*`,
  `cmd/gc/dashboard/`, or generated dashboard types
- The dashboard starts locally and serves the app for dashboard/API-schema
  changes; use `npm run preview -- --host 127.0.0.1 --port <port>` from
  `cmd/gc/dashboard/web` after `make dashboard-check`
- Every exported function has a doc comment
- No premature abstractions
- Tests cover happy path AND edge cases

## Non-Interactive Shell Commands

**ALWAYS use non-interactive flags** with file operations to avoid hanging on confirmation prompts.

Shell commands like `cp`, `mv`, and `rm` may be aliased to include `-i` (interactive) mode on some systems, causing the agent to hang indefinitely waiting for y/n input.

**Use these forms instead:**

```bash
# Force overwrite without prompting
cp -f source dest           # NOT: cp source dest
mv -f source dest           # NOT: mv source dest
rm -f file                  # NOT: rm file

# For recursive operations
rm -rf directory            # NOT: rm -r directory
cp -rf source dest          # NOT: cp -r source dest
```

**Other commands that may prompt:**

- `scp` - use `-o BatchMode=yes` for non-interactive
- `ssh` - use `-o BatchMode=yes` to fail instead of prompting
- `apt-get` - use `-y` flag
- `brew` - use `HOMEBREW_NO_AUTO_UPDATE=1` env var

<!-- BEGIN BEADS INTEGRATION v:1 profile:minimal hash:ca08a54f -->

## Beads Issue Tracker

This project uses **bd (beads)** for issue tracking. Run `bd prime` to see full workflow context and commands.

### Quick Reference

```bash
bd ready              # Find available work
bd show <id>          # View issue details
bd update <id> --claim  # Claim work
bd close <id>         # Complete work
```

### Rules

- Use `bd` for ALL task tracking — do NOT use TodoWrite, TaskCreate, or markdown TODO lists
- Run `bd prime` for detailed command reference and session close protocol
- Use `bd remember` for persistent knowledge — do NOT use MEMORY.md files
- For controller or session reconciler incidents, use `gc trace` and follow `engdocs/contributors/reconciler-debugging.md` for the artifact collection workflow.

## Session Completion

**When ending a work session**, you MUST complete ALL steps below. Work is NOT complete until `git push` succeeds.

**MANDATORY WORKFLOW:**

1. **File issues for remaining work** - Create issues for anything that needs follow-up
2. **Run quality gates** (if code changed) - Tests, linters, builds
3. **Update issue status** - Close finished work, update in-progress items
4. **PUSH TO REMOTE** - This is MANDATORY:
   ```bash
   git pull --rebase
   git push
   git status  # MUST show "up to date with origin"
   ```
   NOTE: gascity Dolt is LOCAL-ONLY (no remote). Do NOT run `bd dolt push`,
   `bd dolt pull`, or `bd dolt remote add` here -- they fail and re-introduce
   a doomed `origin` remote (ga-9wsri). Use `git push` only.
5. **Clean up** - Clear stashes, prune remote branches
6. **Verify** - All changes committed AND pushed
7. **Hand off** - Provide context for next session

**CRITICAL RULES:**

- Work is NOT complete until `git push` succeeds
- NEVER stop before pushing - that leaves work stranded locally
- NEVER say "ready to push when you are" - YOU must push
- If push fails, resolve and retry until it succeeds
<!-- END BEADS INTEGRATION -->

## Architecture Best Practices

These apply to all code in this project — frontend and server:

- **TDD (Test-Driven Development)** - write the tests first; the implementation
  code isn't done until the tests pass.
- **Consider First Principles** to assess your current architecture against the
  one you'd use if you started over from scratch.
- **Leverage Types** using statically typed languages (TypeScript, Rust, etc) so
  that we can leverage the power of the compiler as guardrails and immediate
  feedback on our code at build-time instead of waiting until run-time.
- **DRY (Don't Repeat Yourself)** – eliminate duplicated logic by extracting
  shared utilities and modules.
- **Separation of Concerns** – each module should handle one distinct
  responsibility.
- **Single Responsibility Principle (SRP)** – every class/module/function/file
  should have exactly one reason to change.
- **Clear Abstractions & Contracts** – expose intent through small, stable
  interfaces and hide implementation details.
- **Low Coupling, High Cohesion** – keep modules self-contained, minimize
  cross-dependencies.
- **Scalability & Statelessness** – design components to scale horizontally and
  prefer stateless services when possible.
- **Observability & Testability** – build in logging, metrics, tracing, and
  ensure components can be unit/integration tested.
- **KISS (Keep It Simple, Sir)** - keep solutions as simple as possible.
- **YAGNI (You're Not Gonna Need It)** – avoid speculative complexity or
  over-engineering.
- **Don't Swallow Errors** by catching exceptions, silently filling in required
  but missing values, masking deserialization with nulls or empty lists, or
  ignoring timeouts when something hangs. All of those are errors (client-side
  and server-side) and must be tracked in a centralized log so it can be used to
  improve the app over time. Also, inform the user as appropriate so that they
  can take necessary action.
- **No Placeholder Code** - we're building production code here, not toys.
- **No Comments for Removed Functionality** - the source is not the place to
  keep history of what's changed; it's the place to implement the current
  requirements only.
- **Layered Architecture** - organize code into clear tiers where each layer
  depends only on the one(s) below it, keeping logic cleanly separated.
- **Use Non-Nullable Variables** when possible; use nullability only when there
  is NO other possibility.
- **Use Async Notifications** when possible over inefficient polling.
- **Eliminate Race Conditions** that might cause dropped or corrupted data
- **Write for Maintainability** so that the code is clear and readable and easy
  to maintain by future developers.
- **Arrange Project Idiomatically** for the language and framework being used,
  including recommended lints, static analysis tools, folder structure and
  gitignore entries.
- **Keep Serialization/Deserialization At The Edges** to make full use of
  type-safe objects in the app itself and to centralize error handling for
  type-system translation. Do NOT allow untyped data with known shapes to flow
  through the system and subvert the type system.
- **Prefer Well-Known, High Quality OSS Libraries** instead of hand-rolling your
  own behavior to get more robust, better maintained and better tested results.
- **Treat Static Warnings And Info As Errors To Be Fixed**. The whole point of
  static checking (linting, compilers, etc) is that they surface issues at
  build-time so that they can be fixed now instead of lead to errors at runtime.
  Take advantage of that feedback to fix those errors!
- **Use Centralized Semantic Constant Values** using enums and constants instead
  of spreading magic numbers throughout the code.
