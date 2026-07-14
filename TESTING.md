# Gas City Testing Philosophy

## Checked source-level resource ratchets

`test/test-resources.toml` is the checked P0.4 resource ledger. It scans tracked
Go source through parsed syntax and import identity, while only `*_test.go`
files contribute resource occurrences. The raw audit and source-debt rows
freeze process, sleep, environment, CWD, slow-process, HTTP test-server, and
package-level `net.Listen`, `net.ListenConfig.Listen`, `net.ListenUnixgram`,
and direct `syscall.Listen` call/file totals.
Exact Medium rows name a repository-relative directory, package clause,
top-level runnable owner, and resource list. Small-debt rows apply those exact
owners without weakening the raw anti-growth ratchets.

The Go-owned `bootstrapPolicy` pins every row's ceiling, historical totals,
owner, invariant, resource owner, migration, and expiry. Ordinary source
growth fails against that ceiling, and TOML-only normalization, relabeling, or
metadata edits fail against the policy before the live census is compared.

Changing `bootstrapPolicy` together with the TOML and generated table is an
explicit policy change that requires the same staged-diff council review as
other test-infrastructure changes. The guard makes ordinary drift visible; it
does not claim that self-modifying source can be cryptographically forbidden.

The canonical identity is package directory plus package clause plus top-level
`Test`, `Benchmark`, `Fuzz`, or `TestMain` name. Nested function literals and
subtests retain that top-level lexical owner. Methods, wrong signatures, and
helper functions are not runnable owners; resources lexically inside helpers
remain Small debt even when a Medium test calls the helper. Likewise, a
`TestMain` row classifies inherited package setup but exempts only matching
calls inside `TestMain`, never sibling tests.

This bootstrap does **not** infer resources recursively through arbitrary
helper calls or claim a complete shared-resource inventory. P0.4c currently
covers the three `net/http/httptest` constructors that open loopback servers
and the exact package-level `net.Listen` and `net.ListenUnixgram` constructors,
`net.ListenConfig.Listen` on lexically identified receivers, and direct
`syscall.Listen`. Direct `syscall.Socket`/`Bind` setup calls, typed and
packet-specific `net` constructors, helper-backed listeners whose constructors
live outside test source, tmux, Dolt, and other shared-host resources remain
explicit follow-up catalogs. A Medium resource may describe a helper-backed
runtime cost, but only syntax-owned calls in that exact runnable declaration
leave Small-debt accounting. The `ListenConfig` matcher uses lexical Go types
to follow same-file values, pointers, parameters, aliases, and typed factory
results rooted in the imported `net.ListenConfig` type; it does not load
cross-file package bodies or host toolchain export data.
`ga-80po0c.2.2` owns the listener, tmux, Dolt, and shared-host catalogs. E1
separately owns Large journey and provider entries.

The scanner recognizes direct calls to `os/exec.Command{,Context}` and
`time.Sleep`; package-level `net.Listen` and `net.ListenUnixgram`;
`net.ListenConfig.Listen` on identified receivers; direct `syscall.Listen`;
`net/http/httptest.NewServer`,
`NewTLSServer`, and `NewUnstartedServer`; `os.Setenv`, `os.Unsetenv`,
`os.Clearenv`, and `os.Chdir`; and
`Setenv` or `Chdir` on function parameters typed exactly as `*testing.T` or
`testing.TB`. It also recognizes the receiverless
`skipSlowCmdGCTest(*testing.T, string)` definition and its same-package calls.
An unresolved cross-file call counts only when that directory and package own
the canonical helper. Import, parameter, and same-file helper matches use
lexical object identity; top-level sibling declarations are indexed by
directory and package so cross-file shadows do not masquerade as resources.
Local shadows and wrong signatures do not count. Parenthesized call
expressions retain the same ownership.

Targeted dot imports of `net`, `os/exec`, `time`, `os`, `syscall`, `testing`,
or `net/http/httptest` are rejected with file and import context because their
resources cannot be attributed safely; blank imports remain harmless.
Explicit constraints follow Go's leading-header
rules: a pre-package `//go:build` line is effective, while a legacy
`// +build` line must live in a leading `//` comment block separated from the
package clause by a blank line.
Misplaced and directive-like comments do not tag a file. An untagged scope
means the source file has neither an effective explicit constraint nor a
recognized `_GOOS`, `_GOARCH`, or `_GOOS_GOARCH` filename suffix. Implicit
filename constraints use the portion before the first dot, matching Go's
filename semantics. The code-owned platform set mirrors the Go standard
library's [`internal/syslist.KnownOS` and `KnownArch`](https://go.dev/src/internal/syslist/syslist.go):
the past, present, and future values Go owns for filename matching. Scanning
does not invoke the Go tool or network. The `cmd/gc+untagged` scope additionally
requires the source path to be beneath `cmd/gc/`.

Run the focused check with:

```bash
go test -count=1 ./internal/testpolicy/resourcecensus -run '^TestRepositoryLedgerMatchesCensusAndDocumentation$'
```

The historical regex totals remain visible as point-in-time audit evidence.
They can be higher because comments and strings matched, or lower where the old
needle covered only `t.Setenv` or direct `os.Chdir` and the AST census now
recognizes the full families above. Historical `cmd/gc` needles also included
build-tagged files; the live `cmd/gc+untagged` ratchets do not.
`internal/bdflags/freshness_test.go` is integration-tagged because it invokes
the externally installed `bd` CLI; its process call remains visible in the
all-source audit while staying outside untagged and Small debt.

<!-- BEGIN CHECKED TEST RESOURCE LEDGER -->
| Ledger kind | Source scope | Resource baseline | Tracking owner | Invariant / resource owner | Migration | Expiry |
| --- | --- | --- | --- | --- | --- | --- |
| Audit baseline | all tracked test source | fixed_sleep: 439 calls / 155 files (historical regex census: 447 / 157) | ga-80po0c.2 | tracked test source totals remain visible as audit evidence; ga-80po0c.2 owns this point-in-time source census | P0.4a | 2026-10-01 |
| Audit baseline | all tracked test source | subprocess: 494 calls / 139 files (historical regex census: 495 / 135) | ga-80po0c.2 | tracked test source totals remain visible as audit evidence; ga-80po0c.2 owns this point-in-time source census | P0.4a | 2026-10-01 |
| Medium owner | `cmd/gc` package `main` | TestMain: environment | ga-80po0c.2.1 | cmd/gc TestMain is the checked package-level Medium owner; only environment calls lexically inside TestMain leave Small debt | P0.4b | 2026-10-01 |
| Medium owner | `internal/api` package `api` | TestEveryEmittedErrorCodeIsRegistered: subprocess | ga-80po0c.2.1 | internal/api tracked-source error URN guard is a checked Medium owner; only the git ls-files call lexically inside TestEveryEmittedErrorCodeIsRegistered leaves Small debt | P0.4b | 2026-10-01 |
| Small debt ratchet | `cmd/gc` untagged test source | cwd: 208 calls / 40 files | ga-80po0c.2.1 | untagged Small cmd/gc cwd call/file totals cannot grow; reductions must lower this baseline; non-Medium lexical owners restore or eliminate every cwd mutation | D5/D6 | 2026-10-01 |
| Small debt ratchet | `cmd/gc` untagged test source | environment: 4145 calls / 189 files | ga-80po0c.2.1 | untagged Small cmd/gc environment call/file totals cannot grow; reductions must lower this baseline; non-Medium lexical owners restore or eliminate every process-environment mutation | D5/D6/E6 | 2026-10-01 |
| Small debt ratchet | `cmd/gc` untagged test source | slow_process_gate: 77 calls / 26 files | ga-80po0c.2.1 | untagged Small cmd/gc slow-process marker totals cannot grow; reductions must lower this baseline; each non-Medium marked caller retains an explicit process-suite migration owner | D5/D6/E6 | 2026-10-01 |
| Small debt ratchet | all untagged test source | fixed_sleep: 287 calls / 112 files | ga-80po0c.2.1 | untagged Small fixed-sleep call/file totals cannot grow; reductions must lower this baseline; non-Medium lexical owners replace elapsed wall time with lifecycle signals | W1-W5 | 2026-10-01 |
| Small debt ratchet | all untagged test source | http_test_server: 290 calls / 64 files | ga-80po0c.2.2 | untagged Small HTTP test server call/file totals cannot grow; reductions must lower this baseline; non-Medium lexical owners move server-backed tests to exact Medium ownership or replace the listener | P0.4c | 2026-10-01 |
| Small debt ratchet | all untagged test source | net_listen: 92 calls / 34 files | ga-80po0c.2.2 | untagged Small net.Listen call/file totals cannot grow; reductions must lower this baseline; non-Medium lexical owners move listener-backed tests to exact Medium ownership or replace the listener | P0.4c | 2026-10-01 |
| Small debt ratchet | all untagged test source | net_listen_config: 1 calls / 1 files | ga-80po0c.2.2 | untagged Small net.ListenConfig.Listen call/file totals cannot grow; reductions must lower this baseline; non-Medium lexical owners move ListenConfig-backed tests to exact Medium ownership or replace the listener | P0.4c | 2026-10-01 |
| Small debt ratchet | all untagged test source | net_listen_unixgram: 3 calls / 2 files | ga-80po0c.2.2 | untagged Small net.ListenUnixgram call/file totals cannot grow; reductions must lower this baseline; non-Medium lexical owners move Unix datagram listener-backed tests to exact Medium ownership or replace the listener | P0.4c | 2026-10-01 |
| Small debt ratchet | all untagged test source | subprocess: 374 calls / 97 files | ga-80po0c.2.1 | untagged Small subprocess call/file totals cannot grow; reductions must lower this baseline; non-Medium lexical owners remove or replace each process call site | D1/D2/D5/D6/E6 | 2026-10-01 |
| Small debt ratchet | all untagged test source | syscall_listen: 1 calls / 1 files | ga-80po0c.2.2 | untagged Small syscall.Listen call/file totals cannot grow; reductions must lower this baseline; non-Medium lexical owners move syscall-backed listener tests to exact Medium ownership or replace the listener | P0.4c | 2026-10-01 |
| Source debt ratchet | `cmd/gc` untagged test source | cwd: 208 calls / 40 files (historical regex census: 98 / 13) | ga-80po0c.2.3 | untagged cmd/gc cwd call/file totals cannot grow; reductions must lower this baseline; cmd/gc callers restore or eliminate every recognized cwd mutation | D5/D6 | 2026-10-01 |
| Source debt ratchet | `cmd/gc` untagged test source | environment: 4151 calls / 189 files (historical regex census: 3960 / 184) | ga-80po0c.2.3 | untagged cmd/gc environment call/file totals cannot grow; reductions must lower this baseline; cmd/gc callers restore or eliminate every recognized process-environment mutation | D5/D6/E6 | 2026-10-01 |
| Source debt ratchet | `cmd/gc` untagged test source | slow_process_gate: 77 calls / 26 files (historical regex census: 78 / 27) | ga-80po0c.2.3 | untagged cmd/gc slow-process marker totals cannot grow; reductions must lower this baseline; the helper definition and every marked caller retain an explicit process-suite migration owner | D5/D6/E6 | 2026-10-01 |
| Source debt ratchet | all untagged test source | fixed_sleep: 287 calls / 112 files (historical regex census: 295 / 114) | ga-80po0c.2 | untagged fixed-sleep call/file totals cannot grow; reductions must lower this baseline; each owning test replaces elapsed wall time with its lifecycle signal | W1-W5 | 2026-10-01 |
| Source debt ratchet | all untagged test source | http_test_server: 290 calls / 64 files (historical regex census: 255 / 56) | ga-80po0c.2.2 | untagged HTTP test server call/file totals cannot grow; reductions must lower this baseline; each owning test closes its loopback server and removes duplicate server-backed coverage | P0.4c | 2026-10-01 |
| Source debt ratchet | all untagged test source | net_listen: 92 calls / 34 files | ga-80po0c.2.2 | untagged net.Listen call/file totals cannot grow; reductions must lower this baseline; each owning test closes its listener and removes duplicate listener-backed coverage | P0.4c | 2026-10-01 |
| Source debt ratchet | all untagged test source | net_listen_config: 1 calls / 1 files | ga-80po0c.2.2 | untagged net.ListenConfig.Listen call/file totals cannot grow; reductions must lower this baseline; each owning test closes its configured listener and removes duplicate listener-backed coverage | P0.4c | 2026-10-01 |
| Source debt ratchet | all untagged test source | net_listen_unixgram: 3 calls / 2 files | ga-80po0c.2.2 | untagged net.ListenUnixgram call/file totals cannot grow; reductions must lower this baseline; each owning test closes its Unix datagram listener and removes duplicate listener-backed coverage | P0.4c | 2026-10-01 |
| Source debt ratchet | all untagged test source | subprocess: 375 calls / 98 files (historical regex census: 380 / 98) | ga-80po0c.2 | untagged subprocess call/file totals cannot grow; reductions must lower this baseline; each process-owning test removes or replaces its source call site | D1/D2/D5/D6/E6 | 2026-10-01 |
| Source debt ratchet | all untagged test source | syscall_listen: 1 calls / 1 files | ga-80po0c.2.2 | untagged syscall.Listen call/file totals cannot grow; reductions must lower this baseline; each owning test closes its listening file descriptor and removes duplicate listener-backed coverage | P0.4c | 2026-10-01 |
<!-- END CHECKED TEST RESOURCE LEDGER -->

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

#### Historical timing summaries

The opt-in timing artifacts produced by `scripts/go-test-observable` can be
aggregated offline across caller-curated successful `main` push runs:

```bash
go run ./scripts/test-timing-summary.go /path/to/downloaded-artifacts \
  >> "$GITHUB_STEP_SUMMARY"
```

The summarizer recursively reads schema-v1 JSON artifacts, deduplicates
identical downloads, and rejects conflicting artifacts with the same workflow,
run, attempt, job, shard, and variant identity. It emits the ten slowest
top-level tests by observed p95 and the ten highest-variance top-level tests
for each comparable `(job, variant, runner label, OS, architecture, CPU count)`
profile. Ephemeral runner names do not split profiles. Package terminal rows
are shard totals rather than independently scheduled work, and nested subtests
are diagnostic until the shard manifest explicitly promotes them, so neither
is ranked. Statistics use successful durations only while retaining failure
and skip counts. Percentiles use the empirical nearest-rank method, variance
is population variance in seconds squared, and samples are not trimmed.

Schema v1 does not record the event, ref, or workflow conclusion, so the tool
cannot prove protected-branch provenance. The caller must supply artifacts
from successful `main` push runs. An observed p95 with fewer than 20 successful
samples is diagnostic, not planner-authoritative, and the seven-day artifact
retention window is not a protected historical timing database. Renamed tests
remain separate histories.

In schema v1, `commit_sha` is the exact Git revision checked out and tested
(`GITHUB_SHA`). On `pull_request` runs, GitHub sets it to the synthetic merge
commit, not the contributor branch head. Consumers must not interpret it as
source/head identity. A future schema that needs both identities must add
distinct `tested_sha` and `source_sha` fields; schema v1 must not be
reinterpreted.

Tier A command acceptance and external-provider compatibility are separate
gates. `make test-acceptance` uses controlled subprocess and file providers; it
does not require inference or a `bd` executable. `make test-bd-cli-contract`
runs the four version-sensitive `bd` CLI contracts under the dedicated
`acceptance_bd_contract` build tag. CI applies that focused manifest to the
minimum-supported, current, and main-HEAD `bd` versions without repeating the
unrelated Tier A flows.

#### Resource isolation via gascity-test.slice

On hosts that provision a `gascity-test.slice` systemd user slice (resource
limits for test workloads), the test entrypoints — `scripts/go-test-observable`
(behind `make test` and `make test-cmd-gc-process`), `scripts/test-go-test-shard`,
`scripts/test-integration-shard`, and `scripts/test-local-parallel` — re-exec
themselves inside that slice via
`systemd-run --user --slice=gascity-test.slice --scope --collect --quiet --`.

Enrollment is automatic and strictly best-effort: it only happens when
`systemd-run` exists, the user manager responds, the slice unit is present
(`systemctl --user list-unit-files gascity-test.slice`), and a pre-flight
scope allocation succeeds. Everywhere else (CI runners, macOS, containers)
the entrypoints run unchanged. Nested runners detect existing slice
membership through `/proc/self/cgroup` and never double-wrap. Set
`GC_TEST_NO_SLICE=1` to opt out explicitly. The decision matrix is covered
by `scripts/test-slice-enroll-test` (run by `go test ./scripts`).

Only the wrapped entrypoints listed above are enrolled. Makefile targets
that invoke `go test` directly — `test-acceptance*`, `test-integration`,
`test-integration-huma`, `test-worker-*`, `test-cover`, and similar — run
unconfined even on slice-provisioned hosts.

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

#### Dashboard serve-level projection tests (`test/dashport`)

`test/dashport` is the Go serve-level (Layer A) e2e for the dashboard. It stands
up the real supervisor stack — the typed `/v0` API, the host-side `/api` plane,
and the embedded SPA — over a **seeded event log + bead store** via the exported
`api.ServeSeededCity` seam, then drives the exact endpoints each dashboard view
consumes and asserts the projected JSON. It is the layer that catches the
run-view class of regression: a projection break is visible at the Go wire level
here even when every request still returns 200.

The anchor test (`TestAnchorRunProjection`) seeds one run two ways from a single
`testdata/dashport/` corpus — as a store-resident graph.v2 molecule (the
`/workflow/{id}` read) **and** as a `bead.*` event stream in
`<cityPath>/.gc/events.jsonl` (the runproj-backed `/api/city/{c}/runs/summary`
and `/runs/{id}/detail` routes) — and asserts the run is present and non-empty on
both paths. Responses decode into the generated Go wire types
(`internal/api/genclient`) and the `internal/runproj` projection structs, never
`map[string]any`, so a wire-shape drift fails compilation.

Run it in isolation with `make dashboard-e2e-go`
(`go test -tags integration ./test/dashport/...`). It is a Tier 3 integration
package: the CI `packages` integration shard (`go list ./...` under
`scripts/test-integration-shard packages`, invoked by
`make test-integration-shards-parallel`) picks it up automatically alongside the
REST/formula shards — no dedicated shard registration is needed. The
structured-transcript view is not covered here; it lands with its serving path
(PR #3931) and is asserted then.

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

**The overtesting line:** don't re-verify contracts that an executable
constructor-bound conformance proof already covers. Coordination tests check
call ordering and argument plumbing, not that individual operations produce
correct results.

### Conformance testing

Provider interfaces may expose shared conformance suites in
`*test/conformance.go` packages. Suite availability does not prove that every
implementation or production constructor executes the suite: each consumer
must bind its exact constructor without a pre-run skip. The table names the
shared suites and their current named consumers; the runtime ledger below is
the constructor-specific source of truth.

| Interface | Conformance suite | Current named consumers |
|---|---|---|
| `beads.Store` | `internal/beads/beadstest/conformance.go` | MemStore, FileStore, BdStore |
| `runtime.Provider` | `internal/runtime/runtimetest/conformance.go` | See the checked runtime ledger below |
| `mail.Provider` | `internal/mail/mailtest/conformance.go` | beadmail, exec |
| `events.Recorder` | `internal/events/eventstest/conformance.go` | FileRecorder, exec |
| `fsys.FS` | `internal/fsys/fsystest/conformance.go` | OSFS, Fake |

The `fsys.FS` suite currently proves the portable namespace core: parent and
file/directory collisions, regular-file copying and modes, `ReadDir` errors,
file and directory-tree rename, empty/non-empty removal, and chmod. Symlink
resolution/replacement, atomic-write composition, and operation-scoped fault
and recording decorators remain follow-up contract slices; do not delete their
OS-backed coverage based on the namespace suite alone.

Builtin runtime production compositions are source-bound to `cmd/gc`'s
registry, their constructor-specific contract dispositions, and the table
below. The auto composition lives outside that registry and is bound to the
exact production function and `runtime/auto.New` result it returns. A waiver is
a visible contract gap, not evidence that conformance passes.

Reusable-double discovery is intentionally bounded, not repository-wide. The
designated boundary is `internal/runtime/fake.go` for the `runtime.Provider`
port. The guard type-checks its declared runtime type context, discovers every
exported concrete type in that file whose value or pointer implements
`runtime.Provider`, and scans the package's buildable non-test files for each
exported receiverless function whose first result itself implements
`runtime.Provider` and resolves to that exact type, either as the value or its
pointer. Constructors may return additional results such as `error`; function
bodies are outside the source guard. A value-returning constructor counts only
when the value method set implements the port. The current surface is
`runtime.Fake` through `runtime.NewFake` and `runtime.NewFailFake`. Aliases do
not create a second double type and collapse to their tracked concrete type; an
exported provider alias that exposes an otherwise-untracked type fails closed.
Caller-local types, methods, unexported helpers, and provider types declared in
other files are outside this boundary. An exported generic concrete type in the
boundary fails closed because an uninstantiated generic has no single provider
method set to inventory.

Other reusable-support boundaries remain explicit follow-up work:
`beadstest.RecordingStore`, the events and mail fakes, `fsys.Fake`, and
`clock.Fake` are not claimed by this table.

The hybrid row deliberately chooses `cmd/gc.newHybridProvider` as its
construction boundary because that is the wrapper returned directly by the
runtime registry. This ledger does not recursively claim the wrapper's internal
tmux, K8s, or hybrid constructors.

This first ledger slice records only owned, expiring waivers and explicit
not-applicable dispositions. `ga-80po0c.1.2` owns structural binding of the
existing Fake/subprocess conformance evidence to these exact production
constructors and the resulting proof-row upgrades. E1 (`ga-80po0c.6`)
separately owns the Large provider/E2E manifest and its required lane/cadence
execution; it does not own these constructor bindings.

<!-- BEGIN CHECKED RUNTIME PROVIDER LEDGER -->
This table is rendered from `internal/testutil/providerledger` and checked by `go test ./internal/testutil/providerledger`; edit the Go ledger, then use the expected block printed on drift.

| Provider path | Roles | Reusable type | Port | Constructor | Discovery | Contract | Status |
|---|---|---|---|---|---|---|---|
| `runtime.builtin.acp` | production_provider | — | `runtime.Provider` | `internal/runtime/acp.NewSeamBacked` | runtime.builtin/exact:acp | `runtime.Provider` | waived by ga-80po0c.3 through 2026-08-12: full conformance covers the raw ACP provider, not the NewSeamBacked production composition |
| `runtime.builtin.acp` | production_provider | — | `runtime.Provider` | `internal/runtime/acp.NewSeamBackedWithDir` | runtime.builtin/exact:acp | `runtime.Provider` | waived by ga-80po0c.3 through 2026-08-12: full conformance covers the raw ACP provider, not the NewSeamBackedWithDir production composition |
| `runtime.builtin.exec` | production_provider | — | `runtime.Provider` | `internal/runtime/exec.NewSeamBacked` | runtime.builtin/prefix:exec: | `runtime.Provider` | waived by ga-80po0c.3 through 2026-08-12: full conformance covers the raw exec provider, not the production seam-backed prefix composition |
| `runtime.builtin.exec` | production_provider | — | `runtime.Provider` | `internal/runtime/t3bridge.NewSeamBacked` | runtime.builtin/prefix:exec: | `runtime.Provider` | waived by ga-80po0c.3 through 2026-08-12: the legacy gc-session-t3 prefix branch selects the T3 bridge composition, which has no full shared runtime contract |
| `runtime.builtin.fail` | production_provider, reusable_double | `internal/runtime.Fake` | `runtime.Provider` | `internal/runtime.NewFailFake` | runtime.builtin/exact:fail; reusable: internal/runtime/fake.go | `runtime.Provider` | not applicable: intentional faulting double: a successful lifecycle cannot be exercised, so the successful-provider contract is not applicable |
| `runtime.builtin.fake` | production_provider, reusable_double | `internal/runtime.Fake` | `runtime.Provider` | `internal/runtime.NewFake` | runtime.builtin/exact:fake; reusable: internal/runtime/fake.go | `runtime.Provider` | waived by ga-80po0c.1.2 through 2026-08-12: existing full conformance is not yet structurally bound to runtime.NewFake; exact proof binding is deferred to ga-80po0c.1.2 |
| `runtime.builtin.herdr` | production_provider | — | `runtime.Provider` | `internal/runtime/herdr.New` | runtime.builtin/exact:herdr | `runtime.Provider` | waived by ga-80po0c.3 through 2026-08-12: the existing full conformance run skips in short mode or when the herdr executable is absent |
| `runtime.builtin.hybrid` | production_provider | — | `runtime.Provider` | `cmd/gc.newHybridProvider` | runtime.builtin/exact:hybrid | `runtime.Provider` | waived by ga-80po0c.3 through 2026-08-12: cmd/gc.newHybridProvider is the selected registry construction boundary; its internal tmux, K8s, and hybrid constructors are not claimed here, and the wrapper has no full shared runtime contract |
| `runtime.builtin.k8s` | production_provider | — | `runtime.Provider` | `internal/runtime/k8s.NewSeamBacked` | runtime.builtin/exact:k8s | `runtime.Provider` | waived by ga-80po0c.3 through 2026-08-12: the actual K8s production composition has no full shared runtime contract |
| `runtime.builtin.ssh` | production_provider | — | `runtime.Provider` | `internal/runtime/ssh.NewSeamBacked` | runtime.builtin/prefix:ssh: | `runtime.Provider` | waived by ga-80po0c.3 through 2026-08-12: the production SSH composition has no full shared runtime contract |
| `runtime.builtin.subprocess` | production_provider | — | `runtime.Provider` | `internal/runtime/subprocess.NewSeamBacked` | runtime.builtin/exact:subprocess | `runtime.Provider` | waived by ga-80po0c.1.2 through 2026-08-12: NewSeamBacked exact production-constructor proof binding is deferred to ga-80po0c.1.2 |
| `runtime.builtin.subprocess` | production_provider | — | `runtime.Provider` | `internal/runtime/subprocess.NewSeamBackedWithDir` | runtime.builtin/exact:subprocess | `runtime.Provider` | waived by ga-80po0c.1.2 through 2026-08-12: NewSeamBackedWithDir exact production-constructor proof binding is deferred to ga-80po0c.1.2 |
| `runtime.builtin.t3bridge` | production_provider | — | `runtime.Provider` | `internal/runtime/t3bridge.NewSeamBacked` | runtime.builtin/exact:t3bridge | `runtime.Provider` | waived by ga-80po0c.3 through 2026-08-12: the production T3 bridge composition has focused tests but no full shared runtime contract |
| `runtime.builtin.tmux` | production_provider | — | `runtime.Provider` | `internal/runtime/tmux.NewSeamBackedWithConfig` | runtime.builtin/exact:tmux | `runtime.Provider` | waived by ga-80po0c.3 through 2026-08-12: the existing full conformance run skips when the tmux executable is absent |
| `runtime.composition.auto` | production_provider | — | `runtime.Provider` | `internal/runtime/auto.New` | source: cmd/gc/providers.go#resolveSessionTransportProvider — conditional transport composition is outside the runtime registry | `runtime.Provider` | waived by ga-80po0c.3 through 2026-08-12: the production auto base/ACP composition has no full shared runtime contract |
<!-- END CHECKED RUNTIME PROVIDER LEDGER -->

Conformance tests verify the behavioral contract (create/read/update/delete,
error handling, concurrency). They deliberately don't test lifecycle ordering
or cross-provider coordination — that's what coordination tests are for.

For the new 0.15 config surface, use
`engdocs/design/packv2/doc-conformance-matrix.md` as the release-gating ledger for
what should block CI now, what should start blocking once warning plumbing
lands, and what remains tracked but non-gating.

### Provider seam inventory

All five provider seams, their lifecycle dependencies, and coordination
test coverage. This table is the checklist for new provider implementations.

| Seam | Implementations | Lifecycle deps | Coordination tested? |
|---|---|---|---|
| **Runtime** (`runtime.Provider`) | See checked runtime ledger above | None (stateless start/stop) | Via lifecycle start order test |
| **Beads** (`beads.Store`) | MemStore, FileStore, BdStore | ensure-ready → init → hooks | `TestLifecycleCoordination_*` |
| **Mail** (`mail.Provider`) | beadmail, exec | Depends on beads store | No — not a lifecycle seam; conformance sufficient |
| **Events** (`events.Recorder`) | FileRecorder, exec | None (append-only) | No — stateless append, conformance sufficient |
| **Dolt** (internal) | dolt.EnsureRunning, dolt.StopCity | ensure → init, stop after agents | Covered by beads lifecycle (exec spy) |

**Adding a new provider:** When adding a new implementation of any seam:
1. Run the conformance suite against it (mandatory)
2. If the provider has lifecycle dependencies (startup ordering, shutdown
   sequencing), add a coordination test using the `exec:<spy>` pattern
3. Update this table

## Test deadline rule

Any test timer that races a goroutine, exec, or socket start must be ≥ 10s.
Use `testutil.GoroutineRaceTimeout` or `testutil.ExecRaceTimeout` from
`internal/testutil/timeout.go`.

A sub-second constant for such a timer is a CI reliability defect: the
operation completes in < 1s on an idle machine but fails under CI CPU
saturation. The only exception is a timer that is itself the subject under
test (e.g., testing that a function honours a 100ms deadline).

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
