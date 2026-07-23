# Gas City Testing Policy

This file is the canonical, normative source for how Gas City tests are
designed, placed, reviewed, and timed. If an older plan, audit, or contributor
document conflicts with this policy, this file wins. Existing exceptions are
debt, not precedent. In this document, an **owner** is a tracking bead with a
current assignee. An approved waiver must also name its reason, replacement
proof, and expiry.

### Policy versus enforcement today

The rules below are normative even where automation is still being built. Do
not describe a target as an existing gate.

| Policy area | Mechanical status today |
|---|---|
| Sleep/process/listener/tmux/env/CWD growth | Checked by the source-resource ledger below |
| Runtime constructor and `runtime.Fake` conformance binding | Checked by the runtime provider ledger below; several explicit waivers remain |
| Other provider conformance | Shared suites exist, but exact production-constructor coverage is still a manual audit with known gaps |
| Sub-five-minute PR feedback and timing ratchets | Target; current Go timing artifacts measure test execution, not workflow queue/bootstrap/graph time (`ga-80po0c.4`) |
| Large/E2E ownership and cadence | Target; the executable manifest is owned by `ga-80po0c.6` |
| First-attempt flake and quarantine policy | Target; required Playwright retry and legacy unledgered skips remain noncompliant debt under `ga-80po0c` |

## The outcome: protected PR feedback in under five minutes

The developer-visible service-level objective is p95 **under five minutes**
from GitHub Actions PR-workflow creation until the required automated `CI`
summary reaches a terminal conclusion. Compare the latest 20 non-superseded
full-union runs on the same runner-policy cohort; include failed and timed-out
attempts, and exclude only obsolete-SHA concurrency cancellations. Queueing is
part of the developer-visible metric and is also reported separately. The
execution sub-budget, from the first required job entering `in_progress` until
`CI / required` completes, is p95 at most 4m30s. Current telemetry does not yet
enforce this SLO; `ga-80po0c.4` owns that gap.

The budget changes where a proof runs, never whether an important risk is
proved:

- Required PR lanes should contain fast, deterministic proofs plus the
  relevant real boundaries. Current integration routing is coarse and the
  dashboard job is unconditional; treat that as optimization debt, not the
  desired endpoint.
- Broader real-provider and full-composition proofs run on `main` when they
  cannot fit the PR budget.
- Credentialed, live-inference, cloud, and soak journeys belong in scheduled
  or explicit profile lanes. The full live-inference profile matrix is
  currently local-only; do not claim nightly coverage for it.

A slow PR test may move to a later lane only after lower layers own its branch
and error-detail matrix. The later lane must retain any unique real-composition
risk. Moving a test without that ownership map is deleting quality, not
improving feedback.

## The authoring rule: one risk, one smallest owning proof

Start with a single sentence describing the regression the test must catch.
Then put that assertion at the smallest layer that can fail for the intended
reason. A higher layer may prove wiring across a boundary, but it must not
repeat the lower layer's branch matrix.

Classify the observable promise first:

1. **Behavior promised by a provider interface?** Add the case once to its
   shared conformance suite and run that suite against every production
   implementation with distinct behavior and every reusable fast substitute.
2. **Implementation-only decision or domain transition?** Write a unit test
   next to the code.
3. **User-visible CLI parsing, output, or exit status?** Use testscript with
   fast providers.
4. **Ordering or argument plumbing between components?** Write one focused
   coordination test with recording collaborators.
5. **Real process, protocol, filesystem, database, browser, or provider
   composition?** Keep one integration or end-to-end proof for that boundary.
6. **Documentation-to-code agreement?** Put the invariant in `test/docsync`.

The question is not “where can this test be made to pass?” It is “which layer
uniquely owns this risk?” Search for an existing owner before adding a test. If
one exists, strengthen or parameterize it instead of creating another journey.
Conformance is a reusable testing pattern rather than a sixth execution tier:
one contract suite is intentionally executed against multiple implementations.

### RED, GREEN, refactor, measure

Every behavior change and bug fix follows this loop:

1. **RED:** add the smallest owning test and observe it fail for the intended
   reason. For a bug, reproduce the reported failure before changing code.
2. **GREEN:** make the narrowest production change that satisfies the test.
3. **Refactor:** improve names and boundaries, remove duplicate assertions,
   and replace expensive collaborators with proved substitutes.
   A behavior-neutral migration records a PR-description table from every
   retired assertion to its new owner and retained real-boundary proof. Before
   commit, delegate independent reviews of semantic parity, speed/resource
   policy, and repository accuracy/enforceability.
4. **Measure:** repeat the focused test and run the affected shard. Record
   before/after wall time when adding, moving, or materially changing tests.
5. **Verify the boundary:** run the focused owner plus the relevant
   conformance, coordination, or integration owner.

Never write the large end-to-end test first merely because the production code
has no seam. Refactor the code so the policy can be exercised directly, then
retain the smallest real-boundary proof that demonstrates the wiring.

## Design production code for fast proofs

Core logic receives dependencies; outer constructors choose production
implementations. Prefer an existing provider port. For one isolated side
effect, inject a function value. Introduce a new interface only when it is a
stable domain boundary **and** has at least two real implementations,
consistent with Gas City's no-premature-abstraction rule.

| Source of nondeterminism | Fast seam | Keep real coverage for |
|---|---|---|
| Bead or domain persistence | `beads.Store`, usually `beads.MemStore` in consumer tests | Store conformance and provider lifecycle |
| Wall-time/deadline decisions | Injected clock, including `clock.Fake` | The real-clock adapter, not every consumer |
| Timers, sleeps, scheduling, backoff | Injected timer/sleeper/scheduler or `testing/synctest` | The timer adapter, not every consumer |
| Asynchronous completion | Channel, callback, event watcher, or notifier | One public protocol/event-stream composition |
| Subprocess execution | Narrow executor function/interface with scripted results | Argument-to-real-binary compatibility |
| Generated IDs or randomness | Injected generator with deterministic values | Format/entropy adapter contract |
| Filesystem operations | `fsys.FS`, normally `fsys.Fake` for consumer logic | `fsys.OSFS` conformance and OS-specific semantics |

Environment variables, current working directory, global clocks, package-level
mutable state, and executable discovery belong at composition edges. Unit tests
must not need them to steer domain behavior. Use `t.TempDir()` when the real
filesystem is itself relevant; otherwise prefer `fsys.Fake`.

## Choose meaningful failure edges, not Cartesian products

Test each distinct obligation at its owner. For a typical operation, consider
only the applicable boundaries:

- invalid input or an absent required value;
- collaborator unavailable before any side effect;
- partial success requiring rollback, idempotency, or recovery;
- cancellation or deadline propagation;
- a concurrency conflict or lost-update boundary;
- serialization or protocol incompatibility;
- restart/reconnect behavior at a real provider lifecycle boundary.

Equivalence classes beat exhaustive combinations. If five commands use the
same store port, test the shared store failures in conformance, each command's
distinct response in a unit test, and one command-to-real-store composition.
Do not multiply every command by every provider by every error. Add another
combination only when it represents a different contract. An escaped
regression must first populate the missing equivalence class at the smallest
owner; retain its high-level reproduction only when the defect uniquely
depends on that composition.

## Asynchronous tests wait for facts, not elapsed time

New or modified tests must not use `time.Sleep` to wait for work to “probably”
finish, and must not add open-coded polling loops. Instead:

- expose a completion/error notification and select on it with a context;
- capture the event cursor and subscribe before triggering work, then correlate
  terminal success or failure by request/resource ID, close the subscription,
  and reread durable state;
- use a fake clock or `testing/synctest` for timers, retries, and backoff;
- use a barrier/channel to prove a goroutine reached a state before releasing
  it; and
- assert the terminal state immediately after the notification.

Polling is allowed only at a true black-box boundary that exposes no completion
signal and where adding one would change the public contract. Such polling must
use a shared helper with a context-aware ticker or bounded backoff, fail with
the last observed state, and have one named boundary owner. Busy loops and a
fixed sleep before the helper are forbidden. The deadline rule below supplies
safety timeouts; those deadlines must not determine the normal test duration.

## Test doubles and conformance are one contract

A fast substitute is trustworthy only when it is held to the same observable
contract as production. When a provider method or invariant changes:

1. change the shared conformance suite first;
2. run it against every behaviorally distinct production implementation or
   composition for that port;
3. run it against every reusable fast substitute; and
4. keep implementation-specific tests only for behavior outside the shared
   contract.

Thin aliases that add no state, transformation, or behavior may use a focused
exact-constructor wiring proof instead of repeating the full suite. The checked
runtime ledger remains authoritative for runtime compositions.

Skips do not count as conformance. A temporary incompatibility must be recorded
as an explicit waiver with a tracking-bead owner, reason, replacement proof,
and expiry. Constructor wrappers and provider compositions need coverage for the
actual production path; proving a nearby raw implementation is not
enough.

Fakes need only model observable contract behavior used by consumers. They do
not simulate implementation internals. Add recording only when call order or
arguments are themselves the contract; a stateful fake is not automatically a
spy.

## Keep the critical end-to-end portfolio deliberately small

An end-to-end test is admitted only when all of these are true:

- it protects a high-value user journey or high-blast-radius recovery path;
- the risk exists only when multiple real boundaries are composed;
- lower layers already own the branch and error-detail matrix;
- its assertions use stable public outcomes rather than internal timing;
- it has hermetic setup, targeted cleanup, actionable diagnostics, and a named
  owner; and
- its lane and measured duration fit the cadence above.

Each major effort must point to an existing critical journey or add the one
missing composition proof. It does not receive a new E2E for every acceptance
criterion. Before admitting an E2E, list the lower-layer owners it relies on
and the unique cross-boundary failure it catches. When two journeys catch the
same regression, keep the clearer and faster one.

Record the journey, unique risk, lower-layer owners, path triggers, lane,
budget, diagnostics, and owner in the checked E2E/provider manifest owned by
`ga-80po0c.6`. Until that manifest lands, put the same fields in the PR
description. On-demand coverage does not count as a release proof without a
freshness gate for the exact release SHA.

## Flakes are defects

A deterministic product-test failure may not be retried into green on the same
tested SHA. Repetition is useful for diagnosis, but a required gate must retain
the worst product-test status across attempts. A pre-test runner/service outage
may be retried only when classified with attached infrastructure evidence; it
is reported separately. A code change produces a new SHA and a new result. The
failure has one tracking-bead owner until fixed.

Quarantine is forbidden until a checked ledger exists. Any future quarantine
must include a tracking-bead owner, captured failure evidence, nonblocking
still-failing lane, replacement coverage, and expiry that fails CI;
quarantined coverage cannot satisfy a required gate. Capability-based local
skips likewise require an equipped CI execution or an explicit waiver. Do not
weaken assertions, increase sleeps, or broaden retries to hide an unknown race.
Remove redundant tests; repair unique tests.

## Timing objectives and resource ratchets

Test performance claims require evidence. For a focused change, run the test
repeatedly with the result cache disabled (for example, the command below) and
time the relevant sharded target. This is focused diagnostic evidence, not an
authoritative p95; the history format requires twenty comparable successful
samples for an authoritative p95.

```bash
go test -count=10 -run '^TestName$' ./path
```

Compare like runner, OS, architecture, CPU count, cache condition, and suite
variant. A single warm-cache run is diagnostic, not a regression baseline.

No change may knowingly push the protected graph above its SLO. Once trusted
history and workflow telemetry are authoritative, checked per-profile
baselines must fail material regressions and lower after sustained
improvements; increases require an expiring waiver. Until then, include
before/after observations in the PR and treat the timing tools below as
shard-balancing evidence rather than enforcement.

The checked source-resource ledgers below are anti-growth ratchets for sleeps,
processes, listeners, environment mutation, and CWD mutation. Reductions lower
the checked baseline; new debt requires the same explicit, expiring policy
change as any other waiver.

## Checked source-level resource ratchets

`test/test-resources.toml` is the checked P0.4 resource ledger. It scans tracked
Go source through parsed syntax and import identity, while only `*_test.go`
files contribute resource occurrences. The raw audit and source-debt rows
freeze process, sleep, environment, CWD, slow-process, HTTP test-server, and
package-level `net` stream/packet listeners, `net.ListenConfig` listeners,
direct `syscall.Listen`, and typed or literal tmux dependency call/file totals.
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

`[[reviewed_hermetic_body]]` rows record a narrower fact than a Small-test
classification: the exact untagged top-level test body and every statically
resolved receiverless helper in the same package contain none of the resource
identities cataloged below. A row is exact, code-owned, and stale-checked; it
cannot use a wildcard, silently move to another test, or claim an effective
Small size while package setup remains Medium. The checked call graph follows
direct helper calls and references used as local function aliases across Go
files in the same package, and terminates safely on cycles.

This is intentionally not a universal hermeticity proof. Cross-package calls,
method and interface dispatch, package-level callback indirection, and
resources absent from the catalog remain manual-review boundaries. In
particular, `TestPrepareWaitWakeState_ResolvesRigDependencyBeads` and
`TestDoSessionWake_PokesManagedControllerAfterStateChange` have reviewed
hermetic bodies but still run as Medium because `cmd/gc` owns a process-mutating
`TestMain`. `TestDoSessionWait_RegistersReadyWaitForRigDependency` has the same
reviewed-hermetic guarantee for the wait-registration use case.
`TestCmdSessionWait_AllowsRigDependencyBeads` remains the singular
CLI/config/file-store split-store composition proof for wait, while
`TestManagedBdRigProviderStoreRecoversAfterHardKillPortRebind` owns the real
managed-provider hard-kill/port-rebind boundary. Likewise,
`TestCmdSessionWake_PokesManagedControllerAndRequestsSuspendedStart` remains the
singular CLI/config/file-store/controller-socket composition proof for wake.
`TestDoMailInbox_RendersMessagesFromReader` owns inbox rendering through the
consumer's one-method reader port, while
`TestCmdMailInbox_NormalizesCanonicalManagedProviderEnvAndReadsInbox` remains
the singular CLI/mail/canonical-`GC_BEADS`/real-Dolt store-factory composition
proof. Full managed-city lifecycle and recovery stay with their focused
provider-store owners instead of being repeated by each command consumer. Body
review is not a reason to remove a retained boundary test.

`TestDockerSessionProtocol` owns fast Docker CLI mapping, injected failures,
and cleanup transitions through a strict `PATH`-injected executable. The
real-Docker `scripts/test-docker-session` harness remains the composition owner
until each retained container invariant has a replacement contract and the
real proof is deliberately consolidated.

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
and the exact package-level stream constructors `net.Listen`, `net.ListenTCP`,
and `net.ListenUnix`; packet constructors `net.ListenPacket`, `net.ListenUDP`,
`net.ListenIP`, `net.ListenUnixgram`, and `net.ListenMulticastUDP`;
`net.ListenConfig.Listen` and `ListenPacket` on lexically identified receivers;
and direct `syscall.Listen`. The tmux catalog recognizes canonical `test/tmuxtest`
namespace/lifecycle helpers, imported `internal/runtime/tmux` production
constructors, and literal `os/exec` tmux commands and probes. Its untagged
source census is 6 calls in 2 files, all owned by exact Medium `TestMain`
rows; build-tagged calls remain E1 Large inventory rather than being relabeled
Medium. `NewSocketParentDir`, `HoldAliveSentinel`, and the PID-directory
helpers remain part of the separate shared-host resource tail. Direct
`syscall.Socket`/`Bind` setup calls, `net.FileListener`/`FilePacketConn`
descriptor duplication, helper-backed listeners whose constructors live
outside test source, Dolt, and other shared-host resources remain explicit
follow-up catalogs. A Medium resource may describe a helper-backed runtime
cost, but only syntax-owned calls in that exact runnable declaration
leave Small-debt accounting. The `ListenConfig` matcher uses lexical Go types
to follow same-file values, pointers, parameters, aliases, and typed factory
results rooted in the imported `net.ListenConfig` type; it does not load
cross-file package bodies or host toolchain export data.
The tmux helper match is an explicit dependency/namespace proxy; it does not
claim a recursive inventory of the helper's environment mutations. Function
aliases and wrappers, variable or absolute command names, `sh -c`, `exec.Cmd`
literals, `Guard` methods, and same-package bare constructors remain deliberate
manual-review boundaries. `ga-80po0c.2.2` owns the listener, tmux, Dolt, and
shared-host catalogs. E1
separately owns Large journey and provider entries.

The scanner recognizes direct calls to `os/exec.Command{,Context}` and
`time.Sleep`; package-level `net.Listen`, `ListenTCP`, `ListenUnix`,
`ListenPacket`, `ListenUDP`, `ListenIP`, `ListenUnixgram`, and
`ListenMulticastUDP`; `net.ListenConfig.Listen` and `ListenPacket` on identified
receivers; direct `syscall.Listen`;
`net/http/httptest.NewServer`,
`NewTLSServer`, and `NewUnstartedServer`; `os.Setenv`, `os.Unsetenv`,
`os.Clearenv`, and `os.Chdir`; and
`Setenv` or `Chdir` on function parameters typed exactly as `*testing.T` or
`testing.TB`. For tmux it recognizes `ConfigureProcessEnv`,
`KillAllTestSessions`, `NewGuard`, `NewGuardWithSocket`, and `RequireTmux` from
`test/tmuxtest`; `NewProvider`, `NewProviderWithConfig`,
`NewSeamBackedWithConfig`, `NewTmux`, and `NewTmuxWithConfig` from
`internal/runtime/tmux`; and literal `os/exec.Command("tmux", ...)`,
`CommandContext(ctx, "tmux", ...)`, and `LookPath("tmux")` calls. It also
recognizes the receiverless
`skipSlowCmdGCTest(*testing.T, string)` definition and its same-package calls.
An unresolved cross-file call counts only when that directory and package own
the canonical helper. Import, parameter, and same-file helper matches use
lexical object identity; top-level sibling declarations are indexed by
directory and package so cross-file shadows do not masquerade as resources.
Local shadows and wrong signatures do not count. Parenthesized call
expressions retain the same ownership.

Targeted dot imports of `net`, `os/exec`, `time`, `os`, `syscall`, `testing`,
`net/http/httptest`, `internal/runtime/tmux`, or `test/tmuxtest` are rejected
with file and import context because their resources cannot be attributed
safely; blank imports remain harmless.
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
| Audit baseline | all tracked test source | fixed_sleep: 427 calls / 156 files (historical regex census: 447 / 157) | ga-80po0c.2 | tracked test source totals remain visible as audit evidence; ga-80po0c.2 owns this point-in-time source census | P0.4a | 2026-10-01 |
| Audit baseline | all tracked test source | subprocess: 529 calls / 162 files (historical regex census: 495 / 135) | ga-80po0c.2 | tracked test source totals remain visible as audit evidence; ga-80po0c.2 owns this point-in-time source census | P0.4a | 2026-10-01 |
| Medium owner | `cmd/gc` package `main` | TestMain: environment, tmux | ga-80po0c.2.1 | cmd/gc TestMain is the checked package-level Medium owner for process environment and tmux namespace setup; only declared environment and tmux calls lexically inside TestMain leave Small debt | P0.4b/P0.4c-tmux | 2026-10-01 |
| Medium owner | `internal/api` package `api` | TestEveryEmittedErrorCodeIsRegistered: subprocess | ga-80po0c.2.1 | internal/api tracked-source error URN guard is a checked Medium owner; only the git ls-files call lexically inside TestEveryEmittedErrorCodeIsRegistered leaves Small debt | P0.4b | 2026-10-01 |
| Medium owner | `internal/runtime/herdr` package `herdr` | TestServerAliveDetectsLiveServer: net_listen | ga-80po0c.2.2.2 | herdr live-server liveness regression is a checked Medium stream-listener owner; the Unix stream listener is confined to TestServerAliveDetectsLiveServer and closed by test cleanup | P0.4c-listener | 2026-10-01 |
| Medium owner | `internal/runtime/herdr` package `herdr` | TestServerAliveRejectsStaleSocket: net_listen | ga-80po0c.2.2.2 | herdr stale-socket liveness regression is a checked Medium stream-listener owner; the Unix stream listener is confined to TestServerAliveRejectsStaleSocket and closed before liveness detection | P0.4c-listener | 2026-10-01 |
| Medium owner | `internal/runtime/tmux` package `tmux` | TestMain: environment, tmux | ga-80po0c.2.2.1 | runtime tmux TestMain is the checked Medium owner for isolated tmux process and socket cleanup; only declared environment and tmux calls lexically inside TestMain leave Small debt | P0.4c-tmux | 2026-10-01 |
| Medium owner | `scripts` package `scripts_test` | TestDockerSessionProtocol: subprocess | ga-80po0c.23.1 | Docker session adapter protocol proof is a checked Medium owner; the one adapter subprocess is confined to TestDockerSessionProtocol and Docker itself is a strict PATH-injected fake | W6 | 2026-10-01 |
| Medium owner | `scripts` package `scripts_test` | TestProviderOverridesAndSuiteContractsCrossMakeIsolation: subprocess | ga-80po0c.2.1 | Make/provider and suite-contract proof is a checked Medium owner; the six isolated Make invocations are confined to TestProviderOverridesAndSuiteContractsCrossMakeIsolation | P0.1 | 2026-10-01 |
| Small debt ratchet | `cmd/gc` untagged test source | cwd: 285 calls / 43 files (historical regex census: 284 / 43) | ga-80po0c.2.1 | untagged Small cmd/gc cwd call/file totals cannot grow; reductions must lower this baseline; non-Medium lexical owners restore or eliminate every cwd mutation | D5/D6 | 2026-10-01 |
| Small debt ratchet | `cmd/gc` untagged test source | environment: 4318 calls / 202 files (historical regex census: 4348 / 200) | ga-80po0c.2.1 | untagged Small cmd/gc environment call/file totals cannot grow; reductions must lower this baseline; non-Medium lexical owners restore or eliminate every process-environment mutation | D5/D6/E6 | 2026-10-01 |
| Small debt ratchet | `cmd/gc` untagged test source | slow_process_gate: 57 calls / 24 files (historical regex census: 75 / 25) | ga-80po0c.2.1 | untagged Small cmd/gc slow-process marker totals cannot grow; reductions must lower this baseline; each non-Medium marked caller retains an explicit process-suite migration owner | D5/D6/E6 | 2026-10-01 |
| Small debt ratchet | all untagged test source | fixed_sleep: 288 calls / 111 files (historical regex census: 287 / 113) | ga-80po0c.2.1 | untagged Small fixed-sleep call/file totals cannot grow; reductions must lower this baseline; non-Medium lexical owners replace elapsed wall time with lifecycle signals | W1-W5 | 2026-10-01 |
| Small debt ratchet | all untagged test source | http_test_server: 317 calls / 66 files (historical regex census: 300 / 66) | ga-80po0c.2.2 | untagged Small HTTP test server call/file totals cannot grow; reductions must lower this baseline; non-Medium lexical owners move server-backed tests to exact Medium ownership or replace the listener | P0.4c | 2026-10-01 |
| Small debt ratchet | all untagged test source | net_listen: 92 calls / 34 files | ga-80po0c.2.2.2 | untagged Small stream-listener call/file totals cannot grow; reductions must lower this baseline; non-Medium lexical owners move stream-listener tests to exact Medium ownership or replace the listener | P0.4c-listener | 2026-10-01 |
| Small debt ratchet | all untagged test source | net_listen_config: 1 calls / 1 files | ga-80po0c.2.2.2 | untagged Small net.ListenConfig listener call/file totals cannot grow; reductions must lower this baseline; non-Medium lexical owners move ListenConfig-backed tests to exact Medium ownership or replace the listener | P0.4c-listener | 2026-10-01 |
| Small debt ratchet | all untagged test source | net_listen_packet: 3 calls / 2 files | ga-80po0c.2.2.2 | untagged Small packet-listener call/file totals cannot grow; reductions must lower this baseline; non-Medium lexical owners move packet-listener tests to exact Medium ownership or replace the listener | P0.4c-listener | 2026-10-01 |
| Small debt ratchet | all untagged test source | subprocess: 391 calls / 110 files (historical regex census: 394 / 105) | ga-80po0c.2.1 | untagged Small subprocess call/file totals cannot grow; reductions must lower this baseline; non-Medium lexical owners remove or replace each process call site | D1/D2/D5/D6/E6 | 2026-10-01 |
| Small debt ratchet | all untagged test source | syscall_listen: 1 calls / 1 files | ga-80po0c.2.2 | untagged Small syscall.Listen call/file totals cannot grow; reductions must lower this baseline; non-Medium lexical owners move syscall-backed listener tests to exact Medium ownership or replace the listener | P0.4c | 2026-10-01 |
| Small debt ratchet | all untagged test source | tmux: 0 calls / 0 files | ga-80po0c.2.2.1 | untagged Small tmux dependency call/file totals cannot grow; reductions must lower this baseline; non-Medium lexical owners replace tmux with a fake executor or declare exact isolated ownership | P0.4c-tmux | 2026-10-01 |
| Source debt ratchet | `cmd/gc` untagged test source | cwd: 285 calls / 43 files (historical regex census: 98 / 13) | ga-80po0c.2.3 | untagged cmd/gc cwd call/file totals cannot grow; reductions must lower this baseline; cmd/gc callers restore or eliminate every recognized cwd mutation | D5/D6 | 2026-10-01 |
| Source debt ratchet | `cmd/gc` untagged test source | environment: 4324 calls / 202 files (historical regex census: 3960 / 184) | ga-80po0c.2.3 | untagged cmd/gc environment call/file totals cannot grow; reductions must lower this baseline; cmd/gc callers restore or eliminate every recognized process-environment mutation | D5/D6/E6 | 2026-10-01 |
| Source debt ratchet | `cmd/gc` untagged test source | slow_process_gate: 57 calls / 24 files (historical regex census: 78 / 27) | ga-80po0c.2.3 | untagged cmd/gc slow-process marker totals cannot grow; reductions must lower this baseline; the helper definition and every marked caller retain an explicit process-suite migration owner | D5/D6/E6 | 2026-10-01 |
| Source debt ratchet | all untagged test source | fixed_sleep: 288 calls / 111 files (historical regex census: 295 / 114) | ga-80po0c.2 | untagged fixed-sleep call/file totals cannot grow; reductions must lower this baseline; each owning test replaces elapsed wall time with its lifecycle signal | W1-W5 | 2026-10-01 |
| Source debt ratchet | all untagged test source | http_test_server: 317 calls / 66 files (historical regex census: 255 / 56) | ga-80po0c.2.2 | untagged HTTP test server call/file totals cannot grow; reductions must lower this baseline; each owning test closes its loopback server and removes duplicate server-backed coverage | P0.4c | 2026-10-01 |
| Source debt ratchet | all untagged test source | net_listen: 94 calls / 35 files (historical regex census: 92 / 34) | ga-80po0c.2.2.2 | untagged stream-listener call/file totals cannot grow; reductions must lower this baseline; each owning test closes its stream listener and removes duplicate listener-backed coverage | P0.4c-listener | 2026-10-01 |
| Source debt ratchet | all untagged test source | net_listen_config: 1 calls / 1 files | ga-80po0c.2.2.2 | untagged net.ListenConfig listener call/file totals cannot grow; reductions must lower this baseline; each owning test closes its configured listener and removes duplicate listener-backed coverage | P0.4c-listener | 2026-10-01 |
| Source debt ratchet | all untagged test source | net_listen_packet: 3 calls / 2 files | ga-80po0c.2.2.2 | untagged packet-listener call/file totals cannot grow; reductions must lower this baseline; each owning test closes its packet listener and removes duplicate listener-backed coverage | P0.4c-listener | 2026-10-01 |
| Source debt ratchet | all untagged test source | subprocess: 394 calls / 112 files (historical regex census: 380 / 98) | ga-80po0c.2 | untagged subprocess call/file totals cannot grow; reductions must lower this baseline; each process-owning test removes or replaces its source call site | D1/D2/D5/D6/E6 | 2026-10-01 |
| Source debt ratchet | all untagged test source | syscall_listen: 1 calls / 1 files | ga-80po0c.2.2 | untagged syscall.Listen call/file totals cannot grow; reductions must lower this baseline; each owning test closes its listening file descriptor and removes duplicate listener-backed coverage | P0.4c | 2026-10-01 |
| Source debt ratchet | all untagged test source | tmux: 6 calls / 2 files | ga-80po0c.2.2.1 | untagged tmux dependency call/file totals cannot grow; reductions must lower this baseline; each owning test confines tmux processes and sockets to its isolated namespace and cleanup | P0.4c-tmux | 2026-10-01 |

| Reviewed hermetic body | Effective runnable size | Medium reason | Retained real composition owner |
| --- | --- | --- | --- |
| `cmd/gc` package `main` — TestDoMailInbox_RendersMessagesFromReader | medium | package TestMain mutates process state | `cmd/gc` package `main` — TestCmdMailInbox_NormalizesCanonicalManagedProviderEnvAndReadsInbox |
| `cmd/gc` package `main` — TestDoSessionWait_RegistersReadyWaitForRigDependency | medium | package TestMain mutates process state | `cmd/gc` package `main` — TestCmdSessionWait_AllowsRigDependencyBeads |
| `cmd/gc` package `main` — TestDoSessionWake_PokesManagedControllerAfterStateChange | medium | package TestMain mutates process state | `cmd/gc` package `main` — TestCmdSessionWake_PokesManagedControllerAndRequestsSuspendedStart |
| `cmd/gc` package `main` — TestPrepareWaitWakeState_ResolvesRigDependencyBeads | medium | package TestMain mutates process state | `cmd/gc` package `main` — TestCmdSessionWait_AllowsRigDependencyBeads |
<!-- END CHECKED TEST RESOURCE LEDGER -->

## Five test categories, clear boundaries

### 1. Unit tests (`*_test.go` next to the code)

Test what the CODE does. Internal behavior, edge cases, precise failure
injection. These are fast and run everywhere.

- Use `fsys.Fake` for consumer logic; use `t.TempDir()` when real filesystem
  semantics own the risk
- Use `require` for preconditions (fail immediately), `assert` for checks
- Construct exact broken states in Go — corrupt files, concurrent writes,
  duplicate IDs, missing directories
- No env vars for controlling behavior — pass dependencies directly
- Same package as the code under test (access to unexported functions)

```go
func TestFileStoreOpenCorruptedJSON(t *testing.T) {
    f := fsys.NewFake()
    f.Files["/city/.gc/beads.json"] = []byte("{not json!!!")

    _, err := beads.OpenFileStore(f, "/city/.gc/beads.json")
    require.Error(t, err)
    assert.ErrorContains(t, err, "opening file store")
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

### Cross-category runners, timing, and resource isolation

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

# Focused product-metrics testhook profile.
make test-productmetrics-testhook

# CI integration buckets, sharded.
make test-integration-shards-parallel

# Fast + process-backed cmd/gc + integration shards.
make test-local-full-parallel
```

By default, the local runners bound concurrency by both detected CPUs and
available memory, budgeting 4 GiB per job and capping automatic fan-out at 16.
If memory cannot be detected, they use three jobs. An explicit override always
wins:

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

The `productmetrics_testhook` profile is a required, path-gated CI lane with
six named owners, including the real CLI re-exec process contract. Its tagged
process owner is intentionally absent from ordinary untagged `cmd/gc` shard
enumeration. The serial `make test-cmd-gc-process` target runs the ordinary
suite and then this profile; `make test-cmd-gc-process-parallel` and
`make test-local-full-parallel` add one independent
`productmetrics-testhook` job beside the ordinary shards.
The existing macOS `mac-cmd-gc-process` matrix runs the same profile once on
shard 6 so the Darwin production composition remains covered without another
Mac runner.

#### PR static-check scope

The `preflight-static` job has two fail-safe scopes. Only an effective
`pull_request` event whose default checkout is validated as GitHub's two-parent
synthetic merge, with its first parent equal to the event's exact base SHA, may
use the changed scope. The checkout keeps the default `GITHUB_SHA` and uses
`fetch-depth: 2` so that validation is local and exact. A missing or different
base, a non-merge checkout, or an unknown event selects the full scope.

Pushes to `main`, schedules, manual dispatches, and every other non-PR event run
the full static suite. Reusable workflows inherit their caller's event; the
reusable call itself grants no changed-scope exemption. An effective
`pull_request` event may still qualify after the same synthetic-merge
validation, while an invocation such as the current RC `workflow_dispatch`
remains full. The classifier never guesses a base from `origin/main` or a
merge-base calculation.

Even a validated PR merge runs the full scope when its diff touches static
analysis or build policy:

- `go.mod`, `go.sum`, `go.work`, or `go.work.sum`
- any root `.golangci.*` configuration or `Makefile`
- `.github/workflows/**`, `.github/actions/**`, or `.githooks/**`
- `vendor/**` or `scripts/cipolicy/**`
- `scripts/ci-static-scope` and `scripts/ci-static-select`

The two scopes own different commands:

| Scope | Commands | Selection guarantee |
| --- | --- | --- |
| Changed PR | `make lint-affected`, `make fmt-check-changed` | Lint and vet every package owning a changed Go build input or embedded file, every native package that could consume a changed path, and all transitive reverse dependents; format-check only changed regular `.go` files that still exist. |
| Full/fail-safe | `make lint`, `make fmt-check`, `make vet` | Analyze and format-check the whole repository, then run standalone `go vet ./...`. |

Affected-package discovery examines every changed path. It selects packages
for changed Go-tool build inputs (`.go`, `.c`, `.cc`, `.cpp`, `.cxx`, `.m`,
`.h`, `.hh`, `.hpp`, `.hxx`, `.f`, `.F`, `.for`, `.f90`, `.s`, `.S`, `.sx`,
`.swig`, `.swigcxx`, and `.syso`) and maps changed embedded files to every
owning package using `EmbedFiles`, `TestEmbedFiles`, and `XTestEmbedFiles` from
the canonical records in one complete
`go list -mod=readonly -test -json ./...` graph.
Additions, modifications, deletions, and both sides of cross-package moves are
included. Git rename coalescing is disabled so a move cannot hide the old
package. Native compiler include and linker inputs can have recognized or
arbitrary names and may live outside their consuming package. Every changed
path therefore selects every package with native Go-tool sources, plus their
reverse dependents. This is the smallest sound scope available without trying
to duplicate compiler-specific dependency discovery. An unrelated non-build,
non-embedded path remains a no-op when the graph has no native package that
could consume it.

Reverse dependents are included because analyzers such as `govet` consume
exported facts, including through test-only imports. If the package graph
cannot be loaded completely, affected lint fails safe to `./...` instead of
trusting a partial graph. This includes a deleted required embed input. A
deleted glob member no longer appears in the current resolved embed inventory,
so a deletion that may match any current `EmbedPatterns`, `TestEmbedPatterns`,
or `XTestEmbedPatterns` entry fails safe even when a nested package still owns
the deleted build-input directory. Any other deletion beneath a package that
has neither a current embed owner nor a current direct package owner also fails
safe to full scope. These guards run before native shared-input shortcuts,
including for recognized headers. File selection is NUL-delimited. Formatting
remains limited to
changed `.go` paths, excludes deletions and symlinks, accepts only existing
regular files, and never invokes the formatter with an empty file list.

`lint-affected` is the conservative PR target. It runs the configured
golangci linters, including golangci's `govet`, then runs the Go tool's `vet`
over the exact same affected package closure. The bounded duplicate preserves
both tools' distinct diagnostics without repeating either analysis across the
whole repository. It also retains standalone-vet diagnostics in generated
files and unchanged reverse dependents. If selection fails, the same pair runs
over `./...`; fallback never disables configured linters. `lint-changed`
remains the faster local/pre-commit target and intentionally checks only
packages that contain changed Go files. Both accept `LINT_CHANGED_SCOPE` and
`LINT_CHANGED_REF`; CI uses `tracked` and the event's exact PR base SHA.

The golangci configuration enables `govet` explicitly in both scopes.
Golangci's `govet` execution is not assumed to be semantically equivalent to
standalone `go vet`: generated-file exclusions and analyzer/configuration drift
can differ. Full-scope runs therefore retain standalone `go vet ./...`, while
the changed lane invokes standalone vet on its conservative closure.

`make test-ci-policy` runs independently of changed/full static selection and
always executes the focused workflow-scope, golangci-`govet`, affected-target,
and fail-closed-classifier contracts. A self-binding test in the existing CI
policy package rejects any Makefile change that removes this focused Go suite
from the target.

#### Historical timing summaries

The opt-in timing artifacts produced by `scripts/go-test-observable` can be
aggregated offline across caller-curated successful `main` push runs:

```bash
go run ./scripts/test-timing-summary.go /path/to/downloaded-artifacts \
  >> "$GITHUB_STEP_SUMMARY"
```

Use the same strict parser to emit the versioned machine-readable history
snapshot:

```bash
go run ./scripts/test-timing-summary.go --format=json \
  /path/to/downloaded-artifacts > timing-history-v1.json
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

The JSON snapshot groups units by that same comparable profile and preserves
every successful observation with its exact artifact identity and tested SHA.
Profiles and units have canonical ordering. Observations compare the raw string
tuple `(workflow, run_id, run_attempt, job, shard_id, variant)` lexically, then
tested SHA and duration; run IDs and attempts are opaque strings, so `002`,
`10`, and `2` remain distinct and sort in that order. Identical artifact
downloads increment `duplicate_artifact_count` without duplicating samples.
Units that have only failed or skipped remain present with empty successful
observations and `null` statistics. `p75_authoritative` becomes true at five
successful samples and `p95_authoritative` at twenty. `last_success_sha` is the
SHA of the final successful observation in canonical artifact-identity order;
schema v1 has no trustworthy timestamp, so this field is deterministic but not
a claim about chronological recency.

Timing artifact schema v1 does not record the event, ref, or workflow
conclusion, so the tool
cannot prove protected-branch provenance. The caller must supply artifacts
from successful `main` push runs. The JSON snapshot is workflow-neutral input,
not a protected store or planner decision. An observed p75 with fewer than five
successful samples and p95 with fewer than twenty are diagnostic, not
planner-authoritative. The seven-day artifact retention window is not a
protected historical timing database, and this one-shot builder does not prune
observations. Renamed tests remain separate histories.

The storage-boundary mutation mode persists caller-authenticated cohorts
without merging report snapshots:

```bash
go run ./scripts/test-timing-summary.go \
  --update-history timing-history-db-v1.json \
  --run-envelope trusted-run-v1.json \
  --retain-runs 50 \
  --format=json \
  /path/to/this-run-artifacts > timing-history-v1.json
```

All three mutation flags are required together, and retention has no hidden
default. The versioned run envelope names the repository, event, ref, workflow,
run ID, run attempt, tested SHA, conclusion, and RFC3339 completion time. The
database stores each envelope and artifact once, then stores normalized
pass/fail/skip samples by artifact reference. Replaying identical artifacts is
therefore a byte-for-byte no-op; conflicting copies or envelope metadata fail
before the existing database changes. Retention removes whole oldest cohorts
by parsed completion time and recomputes snapshot statistics and the 5/20
authority thresholds from the retained evidence. Publication uses a synced
temporary sibling and atomic rename.

This command validates envelope shape and checks each timing artifact's
`workflow`, `run_id`, `run_attempt`, and `tested_sha` against it. It does not
authenticate who supplied the envelope, prove that the cohort contains every
expected shard, serialize multiple writers, publish `ci-metrics`, or make the
result planner-authoritative. Those are responsibilities of the later trusted
default-branch workflow. Until that workflow lands, use the database as
deterministic storage-boundary evidence only.

#### Local timing-plan dry runs

The local planner consumes the current runnable inventory, the canonical
schema-v1 timing snapshot above, and planner configuration without changing
the active shard topology:

```bash
go run ./scripts/test-timing-plan.go \
  --inventory runnable-inventory-v1.json \
  --history timing-history-v1.json \
  --config timing-plan-config-v1.json \
  > timing-plan-v1.json
```

Inventory and configuration are independently versioned. The minimal inventory
is `{"schema":1,"units":[{"unit_id":"package:TestName"}]}`. Configuration
schema v1 supplies one exact comparable profile, a shard count, a p95 cap, and
shared conservative fallback estimates for that suite/profile invocation. The
profile key is the complete `(job, variant, runner label, OS, architecture,
CPU count)` tuple; profiles are never merged or selected by a nearest-runner
heuristic. All three inputs reject missing or unsupported schemas, unknown
fields, trailing JSON values, and `null` where a contractual array is required.

The current inventory is the only authority for runnable membership. Every
inventory unit is assigned exactly once, and stale timing rows cannot add work.
An exact profile match contributes history. If the requested profile is absent,
the command still produces a complete static plan and records
`history_profile_status: "profile-missing"`; multiple copies of one comparable
profile are malformed and fail. Snapshot counts, identities, nullable
statistics, observations, and authority flags are validated before planning,
including rows for units no longer in the inventory.

History becomes planner-usable in two stages:

- Before five successful samples, p50, p75, and variance use the configured
  static fallback. At five samples, the empirical values become usable.
- Before twenty successful samples, p95 is
  `max(static_p95, 1.5 * selected_p75)`. At twenty samples, empirical p95
  becomes usable.

Units are sorted deterministically by descending p75, p95, variance, and p50,
then stable unit ID, and placed in the shortest p75 shard that remains within
the aggregate p95 cap. No unit is dropped: an individually oversized unit is
marked `p95-cap-exceeded`, while unavoidable aggregate overflow is marked
`shard-p95-cap-exceeded`. Equivalent shuffled inputs therefore emit identical
canonical JSON.

The output is explicitly marked `authority: "dry-run"`. This command reads only
the three named files and writes the plan to stdout. It does not read GitHub
state, authenticate protected provenance, write timing history, publish
`ci-metrics`, perform path gating or hysteresis, decide required lanes, or
activate workflow/shard execution. Those remain deferred to the trusted
control-plane workflow.

In timing artifact schema v1, `commit_sha` is the exact Git revision checked out and tested
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

Test what the USER sees. Exercise the real CLI entrypoint by re-executing the
package test binary, then assert on stdout/stderr. These are tutorial regression
tests, not production-binary integration tests.

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

exec gc bd create 'Build a Tower of Hanoi app'
stdout 'status: open'

-- $WORK/tower-of-hanoi/.git/HEAD --
ref: refs/heads/main
```

When to use: CLI output format, command success/failure, user-facing error
messages, and tutorial CLI flows.

**The env var rule:** if you need more than two env vars to set up a failure
scenario, it's a unit test, not a testscript. In testscript, omitting the
session/beads env vars now means "use the fake defaults," not "use real tmux."

### 3. Integration tests (`//go:build integration`)

Test that real pieces fit together. These may need real tmux, a real
filesystem, real agent sessions, or a real server. Integration shards currently
run behind a coarse Go/shared-path gate; broader REST coverage runs on `main`.
Use explicit profile commands for credentialed live providers until their
scheduled matrix is wired.

```go
//go:build integration

func TestRealTmuxSession(t *testing.T) {
    // actually creates and kills tmux sessions
}
```

When to use: proving real-boundary composition and testing lifecycle behavior
that exists only with real processes. Shared conformance proves fake parity.

For the broad suite, run `make test-integration-shards-parallel`. Raw `go test`
is for a focused package or test, for example:

```bash
go test -tags integration -run '^TestHumaBinary$' ./test/integration/
```

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
  or `request.failed` event appears. Subscribe to `/v0/events/stream` before
  the mutation and wait for the correlated terminal event. If the event-list
  API itself is under test, query it once after that notification.
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
REST/formula shards — no dedicated shard registration is needed. Its structured
transcript coverage verifies REST-to-SSE cursor handoff, exact replay
suppression, inclusive-tail upserts, and reset parity through the real supervisor
wire.

The opt-in browser layer runs the embedded production SPA in Chromium against
that same `test/dashport` listener. Run it with `make dashboard-e2e-play` after
installing the pinned Playwright browser, or run both layers with `make
dashboard-e2e`. `TestStructuredTranscriptBrowser` asserts the rendered DOM,
request URLs, SSE upsert/reset behavior, duplicate suppression, and a clean
console/network/error-boundary surface. It adds no second HTTP listener and is
not part of the default integration shard because browser binaries are an
explicit local/CI provisioning choice.

#### Dashboard Playwright render smoke (`internal/api/dashboardspa/web/frontend/e2e`)

Layer B is a Chromium render smoke over the **same** `testdata/dashport/` corpus,
loaded through the same importable loader (`test/dashport/corpus`) that Layer A
uses — one fixture source of truth. A small `//go:build integration` binary,
`test/dashport/cmd/fakesupervisor`, serves the seeded stack via
`api.ServeSeededCity` on a loopback listener; the Playwright `webServer` launches
it and points `baseURL` at it, so the SPA and its same-origin `/v0` + `/api`
surfaces are hosted by one handler (no CORS or base-URL override). Each spec
drives a route (Home, Runs, the seeded run detail — the regression view —,
Agents, Beads, Mail, Activity, Health) and asserts three things: the seeded
content renders, **no** React error boundary
(`components/ErrorBoundary.tsx`) is shown, and **no** client-error POST
(`/api/client-errors`) fires. It removes all vitest mocks — the built bundle runs
in a real browser against a real HTTP supervisor, so it exercises the full
fetch → generated client → projection helper → render path.

It is a **Tier 3** browser tier — it needs a built SPA bundle + Chromium, so it
is NOT in the Go integration shard set. Run it with `make dashboard-e2e-play`
(builds the SPA, builds the fakesupervisor with `-tags integration`, installs
Chromium via `npx playwright install chromium`, then runs the specs);
`make dashboard-e2e` runs both layers. In CI it runs as appended steps in the
existing **`dashboard`** job (`.github/workflows/ci.yml`), which already has Go +
Node provisioned; a `playwright-report` artifact is uploaded on failure. Add new
routes/assertions by editing `e2e/render-smoke.spec.ts`; keep
`e2e/fixtures/expected.ts` aligned **manually** with the exported constants in
`test/dashport/corpus/corpus.go` (there is no automated parity check — the two
are kept in sync by convention).

The current CI Playwright configuration retries once. That is legacy,
noncompliant debt under `ga-80po0c`; do not copy it or treat a retry-pass as
first-attempt reliability.

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
The full profile matrix is not wired into nightly CI today. Nightly runs a
separate focused Ollama Tier C subset; use the commands above for these live
profiles until scheduled coverage is added.

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

### Additional integration guidance

**Low-level** (`internal/runtime/tmux/tmux_test.go`): test raw tmux
operations (NewSession, HasSession, KillSession) directly against the
tmux library. Session names use the `gt-test-` prefix.

**End-to-end** (`test/integration/`): build the real `gc` binary and
run it against real tmux. Validates the tutorial experience: `gc init`,
`gc start`, `gc stop`, bead CRUD.

**BdStore conformance** (`test/integration/bdstore_test.go`): runs the
beads conformance suite against `BdStore` backed by a real dolt server.
Proves the full stack: dolt server → bd CLI → BdStore → beads.Store.
Its current caller skips before the suite because of the pinned `bd` version,
so it is a known gap, not a passing production-constructor proof. A local
capability skip is only convenience; required coverage needs an equipped lane
or an explicit expiring waiver.

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
| Does `Store.Get` return `ErrNotFound` for a missing ID? | Conformance |
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
`*test/conformance.go` packages. Suite availability does not prove exact
production-path coverage. Each behaviorally distinct implementation or
composition must execute the suite without a pre-run skip; thin aliases may use
a focused exact-constructor wiring proof. The table names current callers, not
proof status. The runtime ledger below is the only mechanically checked
constructor-specific inventory today.

| Interface | Conformance suite | Current suite callers |
|---|---|---|
| `beads.Store` | `internal/beads/beadstest/conformance.go` | MemStore, FileStore, exec-backed stores; BdStore caller currently skips; NativeDolt caller uses a test-only storage fixture |
| `runtime.Provider` | `internal/runtime/runtimetest/conformance.go` | See the checked runtime ledger below |
| `mail.Provider` | `internal/mail/mailtest/conformance.go` | beadmail, exec, Fake |
| `events.Provider` | `internal/events/eventstest/conformance.go` | FileRecorder, exec, Fake |
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

A proved row names one runnable test whose final top-level statement invokes
the declared shared contract with an inline factory. The source guard requires
that factory to return the row's exact constructor directly, rejects pre-run
helper gates and direct skip syntax, and permits only named testing operations
plus explicitly ledgered setup functions. E1 separately proves that
build-tagged rows execute in their required CI lane; a source-bound proof does
not claim cadence ownership by itself.

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

`runtime.NewFake`, `auto.New`, `exec.NewSeamBacked`,
`subprocess.NewSeamBackedWithDir`, and `acp.NewSeamBackedWithDir` are
source-bound to the shared runtime contract below. The auto proof runs the
exact production composition once with two fresh in-memory fakes and owns no
subprocess or listener; focused auto tests retain base-versus-ACP routing and
optional-capability coverage instead of duplicating the full suite for each
route. The seam-backed proofs are the only full exec, subprocess, and ACP
runtime contracts: duplicate raw contracts are avoided, and parent-owned
fixtures are reused while each contract case receives a fresh production
wrapper. `TestSeamBackedCapabilitiesParity` separately guards exec's
handshake-derived stream and TTY flags because the shared contract does not
assert optional capability fidelity. Focused raw provider and seam tests remain
for these packages, including legacy overlap that later consolidation may
remove case by case. The default subprocess constructor remains a separate
H5-owned gap because its reachable empty-city-path branch uses shared temporary
state. The default ACP constructor is also an H5-owned gap because it always
uses shared `os.TempDir()/gc-acp` state. E1 (`ga-80po0c.6`) owns the Large
provider/E2E manifest and required lane/cadence execution; it does not own
constructor-to-contract source binding.

<!-- BEGIN CHECKED RUNTIME PROVIDER LEDGER -->
This table is rendered from `internal/testutil/providerledger` and checked by `go test ./internal/testutil/providerledger`; edit the Go ledger, then use the expected block printed on drift.

| Provider path | Roles | Reusable type | Port | Constructor | Discovery | Contract | Status |
|---|---|---|---|---|---|---|---|
| `runtime.builtin.acp` | production_provider | — | `runtime.Provider` | `internal/runtime/acp.NewSeamBacked` | runtime.builtin/exact:acp | `runtime.Provider` | waived by ga-80po0c.3 through 2026-08-12: NewSeamBacked always uses shared os.TempDir()/gc-acp state; the WithDir proof does not exercise that composition |
| `runtime.builtin.acp` | production_provider | — | `runtime.Provider` | `internal/runtime/acp.NewSeamBackedWithDir` | runtime.builtin/exact:acp | `runtime.Provider` | proved by internal/runtime/acp/conformance_test.go#TestACPConformance |
| `runtime.builtin.exec` | production_provider | — | `runtime.Provider` | `internal/runtime/exec.NewSeamBacked` | runtime.builtin/prefix:exec: | `runtime.Provider` | proved by internal/runtime/exec/exec_test.go#TestExecConformance |
| `runtime.builtin.exec` | production_provider | — | `runtime.Provider` | `internal/runtime/t3bridge.NewSeamBacked` | runtime.builtin/prefix:exec: | `runtime.Provider` | waived by ga-80po0c.3 through 2026-08-12: the legacy gc-session-t3 prefix branch selects the T3 bridge composition, which has no full shared runtime contract |
| `runtime.builtin.fail` | production_provider, reusable_double | `internal/runtime.Fake` | `runtime.Provider` | `internal/runtime.NewFailFake` | runtime.builtin/exact:fail; reusable: internal/runtime/fake.go | `runtime.Provider` | not applicable: intentional faulting double: a successful lifecycle cannot be exercised, so the successful-provider contract is not applicable |
| `runtime.builtin.fake` | production_provider, reusable_double | `internal/runtime.Fake` | `runtime.Provider` | `internal/runtime.NewFake` | runtime.builtin/exact:fake; reusable: internal/runtime/fake.go | `runtime.Provider` | proved by internal/runtime/fake_conformance_test.go#TestFakeConformance |
| `runtime.builtin.herdr` | production_provider | — | `runtime.Provider` | `internal/runtime/herdr.New` | runtime.builtin/exact:herdr | `runtime.Provider` | waived by ga-80po0c.3 through 2026-08-12: the existing full conformance run skips in short mode or when the herdr executable is absent |
| `runtime.builtin.hybrid` | production_provider | — | `runtime.Provider` | `cmd/gc.newHybridProvider` | runtime.builtin/exact:hybrid | `runtime.Provider` | waived by ga-80po0c.3 through 2026-08-12: cmd/gc.newHybridProvider is the selected registry construction boundary; its internal tmux, K8s, and hybrid constructors are not claimed here, and the wrapper has no full shared runtime contract |
| `runtime.builtin.k8s` | production_provider | — | `runtime.Provider` | `internal/runtime/k8s.NewSeamBacked` | runtime.builtin/exact:k8s | `runtime.Provider` | waived by ga-80po0c.3 through 2026-08-12: the actual K8s production composition has no full shared runtime contract |
| `runtime.builtin.ssh` | production_provider | — | `runtime.Provider` | `internal/runtime/ssh.NewSeamBacked` | runtime.builtin/prefix:ssh: | `runtime.Provider` | waived by ga-80po0c.3 through 2026-08-12: the production SSH composition has no full shared runtime contract |
| `runtime.builtin.subprocess` | production_provider | — | `runtime.Provider` | `internal/runtime/subprocess.NewSeamBacked` | runtime.builtin/exact:subprocess | `runtime.Provider` | waived by ga-80po0c.3 through 2026-08-12: NewSeamBacked selects a distinct reachable empty-cityPath branch with shared /tmp state; the WithDir proof does not exercise that composition |
| `runtime.builtin.subprocess` | production_provider | — | `runtime.Provider` | `internal/runtime/subprocess.NewSeamBackedWithDir` | runtime.builtin/exact:subprocess | `runtime.Provider` | proved by internal/runtime/subprocess/seam_conformance_test.go#TestSubprocessSeamConformance |
| `runtime.builtin.t3bridge` | production_provider | — | `runtime.Provider` | `internal/runtime/t3bridge.NewSeamBacked` | runtime.builtin/exact:t3bridge | `runtime.Provider` | waived by ga-80po0c.3 through 2026-08-12: the production T3 bridge composition has focused tests but no full shared runtime contract |
| `runtime.builtin.tmux` | production_provider | — | `runtime.Provider` | `internal/runtime/tmux.NewSeamBackedWithConfig` | runtime.builtin/exact:tmux | `runtime.Provider` | waived by ga-80po0c.3 through 2026-08-12: the existing full conformance run skips when the tmux executable is absent |
| `runtime.composition.auto` | production_provider | — | `runtime.Provider` | `internal/runtime/auto.New` | source: cmd/gc/providers.go#resolveSessionTransportProvider — conditional transport composition is outside the runtime registry | `runtime.Provider` | proved by internal/runtime/auto/conformance_test.go#TestAutoConformance (default-route conformance; ACP route covered by focused auto routing tests) |
<!-- END CHECKED RUNTIME PROVIDER LEDGER -->

Conformance tests verify the behavioral contract (create/read/update/delete,
error handling, concurrency). They deliberately don't test lifecycle ordering
or cross-provider coordination — that's what coordination tests are for.

For the new 0.15 config surface, use
`engdocs/design/packv2/doc-conformance-matrix.md` as the release-gating ledger for
what should block CI now, what should start blocking once warning plumbing
lands, and what remains tracked but non-gating.

### Provider seam inventory

Core provider and lifecycle seams, their dependencies, and coordination test
coverage. This table is a checklist for new provider implementations; the
shared-suite callers and checked runtime ledger above are the conformance
source of truth.

| Seam | Implementations | Lifecycle deps | Coordination tested? |
|---|---|---|---|
| **Runtime** (`runtime.Provider`) | See checked runtime ledger above | None (stateless start/stop) | Via lifecycle start order test |
| **Beads** (`beads.Store`) | See shared-suite callers above; production selection includes NativeDoltStore and BdStore | ensure-ready → init → hooks | `TestLifecycleCoordination_*` |
| **Mail** (`mail.Provider`) | beadmail, exec, Fake | Depends on beads store | No — not a lifecycle seam; conformance sufficient |
| **Events** (`events.Provider`) | FileRecorder, exec, Fake | None | No — provider conformance covers record, query, and watch behavior |
| **Managed beads lifecycle** (`cmd/gc`) | `ensureBeadsProvider`, `shutdownBeadsProvider` | ensure → init, stop after agents | Covered by beads lifecycle (exec spy) |

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
| Does `gc bd create` print the right output? | Testscript |
| Does `gc start` fail gracefully without tmux? | Testscript (`GC_SESSION=fail`) |
| Does `gc rig add` fail for a missing path? | Testscript (real missing path) |
| Does FileStore reject corrupted JSON? | Unit test |
| Does FileStore roll back after a save failure? | Unit test |
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

No mock libraries. No `gomock`. No `mockgen`. Reusable test doubles are
hand-written concrete types kept beside the port they implement. Small
consumer-local stubs and function fakes may remain beside their consumer when
they are not reusable provider implementations.

### Reusable fast substitutes

| Double | Interface | Package | Strategy |
|---|---|---|---|
| `runtime.Fake` | `runtime.Provider` | `internal/runtime` | In-memory state + spy + broken mode |
| `fsys.Fake` | `fsys.FS` | `internal/fsys` | In-memory maps + spy + per-path error injection |
| `beads.MemStore` | `beads.Store` | `internal/beads` | Real logic, in-memory backing (also used by `FileStore` internally) |
| `mail.Fake` | `mail.Provider` | `internal/mail` | In-memory message state + broken mode |
| `events.Fake` | `events.Provider` | `internal/events` | In-memory event log + event-driven watchers + read/watch failure mode |

### Spy pattern

Some fakes also record calls as `[]Call` structs. Verify interactions only when
the arguments or ordering are the behavior under test; otherwise assert the
resulting state. Use a synchronized snapshot accessor when calls may still be
concurrent:

```go
sp := runtime.NewFake()
_ = sp.Start(context.Background(), "worker-a", runtime.Config{})
_ = sp.Attach("worker-a")

// Verify call sequence recorded by the fake runtime.
want := []string{"Start", "Attach"}
for i, c := range sp.SnapshotCalls() {
    if c.Method != want[i] { ... }
}
```

### Error injection strategies

Use the narrowest pattern that expresses the failure boundary:

**Per-path errors** (`fsys.Fake`) — fine-grained, fail specific operations:
```go
f := fsys.NewFake()
f.Errors["/city/rigs"] = fmt.Errorf("disk full")
```

**Modal errors** (`runtime.Fake`, `mail.Fake`) — whole-provider
unavailability. `events.NewFailFake()` fails reads and watches but still records
events because `Recorder.Record` cannot return an error:
```go
f := runtime.NewFailFake()
```

### Compile-time interface checks

An explicit compile-time assertion is useful for a provider or adapter:

```go
var _ Provider = (*Fake)(nil)
```

The conformance factory also proves interface assignability. Neither form
proves behavioral parity by itself; the shared conformance suite does.

### Fakes live next to the interface

Reusable provider fakes are exported types in the same package as their
interface. This makes them importable by cross-package unit tests (for example,
`cmd/gc` imports `runtime.NewFake()`). One-off stubs stay local to avoid growing
a global support API.

## The do*() function pattern

Many CLI commands use this split when command wiring and testable behavior need
separate owners:

- **`cmdFoo()`** — wires up real dependencies (reads cwd, loads config,
  calls `newSessionProvider()`), then calls `doFoo()`.
- **`doFoo()`** — pure logic. Accepts all dependencies as arguments.
  Returns an exit code.

Unit tests call `doFoo()` directly with fakes:
```go
mp := mail.NewFake()
_, _ = mp.Send("alice", "worker-a", "Build complete", "Ready for review")
code := doMailInbox(mp, "worker-a", &stdout, &stderr)
```

Testscript tests call `gc foo` through the real command construction path. Do
not introduce a `do*()` wrapper mechanically when a smaller injected function
or existing domain API is the clearer seam.

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
sequence (which methods are called in what order). Use a narrow coordination
port or recording collaborator instead.

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
