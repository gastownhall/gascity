**Verdict:** **PASS**

# Drained named-session demand release gate

Bead: `ga-6t11ku`. Build: `ga-j4lqwa.1`. Review: `ga-lklarp`.
Deploy mode: remote. Push remote: fork (`quad341/gascity`).
Isolated branch: `deploy/ga-6t11ku-gate`.

- Reviewed source: `9d4dc7999afb4b3a00233284320d4459d9ce6c9d` (resolved as a commit).
- Full-suite base: `863c89b0e83a90badb66c1df7240b0b38d25c0df`.
- Full-suite materialized merge: `567a1fb222599f22e45361adf609edc597458ff7`.
- Full-suite merge tree: `bf099a0db1deee842220a4c3c5d44de80d5f150d`.
- Current-main compatibility base: `6537ca1fc164a160e297c97d992a5608e43d398b`.
- Current-main materialized merge: `b36a5aae542c01c708ef8b8c6cd9f20db7faab19`.
- Current-main merge tree: `a896c64605810ab31c2d46fc29de6546cebf1a96`.

The source has no associated PR and is not reachable from current main.
Ancestry scope accepts only this deploy and its build bead; no stack,
unrelated feature commits, or `.claude/` paths are present. The builder branch
is provenance; it is not a push target. No rebase or review carryover was used.

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Closed review `ga-lklarp` records PASS for the exact resolved source. |
| 2 | Acceptance criteria met | PASS | (Pre-amendment; see *Maintainer amendment* below.) Routed and named demand wake drained on-demand sessions; broad work-query demand remains gated. Three added regressions pass in both process and integration tiers. Suspension, dependency-only and closed-state guards remain intact. |
| 3 | Full-scope tests | PASS, attributed failures | All 40 jobs completed: 36 raw passing jobs, 4 raw failed jobs; 95708 PASS / 6 FAIL / 327 SKIP test/subtest events. All six raw failures satisfy the pre-existing-failure protocol below; no diff-owned FAIL or SKIP. |
| 4 | No open high-severity findings | PASS | Reviewer PASS has no unresolved HIGH findings. |
| 5 | Clean deployment checkout | PASS | Exact reviewed source checkout was clean before adding this gate record. The record is the only deployment addition; the gate commit leaves the checkout clean. |
| 6 | Clean merge with main | PASS | Both pinned full-suite merge and latest-main merge materialize without conflicts and pass `go build ./...` and `go vet ./...`. |
| 7 | Single feature theme | PASS | (Pre-amendment.) Two source files in `cmd/gc`: the on-demand named-session guard and its regression tests. |

## Test command and environment

`test_cmd_scope: full-suite`

`test_cmd: load-gate-run.sh --threshold 15 --max-wait 1800 -- isolated-test-run.sh -- make test-local-full-parallel LOCAL_TEST_JOBS=4`

The wrappers are the canonical files under
`/home/jaword/projects/gc-management/packs/actual/all/scripts/`.
This is TESTING.md's documented full local sweep, covering fast units,
process shards, all integration package shards, real tmux shards,
review-formula basic/retry/recovery, beads, REST smoke/full, runner checks,
Darwin compile, and productmetrics test-hook coverage. The runners' internal
shard filters partition the full inventory; no custom test-name filter or
package subset was supplied.

Private on-disk HOME contained `.zshrc`; `TMPDIR=/var/tmp`, `GOFLAGS=-v`,
`GO_TEST_TIMEOUT=30m`, normal host GOPATH/GOMODCACHE, and shim-owned Go build
cache were retained. Rootless Podman socket and cached pinned runtime images
were verified before starting. `TESTCONTAINERS_RYUK_DISABLED=true` and
`BEADS_ALLOW_UNREAPED_TESTCONTAINERS=1` were paired. The isolation tripwire
did not fire. Actual wrapper exit: 2, reflecting the six raw failures.

`test_counts: 95708 PASS, 6 FAIL, 327 SKIP`

These are emitted test/subtest events across repeated execution tiers,
including cached results, not distinct test names. Root events alone:
53478 PASS / 6 FAIL /
232 SKIP. Four runner/compile/test-hook jobs emit no Go test-event tally.

`diff_tests_executed: (pre-amendment) all three added regressions PASS in both tiers; zero FAIL/SKIP`

`waiver_ref: none (gascity has no waiver path)`

`ci_lane_run: n/a (no CI configuration change)`

Pre-amendment; see *Maintainer amendment* below.

| Diff-owned test | Full-suite shard | Result |
|----------------|------------------|--------|
| `TestNamedOnDemand_NamedDemandWakesDrainedSession` | `cmd-gc-process-1-of-6` | PASS |
| `TestNamedOnDemand_WorkQueryDoesNotWakeDrainedSession` | `cmd-gc-process-2-of-6` | PASS |
| `TestNamedOnDemand_RoutedDemandWakesDrainedSession` | `cmd-gc-process-6-of-6` | PASS |
| `TestNamedOnDemand_RoutedDemandWakesDrainedSession` | `integration-packages-cmd-gc-3-of-6` | PASS |
| `TestNamedOnDemand_NamedDemandWakesDrainedSession` | `integration-packages-cmd-gc-4-of-6` | PASS |
| `TestNamedOnDemand_WorkQueryDoesNotWakeDrainedSession` | `integration-packages-cmd-gc-5-of-6` | PASS |

## Raw failures and attribution

| Raw failing test | Full-suite shard | Existing condition record |
|------------------|------------------|---------------------------|
| `TestGastown_MailArchive` | `integration-rest-full-1-of-8` | `ga-4w6d2r` |
| `TestGastown_PipelineMailChain` | `integration-rest-full-1-of-8` | `ga-4w6d2r` |
| `TestMail_BashAgent` | `integration-rest-full-2-of-8` | `ga-4w6d2r` |
| `TestGastown_MailRoundTrip` | `integration-rest-full-6-of-8` | `ga-4w6d2r` |
| `TestGastown_PipelineMailAndWork` | `integration-rest-full-6-of-8` | `ga-4w6d2r` |
| `TestGastown_PipelineConvoyTracking` | `integration-rest-full-8-of-8` | `ga-4w6d2r` |

Every row has `failure_attribution: <test> -> ga-4w6d2r | clause 3: a (MECHANISM)`.
The record was created before this run and opened in full, including its
independent untouched-main reproduction and prefix-only repair proof. This
run rejects valid message `ih-16` and convoy `ge-19`; the work wait prints
unprocessed `qj-5`. The remaining failures wait for replies from fixtures
that filter inbox IDs with `^gc-`. `extractBeadID` still accepts only
`bd/gc/mc` prefixes. The BashAgent failure's removed `gc bead` diagnostic
command is a secondary dump, not the reason no reply arrived.

All four clauses are satisfied:

1. Failing test files, helpers, scripts and prefix derivation are untouched.
2. Open `ga-4w6d2r` predates the run, names all six tests, and now carries
   this run's verified sighting. Its unlanded fixture fix is `ga-c2atlu`;
   the landing guard stays open until that fix reaches main.
3. Concrete mechanism: BashAgent's `writeAgentsToml` and the other five
   tests' `renderGasTownToml` generate named sessions with `mode="always"`.
   `compute_awake_bridge` copies that mode unchanged. Although these tests
   run the CLI, they cannot enter the changed switch's `on_demand` branch.
   The changed guard cannot affect their prefix parsing or reply scripts.
   This is proof (a), not an import-only assertion or an unmeasured coverage
   claim. No isolated green rerun is used to erase the failures.
4. No production change is in the failing `test/integration` package.
   No census bump, new suite target, or new test file was added; the three
   regressions are pure tests in an existing file.

This is the bead's first full-gate hit on the prefix signature. Its earlier
initialization deferral was a different condition, now fixed on main.
No fix-carrying exception, INCONCLUSIVE escape, waiver or new tracker is used.

## Skips and load

`skip_justification:` The 327 unrelated skip events cover unit-to-process
lane routing (the process lanes also ran), platform/root/permission and
child-subreaper requirements, helper-only entries, optional provider/usage
conformance, Postgres and live catalog/MCP/Kubernetes/herdr profiles, absent
archived tooling/prompt fixtures, goldens-update and fixed-bug characterization
cases, opt-in real-tmux bindings/cleanup probes, optional upstream bd revision
and persistence profiles, and the existing CityUnregisterAsync timeout skip.
Those profiles do not exercise this pure named-session demand guard. Container
runtime absence was not accepted as evidence for this diff. All three added
regressions ran and passed twice. Per-name skip contexts remain in the audit
JSON and shard logs below.

- `load_threshold: 15`
- `load_waited_seconds: 660`
- `load_wait_timed_out: 0`
- `load_start: 33.98`
- `load_max: 33.98`
- `load_mean: 18.33`
- `load_samples: 98`
- `load_read_errors: 0`

All values come from this run's final `LOAD_GATE_SUMMARY`.

## Required policy and compatibility checks

`policy_lane: PASS`

- `make test-ci-policy`: exit 0.
- `make lint-affected fmt-check-changed` with tracked scope against the
  full-suite base and pinned `golangci-lint 2.12.0`: exit 0, zero issues,
  standalone affected-package vet and formatting clean.
- `make check-gomod-replace check-eventexport-isolation check-core-boundary check-native-dependency-surface test-native-doltlite-beads`: exit 0.
- Both materialized merges: `go build ./...`, `go vet ./...`: exit 0 each.
- `make check-hooks`: `.githooks` active; the staged gate commit uses the
  repository pre-commit hook.

The host default linter is 2.13.2, not the repository's 2.12.0 pin. Its seven
identical candidate/base diagnostics were retained as noncanonical results;
they were not granted policy attribution. The version-verified 2.12.0 tool
at `/var/tmp/mpr-toolchains/golangci-lint-2.12.0/gopath/bin/golangci-lint`
passed the canonical lane. Private TMPDIR/lint cache prevented runner lock
contention; the shared Go cache was not cleared or replaced.

Main advanced during the sweep by one unrelated two-file mail-archive fix.
The full-suite evidence remains pinned to its original base; the newer
materialized merge was separately built and vetted, with no source conflict
or change to this diff's guard/tests or the failing prefix fixtures. No claim
is made that the full sweep executed on the newer base.

There is no API/dashboard, generated schema, import, package or CI-config
change, so dashboard CI, Bazel sync and a new CI lane do not apply. (The
PR's `BUILD files in sync` CI check nevertheless ran and passed.)
`docs/PROJECT_MANIFEST.md` is absent in this checkout; the supplied release
criteria and current TESTING.md/Makefile define the gate.

## Retained evidence

- Master log: `/var/tmp/ga-6t11ku-full-r4.log`.
- Forty job logs: `/var/tmp/ga-6t11ku-full-shards-r4/`.
- Counts, owned results, failure and skip contexts:
  `/var/tmp/ga-6t11ku-full-events-r4.json`.
- Build/vet: `/var/tmp/ga-6t11ku-build-r2.log`,
  `/var/tmp/ga-6t11ku-vet-r2.log`, `/var/tmp/ga-6t11ku-latest-build.log`,
  `/var/tmp/ga-6t11ku-latest-vet.log`.
- Policy/static: `/var/tmp/ga-6t11ku-policy-r2.log`,
  `/var/tmp/ga-6t11ku-static-pinned-r2.log`,
  `/var/tmp/ga-6t11ku-native-policy.log`.

Failed preparation attempts (missing log directory and an aborted private
module-tree compilation) produced no test evidence. They remain disclosed
in the bead's notes and are excluded from the counts above.

## Maintainer amendment

During PR review the drained exemption was narrowed to `routed-demand` only.
`named-demand` stays gated by `Drained`: `NamedSessionDemand`
(`namedWorkReady`) does not filter blocked `in_progress` work, so exempting it
would re-wake sessions that drain-acked on blocked work, the wake/drain loop
the `reset-pending` guard exists to prevent. Ready assignee-direct work
already wakes a drained bead through the `assigned-work` pass, which filters
blocked work via `workBeadHasAwakeDemand`.
`TestNamedOnDemand_NamedDemandWakesDrainedSession` was replaced by
`TestNamedOnDemand_DrainedSessionWithReadyAssignedWorkWakes` and
`TestNamedOnDemand_NamedDemandDoesNotWakeDrainedSessionWithBlockedWork`. The
counts and shard table above describe the original, pre-amendment diff. The
targeted tests (`go test ./cmd/gc/ -run
'TestNamedOnDemand|TestComputeAwake|Drain|ResetPending'`), `go build ./...`,
and `go vet ./cmd/gc/` were re-run on the amended diff and passed.

A second amendment closed the combined case: when a drained holder has both
`NamedSessionDemand` (from blocked `in_progress` work) and
`NamedSessionRoutedDemand`, `named-demand` won the reason switch and the
`Drained` gate discarded the routed signal. The reason is now promoted to
`routed-demand` for drained beads in that case; precedence for non-drained
beads is unchanged. Added
`TestNamedOnDemand_RoutedDemandWakesDrainedSessionDespiteBlockedNamedDemand`.
Re-run on the amended diff and passed: `go build ./...`, `go vet ./cmd/gc/`,
`go test ./cmd/gc/ -run 'TestNamedOnDemand|TestComputeAwake|Drain|ResetPending' -count=1`.
