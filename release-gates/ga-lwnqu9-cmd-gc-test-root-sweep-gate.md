# Release gate: sentinel-aware cmd/gc test-root sweeps (`ga-lwnqu9`)

- Deploy bead: `ga-lwnqu9`
- Build/source bead: `ga-lygcyb`
- Review bead: `ga-nphjb2`
- Reviewed pre-rebase commit: `921901ec08af2a9153b715300c3484f8aa4014a1`
- Gated source: `b8c52bc07e431a34667063862d34a4a2f15e8566`
- Source branch (provenance only): `builder/ga-lwnqu9-gate-rebase`
- Base evaluated: `origin/main@71c94672e09ed57810d227831996edb678192bcd`
- Merge base: `71c94672e09ed57810d227831996edb678192bcd`
- Merge-tree result: `2b6ed8d2cd83fd7c41e00a201eaac38f6d4d774f`
- Deploy mode: remote
- Evaluation date: 2026-09-14
- Verdict: **PASS WITH ATTRIBUTED RAW FAILURES**

`docs/PROJECT_MANIFEST.md` and a matching `work-packages/` file are absent, so
this checklist applies the active deployer protocol, the source bead's done-when
list, and the CI-coverage convention in
`engdocs/contributors/release-gate-criteria-conventions.md`.

## Gate checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-nphjb2` records an independent PASS and two delta confirmations for the same content through `921901ec08af2a9153b715300c3484f8aa4014a1`. After PR #6350 merged, the mandated bounded helper rebased that source without conflict onto current main and lease-protected-pushed `b8c52bc07e431a34667063862d34a4a2f15e8566`. The aggregate stable patch-id before and after is identical: `c4da422d5b388441e716e08904a91437ceff6d4a`. |
| 2 | Acceptance criteria met | **PASS** | Both sweep implementations probe the sentinel before their state-specific age fences: held is preserved, unlocked/free uses two minutes, and absent/legacy retains one hour plus liveness/marker checks. `TestMain` captures the host temp root before overriding `TMPDIR`, then sweeps all five legacy prefixes from that captured root. The four new regressions and their surrounding packages pass. Burst simulation for the PR body: `TestSweepLegacyCmdGCFixtureDirsSweepsAllFivePrefixesUnderGivenRoot` plants **N=5** aged roots (one for each legacy prefix); one `sweepLegacyCmdGCFixtureDirs(root)` invocation reaps all five. |
| 3 | Tests pass | **PASS WITH ATTRIBUTION** | The required `cmd/gc` process coverage and the broader documented local CI union ran through `make test-local-full-parallel`: all 40 jobs completed, 38 PASS and 2 raw FAIL. All six `cmd/gc` process shards, all six `cmd/gc` integration-package shards, `unit-core`, all three runtime/tmux shards, all core integration shards, and all REST jobs except the single attributed shard passed. Both raw failures are the pre-existing concurrent-schema-initializer condition tracked by `ga-e2z1zb`; no diff-owned test failed or skipped. Focused `cmd/gc`, resource-census, and `tmuxtest` runs also passed. |
| 3a | Pre-existing failures attributable | **PASS** | `TestAdoptPRFormulaRetriesTransientReviewerStep` (`v13 -> v66`) and `TestGCLiveContract_BeadsAndEvents` (`v42 -> v66`) both failed during fixture `gc init` when external `bd` refused a mid-migration shared-server database, before their formula/API scenario could execute. Root-cause bead `ga-e2z1zb` predates this run and documents this exact random-cursor race; both sightings were appended and read back. The candidate changes only test harness/sweep behavior and census/docs, not `gc-beads-bd.sh`, schema classification, initializer coordination, or Beads migrations. Neither failing test file nor package is changed. Clause 3(a) mechanism and clause 4 no-overlap therefore pass. The candidate's declared census bump does not invoke the inconclusive guard because the mechanism proof landed and there is no same-package overlap. |
| 3b | Policy/lint lane | **PASS** | `make test-ci-policy`, fresh-cache `make lint-affected`, `make fmt-check-changed`, `make check-docs`, `make check-hooks`, `go build ./...`, `go vet ./...`, changed-file `gofmt -l`, and `git diff --check origin/main...HEAD` all passed. Fresh-cache lint reported `0 issues`; `.githooks` owns `core.hooksPath`. |
| 3c | CI-config lane run | **PASS / n/a** | No workflow, matrix, timeout, runner policy, required-check list, or CI target changed. |
| 4 | No high-severity review findings open | **PASS** | The exact-content review records no security, style, specification, or coverage blocker. The optional unwrapped test-only `os.Setenv` error was explicitly non-blocking. |
| 5 | Final feature branch clean | **PASS** | The disposable evaluation checkout was clean at exact gated source `b8c52bc07e431a34667063862d34a4a2f15e8566` after all tests and before this record was added. The gate record is the deployer's only addition. |
| 6 | Branch diverges cleanly from main | **PASS** | Refreshed `origin/main` remained `71c94672e09ed57810d227831996edb678192bcd` after the suite and is an exact ancestor of the gated source. `git merge-tree --write-tree origin/main HEAD` exited 0 with tree `2b6ed8d2cd83fd7c41e00a201eaac38f6d4d774f`. Remote `builder/ga-lwnqu9-gate-rebase` was verified at the exact gated source. |
| 7 | Single feature theme | **PASS** | Three TDD commits change eight files for one test-root cleanup theme. `assert_deploy_ancestry_scope` passed for the deploy, source, review, GH1654 blocker/fix, and prior-gate bead chain. |

## Criterion 3 evidence

```text
test_cmd: LOCAL_TEST_JOBS=4 GO_TEST_TIMEOUT=30m $GC_CITY_ROOT/packs/actual/all/scripts/isolated-test-run.sh -- make test-local-full-parallel
test_cmd_scope: full-suite
job_counts: PASS=38 RAW_FAIL=2 OMITTED=0 TOTAL=40
aggregate_log: /var/tmp/ga-lwnqu9-full-suite.log
aggregate_log_sha256: b3965cbb0ac456d926190f718e342d23b7416e197f11fff67e9e33dff326b86c
job_logs: /var/tmp/gc-local-tests.YPO9Nw
job_logs_manifest_sha256: f0d27f2743a0d95c031b106e29ac43d8835af74a65c77ec1a627b50d4e29882f
waiver_ref: none
ci_lane_run: n/a (no CI-config change)
```

The full union exercises the required `cmd/gc` process lane with
`GC_FAST_UNIT=0`, including `TestTutorial01`; all six process shards passed.
`unit-core` runs every package except `cmd/gc`, so it includes the changed
`test/tmuxtest` package. The three runtime/tmux integration shards also passed.

Focused acceptance re-verification:

```text
$isolated -- go test -count=1 -timeout 30m ./cmd/gc -run '^(TestSweepOrphan.*|TestCmdGCT.*|TestCreateActiveTestTempRoot.*|TestAdoptPerRunTMPDIR.*|TestSweepLegacyCmdGCFixtureDirs.*|TestTestscriptCommandInvocationDoesNotLeakTempRoot)$'
ok github.com/gastownhall/gascity/cmd/gc 1.949s

$isolated -- go test -count=1 ./internal/testpolicy/resourcecensus
ok github.com/gastownhall/gascity/internal/testpolicy/resourcecensus 3.690s

$isolated -- go test -count=1 ./test/tmuxtest
ok github.com/gastownhall/gascity/test/tmuxtest 0.142s
```

`diff_tests_executed`:

- `TestSweepOrphanAgeFenceBySentinelState`: PASS, including held, free-recent,
  free-aged, and legacy-aged subtests.
- `TestAdoptPerRunTMPDIRCapturesHostRootBeforeOverride`: PASS.
- `TestSweepLegacyCmdGCFixtureDirsSweepsAllFivePrefixesUnderGivenRoot`: PASS;
  N=5 planted roots, one sweep call, all five removed.
- `TestSweepOrphanPIDPrefixedDirsAgeFenceBySentinelState`: PASS, including the
  same four sentinel/age states in the `tmuxtest` mirror.

`skip_justification`: skips, if any, are unchanged suite-controlled platform,
privilege, helper-process, live-provider, or opt-in cases. Every diff-added test
executed in a green owning full-suite job and passed in the focused run; there
were zero diff-owned FAIL or SKIP results.

### Raw failure attribution

| Raw failing test | Tracker | Attribution evidence |
|---|---|---|
| `TestAdoptPRFormulaRetriesTransientReviewerStep` | `ga-e2z1zb` | `integration-review-formulas-retries-1-of-2`; fixture `gc init` classified fresh `hq` at cursor 13, then external `bd` refused 53 pending migrations (`v13 -> v66`). The formula scenario never began. Comment `63efe6c8-6bf8-54fc-af2b-a7dbbc850184` records the sighting. |
| `TestGCLiveContract_BeadsAndEvents` | `ga-e2z1zb` | `integration-rest-full-5-of-8`; API rig creation reached fixture `gc init`, which classified database `rwdleu0nserp1k` during migration and external `bd` refused 24 pending migrations (`v42 -> v66`). Comment `4bfa08a7-5d8f-5b61-a9cf-6788d23b0ff6` records the sighting. |

```text
failure_attribution: TestAdoptPRFormulaRetriesTransientReviewerStep -> ga-e2z1zb | clause 3(a) MECHANISM; clause 4 no overlap
failure_attribution: TestGCLiveContract_BeadsAndEvents -> ga-e2z1zb | clause 3(a) MECHANISM; clause 4 no overlap
```

These are two observations of one root condition, not two new defects. The raw
FAIL results are retained in the logs and are not represented as green.

## Static and policy evidence

Passed on exact gated source `b8c52bc07e431a34667063862d34a4a2f15e8566`:

```text
make test-ci-policy
GOLANGCI_LINT_CACHE=<fresh on-disk directory> LINT_CHANGED_REF=origin/main LINT_CHANGED_SCOPE=tracked make lint-affected
LINT_CHANGED_REF=origin/main LINT_CHANGED_SCOPE=tracked make fmt-check-changed
make check-docs
make check-hooks
go build ./...
go vet ./...
gofmt -l <six changed Go files>
git diff --check origin/main...HEAD
```

## Release disposition

Gate **PASS WITH ATTRIBUTION**. Cut a fresh isolated
`deploy/ga-lwnqu9-gate` branch from exact gated source
`b8c52bc07e431a34667063862d34a4a2f15e8566`, commit only this evidence
record, push that isolated branch, open the PR against `main`, publish exact-head
`release-gate/deploy-clearance=success`, and route the merge request to mayor.
The deployer does not merge.
