**Verdict:** **PASS**

# Release gate: proxy readiness starts at helper spawn (ga-igb4lp)

- Reviewed deploy source: `290d82b0c2855eefb68c271acbd7a3a9bae2ca11` (build ga-fa1jan, review ga-0uxqkt).
- Full-suite base: `origin/main@9f9c2e30133c9cfa375195cabbebe70c2930dea9`; clean merge tree `b1923576b869014d53e189ad99532bab2cf95af3` materialized as `8547758ad75f01d56a25ff9d25ad0574084c33a9`.
- Later base: `origin/main@d1aa1cd5237d451cdacadce9a74bf8c6ddaa4586`; clean merge tree `ef30c13f7f530c617272e174d23e1724a534ea7b` materialized as `22a53e77ce458cf47e6c28bc0d8d23a96aac3400` for build, vet, and acceptance.
- Mode: remote; no waiver. Isolated branch: `deploy/ga-igb4lp-gate`.

## Checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 6 | Clean divergence from main | PASS | Reviewed SHA is not on main and has no PR. `git merge-tree --write-tree` succeeded against both named base commits, producing the trees above. The deploy ancestry scope check accepted ga-igb4lp and source bead ga-fa1jan, with no stack or denylisted path. |
| 1 | Review PASS present | PASS | Review bead ga-0uxqkt records PASS at the exact deploy SHA; no review carryover. |
| 2 | Acceptance criteria met | PASS | `proxyProcessInstance.start` now sets `readyBy` immediately after `cmd.Start`, rather than using the tick time captured before the orphan sweep. Production keeps five seconds. The new deterministic regression test kills a helper, observes its exit, supplies an expired tick time, and verifies the replacement is ready. It passed in the full suite; reviewer verified RED on the old anchor and starvation runs on base and head. The one-minute test bound is set once in `init`; this package has no parallel tests. |
| 3 | Tests pass | PASS with three attributed failures | The documented full-scope 40-job `make test-local-full-parallel` ran with the CI-pinned bd and completed 38 PASS / 2 FAIL jobs, 54,291 PASS / 3 FAIL / 236 SKIP top-level test results. The only diff-owned test passed. The three failures are existing `cmd/gc` TempDir cleanup races tracked by ga-vkhfnj, with independent coverage proof below. `test_cmd_scope: full-suite`. Tier A acceptance passed 145 PASS / 0 FAIL / 11 SKIP. Required topology and native acceptance evidence below. |
| 3a | Pre-existing failure attribution | PASS | All three failing tests are in unchanged `cmd/gc` test files, tracked by pre-existing ga-vkhfnj; the sighting is comment `dda55c29-bb72-5184-843d-38b1344f0185`. A focused rerun passed all three and its coverage profile shows changed `internal/workspacesvc/proxy_process.go:start` at 0.0%. No diff path overlaps. First exact-signature sighting; no repeat hold. |
| 3b | Required policy and lint lanes | PASS | `make test-ci-policy`, the native dependency and DoltLite checks, repository static guards, pinned `make lint-affected fmt-check-changed`, `go build ./...`, `go vet ./...`, `git diff --check`, and `make check-hooks` passed. |
| 3c | CI config own lane | PASS (not applicable) | No CI workflow, matrix, timeout, or required-check list changed. `ci_lane_run: n/a`. |
| 3d | Heavy-package composite | PASS (not applicable) | `heavy-composite-gate.sh classify` returned `mode=none`; this diff does not own a listed heavy package. |
| 4 | No high-severity review findings open | PASS | Reviewer PASS records one optional style nit and no unresolved HIGH finding. |
| 5 | Final branch clean | PASS | The role worktree was clean before the isolated branch was cut. This gate record is the only deployer change; `git status --short` is empty after its commit on `deploy/ga-igb4lp-gate`. |
| 7 | Single feature theme | PASS | The three changed files are one `internal/workspacesvc` readiness fix and its test/Bazel manifest (+117/-2). Source commits all cite build bead ga-fa1jan. |

## Criterion 3 evidence

```text
test_cmd: LOCAL_TEST_JOBS=4 make test-local-full-parallel
wrapper: load-gate-run.sh --threshold 15 --max-wait 1800 -- isolated-test-run.sh -- make test-local-full-parallel
test_cmd_scope: full-suite
test_counts: 38 PASS / 2 FAIL jobs; 54,291 PASS / 3 FAIL / 236 SKIP top-level Go tests
diff_tests_executed: TestProxyProcessTickRestartReadyWindowStartsAtSpawn PASS (0.33s), 0 FAIL, 0 SKIP
waiver_ref: none
heavy_mode: none
ci_lane_run: n/a
load_threshold: 15
load_waited_seconds: 540
load_wait_timed_out: 0
load_start: 24.34
load_max: 36.21
load_mean: 23.36
evidence: /var/tmp/deploy-ga-igb4lp-pinned.HrmJoB/full-suite.log and shards/
```

The full suite used bd v1.3.1-rc.2 at `696e3967b`, matching `BD_CURRENT_REF` and the linked beads library; its host Dolt was 2.3.3. The shell and nested Go processes were probed before the valid run. The later acceptance lanes used the CI-pinned Dolt 2.1.7. Two earlier aborted launches had an incorrect bd PATH or lost pinned-bd variables through `env -i`; their partial output is excluded from this verdict. The isolation wrapper reported pass-through for Gas City, and the Makefile's test command supplied the hermetic `env -i` for each suite shard. `skip_justification:` the 236 skips are platform, helper, opt-in, or test-precondition exclusions; none belongs to the new test. Most named skips execute in another shard; all skip reasons are in the shard logs.

`failure_attribution: TestEvaluatePoolDefaultScaleCheckCountsRoutedReadyWork, TestBuildDesiredState_MinZeroDefaultScaleCheckRoutedWorkCreatesPoolSession, TestEvaluatePoolDefaultScaleCheckIgnoresRoutedActiveUnassignedWork -> ga-vkhfnj, clause 3(c) coverage proof.` Each failed on `testing.TempDir` removal with a directory remaining under the test scratch root. The diff changes no `cmd/gc` file. Focused reruns of all three passed; a `-coverpkg=github.com/gastownhall/gascity/internal/workspacesvc` profile from that run reports the changed proxy `start` function at 0.0% coverage and `waitReady` at 0.0%. Thus those tests did not execute the changed production path. No diff path overlaps the failing test files. ga-vkhfnj predates this run and covers whole-suite host contention. The sighting was appended and read back. Evidence: `cmdgc-cleanup-cover.log` and `cmdgc-cleanup-cover.out` in the evidence directory. This is a first hit of these exact signatures, so the repeat-condition hold is not triggered. The raw FAIL results remain visible.

Required supporting lanes:

- `make test-acceptance` on the later merge tree: PASS, 145 top-level PASS / 0 FAIL / 11 SKIP; `TestBeadsProxiedDefault`, `TestProxiedNativeSafety`, and `TestProxiedNativeLifecycle` executed and passed. The skips include the topology matrix (separate opt-in lane), legacy migration without a pre-journal GC binary, self-host UX placeholders, and live pack-registry import. Load summary: threshold 15, waited 0 seconds, no timeout, start 13.62, max 20.52, mean 15.37. Log: `acceptance-a.log`.
- Beads topology acceptance: `TestBeadsProxiedDefault` PASS in Tier A (523.44s), `TestBeadsProxiedIgnoresUserLevelSharedServer` PASS (73.66s), and `TestBeadsInitTopologyMatrix/M1-proxied-local` PASS with its nine child checks (178.14s). The two migration tests and M5 legacy case SKIP because no pre-journal GC binary is configured, the CI job's documented exception. `GC_REQUIRE_ACCEPTANCE_TOOLING=1` and `GC_ACCEPTANCE_TOPOLOGY_MATRIX=1` were set on the separate lane; `bd` v1.3.1-rc.2 and Dolt 2.1.7 were verified. Logs: `topology-shared-server.log`, `topology-migration.log`, `topology-matrix.log`.
- Proxied-native acceptance: the Tier A `make test-acceptance` invocation ran the CI job's exact `TestProxiedNativeSafety` and `TestProxiedNativeLifecycle` tests with bd v1.3.1-rc.2 and Dolt 2.1.7; both PASS and neither has a skipped child. Required tooling was independently verified (`bd init --help` includes `--proxied-server`). This is a local invocation of the same coverage. A redundant selected rerun was stopped before executing a test while waiting on host load; it is excluded from the verdict.
- `policy_lane: make test-ci-policy` PASS. `make check-gomod-replace check-eventexport-isolation check-core-boundary check-docs`: PASS. `make check-native-dependency-surface test-native-doltlite-beads`: PASS. Pinned `make lint-affected fmt-check-changed`: PASS, 0 issues. Build, vet, whitespace, and hooks: PASS.

The first separate topology attempt used `GIT_CONFIG_GLOBAL=/dev/null`, unlike the repo's `scripts/test-gitconfig-path` that supplies `beads.role=maintainer`. Its `gc doctor` failure was solely `beads-role` missing; that attempt was invalidated. The corrected rerun used the generated hermetic Git config and passed. The invalid log is preserved as `topology-shared-server-invalid-gitconfig.log`.

## Base movement

Main advanced during the full suite from `9f9c2e30133c9cfa375195cabbebe70c2930dea9` to `d1aa1cd5237d451cdacadce9a74bf8c6ddaa4586`. The intervening commits change API/session reset and test HOME handling, with no overlap in `internal/workspacesvc`. The later merge is clean; build, vet, and Tier A acceptance passed on its materialized tree. Recheck the remote base and final branch before publication.
