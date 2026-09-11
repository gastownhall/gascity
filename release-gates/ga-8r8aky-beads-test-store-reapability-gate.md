# Release gate: reapable `beads_test` stores for gc tests

- Deploy bead: `ga-8r8aky`
- Source bead: `ga-szv0ge`
- Reviewed commit: `2ec8017333795d1420586d9f2159a05bd2a4f760`
- Source branch: `builder/ga-szv0ge` (provenance only)
- Base: `origin/main@48d68d13a77647eedeeabe772f362a32954a53b2`
- Planned deploy branch: `deploy/ga-8r8aky-gate`
- Deploy mode: remote; push target: `fork`
- Evaluated: 2026-09-11
- Verdict: **PASS with attributed, non-diff-owned raw test failures**

The target pre-flight ran before criterion 6. Pull request
[`#6213`](https://github.com/gastownhall/gascity/pull/6213), authored by the
internal account `quad341`, remains open and unmerged at the exact reviewed
commit. Its only commenter is the recognized Blacksmith GitHub App; REST
identifies the actor as `blacksmith-sh[bot]` with type `Bot` and app slug
`blacksmith-sh`.

## Gate checklist

| # | Criterion | Result | Evidence |
| --- | --- | --- | --- |
| 1 | Review PASS present | **PASS** | Source bead `ga-szv0ge` records the round-2 reviewer verdict **PASS** at the exact resolved commit `2ec8017333795d1420586d9f2159a05bd2a4f760`. No review carryover is involved. |
| 2 | Acceptance criteria met | **PASS** | `TestInitBeadsForDirWithExecutorTreatsAlreadyInitializedRecoveryAsSuccess` now opens its real test store as `beads_test_gsp`; `defaultStaleDatabasePrefixes` recognizes the gc-owned `beads_test*` marker; the new planner test proves `beads_test_gsp` is droppable while `gsp`, `beads_team`, `beads_tenant`, and `beads_tmp_prod` remain untouched; and the mirror golden documents the deliberate gc-only prefix. Current main also supplies the complementary test-side `cleanupManagedDoltTestCity` teardown inherited by the reviewed head. All three diff-owned tests passed twice in the full-suite output. |
| 3 | Tests pass | **PASS with attribution** | The documented full-scope 40-job command completed **35 PASS / 5 raw FAIL / 0 SKIP jobs**. All six diff-owned test executions reported real PASS events, with zero diff-owned FAIL or SKIP. The five raw-fail jobs contain six top-level failing tests, all attributed under criterion 3a. `test_cmd_scope: full-suite`; `waiver_ref: none`. |
| 3a | Pre-existing failures attributable | **PASS** | Every failure is outside the three-file candidate diff, has a predating open tracker covering the exact condition, has no failing-path overlap, and has a landed mechanism proof. The Docker cleanup assertion is the known async-cleanup race `ga-d5l7kb`; the city suspend/resume timeout is the proven named-session retirement defect `ga-dc9utn`; and four fixture bootstrap failures are the shared-server `bd` schema-migration condition `ga-esyijp`. Current sightings were appended to and read back from all three trackers. |
| 3b | Policy and static lanes | **PASS** | `go build ./...`, `go vet ./...`, `make test-ci-policy`, `make fmt-check-changed`, and `git diff --check origin/main...HEAD` all exited 0. The repository hook path is `.githooks`, and the required pre-push fast suite completed before the release run. `policy_lane: make test-ci-policy — PASS`. |
| 3c | CI-config lane | **PASS — n/a** | `ci_lane_run: n/a (no CI job, workflow, matrix, timeout, required-check list, Makefile, runner script, or test-resource census changed)`. |
| 4 | No high-severity review findings open | **PASS** | Unresolved HIGH findings: 0. The reviewer recorded PASS, and the mayor's 2026-09-09 architectural ruling for PR `#6213` explicitly found no hard hold. |
| 5 | Final branch is clean | **PASS** | `git status --porcelain=v1` was empty after the complete test and static runs and before this checklist was created. |
| 6 | Branch diverges cleanly from main | **PASS** | After a fresh fetch, `origin/main` remains `48d68d13a77647eedeeabe772f362a32954a53b2`. The reviewed head is 0 commits behind and 2 ahead. `git merge-tree --write-tree origin/main 2ec8017333795d1420586d9f2159a05bd2a4f760` exited 0 and produced tree `628ac8f360a9e97f0c042edcaa52d2e56d57d6ef`. No bounded self-rebase was needed. |
| 7 | Single feature theme | **PASS** | Two commits and three files implement one test-infrastructure theme in `cmd/gc`: give real test-created Dolt stores an explicit disposable name and teach the existing cleanup planner to recognize it. No independent behavior is bundled. |

## Source and scope evidence

The recorded SHA resolved to the full commit used by review and this gate:

```text
git rev-parse --verify --quiet '2ec8017333795d1420586d9f2159a05bd2a4f760^{commit}'
2ec8017333795d1420586d9f2159a05bd2a4f760
```

The reviewed range contains two source-tagged commits:

```text
f4ecac4cd6f7b04b6f1349d8558694735523afcb fix(dolt): make leaked test store reapable via beads_test prefix (ga-szv0ge)
2ec8017333795d1420586d9f2159a05bd2a4f760 test(dolt): cover the beads_test override name and fix stale golden list (ga-szv0ge)
```

Net diff: 3 files, 42 insertions, 3 deletions. Paths are confined to
`cmd/gc/beads_provider_lifecycle_test.go`,
`cmd/gc/dolt_cleanup_drop_planner.go`, and
`cmd/gc/dolt_cleanup_drop_planner_test.go`. There are no `.claude/**`, API,
dashboard, workflow, configuration-schema, or unrelated package paths.

## Full-suite test evidence

The rootless Podman socket was active before the run. Cached images included
the repository-used Dolt server versions and Testcontainers Ryuk; Ryuk remained
disabled as required by the host's container-sweep contract.

```text
DOCKER_HOST=unix:///run/user/1000/podman/podman.sock \
TESTCONTAINERS_RYUK_DISABLED=true \
GO_TEST_TIMEOUT=30m \
LOCAL_TEST_JOBS=4 \
GOFLAGS=-v \
make test-local-full-parallel
```

- `test_cmd_scope: full-suite`
- `test_counts: 35 PASS jobs / 5 raw FAIL jobs / 0 SKIP jobs (40 total)`
- Top-level Go test events: 49,910 PASS / 6 FAIL / 210 SKIP.
- All Go test and subtest events: 88,267 PASS / 7 FAIL / 300 SKIP. The extra failure event is the parent `TestDockerSessionProtocol` plus its one failing subtest.
- Full command log: `/var/tmp/ga-8r8aky-full-suite.log`
- Per-job logs: `/var/tmp/gc-local-tests.yYlaqn`
- `skip_justification`: the suite's expected platform, privilege, live-provider, opt-in integration, helper-process, and shard-control skips. No diff-owned test skipped, and all 40 required jobs ran to completion.
- `waiver_ref: none`

`diff_tests_executed` — **6 PASS / 0 FAIL / 0 SKIP** across the unfiltered
`cmd/gc` process and integration-package shards:

- `TestInitBeadsForDirWithExecutorTreatsAlreadyInitializedRecoveryAsSuccess` — PASS twice
- `TestPlanDoltDrops_RecognizesGCSideTestScopeOverrideMarker` — PASS twice
- `TestDefaultStaleDatabasePrefixes_MirrorsBeadsCleanDatabases` — PASS twice

## Failure attribution

| Raw result | Test | Tracker and proof |
| --- | --- | --- |
| **FAIL — ATTRIBUTED** | `TestDockerSessionProtocol/context_cancellation_rolls_back_created_container` | `ga-d5l7kb`, open since 2026-07-28. Clause 3(a), mechanism: the `scripts` test executes `scripts/gc-session-docker` and imports `internal/runtime` plus `internal/testutil`; it cannot import or execute the changed `cmd/gc` main-package planner. The observed empty immutable-ID cleanup list after stop/name removal is the tracker's exact async-cleanup race. No path overlap. |
| **FAIL — ATTRIBUTED** | `TestE2E_SuspendResume_City` | `ga-dc9utn`, open before this run with a proven root cause. Clause 3(a), mechanism: the canonical 94.02-second missing `citysus.report` result comes from configured-session retirement in `buildDesiredStateWithSessionBeads` / `session_reconciler`. This candidate touches neither path and changes only Dolt stale-name classification. No path overlap. |
| **FAIL — ATTRIBUTED** | `TestHumaBinary_SessionMessageAsync` | `ga-esyijp`, open since 2026-08-29. Clause 3(a), mechanism: external `bd init` refused shared-server migration `v59 -> v66` for database `hq` before the session-message assertion. The only production change recognizes names beginning `beads_test`, which cannot match `hq`, and changes no bootstrap or migration code. No path overlap. |
| **FAIL — ATTRIBUTED** | `TestCleanInstallTutorialPath` | `ga-esyijp`. Clause 3(a), same mechanism: external `bd init` refused shared-server migration `v48 -> v66` for `hq` before tutorial assertions. The candidate cannot match or mutate `hq`. No path overlap. |
| **FAIL — ATTRIBUTED** | `TestGraphWorkflowFailureRunsCleanup` | `ga-esyijp`. Clause 3(a), same mechanism: external `bd init` refused shared-server migration `v50 -> v66` for `hq` before graph-workflow assertions. The candidate cannot match or mutate `hq`. No path overlap. |
| **FAIL — ATTRIBUTED** | `TestGCLiveContract_BeadsAndEvents` | `ga-esyijp`. Clause 3(a), same mechanism: external `bd init` refused shared-server migration `v50 -> v66` for `hq` before live-contract assertions. The candidate cannot match or mutate `hq`. No path overlap. |

`inconclusive-guard: n/a — a clause-3 mechanism proof landed for every raw
failure, and the candidate changes neither the test-resource census nor any
test target.`

## Disposition

All applicable release criteria pass. Commit this checklist as the sole
deploy-only addition on the isolated `deploy/ga-8r8aky-gate` branch, push the
exact gated head, open an internal pull request, publish
`release-gate/deploy-clearance=success` on that exact PR head, and route merge
authority to the mayor. The deployer does not merge.
