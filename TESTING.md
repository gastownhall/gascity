# Gas City Testing Philosophy

## Three tiers, clear boundaries

### 1. Unit tests (`*_test.go` next to the code)

Test what the CODE does. Internal behavior, edge cases, precise failure
injection. These are fast and run everywhere.

- Use `t.TempDir()` for filesystem tests
- Use `require` for preconditions (fail immediately), `assert` for checks
- Construct exact broken states in Go — corrupt files, concurrent writes,
  duplicate IDs, missing directories
- No env vars for controlling behavior — pass dependencies directly
- Same package as the code under test (access to unexported functions)

```go
func TestBeadStore_CorruptLine(t *testing.T) {
    dir := t.TempDir()
    os.WriteFile(filepath.Join(dir, "beads.jsonl"),
        []byte("{\"id\":\"gc-1\"}\nthis is not json\n"), 0644)
    store := beads.NewStore(dir)
    items, err := store.List()
    require.NoError(t, err)
    assert.Len(t, items, 1) // skips bad line, doesn't crash
}
```

When to use: corrupted data, concurrent writes, specific error types,
double-claim conflicts, rollback behavior, boundary conditions.

`make test` and `make test-cover` now follow this boundary strictly: they
run the fast unit loop only, with `GC_FAST_UNIT=1` gating slow `cmd/gc`
process scenarios. Slow process-backed cases
such as managed Dolt recovery, real `bd` lifecycle, tutorial regression
scripts, and the large `gc-beads-bd` provider suite are routed out of the
default path so local `make check` and CI `Check` stay focused on quick
feedback. If you need that full `cmd/gc` scenario coverage locally, run
`make test-cmd-gc-process`. In CI, the required non-short path is the
dedicated Linux `cmd/gc process` job. The generic integration package
shards keep `GC_FAST_UNIT=1` for `cmd/gc` unless explicitly overridden,
so they exercise the fast package sweep without duplicating the slow
process-backed suite. If you need the heavier package
coverage sweep locally, use `make test-integration-packages-cover` or
`make test-integration-shards-cover`. As a result, `coverage.txt` is the
fast unit-only baseline; the integration contribution comes from the
shard-specific `coverage.integration-*.txt` profiles and their matching
Codecov flags.

#### Sharded local runners

For broad local runs, prefer the repo's sharded wrappers over raw `go test`
commands. They use the same buckets as CI, run under a scrubbed environment,
and split single-package bottlenecks such as `cmd/gc` across multiple
processes.

Use these as the default entry points:

```bash
# Fast unit baseline, with cmd/gc split into shards.
make test-fast-parallel

# Full process-backed cmd/gc suite, sharded.
make test-cmd-gc-process-parallel

# CI integration buckets, sharded.
make test-integration-shards-parallel

# Fast + process-backed cmd/gc + integration shards.
make test-local-full-parallel
```

On large local machines, tune parallelism explicitly:

```bash
LOCAL_TEST_JOBS=48 CMD_GC_PROCESS_TOTAL=12 make test-local-full-parallel
```

For one package, shard top-level Go tests directly:

```bash
GO_TEST_COUNT=1 GO_TEST_TIMEOUT=20m ./scripts/test-go-test-shard ./cmd/gc 1 6
GO_TEST_TAGS=acceptance_b GO_TEST_TIMEOUT=10m ./scripts/test-go-test-shard ./test/acceptance/tier_b 2 3
```

For integration buckets, use the named shard runner:

```bash
./scripts/test-integration-shard packages-cmd-gc-3-of-6
./scripts/test-integration-shard review-formulas-retries-1-of-2
./scripts/test-integration-shard rest-full-4-of-8
```

To force the process-backed `cmd/gc` tests through the package shard for
diagnostics, override the default explicitly:

```bash
GC_FAST_UNIT=0 ./scripts/test-integration-shard packages-cmd-gc-3-of-6
```

Raw `go test` is still appropriate for a focused package or a single failing
test. Do not use it as the default for full local sweeps when a sharded target
exists.

### 2. Testscript (`.txtar` files in `cmd/gc/testdata/`)

Test what the USER sees. Run the real `gc` binary, assert on stdout/stderr.
These are the tutorial regression tests — each `.txtar` corresponds to a
tutorial's shell interactions.

- Uses `github.com/rogpeppe/go-internal/testscript`
- Testscript defaults missing backend env vars to local fakes:
  `GC_SESSION=fake`, `GC_BEADS=file`, `GC_DOLT=skip`
- Fakes have at most three modes per dependency:
  - `GC_SESSION=fake` — works, but in-memory
  - `GC_SESSION=fail` — all operations return errors
  - `GC_SESSION=tmux` — use real tmux explicitly
- `!` prefix means command should fail
- `stdout` / `stderr` assert on output
- `-- filename --` blocks create test fixtures

```
env GC_SESSION=fake

exec gc init $WORK/bright-lights
stdout 'City initialized'

exec gc rig add $WORK/tower-of-hanoi
stdout 'Adding rig'

exec bd create 'Build a Tower of Hanoi app'
stdout 'status: open'

-- $WORK/tower-of-hanoi/.git/HEAD --
ref: refs/heads/main
```

When to use: CLI output format, command success/failure, user-facing error
messages, tutorial flows end to end.

**The env var rule:** if you need more than two env vars to set up a failure
scenario, it's a unit test, not a testscript. In testscript, omitting the
session/beads env vars now means "use the fake defaults," not "use real tmux."

### 3. Integration tests (`//go:build integration`)

Test that real pieces fit together. Need real tmux, real filesystem, real
agent sessions. Run separately — not in CI by default.

```go
//go:build integration

func TestRealTmuxSession(t *testing.T) {
    // actually creates and kills tmux sessions
}
```

When to use: proving the fakes are honest, smoke testing the real infra,
testing tmux session lifecycle with real processes.

Run with: `go test -tags integration ./test/...`

**Supervisor binary smoke test** (`test/integration/huma_binary_test.go`):
builds `gc`, boots the supervisor against an isolated `GC_HOME`, waits
for `/health`, fetches `/openapi.json`, and runs `gc cities` as a
subprocess. Proves the whole stack — build tags, Huma registration,
listener bootstrap, socket paths — wires end-to-end through a real
binary. Run with `make test-integration-huma` or
`go test -tags integration -run TestHumaBinary ./test/integration/`.

**Supervisor API contract tests** (`test/integration/gc_live_contract_test.go`
and focused cases in `test/integration/huma_binary_test.go`): build the real
`gc` binary, start `gc supervisor run` against an isolated `GC_HOME` and
runtime dir, then exercise the HTTP API as a client would. These tests are
not handler unit tests and are not CLI tutorial tests; they prove that the
published API contract survives the full control plane: Huma registration,
OpenAPI generation, supervisor routing, city lifecycle, event publication,
storage providers, and asynchronous request completion.

The live API contract test has a few load-bearing rules:

- Validate responses against the supervisor's live `/openapi.json`. If the
  server says a route returns a schema, the integration test should prove the
  real response matches that schema.
- Exercise API mutations through HTTP only. Set `X-GC-Request` for mutating
  calls and observe durable results through API reads or events, not by
  reaching into internal Go state.
- Treat asynchronous operations as two-step contracts: the HTTP call returns
  quickly with `202 Accepted` and a `request_id`, then a `request.result.*`
  or `request.failed` event appears. Focused Huma binary tests should use
  `/v0/events/stream` for the critical async paths; broader coverage may poll
  event-list endpoints when the thing being tested is the API surface rather
  than SSE framing.
- Prefer self-provisioned fixtures. The test should create its own city, rig,
  provider/agent/session, beads, mail, formulas, convoys, and order-history
  fixtures where practical, then clean them up through the API.
- Keep the test hermetic. It must not depend on the developer's machine-wide
  supervisor, personal `~/.gc`, default tmux server, or a pre-existing city.
  Use isolated `GC_HOME`, runtime dir, ports, and process cleanup.
- Lock compatibility surfaces explicitly. If generated clients rely on an
  operation ID, method, path template, status code, or response schema, add an
  assertion for that contract rather than relying only on incidental behavior.
- Keep generated-read sweeps read-only. A sweep over OpenAPI GET routes is
  useful for schema and routing drift, but any GET route with unbound identity
  parameters still needs an explicit fixture-backed test.

Use supervisor API contract tests for externally visible behavior that only
exists when the real supervisor process is running: async city/session request
results, event streams, OpenAPI/response agreement, cross-route lifecycle
coherence, and end-to-end provider wiring. Do not put low-level edge cases
here. Corrupt files, exact parser failures, request validation branches, and
single handler error cases belong in unit tests next to the implementation.

#### Live worker inference tests (`//go:build acceptance_c`)

`test/acceptance/worker_inference` runs live Claude/Codex/Gemini/OpenCode CLI
sessions through tmux and requires local or CI-provided provider auth. It is
not part of PR CI. Run it deliberately when validating provider behavior:

```bash
make setup-worker-inference PROFILE=claude/tmux-cli
make test-worker-inference PROFILE=claude/tmux-cli
```

Supported profiles are `claude/tmux-cli`, `codex/tmux-cli`,
`gemini/tmux-cli`, and `opencode/tmux-cli`. OpenCode live tests use Gemini via
`--model google/gemini-2.5-flash` by default; set
`GC_WORKER_INFERENCE_OPENCODE_MODEL` to override it and provide
`GOOGLE_GENERATIVE_AI_API_KEY`, `GEMINI_API_KEY`, or `GOOGLE_API_KEY` for auth.
Nightly CI runs the configured profile matrix with its credentials and uploads
worker report artifacts.

### 4. Documentation sync tests (`test/docsync`)

These tests keep the public docs surface honest.

They currently verify:

- tutorial command coverage against the corresponding txtar tests
- local Markdown link targets across the repo docs
- Mintlify navigation page references in `docs/docs.json`

Run them directly with:

```
go test ./test/docsync
```

Gas City's own tests for this code live in `gascity_test.go` (adapter
unit tests) and `test/integration/bdstore_test.go` (conformance).

#### Two flavors of integration tests

**Low-level** (`internal/runtime/tmux/tmux_test.go`): test raw tmux
operations (NewSession, HasSession, KillSession) directly against the
tmux library. Session names use the `gt-test-` prefix.

**End-to-end** (`test/integration/`): build the real `gc` binary and
run it against real tmux. Validates the tutorial experience: `gc init`,
`gc start`, `gc stop`, bead CRUD.

**BdStore conformance** (`test/integration/bdstore_test.go`): runs the
beads conformance suite against `BdStore` backed by a real dolt server.
Proves the full stack: dolt server → bd CLI → BdStore → beads.Store.
Requires dolt and bd installed; skips otherwise.

#### Session safety for end-to-end tests

Test cities use a **`gctest-<8hex>` naming prefix** so sessions are
visually distinct from real gascity sessions (`gc-<cityname>-<agent>`).

Three layers prevent orphan sessions:

1. **Pre-sweep** (TestMain): `KillAllTestSessions()` kills all
   `gc-gctest-*` sessions from prior crashed runs.
2. **Per-test** (`t.Cleanup`): the `tmuxtest.Guard` kills sessions
   matching its specific city prefix.
3. **Post-sweep** (TestMain defer): final sweep after all tests.

#### The `tmuxtest.Guard` pattern

```go
guard := tmuxtest.NewGuard(t) // generates "gctest-a1b2c3d4", registers cleanup
cityDir := setupRunningCity(t, guard)

session := guard.SessionName("mayor") // "gc-gctest-a1b2c3d4-mayor"
if !guard.HasSession(session) { ... }
```

- `test/tmuxtest/guard.go` — reusable session guard helper
- `RequireTmux(t)` — skips test if tmux not installed
- `KillAllTestSessions(t)` — package-level sweep for TestMain

### 5. Coordination tests (`cmd/gc/lifecycle_coordination_test.go`)

Test that components are **called in the right order**. Conformance tests
verify each component's contract in isolation; coordination tests verify
the wiring between components.

**What coordination tests prove:**
- Lifecycle ordering (ensure-ready before init, shutdown after agents stop)
- Hook survival (hooks reinstalled after init wipes them)
- Qualification consistency (all effective methods use the same name form)

**What they don't prove:**
- Component correctness — that's what conformance tests cover
- Full E2E behavior — that's integration tests

**The `exec:<spy>` pattern:**

```go
t.Setenv("GC_BEADS", "exec:"+spyScript)
```

The spy script logs every operation (`ensure-ready`, `init <dir> <prefix>`,
`shutdown`) to a file. Tests read the log and assert on ordering and
arguments. This exercises the real lifecycle code paths in
`beads_provider_lifecycle.go` without needing Dolt.

```go
// Verify ensure-ready precedes init.
ops := readOpLog(t, logFile)
if !strings.HasPrefix(ops[0], "ensure-ready") {
    t.Fatalf("first op should be ensure-ready, got: %s", ops[0])
}
```

**When to write a coordination test vs conformance test:**

| Question | Test type |
|---|---|
| Does the beads store handle corrupt JSONL? | Conformance |
| Does `gc start` call ensure-ready before init? | Coordination |
| Does the mail provider deliver to the right inbox? | Conformance |
| Do all three Effective* methods use the qualified name? | Coordination |
| Does the session provider start a session correctly? | Conformance |
| Does `gc stop` shut down beads after agents? | Coordination |

**The overtesting line:** don't re-verify contracts that conformance tests
already cover. Coordination tests check call ordering and argument plumbing,
not that individual operations produce correct results.

### Conformance testing

Every provider interface has a conformance test suite that validates the
contract against all implementations. These live in `*test/conformance.go`
packages and are imported by each implementation's test file:

| Interface | Conformance suite | Implementations tested |
|---|---|---|
| `beads.Store` | `internal/beads/beadstest/conformance.go` | MemStore, FileStore, BdStore |
| `runtime.Provider` | `internal/runtime/runtimetest/conformance.go` | Fake, tmux, subprocess, exec, k8s |
| `mail.Provider` | `internal/mail/mailtest/conformance.go` | beadmail, exec |
| `events.Recorder` | `internal/events/eventstest/conformance.go` | FileRecorder, exec |

Conformance tests verify the behavioral contract (create/read/update/delete,
error handling, concurrency). They deliberately don't test lifecycle ordering
or cross-provider coordination — that's what coordination tests are for.

For the new 0.15 config surface, use
`docs/packv2/doc-conformance-matrix.md` as the release-gating ledger for
what should block CI now, what should start blocking once warning plumbing
lands, and what remains tracked but non-gating.

### Provider seam inventory

All five provider seams, their lifecycle dependencies, and coordination
test coverage. This table is the checklist for new provider implementations.

| Seam | Implementations | Lifecycle deps | Coordination tested? |
|---|---|---|---|
| **Runtime** (`runtime.Provider`) | tmux, exec, k8s, fake | None (stateless start/stop) | Via lifecycle start order test |
| **Beads** (`beads.Store`) | MemStore, FileStore, BdStore | ensure-ready → init → hooks | `TestLifecycleCoordination_*` |
| **Mail** (`mail.Provider`) | beadmail, exec | Depends on beads store | No — not a lifecycle seam; conformance sufficient |
| **Events** (`events.Recorder`) | FileRecorder, exec | None (append-only) | No — stateless append, conformance sufficient |
| **Dolt** (internal) | dolt.EnsureRunning, dolt.StopCity | ensure → init, stop after agents | Covered by beads lifecycle (exec spy) |

**Adding a new provider:** When adding a new implementation of any seam:
1. Run the conformance suite against it (mandatory)
2. If the provider has lifecycle dependencies (startup ordering, shutdown
   sequencing), add a coordination test using the `exec:<spy>` pattern
3. Update this table

## Decision guide

| Question you're testing | Tier |
|---|---|
| Does `bd create` print the right output? | Testscript |
| Does `gc start` fail gracefully without tmux? | Testscript (`GC_SESSION=fail`) |
| Does `gc rig add` fail for a missing path? | Testscript (real missing path) |
| Does the beads store skip corrupted JSONL lines? | Unit test |
| Does claim return ErrAlreadyClaimed on double-claim? | Unit test |
| Does concurrent bead creation avoid corruption? | Unit test |
| Does startup roll back if step 3 of 5 fails? | Unit test |
| Does a real tmux session start and respond to send-keys? | Integration |

## Dependencies

| Package | Purpose |
|---|---|
| `testing` (stdlib) | `t.TempDir()`, `t.Run()`, subtests, build tags |
| `github.com/stretchr/testify` | `assert` and `require` — cleaner assertions |
| `github.com/rogpeppe/go-internal/testscript` | Tutorial regression from `.txtar` files |

## Test doubles

No mock libraries. No `gomock`. No `mockgen`. Every test double is a
hand-written concrete type that lives in the same package as the
interface it implements.

### The four test doubles

| Double | Interface | Package | Strategy |
|---|---|---|---|
| `runtime.Fake` | `runtime.Provider` | `internal/runtime` | In-memory state + spy + broken mode |
| `fsys.Fake` | `fsys.FS` | `internal/fsys` | In-memory maps + spy + per-path error injection |
| `beads.MemStore` | `beads.Store` | `internal/beads` | Real logic, in-memory backing (also used by `FileStore` internally) |

### Spy pattern

Every fake records calls as `[]Call` structs. Tests verify both the
result AND the call sequence:

```go
sp := runtime.NewFake()
_ = sp.Start(context.Background(), "mayor", runtime.Config{})
_ = sp.Attach("mayor")

// Verify call sequence recorded by the fake runtime.
want := []string{"Start", "Attach"}
for i, c := range sp.Calls {
    if c.Method != want[i] { ... }
}
```

### Error injection strategies

Three patterns, used where they fit:

**Per-path errors** (`fsys.Fake`) — fine-grained, fail specific operations:
```go
f := fsys.NewFake()
f.Errors["/city/rigs"] = fmt.Errorf("disk full")
```

**Modal errors** (`runtime.Fake`) — all-or-nothing broken mode:
```go
f := runtime.NewFake()
f.Broken = true // Start/Stop/Attach and related operations return errors
```

### Compile-time interface checks

Every fake has a compile-time assertion in its test file:

```go
var _ Provider = (*Fake)(nil)
```

### Fakes live next to the interface

Fakes are exported types in the same package as their interface. This
makes them importable by cross-package unit tests (e.g., `cmd/gc`
imports `runtime.NewFake()`).

## The do*() function pattern

Every CLI command splits into two functions:

- **`cmdFoo()`** — wires up real dependencies (reads cwd, loads config,
  calls `newSessionProvider()`), then calls `doFoo()`.
- **`doFoo()`** — pure logic. Accepts all dependencies as arguments.
  Returns an exit code.

Unit tests call `doFoo()` directly with fakes:
```go
sp := runtime.NewFake()
code := doSessionAttach(sp, "mayor", &stdout, &stderr)
```

Testscript tests call `gc foo` which routes through `cmdFoo()` →
`doFoo()`.

### When to use each

| I want to test... | Call |
|---|---|
| Pure logic with injected failures | `doFoo()` with a fake |
| CLI output format, exit codes | `exec gc foo` in txtar |
| That the factory wiring is correct | `exec gc foo` in txtar with `GC_SESSION=fake` |

## The executor interface pattern

When a function's **argument construction** is the behavior under test
(flag injection, command building), extract the subprocess call behind
an executor interface. This separates "what arguments are built" from
"running a real binary."

**When to use:** Code that constructs `exec.Command` arguments
conditionally (socket flags, env vars, flag lists). The test verifies
the args array, not the subprocess outcome.

**When NOT to use:** When the logic under test is the orchestration
sequence (which methods are called in what order). Use the `startOps`
interface pattern instead.

**Example:** `tmux.executor` — `fakeExecutor` captures the `[]string`
args passed to each tmux command. Tests verify socket flags, UTF-8
flags, and argument ordering without a tmux binary.

## Env var fakes for testscript

Testscript needs fakes too, but can't inject Go objects. The CLI has
factory functions that check env vars and return the appropriate
implementation.

**Current env vars:**

| Env var | Values | Factory | Used by |
|---|---|---|---|
| `GC_SESSION` | `fake`, `fail`, (absent) | `newSessionProvider()` in `cmd/gc/providers.go` | `cmd_start.go`, `cmd_stop.go`, `cmd_agent.go` |
| `GC_BEADS` | `file`, `bd`, (absent) | `beadsProvider()` in `cmd/gc/providers.go` | bead commands, `cmd_init.go`, `cmd_start.go` |
| `GC_DOLT` | `skip`, (absent) | N/A (checked inline) | dolt lifecycle in `cmd_init.go`, `cmd_start.go`, `cmd_stop.go` |

**Design rules for env var fakes:**
- The fake never reads env vars itself — the factory function does
- At most three modes per dependency: works, fails, real
- If you need more than two env vars to set up a test scenario, it
  belongs in a unit test, not testscript

## MemStore: real implementation, not a fake

`beads.MemStore` is not a test-only fake — it's a real `Store`
implementation backed by a slice. `FileStore` composes `MemStore`
internally for its in-memory state and adds persistence on top. This
makes `MemStore` usable both as a production building block and as a
test double for code that needs a `Store` without disk I/O.
