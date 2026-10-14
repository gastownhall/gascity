# Release gate: cron-order initial reconciliation timeout

**Verdict:** **PASS**

Evaluated 2026-09-29T04:25:19.740865+00:00. Deploy `ga-bf90h9`; build `ga-64pxsy`; review `ga-7pm2gx`.

This fresh run supersedes the prior dependency-HOLD record at `ae27f380e837071e47e5ccf44839c826297347d5`. Previous counts and unfinished jobs are not reused. The previous prefix/migration prerequisite fixes have landed. No prior PR is associated with the reviewed source.

## Coordinates

- Deploy mode: remote; base repository `gastownhall/gascity`; push remote `fork` (`quad341/gascity`).
- Reviewed source: `e64d7f827af871787036606569e880937a57936d`; builder branch is provenance only.
- Full-run base (`origin/main` at gate start): `58022309bdb11d24930ca545e7aeca8d0208b184`.
- Materialized two-parent merge: `bc81d40bab86eae80fb8d847d98f1806ed0247eb`; exact tree `f4c26ba66ea5142c0b844813fa18665a8a353013` equals `git merge-tree --write-tree` for this head/base.
- Evidence directory: `/var/tmp/ga-bf90h9-gate.Rtse6J`. The throwaway checkout and private test HOME are removed by the runner's EXIT trap; logs remain.
- `docs/PROJECT_MANIFEST.md` is absent; supplied release criteria and the repository's `TESTING.md`/Makefile define the gate.

## Criteria

| # | Result | Evidence |
|---|---|---|
| 1. Review PASS | PASS | Closed review `ga-7pm2gx` records independent PASS at the exact reviewed source. Single review is the current protocol; no patch carryover substitution. |
| 2. Acceptance criteria | PASS | The initial-reconcile wait uses the existing `hangBudget` (6 x 10 seconds = 60 seconds), consistent with controller startup waits. Only this timeout argument changes. The dynamic cron-order behavior is exercised by the named full-suite PASS below. |
| 3. Full tests/build/vet | PASS | Completed all 40 jobs of the documented full union. Build/vet exit 0. Five raw failures in two jobs are attributed to two pre-run condition trackers below; no waiver or rerun-to-green. |
| 4. Open high-severity findings | PASS | Review style/security/spec findings are clear, verdict PASS, zero unresolved HIGH findings. |
| 5. Final branch clean | PASS | Materialized merge status is empty before/after tests and policy. Source worktree clean before cutting isolated deploy branch; this record is staged and committed there. |
| 6. Clean merge with main | PASS | Canonical helper exit0 and exact tree equality at full-run coordinates; latest-main merge also materialized, built and vetted successfully (supplement below). No self-rebase required. |
| 7. Single feature theme | PASS | One-line change in existing `cmd/gc/order_dynamic_integration_test.go`, reducing false initial-reconcile timeouts. No product behavior, imports, API, CI config, census, new file or suite target changes. Scope guard accepts confirmed deploy/build/review IDs and rejects unrelated ancestry. |

## Full-scope evidence

`test_cmd_scope: full-suite`

`test_cmd`: `load-gate-run.sh --threshold 15 --max-wait 1800 -- isolated-test-run.sh -- make test-local-full-parallel`

Wrappers are absolute paths under `/home/jaword/projects/gc-management/packs/actual/all/scripts/`; exact invocation/environment are retained in `run.sh`. The union's own shard filters are documented repository behavior. No deployer-created test-name or package filter narrowed it.

Environment: `LOCAL_TEST_JOBS=4`, `GOFLAGS=-v`, `GO_TEST_TIMEOUT=30m`, disk `TMPDIR=/var/tmp`, private 17-character HOME `/var/tmp/g.czgzPS`, documented hermetic Git configuration. Rootless Podman socket and linked-beads image `dolthub/dolt-sql-server:2.2.0` were verified before launch. `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock`, `TESTCONTAINERS_RYUK_DISABLED=true`, `BEADS_ALLOW_UNREAPED_TESTCONTAINERS=1`; host sweep handles stale containers. Host bd stayed at 1.1.0; no client upgrade, Go cache purge or live-store migration.

```
LOAD_GATE_SUMMARY threshold=15 waited_seconds=120 wait_timed_out=0 load_start=18.62 load_max=40.39 load_mean=25.85 samples=86 read_errors=0
load_threshold: 15
load_waited_seconds: 120
load_wait_timed_out: 0
load_start: 18.62
load_max: 40.39
load_mean: 25.85
```

All 40 jobs completed: 38 PASS, 2 FAIL (attributed). Actual full make exit is 2; it is retained, not rewritten as a clean suite exit.

| Counting unit | PASS | FAIL | SKIP |
|---|---:|---:|---:|
| Terminal test/subtest executions | 96861 | 5 | 331 |
| Top-level terminal executions | 54017 | 3 | 236 |

Counts parse per-shard terminal results and exclude package-result lines/duplicate JSON/plain events. They include repeated executions across lanes, not unique tests. Compiler/selftest jobs have job results without invented test counts. Complete ledger: `events.json`; parser: `summarize.py`.

`diff_tests_executed: yes` — `TestControllerDiscoversAddedCronOrderWithoutRestart` PASS in `shards/integration-packages-cmd-gc-3-of-6.log:5889`. One unique changed test, one full-suite PASS, zero FAIL/SKIP. It is integration-tagged and intentionally absent from the untagged process lane.

`waiver_ref: none`

`skip_justification`: no changed test skipped. Other skips are each captured with their logged diagnostic in `skip-ledger.tsv` (331 rows), including helper entry points, platform/root/SSH-only behavior, optional provider/assets/live catalog setup, deliberate provider-error skip cases, supported-backend/capability gates, safety-refused ambient discovery and characterization/update-only tests. Raw skip premises remain alongside logs; no skip is counted as an executed PASS.

## Failure attribution

### Embedded eventkit cleanup race — ga-aik16g

`failure_attribution: TestBuildDesiredState_MinZeroDefaultScaleCheckRoutedWorkCreatesPoolSession -> ga-aik16g | clause 3: a — changed test excluded from failed lane`

`failure_attribution: TestEvaluatePoolDefaultScaleCheckIgnoresRoutedActiveUnassignedWork -> ga-aik16g | clause 3: a — changed test excluded from failed lane`

Raw failures in `cmd-gc-process-6-of-6.log:2424,11327`, 6.60s/4.95s, only during `testing.TempDir` RemoveAll cleanup. Live read-only inspection of both leftover `001/.beads/eventsData` directories found only `eventkit.lock`. Opened tracker predates this run (August 7), explicitly names both tests and the condition; sighting was appended and read back.

The only changed file is integration-tagged test code. `scripts/test-local-parallel` passes empty tags to command-process shards, so this file is excluded from their binaries. Neither failing file is modified, no production function changes, no census bump/new target/new test file. Clause 4's stronger same-package route applies to non-test-file changes; none exist. Both clauses 1/4 are clear and proof(a) lands. This is this deploy's first occurrence of this condition; previous notes have neither signature. Current build is not fix-carrying, and no repeat exception is claimed. Designated fix `ga-zq8iwb` is closed/blocked, resolved work commit `271d711c80239ffbf2ab5b287a8cceda7ef92722` not an ancestor of pinned main (exit 1); tracker remains open until landing. No diagnostic rerun or new duplicate tracker.

### Legacy proxied backup response — ga-l49tcc

`failure_attribution: TestMaintenanceOrdersOnRealBdTopologies (parent and two proxied subcases) -> ga-l49tcc | clause 3: a — fixture cannot execute changed test`

Raw failures in `integration-packages-core-2-of-4.log:329-332`: `proxied_city_and_proxied_rigs` (54.57s) and `mixed:_proxied_city_with_a_direct-server_rig` (20.17s), parent100.17s. bd1.1.0 returns unsupported-backup error-only JSON/schema_version1, while fixture recognizes `proxy.backup.unsupported`. Direct-server control PASS25.42s.

Opened tracker predates this gate; its prior ga-myuhtv exact-base reproduction is recorded as prior-run evidence, not a reproduction in this run. Current structural proof is sufficient: the `examples/gastown` fixture's fake shell gc router executes bd directly and stubs session/mail. It never executes real gc or this cmd/gc cron test. No failing-package/path overlap, new file/target/census or production changes. Clauses1/4 clear, proof(a) landed. No earlier ga-bf90h9 sighting, no repeat exception. Sighting appended/read back; raw parent and both subcase FAILs remain in counts. HQ fix `gm-2z9uot` is pending: mayor selected a test-only CLI pin, with host bd unchanged. No tool/source change was made mid-run.

Herdr actually PASSed (17.03s), `cmd-gc-process-4-of-6.log:15456`. An earlier bead/tracker inference from shard3 completion was withdrawn explicitly; this record uses the named terminal line.

## Policy and hooks

`policy_lane: PASS` — `make test-ci-policy lint-affected fmt-check-changed GOLANGCI_LINT=/var/tmp/mpr-toolchains/golangci-lint-2.12.0/gopath/bin/golangci-lint LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=58022309bdb11d24930ca545e7aeca8d0208b184`, actual exit0, `static-policy.log`.

`native_policy_lane: PASS` — `make check-gomod-replace check-eventexport-isolation check-core-boundary check-native-dependency-surface test-native-doltlite-beads check-docs`, actual exit0, `native-policy.log`.

`go build ./...` and `go vet ./...` exit0 (`build.log`, `vet.log`). Active materialization pre-commit ran pinned lint/codegen/vet/docsync. Hook ownership verified on the current gate base with `make check-hooks`. The older reviewed checkout has no such target; before this staged record commit, the canonical `scripts/check-githooks-owner.sh` read from validated base178798fd4c1fedd7d80186d975b8cb508456a9f2 was run directly and PASSed. Core hooks path is `.githooks`; the real staged commit runs its active pre-commit. No changed imports/packages need Bazel sync; no dashboard/API/schema paths require dashboard gates. `ci_lane_run: not applicable — no CI configuration change`.

## Job ledger

| Job | Actual result |
|---|---|
| `unit-core` | PASS |
| `fsys-darwin-compile` | PASS |
| `push-gate-lock-selftest` | PASS |
| `local-concurrency-selftest` | PASS |
| `cmd-gc-process-1-of-6` | PASS |
| `cmd-gc-process-2-of-6` | PASS |
| `cmd-gc-process-3-of-6` | PASS |
| `cmd-gc-process-4-of-6` | PASS |
| `cmd-gc-process-5-of-6` | PASS |
| `cmd-gc-process-6-of-6` | FAIL (attributed) |
| `productmetrics-testhook` | PASS |
| `integration-packages-core-1-of-4` | PASS |
| `integration-packages-core-2-of-4` | FAIL (attributed) |
| `integration-packages-core-3-of-4` | PASS |
| `integration-packages-core-4-of-4` | PASS |
| `integration-packages-cmd-gc-1-of-6` | PASS |
| `integration-packages-cmd-gc-2-of-6` | PASS |
| `integration-packages-cmd-gc-3-of-6` | PASS |
| `integration-packages-cmd-gc-4-of-6` | PASS |
| `integration-packages-cmd-gc-5-of-6` | PASS |
| `integration-packages-cmd-gc-6-of-6` | PASS |
| `integration-packages-runtime-tmux-1-of-3` | PASS |
| `integration-packages-runtime-tmux-2-of-3` | PASS |
| `integration-packages-runtime-tmux-3-of-3` | PASS |
| `integration-review-formulas-basic-1-of-2` | PASS |
| `integration-review-formulas-basic-2-of-2` | PASS |
| `integration-review-formulas-retries-1-of-2` | PASS |
| `integration-review-formulas-retries-2-of-2` | PASS |
| `integration-review-formulas-recovery` | PASS |
| `integration-bdstore` | PASS |
| `integration-rest-smoke-1-of-2` | PASS |
| `integration-rest-smoke-2-of-2` | PASS |
| `integration-rest-full-1-of-8` | PASS |
| `integration-rest-full-2-of-8` | PASS |
| `integration-rest-full-3-of-8` | PASS |
| `integration-rest-full-4-of-8` | PASS |
| `integration-rest-full-5-of-8` | PASS |
| `integration-rest-full-6-of-8` | PASS |
| `integration-rest-full-7-of-8` | PASS |
| `integration-rest-full-8-of-8` | PASS |

## Handoff

Cut isolated `deploy/ga-bf90h9-gate` from the resolved reviewed source, commit this record, push and open the team PR. Verify its exact head before posting success clearance in the base repository, then send and peek-verify mayor's merge-request. These future side effects are recorded separately in the bead only after verification. Merge authority is mpr via mayor; deployer does not merge. Work remains blocked until landed.

## Supplemental latest-main check

Main advanced during the full measurement: PR #6692 landed, then controller wake-up key plumbing (#6810). The pre-publication equality check stopped before any branch/push. The source remains `e64d7f827af871787036606569e880937a57936d`; the full40-job evidence above is still labelled at its original58022309bdb11d24930ca545e7aeca8d0208b184 base, not claimed as a full run on newer code.

- Supplemental build/vet base captured at check: `d813a311005573d3e2333943c6f39495ec4cf4be`.
- Canonical supplemental merge: `3b89c8da209543013885743f2e245042878f960d`; exact tree `43ff4c00da9c8c9a3f74d686b7c89125dd4bf632` equals merge-tree for latest base/source.
- Active materialization hooks, `go build ./...`, `go vet ./...`, and `go vet -tags integration ./cmd/gc` all completed successfully (actual runner exit0, `latest-master.log` ends `STAGE=complete-latest`).
- Supplemental checkout is clean and removed by EXIT cleanup. Logs: `latest-materialize.log`, `latest-build.log`, `latest-vet.log`, `latest-integration-vet.log`, `latest-final-status`.
- This is a build/vet/merge-freshness supplement, not a narrowed replacement test command or new full-suite count. No source change, new test execution, self-rebase or review substitution.

At publication main advanced once more to `178798fd4c1fedd7d80186d975b8cb508456a9f2` (failed-start telemetry, #6466). Fresh `git merge-tree --write-tree` with the unchanged reviewed source succeeds, tree `730c5708727b5d8888d6be7dbccef3fee6dd0e75`. Both measured checkouts above keep their own base labels; no full-suite result is represented as a run on this newer main. Required PR CI tests the PR merge as main advances. No rebase or source substitution.
