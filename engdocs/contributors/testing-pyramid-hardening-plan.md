# Testing Pyramid Audit and Hardening Plan

- **Status:** Proposed
- **Audit date:** 2026-07-13
- **Audit base:** `origin/main` at `31a8d0b7e` (after PRs #4193 and #4151)
- **Audit bead:** `ga-c4ky0l` (closed)
- **Implementation epic:** `ga-80po0c` (open)

## Executive verdict

Gas City has unusually broad test coverage, good hermetic-fixture instincts, and
several strong shared conformance suites. It does not have a weak-testing
problem. It has an inverted-cost and unclear-ownership problem.

The default checkout contains 1,548 Go test files and 735,331 lines of Go test
code, 1.72 times the 428,161 lines of production Go. The `cmd/gc` package alone
contains 318,018 test lines, 43.2% of all test Go; it produces a 293 MB test
binary, needs about 4.1 GB of memory to compile, and took 55 seconds merely to
compile in a warm local measurement. Much of that package tests domain
decisions through CLI globals, environment variables, subprocesses, mutable
mega-fakes, and polling. Splitting its test files or adding shards does not
remove that package tax.

The suite also contains confidence gaps that must be fixed before broad tests
are removed. The real `BdStore` conformance test is unconditionally skipped.
The NativeDoltStore adapter contract uses an in-process `beadslib.Storage`
fixture and is correctly separate from the real-Dolt boundary proof, but its
name should state that distinction. `fsys.Fake` has no OSFS contract and
differs from OS semantics, and the mail fake contradicts the documented
archive contract. Fast false confidence is not the objective.

The target is a resource-aware test architecture built around one rule:

> Give each invariant one primary proof at the lowest truthful layer. Duplicate
> it only to prove a distinct adapter or composition risk.

This is the Clean Architecture answer to test speed. Business rules move into
cohesive use cases with consumer-owned ports. Their tests become small,
deterministic, and parallel-safe. Production adapters and their doubles share
the same executable contracts. Coordination tests prove only wiring and order.
A small portfolio of journeys proves that the major boundaries compose.

Merged PR [#4193](https://github.com/gastownhall/gascity/pull/4193) established
the immediate latency baseline. Its exact-main Actions run
[29220498625](https://github.com/gastownhall/gascity/actions/runs/29220498625)
completed green in 4m59s workflow wall time, including queueing, and 4m15s from
runner-policy start to `CI / required`. It removed repeated broad suites, used
a hermetic provider executable, narrowed the external `bd` contract, shortened
CI fan-in, and retained the real managed-Dolt hard-kill/port-rebind proof in an
explicit path-owned integration manifest. Phase 0 preserves these changes; the
remaining phases make the result durable by changing the test and production
architecture beneath it.

## Goals

- Preserve or increase defect detection while reducing local and PR feedback
  latency.
- Make test placement predictable from the risk being proved.
- Make every reusable test double behaviorally honest through a shared
  contract.
- Replace fixed sleeps and state polling with events, channels, process exit,
  readiness signals, or virtual time.
- Reduce the compile and global-state cost of `cmd/gc` by extracting cohesive
  production use cases, not by moving test text alone.
- Keep a small, explicit set of end-to-end proofs for Gas City's major promises.
- Measure p50, p95, variance, and first-attempt reliability, not only one green
  run.
- Keep the plan compatible with upstream by preferring small internal packages,
  adapters, and provider-owned boundaries.

## Non-goals

- Chasing a coverage percentage without regard to behavior or risk.
- Replacing all fakes with mocks or asserting every collaborator call.
- Introducing a universal filesystem, clock, process, or service abstraction.
  A port is justified by a real consumer and at least two implementations.
- Deleting tests solely because they are large or slow.
- Moving all expensive tests off PRs before equivalent lower-level proofs and
  targeted triggers exist.
- Duplicating CI scheduling work owned by
  [the two-minute CI design](../design/two-minute-ci-blacksmith.md).
- Putting provider-specific T3 Code or DoltLite behavior into generic SDK paths.

## Audit method

The audit combined:

- a repository census of Go test files, functions, build tags, line counts,
  process usage, timers, environment mutation, and parallelism;
- direct inspection of test entrypoints, Make targets, CI workflows, shard
  manifests, and `TESTING.md`;
- inspection of provider interfaces, production implementations, doubles,
  conformance suites, and production constructor paths;
- review of acceptance, integration, REST, dashboard, worker, provider, and
  tutorial coverage for duplicate system promises;
- compile-only measurements of the two largest representative package shapes;
- live CI timings from the sub-five-minute PR run; and
- three independent read-only audits focused on quantitative shape,
  conformance/doubles, and CI/E2E policy.

Counts are point-in-time architectural indicators, not score targets. Generated
and fixture-heavy subdirectories can change directory totals depending on
whether a command counts recursively; package-level figures below use the
direct package where applicable. Reproduction commands are included near the
end of this document.

## Current-state census

### Overall shape

| Measure | Current value | Interpretation |
|---|---:|---|
| Go test files | 1,548 | Breadth is high; navigation and ownership matter. |
| Test-prefixed functions | 18,228 | 18,216 runnable tests plus 12 `TestMain` entrypoints; also 9 benchmarks and 1 fuzz target. |
| Test Go LOC | 735,331 | 63.2% of all Go LOC. |
| Production Go LOC | 428,161 | Test:production ratio is 1.72:1. |
| Untagged/default files, functions, LOC | 1,411 / 17,587 / 687,073 | 93.4% of test LOC enters the default package shape. |
| `t.Run` calls | 2,132 | Table/subtest use is modest relative to test count. |
| `t.Parallel` call sites | 780 in 65 files | Sparse usage is consistent with global-state constraints; nested calls mean this is not a percentage of top-level tests. |
| `t.Setenv` calls | 5,055 | 3,960 are under `cmd/gc`. |
| `t.TempDir` calls | 8,677 | Good isolation habit, but many tests still mutate process globals. |
| `time.Sleep` calls | 447 in 157 files | 295 calls across 114 files are in untagged tests. |
| `exec.Command*` calls | 495 in 135 files | 380 calls across 98 files are in untagged tests. |
| Explicit `t.Skip*` calls | 535 | Skips need ownership and expiration where they suppress contracts. |

Build-tagged test files currently break down as follows:

| Declared class | Files | Test-prefixed functions | LOC | Current role |
|---|---:|---:|---:|---|
| `integration` | 58 | 282 | 19,062 | Real processes/providers plus some seam conformance. |
| `acceptance_a` | 35 | 105 | 5,941 | PR smoke; external `bd` contracts now have an exact dedicated manifest. |
| `acceptance_b` | 3 | 10 | 996 | Nightly lifecycle/stability. |
| `acceptance_c` | 24 | 139 | 15,252 | Live inference and tutorial-golden coverage. |
| Other compound/native/OS tags | 17 | 105 | 7,007 | Platform and special compatibility checks. |

The tags describe invocation history more than resource consumption. Some
untagged tests spawn processes or listeners, while some `integration` tests
exercise hermetic protocol doubles. That makes the current pyramid impossible
to reason about from names alone.

### Concentration and compile tax

| Direct package/directory | Test files | Test-prefixed functions | Test LOC |
|---|---:|---:|---:|
| `cmd/gc` | 447 | 7,457 | 318,018 |
| `internal/api` | 141 | 1,401 | 52,625 |
| `internal/config` | 63 | 1,390 | 41,508 |
| `internal/beads` | 52 | 679 | 31,258 |
| `internal/session` | 49 | 560 | 21,349 |
| `internal/dispatch` | 13 | 315 | 20,054 |
| `examples/gastown` | 10 | 244 | 16,395 |
| `test/integration` | 44 | 162 | 15,350 |

Largest files include:

| File | LOC | Test-prefixed functions |
|---|---:|---:|
| `cmd/gc/beads_provider_lifecycle_test.go` | 11,882 | 211 |
| `cmd/gc/build_desired_state_test.go` | 11,821 | 220 |
| `examples/gastown/maintenance_scripts_test.go` | 10,950 | 154 |
| `cmd/gc/session_reconciler_test.go` | 10,881 | 215 |
| `internal/dispatch/runtime_test.go` | 9,932 | 144 |
| `cmd/gc/order_dispatch_test.go` | 9,579 | 196 |
| `cmd/gc/cmd_sling_test.go` | 8,792 | 237 |
| `internal/config/config_test.go` | 8,147 | 405 |

Pre-#4193 compile-only measurements make the structural cost visible. They were taken
with Go 1.26.5 on Linux/amd64, an AMD EPYC 9654 host exposing 192 logical CPUs,
the shared warm `/data/cache/go-build`, and no cache clean. The host had
concurrent fleet load, so these values are evidence of package shape, not a
reproducible performance SLO baseline:

| Package | Wall time for `go test -c` | Max RSS | Test binary |
|---|---:|---:|---:|
| `./cmd/gc` | 55.44s | 4.10 GB | 293 MB |
| `./internal/config` | 11.40s | 1.55 GB | 163 MB |

The Go compiler works at package granularity. Renaming or splitting a giant
`*_test.go` file improves reviewability but not this compile tax. Cohesive
production logic and its tests must leave the package.

### Process, global-state, and synchronization signals

- `skipSlowCmdGCTest` appears in 78 markers across 27 `cmd/gc` files: 77 gated
  call sites plus the helper definition. Eighty-eight other untagged
  process-using files contain neither that gate nor a `testing.Short` guard.
- `cmd/gc` contains only 113 `t.Parallel` call sites among 7,457 test-prefixed
  functions. Its `TestMain`, 3,960 `t.Setenv` calls, 98 direct `os.Chdir` calls,
  mutable package hooks, tmux roots, and provider factories make
  indiscriminate parallelization unsafe.
- Test code contains 337 `time.After` occurrences across 83 files and tmux
  references in 172 test files.
- Non-test Go outside `test/**` contains 653 `time.Now`, 93 `time.Sleep`, 54
  `time.After`, 49 `time.NewTicker`, and 44 `time.NewTimer` occurrences. The
  current `internal/clock.Clock` exposes only `Now`, so it cannot drive most
  lifecycle or scheduler tests.
- `scripts/go-test-observable` already emits per-test elapsed time from
  `go test -json`, but successful data is normally ephemeral. Current sharding
  uses static manifests or test-index modulo, not measured duration or
  variance.

Concrete authored wait floors include:

- `test/acceptance/helpers/lifecycle.go` sleeps two seconds after an already
  blocking `supervisor stop --wait`; nine call sites author 18 seconds of idle
  time.
- Tier C has three fixed 15-second sleeps, a 45-second floor before work does
  anything useful.
- `test/integration/gc_live_contract_test.go` contains 15 explicit 250/500 ms
  sleeps even though the API publishes typed request-result events over SSE.
- `events.Fake` uses a single-consumer notify channel and a 50 ms polling
  fallback because concurrent watchers can steal one another's wakeup.
- `FileRecorder.Watch` scans every 250 ms. `fsnotify` is already a dependency,
  and `internal/api/logwatcher.go` already has a file-notification plus bounded
  fallback pattern.
- Workspace proxy readiness polls every 100 ms and shutdown every 50 ms;
  tests duplicate deadline/sleep loops around the same process lifecycle.

### Merged PR #4193 latency evidence

The original optimized run completed in 271 seconds end to end. The final
exact-main run completed in 299 seconds including queueing and 255 seconds from
runner-policy start to `CI / required`. Its longest test job was Docker at 224
seconds; the retained managed-Dolt integration manifest completed in 181
seconds. The original run's lane measurements were:

| Lane | Duration |
|---|---:|
| Docker session | 219s |
| Slow `cmd/gc` shards | 183-200s |
| Tier A | 176s |
| Static checks | 170s |
| Integration smoke | 99-146s |
| Generated artifacts | 101s |
| Bdstore | 80s |
| Runtime tmux shards | 63-76s |
| Focused `bd` compatibility cells | 27-62s |

Recent contributor runs cluster near five minutes when healthy, but include
8-9 minute runs and pathological multi-hour failures. The operating target
therefore needs a p95 and variance budget, not merely one p50 success.

## What is already strong

- `beads.Store`, `runtime.Provider`, `mail.Provider`, and `events.Provider`
  have reusable conformance foundations.
- Beads conformance skips use a governed ledger rather than arbitrary local
  `t.Skip` calls.
- `t.TempDir`, isolated tmux sockets, test-specific city names, scrubbed
  environments, and local sharded runners show strong hermeticity intent.
- Testscript covers real CLI behavior and tutorial contracts.
- Typed OpenAPI, generated clients, event payload registration, and dashboard
  projection tests catch important cross-layer drift.
- Acceptance tiers separate unauthenticated smoke from live inference.
- Merged PR #4193 demonstrates how to eliminate obvious repeated work without
  dropping the external `bd` or macOS compatibility promises.
- Merged PR #4197 adds an injected reopen callback that re-resolves the managed
  Dolt endpoint, one wall-clock retry context, single-flight reconnect,
  terminal close semantics, close-versus-reconnect protection, focused
  transient/error/budget/concurrency tests, production wiring guards, and a
  tagged real hard-kill/port-rebind contract.

The plan builds on those assets; it does not replace them.

## Principal findings

### 1. `cmd/gc` is an architectural monolith disguised as a test bottleneck

The CLI package contains domain decisions, lifecycle coordination, environment
resolution, process control, and presentation. Its tests therefore need broad
fixtures and process-global setup. The sustainable fix is to extract one
cohesive use case at a time behind consumer-owned ports, leaving Cobra parsing,
wire formatting, and production construction in `cmd/gc`.

The desired outcome is not more interfaces around every function. It is a thin
command adapter calling ordinary Go services whose dependencies are explicit.
Those services compile and test independently. The command retains a few
coordination and user-contract tests and stops retesting the service's branch
matrix.

### 2. The documented pyramid does not describe resource cost

`TESTING.md` starts with “three tiers” and then documents unit, testscript,
integration, docs sync, coordination, conformance, acceptance tiers, and
dashboard E2E as overlapping categories. These are useful *purposes*, but they
are not sizes. A testscript can be hermetic and medium; a conformance suite can
be small or large; a test named “unit” can still spawn a process.

Gas City needs two orthogonal labels:

1. **Purpose:** unit, contract, adapter, coordination, or journey.
2. **Size:** small, medium, or large, determined by resources and isolation.

### 3. Some shared doubles are not yet trustworthy

The most important gaps are:

- Real `BdStore` conformance is unconditionally skipped by a stale June-era
  guard even though the default pin moved to `bd` v1.1.0 in
  [PR #4007](https://github.com/gastownhall/gascity/pull/4007). Only the
  minimum-supported compatibility cell remains on v1.0.4. The NativeDoltStore
  adapter contract uses an in-process `beadslib.Storage` fixture; that is a
  valid adapter proof, but its name must distinguish it from live Dolt.
- `fsys.Fake` has no shared OSFS/Fake contract and differs on parent existence,
  file/directory collisions, symlink replacement, missing-directory reads,
  directory rename/remove semantics, and symlink chmod.
- `mail.Provider.Archive` says the message disappears from all views, but
  `mail.Fake.Get` and `Thread` still return archived messages. Shared
  conformance checks only `Inbox` for this case.
- Events exec conformance omits the shared concurrency contract. Its fixture's
  file counter is non-atomic, so the omission is material.
- Runtime raw providers and seam adapters duplicate full subprocess/tmux
  conformance, while several production constructor compositions lack their
  applicable contract.
- The test called K8s session conformance exercises a generic exec script
  provider rather than `internal/runtime/k8s.Provider`.

The current entrypoint census is six store paths (Mem, File, NativeDoltStore
with in-process storage, exec-script, skipped real BdStore, and `br`
integration), nine runtime entries including raw/seam duplicates, five mail
entries including Fake and MCP, and three events entries. This is an inventory
of entrypoints, not proof that every production constructor is covered; the
checked ledger in P0.3 must name exact constructors, applicable contracts, and
skips.

Honesty fixes precede broad test deletion.

### 4. Broad fakes and broad interfaces amplify test cost

The exact census needles `newFakeState(` plus multiline `fakeState\s*{` occur
538 times across 57 test files around `internal/api`'s giant state fake.
`beads.Store` has roughly 47 one-off wrappers or embeddings for faults and
recording. `runtime.Fake` combines a state machine, spy, stub, fault sequencer,
gates, and support for impossible states. Direct `.Calls` selectors appear 429
times across 53 test files.

These tools make tests easy to start and hard to understand. A handler that
only reads sessions should depend on `SessionReader`, not all application
state. A use case that releases an assignment should depend on that capability,
not the entire store. A conformant state double should be separate from
recording, faulting, gating, and scripting decorators.

### 5. Wall time substitutes for missing lifecycle signals

Many sleeps do not test time. They wait for state that the system can already
observe: an event cursor, process exit, socket creation/removal, readiness
response, file change, bead transition, or request-result event. Tests should
block on that signal and use a context deadline only as a diagnostic safety
ceiling.

Real external systems sometimes expose no notification. At that edge only,
bounded polling is appropriate through one diagnostic waiter that records the
last observed state and attempts. Poll loops scattered through tests are not.

### 6. End-to-end breadth overlaps instead of forming a portfolio

PR integration routing can run seven REST smoke tests and then roughly 150
additional `rest-full` tests. Mail behavior appears in generic E2E, Gas Town,
event, and shell-agent families. Events and lifecycle have similar overlap.
`TestTutorial01` exposes 32 stable parallel txtar subtests through
`testscript.Run`, but Gas City's top-level-only sharder treats the parent as one
unit. Two dedicated parents also rerun subsets already included by it.

Each journey needs a named system promise, an owning subsystem, a unique
cross-boundary risk, a runtime budget, and a lane. If two journeys own the same
promise, consolidate them. Edge cases move down; provider matrices move to
targeted/nightly lanes.

### 7. Some guardrails preserve implementation coupling and duplicated policy

`scripts/check-routed-test-rows.sh`, wired into `make check`, requires every
migrated `cmd/gc/cmd_*_test.go` containing one routed-read marker to contain all
six markers: API success, cache-not-live, generic 500, 404, controller-down,
and escape hatch. Ten command test files currently carry that matrix, and
several repeat it for multiple list/show operations. The guard prevents
missing rows by institutionalizing duplicated route-policy tests in every
adapter. The six route decisions need one owner below the commands; each
command should prove only its endpoint translation, fallback wiring, and
unique user-facing result.

Architecture migration guards also use raw source substrings and line scans.
For example, the worker-boundary guard can miss aliased or renamed calls and
match comments, while the beads exec guard can miss indirect or multiline
construction. These are useful migration intentions implemented as lexical
heuristics. Architecture rules should inspect Go syntax and, where identity
matters, types; they must ignore comments/string fixtures, handle aliases and
multiline calls, and remain separate from behavioral correctness proofs.

## Guideline drift to correct

| Current statement or pattern | Evidence of drift | Required correction |
|---|---|---|
| “Three tiers” | The document defines more than three overlapping purposes. | Separate purpose from resource size. |
| Unit tests receive dependencies directly and do not use env vars. | 5,055 `t.Setenv` calls; 3,960 in `cmd/gc`. | Ban new env-controlled small tests; migrate through injected `Env`/config. |
| Unit tests use testify `require`/`assert`. | Only 6 of 1,548 test files import testify; the suite overwhelmingly uses the standard library. | Remove the fictional mandate or adopt it through a deliberate, separately justified convention change; do not churn tests mechanically. |
| Tests use the implementation package to access unexported symbols. | 130 external-package test files already provide useful black-box contracts; blanket same-package testing couples tests to private structure. | Use external packages for public/adapter contracts and same-package tests only when a private invariant is the actual subject. |
| Integration tests are not in CI by default. | PR, macOS, nightly, and RC workflows run extensive integration matrices. | Document exact lane placement and path triggers. |
| “The four test doubles” | The table lists three. Other exported fakes also exist. | Replace the inventory prose with a generated/checked conformance ledger. |
| Every fake has a compile-time interface assertion. | `fsys.Fake` has no visible assertion and no shared behavior contract. | Require both compile-time satisfaction and executable conformance. |
| Every provider implementation runs conformance. | Real BdStore is skipped; NativeDoltStore's adapter contract uses in-process `beadslib.Storage`; runtime coverage is duplicated and incomplete. | Name adapter and real-Dolt proofs separately; test production constructor paths; govern and expire skips. |
| Every command follows `cmdFoo`/`doFoo`. | The giant package and global factories show that dependency ownership remains ambient. | Prefer constructor-injected command gateways and extracted use cases. |
| Timer races need at least a 10s deadline. | Sensible ceilings coexist with fixed sleeps and blocking doubles that wait 3-10s. | Keep generous deadlines; forbid using them as authored wait time. |
| No mock libraries. | Mega-fakes and one-off embedded stores provide mock-like interaction coupling. | Judge doubles by contract and responsibility, not library origin. |
| Testscript tests user behavior. | Thirty-two stable subtests sit under one top-level parent that the current sharder cannot split; two parents rerun subsets. | Make existing subtests selectable/timed by the shard layer and give each one owner. |
| Every routed-read command repeats the six-row matrix. | `check-routed-test-rows.sh` requires all six markers in each of 10 command files; some files carry multiple matrices. | Extract a typed route-selection policy, test its six outcomes once, and leave adapter-specific mapping/composition proofs in each command. |
| Architecture boundaries are guarded with source substrings. | Lexical scans can match comments/fixtures and miss aliases, multiline calls, renamed receivers, or indirect construction. | Use scoped AST/type-aware analyzers with negative fixtures; source guards prove dependency shape, never runtime behavior. |
| Pack checks retry whole commands up to three times. | An assertion or deterministic product failure can be rerun instead of classified, obscuring first-attempt reliability. | Retry only explicitly classified network operations, retain the first failure as evidence, and never retry assertions or whole test commands to green. |

## Target test architecture

### Resource sizes

Size is a declaration about what a test consumes, not how important it is.

| Size | Allowed resources | Forbidden dependencies | Target placement | Budget |
|---|---|---|---|---:|
| **Small** | One Go process; in-memory doubles; `testing/synctest`; a tiny `t.TempDir` only when filesystem semantics are the subject | Wall-clock sleeps, external processes, listeners, tmux, Dolt, Docker/K8s, host env/cwd mutation | Default local and every PR | Top-level p95 <=100ms; focused package p95 <=5s |
| **Medium** | Hermetic repo-owned helper process, loopback listener, file watcher, rooted OSFS, generated HTTP server/client | External auth/network, shared host services, personal tmux, unbounded polling | Default or path-targeted PR shards | Test p95 <=5s; shard p95 <=60s |
| **Large** | Real `gc` binary, tmux, `bd`/Dolt, browser, Docker/K8s, provider CLI/inference, chaos | Shared user state and unisolated resources remain forbidden | Small PR portfolio plus targeted/nightly/RC | PR journey <=90s; PR portfolio lane <=270s |

Exceptions must name the boundary being tested. For example, OSFS conformance
uses a real temporary filesystem because OS behavior is the subject; ordinary
business-rule tests use a filesystem port or value inputs.

### Test purposes

| Purpose | Owns | Does not own |
|---|---|---|
| **Unit** | One domain rule or use case through its public behavior | Adapter wiring, real process behavior, broad collaborator call logs |
| **Contract** | Behavior shared by every implementation of a port | Caller sequencing or whole-system composition |
| **Adapter** | Translation to OS, wire, file, process, or external protocol | Repeating the domain branch matrix |
| **Coordination** | Argument plumbing, ordering, transaction/rollback boundary | Re-proving collaborator correctness |
| **Journey/E2E** | A named user/system promise across essential boundaries | Parser edge cases, injected failures, route enumeration |

Every test should have one size and one purpose. Classification has one
mechanical precedence rule:

1. The canonical identity is package plus top-level test name; subtests inherit
   the top-level test's size. If any subtest needs a larger resource, the whole
   top-level test has that larger size.
2. An exact checked entry declares Medium or Large. A package-level entry may
   declare inherited `TestMain` resources once; exact top-level overrides name
   additional resources or a larger size. `TestMain` setup raises every test
   in that package to at least the package default without requiring thousands
   of duplicate rows.
3. `integration` and acceptance build tags default to Large until an exact
   manifest entry truthfully classifies a hermetic Medium test.
4. Every other untagged, unlisted test is Small. Existing violations live in a
   baseline debt ledger and cannot grow; the exception does not redefine the
   test as Medium.

Purpose is reviewer-facing metadata and naming except for the checked contract
and E2E/provider ledgers. Existing Go package, build-tag, and CI-manifest
mechanisms carry the classification; do not build a new test framework merely
to attach labels.

### Ownership rule: one primary proof

| Risk | Owning proof |
|---|---|
| Pure policy, validation, state transition | Small unit test |
| Use-case branching and failure recovery | Small unit test with consumer-owned ports |
| All implementations obey one interface | Shared contract against production adapters and reusable doubles |
| Raw adapter maps types/arguments/errors correctly | Focused adapter test |
| Two components are called in the required order | Coordination test with recording decorator |
| Wire schema/status/event framing | Typed HTTP/SSE contract |
| Real provider binary starts and stops | One provider lifecycle proof |
| Major workflow composes across persistence, controller, and runtime | One journey |
| External version compatibility | Exact, versioned compatibility manifest |

When a journey finds a bug, first add the smallest regression that reproduces
the defect at its owning layer. Keep an E2E regression only if the defect
required the boundary composition to exist.

### Crisp small-test standard

A small test should be readable as a short behavioral specification:

- The name states one rule and condition, not an implementation method chain.
- Arrange the minimum valid state, perform one meaningful action, then assert
  the returned result, durable state, or emitted event.
- Pass dependencies through the constructor/function. Do not select behavior
  with env vars, cwd, package hooks, or ambient files.
- Use a table only when rows exercise the same rule with the same assertion
  shape. Give different behaviors and failure policies separate tests.
- Assert typed errors with `errors.Is`/`errors.As`. Assert text only when the
  text is the user-facing contract.
- Prefer state/output assertions. Inspect collaborator calls only when the
  protocol or absence/order of a side effect is the rule.
- Use `t.Cleanup` for owned resources. Never rely on another test's setup or
  execution order.
- Use channels, events, or `testing/synctest` for concurrency. No retry loop or
  sleep should be needed to make an in-process assertion pass.
- Include the happy path, each distinct boundary, and each failure policy; do
  not enumerate combinations that the same lower-level contract already
  proves.
- Keep helpers domain-specific and make them call `t.Helper()`. A helper should
  improve the test's language, not hide a second system under test.

Characterization tests may temporarily reach unexported structure during an
extraction. The final tests should bind to the stable use-case behavior so a
refactor does not require rewriting assertions that describe no changed
promise.

## Test-double architecture

### Required shape

A reusable port should have these layers only as needed:

1. **Conformant implementation** — the smallest useful stateful
   implementation. It obeys the same observable contract as production.
2. **Recording decorator** — records an actual collaborator protocol when
   ordering or arguments are the behavior under test.
3. **Faulting decorator** — injects typed, method-scoped failures while
   delegating all other behavior to a conformant implementation.
4. **Gated decorator** — exposes channels that let a test control entry,
   release, cancellation, and completion without sleeping.
5. **Scripted adapter** — models an explicit protocol transition sequence when
   a state fake would hide the behavior being proved.

Do not combine all five into one mutable object. Do not expose public slices or
maps that let a test construct states the real implementation cannot reach.
Prefer a real lightweight implementation such as `beads.MemStore` where it is
already fast and truthful.

Every reusable double must:

- have a compile-time interface assertion;
- run every applicable shared contract;
- accept deterministic ID and time sources when those values are observable;
- be race-safe if the production contract is concurrent;
- fail loudly on unsupported behavior;
- live with the port or in a dedicated `*test` support package, not in an
  unrelated caller; and
- have tests for its decorators, not only tests that happen to use them.

### Conformance and seam matrix

| Seam | Production path(s) | Current double/support | Shared proof today | Gap and target decision |
|---|---|---|---|---|
| `beads.Store` | MemStore, FileStore, BdStore, NativeDoltStore, CachingStore, DoltLite read, exec, library store | MemStore plus many embedded one-off wrappers | `beadstest` with governed skip ledger | Split consumer capabilities; run applicable contracts. Restore one real BdStore smoke. Name the in-process `beadslib.Storage`-backed NativeDoltStore adapter conformance separately from live Dolt. Replace wrappers with recording/faulting/gated decorators. |
| `runtime.Provider` | fake, subprocess, tmux, exec, ACP, herdr, K8s, SSH, T3 bridge, auto, hybrid | Broad mutable `runtime.Fake` | `runtimetest` | Run full applicable conformance once on each production constructor composition. Replace duplicate raw/seam suites with narrow forwarding proofs. Add an expiring skip ledger. |
| `mail.Provider` | beadmail, exec, MCP | `mail.Fake` | `mailtest` | Expand archive/delete visibility across every read/reply/thread operation; repair Fake; inject ID/time. Retain backend contracts. |
| `events.Provider` | FileRecorder, exec | `events.Fake` | provider, rotation, and concurrency contracts | Make Fake broadcast to all watchers without a timer. Run concurrency against exec or serialize in the adapter and specify it. Reuse file notifications for FileRecorder. |
| Event aggregation | Multiplexer over registered providers | provider/test watchers | focused multiplexer tests | Define an aggregation contract for attach failure, cursor isolation, fan-in, close, and slow sources; do not claim Multiplexer implements `events.Provider`. |
| `fsys.FS` and extensions | OSFS | mutable map-backed `fsys.Fake` | None | Add one OSFS/Fake contract for errors, parents, collision, links, rename, remove, modes, and atomic replacement. Repair Fake before further reliance. Keep recording/faults as decorators. |
| Worker capabilities | SessionHandle, RuntimeHandle | scripted worker process; no general Handle double | telemetry and phase-specific tests | Define narrow lifecycle, messaging, observation, and history contracts. Do not create one enormous Handle contract. Preserve explicit unsupported capabilities. |
| Session use cases | Manager over broad store/runtime seams | conformant `sessiontest` builder plus runtime Fake | state-specific suites and permanent-zero migration guard | Retain #4158's completed fixture cutover; narrow ports by consuming use case without reopening codec-fixture migration. |
| API handler state | controller state and generated client | 538 exact constructor/literal syntax occurrences for broad `fakeState` | schema/OpenAPI checks | Introduce handler-owned gateways by vertical slice. Use tiny value fakes. Retain one typed live server/client contract. |
| Workspace process lifecycle | OS process groups, proxy, readiness | local test runtime/instance | None | Extract `ProcessSupervisor`, `Process.Done`, and `ReadinessProbe`; conformance-test a scripted process and retain one real-process proof. |
| Maintenance scheduling | timer loop, SQL Dolt ops, backup exec | local runners | partial | Use `testing/synctest` or a narrow Scheduler for cycle behavior; add adapter contracts for the real/scripted operations. |
| E2E worker actor | user-supplied runtime process behavior | about 19 behavior-specific shell actors | no one generic actor contract | Add one role-neutral configurable executable with deterministic ready, claim, complete, fail, block, and exit steps; J2/J3 compose it instead of adding more scripts. |

### Interaction assertions

Interaction tests remain appropriate for:

- a required transaction or rollback order;
- exact process arguments, environment projection, or protocol frames;
- atomic file-write order; and
- proving that a destructive side effect did not occur.

They are not the default way to test business behavior. A use case should
normally be asserted through its return value, durable state, or emitted event.
Once an outcome contract owns a behavior, delete interaction-only tests that
merely restate the implementation sequence.

## Event-driven synchronization policy

Use the coordination primitive that matches the boundary:

| Boundary | Primary wake mechanism | Safety/verification |
|---|---|---|
| In-process state transition | Closed/replaced generation channel or typed event | Re-read state after wake; context deadline for diagnostics |
| Concurrent timer logic | `testing/synctest` | Assert virtual-time outcome; no real sleep |
| Request lifecycle | Request-result SSE/event cursor | Typed durable GET confirms final state |
| Worker/controller cycle | Structured operation/cycle event | Read bead/session state after event |
| Child process | `Process.Done() <-chan Exit` | Inspect exit status and final resources |
| Service readiness | `ReadinessProbe.Wait(ctx, endpoint)` | Include last probe error in timeout |
| File append/rotation | `fsnotify` or reusable file-change notifier | Bounded low-frequency state reread for missed events |
| External provider without notifications | Shared bounded diagnostic waiter | Deadline, attempt count, last state/error; no fixed settle sleep |

For in-memory fan-out, use a generation channel: recording a change closes the
current channel under lock and replaces it. Every waiter holding the old
channel wakes. This removes `events.Fake`'s competing-consumer bug and 50 ms
fallback.

Do not grow `clock.Clock` into a universal abstraction. Use context for request
deadlines, `testing/synctest` for concurrent timer code, a RetryPolicy for
adapter retries, and a Scheduler only where scheduled domain actions are a
real boundary.

The test deadline rule remains: safety ceilings for goroutines, exec, and
sockets must be generous under CI saturation. A ten-second deadline is a
ceiling, not permission to sleep for ten seconds. A correct deterministic test
usually completes immediately.

## End-to-end portfolio

### Four PR-blocking system promises

The required PR path should own four whole-system journeys. They run in
parallel-capable isolated jobs and use a real `gc` binary plus repo-owned
hermetic providers unless the external provider is the boundary under test.

| ID | System promise | Unique boundary risk | Budget |
|---|---|---|---:|
| J1 | Pack bootstrap -> city initialization/start -> configured session ready -> clean stop | Pack/config materialization, controller startup, runtime construction, durable lifecycle, orphan cleanup | 60s |
| J2 | Rig-scoped source bead in the rig store -> formula v2 materialization -> city-scoped orchestrator/control-dispatcher readiness -> scoped `gc bd` reads and writes -> cross-store fan-out/dependency gates -> completion and convoy drain | Different store tiers and ID prefixes; rig-store roots/dependencies; city-store worker/control state; store-aware readiness/dispatch | 90s |
| J3 | Worker exits during an attempt -> persistent retry/recovery -> one durable terminal state with no duplicate active assignment or finalization | Session loss, persistent state, retry ownership, idempotent convergence; this does not promise exactly-once execution or external effects | 90s |
| J4 | Typed HTTP `202` mutation -> correlated SSE result -> durable typed API read | Huma routing, async request correlation, event stream, storage, and generated Go wire types | 90s |

Each journey must declare its fixture, resource ownership, last-progress
diagnostic, and cleanup assertions. No journey may enumerate low-level error
branches already owned below.

J2 must fail if dispatch readiness looks only in the city store or a worker
resolves the rig root through ambient/default store state. It asserts durable
terminal state in both stores and exercises a rig-scoped source/root with
city-scoped orchestrator, worker, and control beads. The direct static owner
for shipped prompts/scripts using `gc bd`, never ambient raw `bd`, is
`internal/bootstrap/packs/core/pack_assets_test.go` plus pack-specific asset
contracts; J2 proves only that the scoped command composes correctly.

### Provider and compatibility proofs

These are not additional generic E2Es. They are boundary-specific contracts
and run when their boundary changes, plus on nightly/RC:

| Boundary proof | PR placement | Broader placement |
|---|---|---|
| Real BdStore/DoltLite read-write-dependency and restart smoke | Path-targeted, exact manifest | Nightly/RC full contract and recovery |
| Managed Dolt hard-kill/port-rebind through production NativeDoltStore reopen wiring | Managed-Dolt path-targeted exact manifest | Nightly/RC broader recovery matrix |
| Four external `bd` CLI compatibility tests against previous/current/HEAD | Path-targeted focused cells | RC compatibility matrix |
| One real tmux start/nudge/stop/orphan-cleanup proof | Runtime-path targeted | Nightly platform matrix |
| One subprocess/exec protocol lifecycle proof | Runtime-path targeted | Nightly provider canaries |
| Gas City -> T3Bridge -> T3 Code using DoltLite-backed scoped bead/session state | Path-targeted protocol/identity/state contract against pinned T3 fixture | Real visible-thread/start/resume/stop composition on nightly/RC |
| Dashboard seeded projection and one browser interaction smoke | Dashboard-path targeted | Broader browser suite on push/RC |
| Live provider auth/inference | Never generic PR | Nightly/explicit profile matrix |
| Docker/K8s lifecycle | Path-targeted smoke if changed | Nightly/RC platform suite |
| Remaining chaos, rotation, and exhaustive recovery matrices | No | Nightly/RC |

The hermetic T3Bridge/DoltLite proof requires exact start/resume/stop protocol,
stable session identity, durable scoped state, clean stop, and no leaked
runtime resources. It runs when any owned boundary changes. Only the live
cross-repository nightly/RC proof claims visible thread and UI/runtime
composition.

### Retain, move, consolidate, delete

| Decision | Coverage |
|---|---|
| Retain on every PR | All small unit/use-case tests; hermetic contracts; OpenAPI/generated/event registries; the four journeys; relevant focused compatibility cells |
| Retain path-targeted | Real OS/process/provider contracts, managed-Dolt hard-kill/rebind, dashboard/browser smoke, Docker/K8s, real tmux, real BdStore |
| Move to push/nightly/RC | Broad REST read sweeps, full provider matrices, broad chaos/recovery permutations, live inference, exhaustive tutorial permutations, full formula retry matrices |
| Consolidate | Mail E2E families, event E2E families, lifecycle permutations, repeated `httptest.Server` setup, raw/seam runtime contracts |
| Delete after replacement proof | One-off embedded MemStore fakes, interaction-only outcome duplicates, polling tests replaced by event/channel tests, catalog-only “conformance” tests |

Deletion requires an invariant-to-owner map in the same change. Runtime alone
is never a deletion justification.

## Performance and reliability budgets

### Developer loop

| Signal | Target |
|---|---:|
| One focused small test's reported execution | p95 <=100ms |
| Extracted small package edit-to-result | p95 <=5s |
| `cmd/gc` incremental edit-to-result at program completion | p95 <=20s |
| Entire small-test local loop | p50 <=30s; p95 <=60s |
| One medium shard | p95 <=60s |
| Failed-test diagnostic availability | <=10s after shard exits |

Edit-to-result includes package compile, link, and test execution. Canonical
samples use a named runner image/class recorded with the result, a warm Go
object/module cache, test-result caching disabled with `-count=1`, and no source
changes except the measured package. The focused form is
`go test -count=1 -run '^TestName$' ./path`; the whole Small loop is
the P0.4 manifest-filtered mode (exposed as `make test-small-parallel` or an
equivalent checked target). The existing `make test-fast-parallel` is not a
Small-only target because package `TestMain` resources and intentional Medium
tests still participate. Cold builds use an isolated temporary `GOCACHE` and
are reported separately. Local hardware is useful for trends; the named
Blacksmith runner cohort is the enforcement baseline.

### PR and release

| Signal | Target |
|---|---:|
| `CI / required`, rolling full-union window | p95 <=4m30s; every non-platform-outage run <5m |
| Static/schema lane | p95 <=2m |
| Deterministic unit/contracts lane | p95 <=2m |
| Four-journey lane | p95 <=3m30s; hard <=4m30s |
| Changed-package race gate | p95 <=2m30s; each planner shard p95 <=90s |
| Known deterministic product-test flakes | 0 |
| Required-suite first-attempt reliability | >=99.5% over at least 200 classified runs |
| Automatic retries for deterministic tests | 0 |
| Unledgered required-contract skips | 0 |
| Release | exact SHA has a successful RC gate before publication |

Infrastructure retries, if unavoidable, must be reported separately and may
not turn a product-test failure green. Quarantine requires an owner, linked
bead, expiration date, and a still-failing nonblocking lane.

A **full-union** run forces every conditionally required PR lane selected by
the union of path filters. CI elapsed time starts when the first required job
enters `in_progress` and ends when `CI / required` completes; GitHub queue time
is tracked separately, while checkout and job setup remain included. Twenty
consecutive full-union runs are the migration/branch-protection overlap gate,
not proof of 99.5% reliability. The operating window is the latest 200
classified full-union runs (and a trailing 30-day view). A failure that passes
on the same SHA without a product change is a test/infra failure; classifying
it as platform infrastructure requires attached runner/service evidence. Real
product regressions remain valid test detections and are reported separately
from first-attempt test-system reliability.

### Architectural ratchets

- No new `time.Sleep` in small tests. Existing count decreases each phase.
- No new process, listener, tmux, Dolt, environment-controlled, or cwd-mutating
  test can be classified small.
- No new reusable fake without compile-time satisfaction and applicable
  conformance.
- No new bare conformance `t.Skip`; use an owned, expiring ledger.
- No new test file above 2,000 lines. Existing oversized files must not grow
  and decline as production seams are extracted.
- No package test binary may regress in compile time, RSS, or bytes without an
  approved decomposition bead. Target `cmd/gc` below 150 MB and 1.5 GB RSS,
  with no individual extracted package above those limits.
- Parallelism is enabled only after global env, cwd, ports, and mutable hooks
  are made instance-owned. `t.Parallel` count is not itself a quality metric.
- Every large test is present in the checked E2E/provider manifest with owner,
  promise, resources, budget, lane, and last measured p50/p95.
- Reclassification cannot cure debt. Freeze the initial ledgers at 78
  `skipSlowCmdGCTest` markers, 98 direct `os.Chdir` calls in `cmd/gc` tests, 538
  broad API `fakeState` constructions, 98 untagged process-using files, 3,960
  `cmd/gc` `t.Setenv` calls, and 447 repository test sleeps. Changed code may
  not grow its applicable category. Every extraction maps the moved invariants
  and reports category counts plus package runtime before and after. Approve
  burn-down milestones by owned invariant and measured impact; do not game
  ungrounded global terminal counts.

## CI placement and protection

Merged PR #4193's immediate reductions are the baseline, not optional cleanup:

- Tier A uses a hermetic idle provider executable and does not require host
  inference/auth.
- The four external `bd` CLI contracts run in exact versioned manifests rather
  than rerunning all Tier A.
- Full REST runs on push rather than duplicating PR smoke.
- Tmux full conformance runs once through the production constructor.
- `CI / preflight` and compatibility fan-in occur concurrently.

As of the audit, branch protection requires historical `Check` and four CodeQL
contexts, but not the more complete `CI / required` aggregate. After the
two-minute design's full protected-check migration window passes—including 20
consecutive full-union overlap runs—protect `CI / required`; retain `Check`
only for that design's time-bounded compatibility interval.

The CI graph should execute independent static, small, contract, medium, and
journey lanes concurrently. The longest required lane determines latency.
Timing collection, longest-first bin packing, warm images, and path-planning
semantics remain owned by the
[two-minute CI design](../design/two-minute-ci-blacksmith.md). This plan supplies
the truthful runnable units that planner needs.

## Phased implementation plan

The dependency order is:

`truthful contracts -> narrow boundaries/doubles -> event-driven tests -> E2E consolidation -> enforcement`

Timing instrumentation and the demonstrated PR-latency work can proceed in
parallel. Every task follows TDD, lands as a reviewable vertical slice, and
keeps the installed `gc` dogfoodable. “Likely files” is a scope boundary, not
permission for a broad mechanical rewrite; if a task needs more than five
production/test files, split it by capability or caller.

Program-sized tasks use the same wave template: freeze an exact census, choose
one <=5-file/caller slice, add the lower-level proof first, migrate and remove
the old owner, record before/after compile/runtime/debt counts, update the
ledger, then repeat. Their first schedulable slices and terminal censuses are:

| Program | First bead-sized slice | Terminal census |
|---|---|---|
| H5 runtime contracts | Retain PR #4193's production-path tmux/subprocess deduplication, then handle one remaining constructor family per bead | Every production constructor has applicable contract/ledger rows; raw/seam duplicate full runs = 0 |
| D5 desired state | Extract the pure fair-share/create-budget policy cluster and its tests before store/runtime effects | All desired-state policy branches live outside `cmd/gc`; command retains translation/coordination only |
| D6 provider lifecycle | Extract ensure-ready -> init -> hook ordering and rollback through a narrow lifecycle port | Lifecycle branch matrix lives in the service; `cmd/gc` owns construction/presentation only |
| E3 integration split | Move the typed API live-contract family into a resource-specific package with its own minimal setup | No test pays a `TestMain` resource it does not use; old 162-test package is dissolved or journey-only |
| E6 process markers | Migrate the readiness/timeout marker cluster first using context-blocking doubles and probes | `skipSlowCmdGCTest` call sites = 0 and the 12-way all-tests process lane is deleted |

### Phase 0: Preserve the sub-five-minute baseline and make it observable

#### P0.1 — Retain the focused PR topology

**Change:** Retain merged PR #4193's exact external `bd` manifest, hermetic
provider double, production-path tmux conformance, push-only full REST
coverage, path-owned managed-Dolt rebind proof, and parallel fan-in as the
starting topology.

**Acceptance:**

- The required PR workflow stays below five minutes on a full-union change.
- Previous/current/HEAD `bd` compatibility and macOS path-filter coverage stay
  present.
- No broad Tier A or full REST suite is reintroduced as a duplicate PR row.

**Verification:** Actions timings and the workflow policy tests introduced by
that PR.

**Dependencies:** None; PR #4193 is merged. **Estimate:** implemented; monitor
the rolling full-union runtime and prevent coverage/topology regression.

#### P0.2 — Consume the two-minute design's timing milestone

**Change:** Execute the timing-storage, planner, and summary tasks in the
[two-minute CI design](../design/two-minute-ci-blacksmith.md#timing-database).
This testing program consumes their normalized per-test/package p50, p75, p95,
failure, retry, and variance records; it does not create a second timing store
or shard planner.

**Acceptance:**

- A PR summary identifies the ten slowest and highest-variance runnable units.
- Shard assignment consumes historical duration rather than source-order
  modulo once enough samples exist.
- Missing timing data degrades to conservative static routing, never skipped
  tests.

**Verification:** the linked design's script/policy tests plus a dry-run
planner fixture with cold, warm, missing, and outlier samples.

**Owner/status:** `ga-80po0c`; blocked. The linked design names `ga-nakct`,
which is not present in the current Gas City store. Create and assign a live
timing/planner child bead under the implementation epic and record it in both
documents before scheduling this task.

**Dependencies:** P0.1 and an assigned timing/planner child bead.
**Estimate:** owned by the linked design after ownership repair.

#### P0.3 — Establish a checked architecture ledger

**Change:** Add a machine-readable or Go-table ledger for reusable providers
and reusable doubles only: production constructor, applicable contract, and
approved skips with owner/expiry. E1 is the sole owner of the large-test and
journey manifest.

**Acceptance:**

- Adding a provider in the explicit production catalogs or a reusable exported
  double in the designated `*test` support packages without a ledger row fails
  a focused guard test. Caller-local test types are excluded.
- A required contract cannot be silently skipped.
- The ledger generates or is checked against the provider/test tables in
  `TESTING.md`; prose is no longer the only inventory.

**Likely slice:** one ledger under `internal/testutil` or `test/`, its guard
test, `TESTING.md`, and AST/catalog discovery over the explicit production
catalogs and designated reusable-test packages. Bootstrap known legacy gaps as
owned expiring rows so inventory work does not block H1-H4.

**Verification:** guard-test fixtures prove missing, expired, and inapplicable
contract cases.

**Dependencies:** None. **Estimate:** medium.

#### P0.4 — Establish the size/resource debt ledger

**Change:** Add a checked Medium-test manifest and a baseline ledger for
untagged tests that violate the Small contract through subprocesses, listeners,
tmux, Dolt, fixed sleep, env/cwd mutation, or shared host resources. Record the
owning invariant, intended size, resource owner, migration target, and expiry.
E1 remains the sole owner of Large journey/provider entries.

**Acceptance:** Every Medium test inherits a checked package default or has an
exact top-level owner and resource list. `cmd/gc` records its `TestMain` cost in
one package row, not 7,456 copies; exact overrides name additional resources.
Every known untagged Small violation is ledgered; new or growing debt fails a
focused check; reclassification requires evidence rather than a label-only
edit. The initial counts match this audit's baselines.

**Likely slice:** One machine-readable ledger, resource-census checker,
positive/negative fixtures, and `TESTING.md` generated/checked tables.

**Verification:** Exact-manifest dry run plus fixtures for an unlisted process,
env mutation, fixed sleep, expired entry, and valid Medium owner.

**Dependencies:** The size/purpose taxonomy in this plan. **Estimate:** medium.

#### P0.5 — Establish an automated race-detector cadence

**Change:** Add independent path-targeted, planner-sharded PR work that runs
`go test -race` for changed concurrency-owning packages and their shared
contracts. Cap every race shard at p95 <=90s and the aggregate race gate at p95
<=2m30s so required fan-in retains margin. Add a broad sharded scheduled sweep
across race-capable Go packages. PR race work runs in parallel with other
required work and does not serialize the sub-five-minute graph.

**Acceptance:** Events, runtime, session, worker, dispatch, workspacesvc, and
new gated/broadcast doubles enter the required changed-package race lane.
`cmd/gc` ledgers exact race-capable controller, reconciler, and provider-
lifecycle test families rather than forcing its entire 4.1 GB package shape
through every race PR; D5/D6/E6 migrate those families into extracted packages
and the normal path-targeted lane. Nightly reports the broad package census;
unsupported process/provider cases are explicit; race failures never
auto-retry to green. A full-union selection stays within the shard/gate
budgets.

**Likely slice:** existing Go shard runner flag support, checked package/path
manifest, PR/nightly workflow rows, policy tests.

**Verification:** a deliberate fixture race fails both targeted and scheduled
policy tests; planner fixtures select and balance the full union of listed
packages; measured full-union race shards/gate meet the 90s/2m30s budgets on
the reference runner.

**Dependencies:** P0.2 timing output. **Estimate:** medium.

#### E1 — Make the E2E/provider manifest executable

**Change:** Encode J1-J4 and provider proofs with owner, system promise,
resources, budget, lane, path triggers, diagnostics, and exact top-level tests.
Have policy tests reject unlisted large tests, duplicate ownership, empty
manifests, and stale test names.

**Acceptance:** Every large test maps to one promise and exactly one cadence
owner; each generic PR promise has exactly one PR-blocking owner, while
nightly-only promises may have zero PR rows. Adding an E2E requires stating why
lower layers cannot prove it and what it replaces or complements.

**Likely slice:** Manifest, policy test, shard resolver, `TESTING.md`, and CI
suite-coverage policy.

**Verification:** Positive and negative policy fixtures; dry-run lists exactly
the intended tests.

**Dependencies:** P0.3 and P0.4. **Estimate:** medium.

### Phase 1: Restore contract honesty

No duplicate E2E or provider suite is deleted until the relevant Phase 1 task
passes.

#### H1 — Conformance-test OSFS and `fsys.Fake`

**Change:** Write the shared behavior contract first against rooted OSFS and
Fake. Cover missing parents, file/directory collisions, symlink creation and
replacement, missing `ReadDir`, file and directory rename, non-empty remove,
chmod, modes, errors, and atomic-write semantics actually promised by the
interface. Define the portable semantic core before using one host as oracle;
ledger OS-specific rename/remove/chmod/symlink cases separately and execute
OSFS on Linux and Darwin. Repair Fake to pass the portable contract; move
recording and path-error injection into decorators where practical.

**Acceptance:** OSFS and Fake pass the same applicable suite; Fake has
compile-time assertions; no caller depends on a state OSFS cannot produce.

**Likely files:** `internal/fsys/fsystest/conformance.go`, `fsys.go`, `fake.go`,
`fake_test.go`, OSFS conformance entrypoint.

**Verification:** `go test -count=1 ./internal/fsys/...` on Linux and Darwin,
plus race coverage for any concurrent promise and explicit platform cases.

**Dependencies:** P0.3 can land concurrently. **Estimate:** medium.

#### H2 — Make mail archive semantics executable

**Change:** Extend `mailtest` so Archive/Delete disappear consistently from
`Inbox`, `Check`, `Get`, `Read`, `All`, `Thread`, and counts. Specify that Reply,
Get, and Read on an archived/deleted original return `ErrNotFound`.
`Thread(archivedMessageID)` follows the existing unknown ID/thread-ID behavior
and must not return the archived message; lookup by a surviving stable thread
ID returns only remaining open messages. This avoids inventing tombstone
persistence solely for tests. Write the failing Fake proof first, repair Fake,
and inject deterministic ID/time suppliers. Run the contract against beadmail,
exec, MCP, and Fake as applicable.

**Acceptance:** All implementations agree or the interface explicitly narrows
the promise; archive never remains visible accidentally; the fake contains no
wall-clock or random output unless supplied.

**Likely files:** `internal/mail/mailtest/conformance.go`, `mail.go`, `fake.go`,
`fake_conformance_test.go`, one affected production adapter if the clarified
contract exposes a bug.

**Verification:** `go test -count=1 ./internal/mail/...` plus the targeted
beadmail and exec contract entrypoints.

**Dependencies:** None. **Estimate:** small-medium.

#### H3 — Restore a truthful real beads boundary

**Change:** Remove the stale unconditional skip and make `RunStoreTests`,
`RunMetadataTests`, and `RunDepTests` executable against real BdStore on the
default `bd` v1.1.0 pin. Keep the exhaustive suites on nightly/RC. Define one
path-targeted PR smoke for representative read, write, dependency, and
close/reconstruct-the-same-workspace durability. Keep the v1.0.4
minimum-supported cell focused on its declared external compatibility surface;
any version-specific unsupported behavior needs a governed, explicit skip.
Rename the in-process `beadslib.Storage`-backed NativeDoltStore adapter
entrypoint to state exactly what it proves.

**Acceptance:**

- The path-targeted default-version BdStore PR job executes the exact smoke
  manifest through BdStore -> `bd` CLI -> Dolt and verifies persistence across
  store reconstruction within its declared budget.
- Nightly/RC executes all three applicable shared suites. A zero-test or
  all-skipped smoke or full manifest fails.
- Native storage adapter conformance and live Dolt integration are named and
  reported separately.
- #4197's fast injected-reopen tests and path-targeted real hard-kill/rebind
  contract remain distinct required owners; broad recovery matrices may not
  replace or silently absorb them.

**Likely files:** `test/integration/bdstore_test.go`,
`internal/beads/export_test.go`,
`internal/beads/native_dolt_store_conformance_test.go`, beads skip ledger,
focused CI target.

**Verification:** focused integration target with default v1.1.0 plus the
separate focused minimum/current compatibility cells, and a guard that asserts
required subtests ran.

**Dependencies:** P0.1 for focused CI routing. **Estimate:** small-medium; no
future external release is required to remove the stale default skip.

#### H4 — Complete events concurrency and wake contracts

**Change:** Add multi-watcher, concurrent-record, cancellation, close, and
rotation expectations to the shared contract. Make the exec fixture atomic or
serialize the production adapter. Replace Fake's single-consumer notification
with generation-channel broadcast.

**Acceptance:** Multiple watchers receive the same new event without timer
fallback; exec passes the concurrency contract or documents and enforces
serialization; cancellation unblocks promptly under `-race`.

**Likely files:** `internal/events/eventstest/conformance.go`, `fake.go`,
`conformance_test.go`, `internal/events/exec/exec_test.go`, exec adapter only if
serialization is needed.

**Verification:** `go test -race -count=20 ./internal/events/...` with virtual
or bounded deadlines and no fixed sleep.

**Dependencies:** None. **Estimate:** medium.

#### H5 — Contract production runtime compositions once

**Change:** Inventory production constructors in the runtime registry. Run the
full applicable contract against each production composition. Keep raw seam
tests only for argument/error/capability forwarding, remove duplicate full
tmux/subprocess executions, and add a governed runtime skip ledger.

**Acceptance:**

- Production tmux and subprocess compositions each run full conformance once.
- Exec, ACP, SSH, T3 bridge, auto, hybrid, and herdr declare and run every
  applicable capability contract or an owned expiring skip.
- Raw adapter tests do not fork real infrastructure to re-prove the same state
  machine.

**Likely slice:** `internal/runtime/runtimetest`, one provider family at a time,
`cmd/gc/runtime_registry.go`, and its production-constructor contract.

**Verification:** focused provider package tests; real tmux/subprocess proofs
run once in their targeted lane.

**Dependencies:** P0.3. **Estimate:** large, split by provider family.

#### H6 — Correct mislabeled K8s conformance

**Change:** Make the K8s integration proof instantiate the actual K8s provider,
or rename it as an exec protocol proof. Treat this as the K8s provider-family
slice of H5 rather than requiring H5 to claim K8s coverage first.

**Acceptance:** Test names, reports, and docs state the exact production path
exercised; the real-provider proof declares its supported capability contract
or an owned expiring skip.

**Likely files:** `test/integration/session_k8s_test.go`,
`internal/runtime/k8s/provider_test.go`, and the runtime ledger.

**Verification:** `make test-k8s` for the real boundary and focused K8s
capability contracts.

**Dependencies:** P0.3 runtime ledger; contributes one family to H5.
**Estimate:** small-medium.

#### H7 — Make Worker capability catalogs executable

**Change:** Convert Worker phase-3 catalog entries into executable narrow
capability contracts, or stop calling the catalog conformance until they are
executable.

**Acceptance:** Unsupported Worker capabilities fail explicitly; supported
ones run against both handle implementations; catalog-only rows cannot satisfy
the checked conformance ledger.

**Likely slice:** Bounded files under `internal/worker/workertest` plus one
handle implementation at a time.

**Verification:** Focused Worker capability contracts under `-race` and a
negative ledger fixture for a catalog-only claim.

**Dependencies:** P0.3 architecture ledger. **Estimate:** medium, split by
capability family.

### Phase 2: Build narrow use cases and canonical fast doubles

#### D8 — Extract routed-read policy and retire the per-command six-row matrix

**Change:** Extract API-versus-fallback selection into a small consumer-owned
CLI routing package. Model API success, cache-not-live, generic server failure,
not-found, controller absence, and explicit bypass as typed inputs and
decisions. Test that policy table once. Keep only command-specific tests for
request construction, response decoding, fallback invocation, exit/output
mapping, and one composition proof per distinct adapter shape. Delete
`scripts/check-routed-test-rows.sh` when the shared contract owns the policy.

**Acceptance:** One shared table owns all six route decisions; no command test
restates the shared branch matrix; every command still proves its unique API
and fallback translation; route errors are typed rather than classified by
message text; the old marker guard and duplicated helpers are gone; focused
routing-package tests complete in <=5s.

**Likely slice:** Shared route policy package and tests, one command migration,
and its old matrix/helper deletion, followed by bounded slices for the
remaining command families. Remove the Make target only with the final
migration.

**Verification:** Shared policy tests, focused command adapter tests, and a
final exact census showing zero per-command six-row matrices with no lost
endpoint/fallback contract.

**Dependencies:** None; run early in Phase 2. **Estimate:** small first slice,
medium migration program.

#### D9 — Make split-store dispatch ownership explicit

**Change:** Build on `internal/storeref` with a small consumer-owned store-set
port for readiness and dispatch. The caller supplies the city coordination
stores and the rig work store explicitly; point reads use ID-prefix ownership
with hard-error preservation, while list/readiness queries declare which
stores participate. Keep filesystem/config/provider construction in `cmd/gc`.

**Acceptance:** MemStore-backed tests use distinct city and rig stores with
`gcg-*` and `ga-*`-shaped IDs. A rig-rooted review/formula step becomes ready
when its source/root dependency is in the rig store and its control state is
in the city store; it remains blocked on a real missing dependency or hard
store error. No dispatch path silently substitutes a default store, and no
domain package opens a provider or derives scope from cwd/environment.

**Likely slice:** `internal/storeref`, the readiness/dispatch consumer under
`internal/dispatch` or `internal/convoy`, focused split-store tests, and the
`cmd/gc` composition adapter. Migrate one read/list path at a time.

**Verification:** Small split-store contract tests under `-race -count=20`,
the J2 hermetic journey, and the path-owned real store composition proof.

**Dependencies:** None for the in-memory split-store use-case slice. H3 gates
the path-owned real-store composition proof. **Estimate:** medium, vertical
slices.

#### D1 — Split state, recording, fault, and gate behavior for stores

**Change:** Introduce small decorators around a conformant `beads.Store` or
consumer capability. Migrate one representative cluster of embedded MemStore
wrappers at a time. Do not add methods to a global mega-double.

**Acceptance:** Each decorator has its own tests; delegated behavior still runs
the applicable contract; migrated tests assert outcomes except where protocol
interaction is the subject; one-off wrappers decline.

**Likely slice:** a `beadstest` support file, its tests, and no more than three
caller test files per migration.

**Verification:** affected package plus `go test -race ./internal/beads/...`.

**Dependencies:** Existing `beadstest` contract; H3 must finish before any real
provider proof is removed, but does not block the first decorator slice.
**Estimate:** medium per migration wave.

#### D2 — Split state, recording, fault, gate, and script behavior for runtime

**Change:** Preserve a conformant runtime state fake, then compose independent
recording, faulting, gated, and scripted wrappers. Make concurrency tests use
gates rather than sleeps or shared mutable call slices.

**Acceptance:** The state fake passes runtime conformance; decorators cannot
create undocumented impossible states; migrated session/reconciler tests are
deterministic under `-race -count=20`.

**Likely slice:** `internal/runtime/runtimetest`, `fake.go`, decorator tests, and
two caller files per wave.

**Verification:** focused runtime/session packages under race and repetition.

**Dependencies:** H5. **Estimate:** medium-large, incremental.

#### D4 — Replace API `fakeState` one handler family at a time

**Change:** Start with session lifecycle/read handlers, define handler-owned
gateways containing only used operations, inject them through construction, and
replace broad fakeState fixtures with tiny value fakes. Repeat for mail, orders,
maintenance, services, and diagnostics.

**Acceptance:** Each migrated handler depends only on its gateway; gateway
methods use canonical domain types, never API wire types. Composition/adapters
stay in `internal/api` or the root composition layer—the canonical
`internal/{beads,mail,events,session,worker,...}` packages never import
`internal/api`. Tests do not construct global API state; typed Huma wire
behavior remains unchanged. Track the exact fakeState census from 538 toward
zero and add an import-boundary guard.

**Likely slice:** one handler production file, its tests, gateway definition,
composition adapter, and shared test helper if justified.

**Verification:** focused `internal/api` tests, OpenAPI sync, dashboard check
when wire surfaces are touched.

**Dependencies:** P0.3; independent of other Phase 2 work. **Estimate:** medium
per handler family.

#### D5 — Extract the desired-state calculator from `cmd/gc`

**Change:** Separate desired-state policy and immutable inputs/results from
environment loading, stores, process calls, and session mutation. Place it in
the existing owning internal layer if one fits; create a small new internal
package only after the import/layering audit. Leave a thin command/controller
adapter.

**Acceptance:**

- Policy branches from `build_desired_state_test.go` run in a small package
  without env, cwd, process, tmux, or real clock.
- Adapter tests prove input translation and side effects once.
- `cmd/gc` test binary LOC/bytes and compile RSS decrease measurably.

**Likely slice:** new/existing internal calculator and tests,
`cmd/gc/build_desired_state.go`, its adapter-focused test, and composition.

**Verification:** new package `-race -count=20`, focused `cmd/gc` adapter tests,
compile-size comparison.

**Dependencies:** None for the first pure policy slice; later side-effect slices
consume D1/D2 only where they actually need those capabilities. **Estimate:**
large; split by one decision cluster at a time.

#### D6 — Extract one provider-lifecycle use case from `cmd/gc`

**Change:** Move ensure-ready -> init -> hook -> shutdown orchestration into a
cohesive service with narrow lifecycle ports. Keep command parsing and provider
construction in `cmd/gc`; retain one coordination proof for ordering.

**Acceptance:** Failure/rollback branches no longer require env-selected exec
spies; exact process argument construction stays in adapter tests; the 11,882
line lifecycle test file shrinks as behavior moves to the owning package.

**Likely slice:** lifecycle service and tests,
`cmd/gc/beads_provider_lifecycle.go`, focused coordination tests, composition.

**Verification:** service tests under race/repetition, focused real provider
contract, `cmd/gc` compile metrics.

**Dependencies:** D1 and H3. **Estimate:** large, vertical slices.

#### D7 — Build one hermetic configurable E2E actor

**Change:** Add a role-neutral repo-owned executable that follows a small
declarative script: publish ready, claim work, complete/fail work, wait on a
gate, exit/crash, and emit deterministic progress. Keep reasoning and role
names out of it. It is test infrastructure for composing real controller/runtime
boundaries, not a production agent implementation.

**Acceptance:** The actor has a typed configuration/progress protocol, an
executable contract for every step and cancellation, deterministic IDs/timing,
and no ambient auth. J2 can fan out/complete graph work and J3 can crash at an
exact gate without fixed sleep. Existing behavior-specific shell actors are
retained only for a distinct protocol promise.

**Likely slice:** one package/executable under `test/`, its contract tests,
build helper, and one pilot journey.

**Verification:** actor contract under `-race -count=20`; pilot process proof
under a deadline with event/progress diagnostics.

**Dependencies:** H4 progress/event semantics. **Estimate:** medium.

### Phase 3: Replace wall time and polling with lifecycle signals

#### W1 — Extract workspace process supervision and readiness

**Change:** Put `os/exec`, process groups, readiness, and exit observation
behind consumer-owned `ProcessSupervisor`, `Process`, and `ReadinessProbe`
ports. `Process.Done()` becomes the shutdown signal. Use a conformant scripted
process for use-case tests and retain one real spawn/readiness/terminate/orphan
proof.

**Acceptance:** Workspace manager and proxy tests contain no deadline/sleep
poll loops; cancellation and early exit are deterministic; the production
adapter still owns all OS effects.

**Likely files:** `internal/workspacesvc/proxy_process.go`, process port/adapter,
scripted test support, `proxy_process_test.go`, manager tests.

**Verification:** `go test -race -count=20 ./internal/workspacesvc` plus one
tagged real-process smoke.

**Dependencies:** P0.3. **Estimate:** medium-large.

#### W2 — Create one reusable file-change notifier

**Change:** Extract the proven fsnotify-with-bounded-fallback behavior from the
session log watcher into a small lower-layer notifier usable by event files and
logs. Convert `FileRecorder.Watch` from 250 ms primary polling to notification,
while preserving rotation, external append, and missed-event recovery.

**Acceptance:** File and event watchers wake promptly without a busy loop;
rename/rotation does not lose or duplicate sequence numbers; fallback remains
bounded and observable; no dependency points upward into API.

**Likely slice:** a lower-layer file notification package, its tests,
`internal/events/recorder.go`, and `internal/api/logwatcher.go` adapter usage.

**Verification:** event rotation/conformance under `-race -count=20`, logwatcher
tests, no API wire change.

**Dependencies:** H4 and H1. **Estimate:** medium.

#### W3 — Standardize typed async API waits on SSE

**Change:** Build one test helper that subscribes from a cursor, correlates a
request ID, returns typed success/failure, and then performs a durable typed
read. Replace mutation polling and fixed sleeps in the live API contract.

**Acceptance:** Critical HTTP `202` tests use `/v0/events/stream`; timeout
errors include request ID, cursor, last event, and last durable state; the 15
explicit settle sleeps in `gc_live_contract_test.go` are removed.

**Likely files:** live-contract helper, `gc_live_contract_test.go`, Huma binary
test, and at most two focused async test files.

**Verification:** focused integration smoke repeated under contention; typed
OpenAPI validation remains enabled.

**Dependencies:** H4. **Estimate:** medium.

#### W4 — Replace acceptance lifecycle settling waits

**Change:** Make supervisor helpers observe process exit/socket removal and new
health/event readiness. Replace generic 500 ms `WaitForCondition` call sites
with domain-specific event, session, or provider-ready waits. Tier C waits on
observable runtime/session state instead of three 15-second sleeps.

**Acceptance:** The 18-second supervisor settle floor and 45-second Tier C
floor disappear; failures report the last process/session/provider state;
cleanup still targets only isolated test resources.

**Likely slice:** `test/acceptance/helpers/lifecycle.go`, Tier C helper, and no
more than three caller files per wave.

**Verification:** Tier A repeated on Linux/macOS; Tier C repeated in its
authenticated lane; no bare default tmux cleanup.

**Dependencies:** W3 where API events are used. **Estimate:** medium.

#### W5 — Use structured Worker and Dolt lifecycle publications

**Change:** Consume, do not duplicate, the structured Worker operation events
that landed in `c86f102bc`; the linked
[Worker API hardening Task 4](worker-api-hardening-plan.md#task-4-add-structured-worker-operation-events-and-reduce-polling)
has stale Pending prose and is not an implementation dependency. Consume the
managed Dolt publication/broker from
[Dolt hardening Task 7](dolt-quality-hardening-plan.md#task-7-extract-managed-lifecycle-publication-and-ownership-from-gc-beads-bd)
and [Task 8](dolt-quality-hardening-plan.md#task-8-introduce-a-dolt-state-brokercache-for-steady-state-consumers).
Build on #4197's injected reopen seam, shared retry context, and terminal close
semantics; do not create a second reconnect mechanism. Replace caller-local
state polling only after those owning tasks land.

**Acceptance:** Worker start/interrupt/message/history tests wait on structured
operation outcomes; steady-state Dolt consumers wait on authoritative
publication changes; fallback reads verify state without becoming the wake
mechanism.

**Likely slice:** bounded caller migrations; production event/broker ownership
stays in the linked plans.

**Verification:** Worker/session/API regressions and Dolt lifecycle/recovery
contracts under repetition.

**Owner/status:** `ga-80po0c`. Worker caller/test-wait migration is ready because
its events are merged. The Dolt half is blocked: create and assign child beads
for Dolt Tasks 7/8, then record those IDs and statuses in the owning plan and
here. The prose links alone are not schedulable ownership.

**Dependencies:** Worker half: none. Dolt half: linked owning tasks with
assigned child beads. **Estimate:** medium per migration wave.

#### W6 — Replace Docker command permutations with a protocol double

**Change:** Extract the Docker CLI command/response protocol used by the session
harness. Test image/container argument construction, failure mapping, cleanup,
and ordering against a scripted executable. Keep one real container lifecycle
smoke that proves image build, session operation, and cleanup.

**Acceptance:** The Docker PR lane no longer builds multiple images or starts
many containers to prove argument branches; the real smoke has one purpose and
a <=90s budget; fixed cleanup sleeps are replaced by container-exit inspection.

**Likely slice:** `scripts/test-docker-session`, its protocol helper/tests,
Docker session adapter, and focused workflow target.

**Verification:** hermetic protocol tests on every PR; one path-targeted real
Docker smoke with resource-leak assertion.

**Dependencies:** P0.3. **Estimate:** medium-large.

### Phase 4: Rebuild E2E as a small requirements portfolio

#### E2 — Land J1-J4 as the canonical PR journeys

**Change:** Compose existing fixtures into the four named journeys; do not
write a fifth parallel family. Each journey uses event/process readiness,
asserts durable final state, and verifies cleanup.

**Acceptance:** J1-J4 pass independently and concurrently within budgets;
failures identify the last completed boundary; lower-level error permutations
are absent. J2 fails when the dispatcher reads readiness only from the city
store or a worker resolves the rig root through ambient/default state, and it
asserts durable terminal state in both stores.

**Likely slice:** one journey and its shared fixture per change, no more than
five files.

**Verification:** each test repeated, entire portfolio under race where
possible, and a full-union CI run.

**Dependencies:** Use the journey dependency table below; no journey waits on
an unrelated provider contract. **Estimate:** medium per journey.

| Journey | Required predecessor slices |
|---|---|
| J1 | H5 contract for the production-selected hermetic runtime composition; W4 lifecycle readiness helper |
| J2 | H3 default store truth; H4 event wake contract; D9 scoped-store resolver/dispatch contract; D7 hermetic actor; existing formula/dispatch unit owners |
| J3 | D2 gated runtime behavior; D7 hermetic actor; Worker structured-operation slice from W5 |
| J4 | H4 event concurrency/cursor behavior; W3 typed SSE request-result helper |

#### E3 — Break up the heavyweight integration package

**Change:** Inventory the 162 direct top-level tests under `test/integration`. Move
helper, parser, adapter, and single-boundary tests to their owning packages.
Split remaining provider/formula/journey packages so they do not all pay one
`TestMain` that builds binaries, configures tmux, and sweeps processes.

**Acceptance:** A test pays only for resources it uses; smoke does not rerun in
full REST; package setup is explicit; compile/setup time per shard falls.

**Likely slice:** one test family and its helpers per change, plus shard
manifest update.

**Verification:** old/new top-level test census, invariant map, package timing,
and full integration shards.

**Dependencies:** E1 and owning lower-level proof. **Estimate:** large,
parallelizable by family.

#### E4 — Make txtar subtests shardable and remove duplicate parents

**Change:** Preserve the 32 stable parallel subtests already exposed by
`testscript.Run`, but add a checked txtar manifest and a subtest-aware
selection/timing path because the current sharder enumerates only top-level Go
tests. Stop rerunning migrate-v2 and pack-v2 import scenarios through both
`TestTutorial01` and dedicated parents.

**Acceptance:** Every scenario runs once in the full manifest, can be sharded
and timed independently, and remains linked to its tutorial/user contract.

**Likely files:** `cmd/gc/main_test.go`, testscript manifest/helper, shard
resolver, and tests.

**Verification:** exact scenario census and a no-duplicate policy test.

**Dependencies:** P0.2. **Estimate:** small-medium.

#### E5 — Consolidate overlapping mail, event, and lifecycle E2Es

**Change:** Map `e2e_*`, Gas Town, shell-agent, event, and acceptance cases to
their unique promises. Move edge cases to unit/contract owners. Retain only
composition differences a lower layer cannot prove.

**Acceptance:** Mail archive/send/read behavior is owned below plus at most one
composition proof; event record -> persist -> SSE -> typed client is proved
once; lifecycle permutations have one journey and provider-specific contracts.

**Likely slice:** one behavior family, invariant map, and no more than five
test files per consolidation.

**Verification:** contract + retained journey pass; deleted test names and
invariants appear in review evidence.

**Dependencies:** H2, H4, E1-E2. **Estimate:** medium per family.

#### E6 — Retire the all-7,456-runnable-test process lane

**Change:** Classify all 77 gated `skipSlowCmdGCTest` call sites represented by
the 78-marker census. The package has 7,456 runnable tests plus one `TestMain`
entrypoint that every shard pays. Move argument, retry, ordering, and failure
cases to Small tests through injected ports. Move the few real boundary proofs
into explicit process-contract packages/manifests. Delete the 12-way lane only
when the marker census reaches zero, including removal of the helper.

**Acceptance:** No test selection depends on running every `cmd/gc` test with
`GC_FAST_UNIT=0`; each retained process proof names its boundary and budget;
the default small loop does not change production behavior.

**Likely slice:** one marker cluster and its production seam per change,
followed by Make/workflow cleanup after zero.

**Verification:** marker census, focused small/real proofs, full-union run,
`cmd/gc` compile and duration comparison.

**Dependencies:** D5-D6, W1/W6 as applicable. **Estimate:** large,
parallelizable clusters.

#### E7 — Route broad coverage to the right cadence

**Change:** Keep REST smoke and relevant compatibility/provider proofs on PRs.
Run broad route/generated-read sweeps, full formula retry/recovery, provider
matrices, chaos, live inference, and exhaustive tutorials on push,
path-targeted, nightly, or RC according to E1.

**Acceptance:** No behavior disappears from all automation; PR critical path
contains only listed promises; targeted workflows are automatically triggered
by their owned adapter/protocol paths; nightly/RC failures have owners.

**Likely slice:** CI suite coverage policy, workflow manifests, E2E ledger,
policy tests.

**Verification:** path-filter truth table and scheduled/full-dispatch dry runs.

**Dependencies:** E1-E6. **Estimate:** medium.

#### E8 — Land the T3Bridge + T3 Code + DoltLite composition proof

**Change:** Add one bounded cross-repository provider proof for Gas City ->
T3Bridge -> T3 Code using DoltLite-backed scoped bead/session state. The
hermetic PR form owns a repo-pinned T3 fixture/runtime double; the live form
runs against the compatible T3 Code checkout on nightly/RC. Do not put T3 or
DoltLite assumptions into generic runtime/session packages.

**Acceptance:** The path-targeted hermetic contract proves the exact
start/resume/stop protocol, stable session identity, scoped durable state, and
cleanup against the pinned T3 protocol fixture. The nightly/RC real-checkout
proof additionally proves visible thread creation and UI/runtime composition.
Path triggers cover Gas City T3Bridge, DoltLite composition, and the pinned T3
contract. A version mismatch fails with both repository SHAs.

**Likely slice:** T3Bridge provider fixture, cross-repo contract manifest, one
Gas City integration entrypoint, and the T3 Code-side fixture/compatibility
hook owned in that repository.

**Verification:** Hermetic path-targeted proof on relevant PRs; live
cross-repository proof on nightly/RC; exact-SHA diagnostics on failure.

**Owner/status:** `ga-80po0c`; blocked until the T3 Code-side owner and pinned
contract SHA are recorded as a child task.

**Dependencies:** H5 T3Bridge runtime contract, D9 scoped-store ownership, and
the T3 Code-side fixture owner. **Estimate:** medium cross-repository slice.

### Phase 5: Enforce the architecture and release discipline

#### G1 — Add source and manifest guardrails

**Change:** Extend the
[two-minute design's isolation audit gate](../design/two-minute-ci-blacksmith.md#isolation-audit-gate)
rather than creating a second scanner. New Go architecture rules use `go/ast`
plus import/type identity where needed, preferably through focused
`go/analysis` analyzers; do not add raw substring/line scans for dependency
rules. Migrate existing lexical guards incrementally. Each rule declares its
package/file scope, semantic violation, narrow owned exceptions, and whether
it is a completeness proof or only a ratchet. Add this plan's size/resource
ratchets in the same gate, starting with changed files and an explicit legacy
ledger.

**Acceptance:** Alias and multiline fixtures are detected; comments, string
literals, testdata, and unrelated same-named methods do not trigger. Every
rule has positive and negative fixtures; exceptions carry owner and expiry; a
failure names the semantic boundary and remediation; changed-file analysis
completes in <=5s locally. Architecture guards never substitute for behavioral
or conformance tests. Existing debt has an owner/count/baseline.

**Likely slice:** one check script or Go analyzer, tests/fixtures, Make target,
pre-commit/CI wiring, legacy ledger.

**Verification:** negative fixtures for every rule and a repo-wide baseline
run.

**Dependencies:** taxonomy, P0.3, P0.4, and E1. **Estimate:** medium.

#### G2 — Ratchet package compile and maintainability size

**Change:** Record test-binary bytes, compile wall time, and peak RSS for large
packages; fail regressions beyond a noise allowance. Add a no-growth guard for
existing >2,000-line tests and a ban for new ones.

**Acceptance:** After five stable samples establish per-runner noise, bytes are
an exact no-growth ratchet and wall/RSS allow at most 10% noise. `cmd/gc`
reaches <150 MB/<1.5 GB and p95 <=20s incremental edit-to-result on the named
reference runner. Extraction PRs report before/after package cost; file
splitting cannot be presented as a compile improvement without package
movement.

**Likely slice:** measurement script, fixture tests, CI artifact/summary,
threshold ledger.

**Verification:** deterministic size fixtures and repeated baseline samples;
wall/RSS become enforcing after the five-sample calibration.

**Dependencies:** P0.2. **Estimate:** small-medium.

#### G3 — Rewrite `TESTING.md` around size, purpose, and ownership

**Change:** Replace the overlapping tier story and stale fake inventory with
the two-axis taxonomy, conformance ledger, wait policy, E2E manifest, exact
local/PR/push/nightly/RC placement, and examples of consumer-owned ports.

**Acceptance:** A contributor can classify a new test without reading CI YAML;
every command and lane named in the guide exists; integration placement and
BdStore status are truthful; generated tables remain in sync.

**Likely files:** `TESTING.md`, contributor index, ledger-generated section,
docs sync test.

**Verification:** `make check-docs`, docs sync, command/link policy tests.

**Dependencies:** P0.3 and decisions from H/E phases; update incrementally as
each phase lands. **Estimate:** medium.

#### G4 — Protect the complete gate and require exact-SHA release evidence

**Change:** Consume the protected-check migration and reusable full-CI work
owned by the [two-minute CI design](../design/two-minute-ci-blacksmith.md#protected-check-migration);
do not create a second workflow topology. After that design's overlap window,
make `CI / required` merge-blocking. In a separate release-policy slice,
require a successful RC gate for the exact publish SHA.

**Acceptance:** Integration/process/worker/pack/container failures block merge
when their paths are in scope; RC does not duplicate normal CI matrices; stable
and RC release refuse an unverified commit.

**Likely slice:** the linked design owns reusable CI and branch-protection
migration; this plan owns only release workflow/policy tests for exact-SHA
evidence.

**Verification:** policy unit tests plus a non-publishing release dry run.

**Owner/status:** `ga-80po0c`; blocked. Create assigned child beads for the
protected-check migration and exact-SHA release-policy slice, then record them
in the linked design and here. The missing `ga-nakct` reference is not live
ownership.

**Dependencies:** P0.2 target met, E7, and assigned protected-check/release
child beads. **Estimate:** shared milestone plus a small-medium release-policy
slice.

#### G5 — Add high-signal generative techniques at pure boundaries

**Change:** Grow fuzz/property tests only for parsers, serializers, graph
invariants, ID/path normalization, config layering, and event round trips where
the oracle is crisp. Use sampled mutation testing on extracted pure packages to
find assertion gaps; do not gate the repo on a vanity mutation score.

**Acceptance:** Every fuzz target has a bounded deterministic corpus, no
external resources, and a stated invariant. Surviving sampled mutations become
specific test-quality beads, not broad test duplication.

**Likely slice:** one owning package and corpus per change; tooling kept out of
the default developer loop unless it is fast.

**Verification:** short seeded fuzz run on PR for touched targets; longer
scheduled fuzz/mutation jobs.

**Dependencies:** extracted small packages from Phase 2. **Estimate:** ongoing.

## Recommended execution order and parallelism

With P0.1 merged, start four bounded workstreams:

| Workstream | First tasks | Why first |
|---|---|---|
| Contract truth | H1, H2, H3, H4 | Prevents false confidence before consolidation. |
| Architectural extraction | D8 and D9, then D4 and D5/D6 | Removes duplicated route policy, makes split-store dispatch testable, then attacks the API and `cmd/gc` compile/global-state centers. |
| Lifecycle signals | W1 and W3 | Replaces representative process and API polling with reusable patterns. |
| Measurement/policy | P0.2-P0.5 and E1 | Makes runtime, size, race, skip, and E2E ownership enforceable. |

Start D8 and D9 immediately as bounded high-ROI extractions; add D9's real-
store composition proof after H3 truth is established. H5 follows the runtime
ledger, then D2. D1
follows the beads truth work. W4 follows the
reusable request/process readiness helpers. E2 lands one
journey at a time as its owning signals become available. E3/E5/E6 delete or
move coverage only after those replacements pass. G1 starts in changed-line
mode early and becomes a repo-wide ratchet at the end.

Recommended phase exit gates:

| Phase | Exit evidence |
|---|---|
| 0 | Sub-five-minute topology retained; timing samples persist; provider/E2E ledger checked. |
| 1 | OSFS/Fake and mail contracts agree; real beads smoke executes; events concurrency passes; production runtime contract inventory is truthful. |
| 2 | Reusable decorators exist; split-store readiness/dispatch is explicit; first API and `cmd/gc` vertical slices leave the monolith; compile metrics improve; #4158's session fixture cutover remains at its permanent-zero guard. |
| 3 | Representative API, workspace, acceptance, Worker, and Dolt paths wake on signals; no fixed sleep remains in migrated small tests. |
| 4 | J1-J4 are the only generic PR journeys; every other large test has a targeted cadence; process-marker and duplicate-family counts materially decline. |
| 5 | Source/manifest ratchets block drift; `TESTING.md` is truthful; `CI / required` is protected after its p95 sample; releases require exact-SHA evidence. |

## Migration and review rules

Every test-moving PR must include an invariant map:

| Invariant | Old owner | New owner | Size/purpose | Why truthful | Runtime before/after |
|---|---|---|---|---|---:|

Reviewers should reject a migration when:

- the new fake has not passed the production contract;
- a command or handler test still retests all use-case branches;
- a deleted E2E has no lower-layer or replacement owner;
- polling was hidden inside a generic `Eventually` helper rather than replaced
  by a lifecycle signal;
- a new port mirrors a producer's broad API instead of the consumer's need;
- a test passes only because a required subtest skipped;
- process-global mutation makes parallel callers unsafe; or
- a faster shard merely moved work to an unowned workflow;
- a shared policy branch matrix is copied into adapters instead of being owned
  once; or
- a new architecture rule scans raw source text when syntax/type identity can
  express it.

For code changes, TDD means the sequence is visible: failing focused proof,
minimal implementation, passing focused proof, applicable contract, broader
shard, and review. Characterization tests are appropriate before extraction,
but once the seam exists they should be rewritten around stable behavior rather
than private structure.

## Relationship to existing work

This plan is a companion, not a replacement, for:

- [Two-minute CI on Blacksmith](../design/two-minute-ci-blacksmith.md), which
  owns timing storage, runnable-unit planning, sharding, runner images, path
  gating, and CI summary topology after its missing tracker is replaced with an
  assigned child under `ga-80po0c`. This audit owns the architecture and
  truthfulness of those runnable units.
- [Worker API hardening Task 4](worker-api-hardening-plan.md#task-4-add-structured-worker-operation-events-and-reduce-polling),
  whose Pending status is stale: structured Worker operation events landed in
  `c86f102bc`. W5 can migrate Worker waits now and does not invent a second
  Worker event model.
- [Dolt contract quality hardening Task 7](dolt-quality-hardening-plan.md#task-7-extract-managed-lifecycle-publication-and-ownership-from-gc-beads-bd)
  and [Task 8](dolt-quality-hardening-plan.md#task-8-introduce-a-dolt-state-brokercache-for-steady-state-consumers),
  which own managed lifecycle publication and the state broker. W5 migrates
  test waits after that ownership exists and consumes #4197's reopen seam
  rather than inventing another reconnect path; Tasks 7/8 need assigned child
  beads under `ga-80po0c` before that half is schedulable.
- Merged PR [#4158](https://github.com/gastownhall/gascity/pull/4158)
  (`25d395fc0`), which completed the `sessiontest` fixture
  cutover and left a permanent-zero guard. This plan retains that boundary and
  does not schedule the archived ~498-site migration again.
- Merged PR [#4193](https://github.com/gastownhall/gascity/pull/4193), which
  establishes the immediate under-five-minute topology and removes several
  duplicate CI/provider paths. P0.1 prevents those gains from regressing.
- Merged PR [#4197](https://github.com/gastownhall/gascity/pull/4197), which
  owns NativeDoltStore reopen/retry lifecycle semantics and the path-targeted
  real hard-kill/port-rebind proof.

If an owning plan changes its boundary, update the references here rather than
forking its production design inside a test helper.

## Risks and mitigations

| Risk | Mitigation |
|---|---|
| Broad coverage moves before lower layers are honest | Phase 1 gates all deletion; require invariant maps. |
| Interface proliferation | Ports are consumer-owned, small, and justified by production plus double/adapter implementations; avoid universal abstractions. |
| Fakes drift from production | Same executable contract, deterministic sources, compile assertions, race coverage. |
| Event conversion introduces lost wakeups | Subscribe/capture cursor before action, use broadcast generations, re-read durable state after wake, repeat under race. |
| Nightly becomes a failure graveyard | Every lane/test has an owner and SLO; failures create beads; expired quarantine fails policy. |
| Timing planner overfits outliers or warm cache | Store p50/p75/p95 and variance, use conservative cold-start fallback, retain hard shard ceilings. |
| File splitting games size metrics | Compile/binary/RSS budgets are package-level; line limits are maintainability-only. |
| Upstream merge becomes harder | Extract small internal services/adapters; minimize edits to generic upstream paths; avoid T3/Dolt assumptions outside providers. |
| Minimum-supported `bd` lacks newer default behavior | Run full conformance on default v1.1.0; keep v1.0.4 coverage focused on the declared compatibility surface with explicit version-specific skips only. |
| Path gating misses a transitive impact | Full-union sample and push/RC coverage; conservative shared-path rules; suite-coverage policy tests. |
| Developers bypass slow local commands | Small loop is the default and genuinely fast; focused medium/provider commands are documented and observable. |

## External dependencies and open decisions

Only these decisions should remain open during execution:

1. **Measured package thresholds:** the initial `cmd/gc` 150 MB/1.5 GB targets
   are directionally correct. Freeze enforcement thresholds after five stable
   samples on representative Blacksmith and developer hosts; never raise them
   to excuse growth without a bead.
2. **Protected-context administration:** changing branch protection and
   release rules requires repository-owner access. The evidence threshold is
   settled here; execution waits only for that authority.

The four PR journeys, resource taxonomy, no-fixed-sleep rule, contract-first
doubles, and exact-SHA release requirement are proposed decisions, not open
questions.

## Definition of done

This program is complete when:

- all small tests are deterministic, in-process, and free of wall-clock sleep,
  external processes, host env/cwd mutation, and shared resources;
- every reusable double passes its applicable production contract;
- required provider contracts execute with zero silent skips;
- `cmd/gc` is a thin adapter over cohesive tested use cases and its test binary
  is below the ratcheted size/RSS target with p95 <=20s incremental
  edit-to-result on the reference runner;
- internal state transitions wake tests through events/channels/process exit,
  with polling confined to explicit external adapters;
- J1-J4 are the only generic PR-blocking whole-system journeys and every other
  large proof has an owned targeted cadence;
- the entire small loop has p95 <=60s and focused packages p95 <=5s;
- the checked fixed-sleep, process-marker, env/cwd, and broad-fake debt ledgers
  do not regress, and approved invariant-owned burn-down milestones are met
  without relabeling the debt;
- `CI / required` passes the 20-run protection overlap and then holds p95
  <=4m30s with every non-platform-outage full-union run under five minutes over
  the rolling 200-run operating window;
- deterministic tests have zero known flakes or automatic retries;
- changed concurrency packages pass the required race lane and the broad
  scheduled race sweep has no unowned failures;
- `TESTING.md`, the checked ledgers, Make targets, and CI policy agree; and
- the exact release SHA cannot publish without successful RC evidence.

## Reproducing the census

This is a source census across every tracked `*_test.go` file and all build
tags/OS variants, not the runnable set for one platform. The historical test
regex includes `TestMain`; the runnable count excludes it explicitly. Exact
tag counts use `^//go:build <tag>$`; compound, native, and OS expressions belong
in “Other.” Run from the repository root:

```bash
rg --files -g '*_test.go' | wc -l
# Historical-compatible source count; includes TestMain.
rg -g '*_test.go' '^func Test[A-Za-z0-9_]*\(' | wc -l
# Runnable Test functions only.
rg --pcre2 -g '*_test.go' '^func Test(?!Main\()[A-Za-z0-9_]*\(' | wc -l
# TestMain entrypoints.
rg -g '*_test.go' '^func TestMain\(' | wc -l
rg --files -g '*_test.go' | xargs wc -l | tail -1
comm -23 <(rg --files -g '*.go' | sort) <(rg --files -g '*_test.go' | sort) \
  | xargs wc -l | tail -1
rg -o -g '*_test.go' '\bt\.Run\(' | wc -l
rg -o -g '*_test.go' 't\.TempDir\(' | wc -l
rg -o -g '*_test.go' 'time\.Sleep\(' | wc -l
rg -l -g '*_test.go' 'time\.Sleep\(' | wc -l
rg -o -g '*_test.go' 'exec\.Command(Context)?\(' | wc -l
rg -l -g '*_test.go' 'exec\.Command(Context)?\(' | wc -l
rg -o -g '*_test.go' '\bt\.(Skip|Skipf|SkipNow)\(' | wc -l
rg -g '*_test.go' 'skipSlowCmdGCTest\(' | wc -l
rg -o -g '*_test.go' '\bt\.Parallel\(\)' | wc -l
rg -o -g '*_test.go' 't\.Setenv\(' | wc -l
rg -o -U -g '*_test.go' 'newFakeState\(|fakeState\s*\{' | wc -l
rg -l -U -g '*_test.go' 'newFakeState\(|fakeState\s*\{' | wc -l
rg -o -g '*_test.go' '\.Calls\b' | wc -l
rg -l -g '*_test.go' '\.Calls\b' | wc -l
```

Compile measurements used:

```bash
/usr/bin/time -v go test -c -o /tmp/gc-cmd.test ./cmd/gc
/usr/bin/time -v go test -c -o /tmp/gc-config.test ./internal/config
```

Do not run `go clean -cache` before a cold measurement. Use an isolated
temporary `GOCACHE` when cold-build evidence is required.
