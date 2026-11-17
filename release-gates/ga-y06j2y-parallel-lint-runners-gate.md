**Verdict:** **PASS**

# Parallel lint runners release gate

Deploy bead: ga-y06j2y. Build: ga-p37sng. Review: ga-6vmdfk.
Evaluated 2026-09-28T02:34:42Z.

This change enables concurrent golangci-lint runners and adds the regression
in the existing lint contract file: two files, 25 added lines, one feature theme.
It carries the lint-lock fix for ga-88dvlm.

## Source and criteria

Reviewed source: `ff7e8c01e4cffe9fdc265a6f8d4c154ea08fb1f3` (resolved as a commit).
Tested base: `origin/main` pinned at `0f41de797ef54adbc18ec7f28b9e5b01e59dc458`.
Publication base: `origin/main` at `79794578fa45b216fa9b1a35f104affdd37dda06`.
Main advanced after the test snapshot with the source-store-delete fix (#6733)
and tmux cache eviction fix (#6735); neither overlaps this candidate. Fresh
`git merge-tree --write-tree` against that publication base returned exit 0,
tree `0ae40d0db8baadec86ba2c6a19f4e7d852201239`. The reviewed source was not
rebased or changed. Tests below cover the earlier pinned merge tree; this record
does not claim they executed the two later main commits. PR merge-ref CI remains
the merge authority's check on the current base.

Verified tested synthetic merge commit: `572991c1aaa4cc136e513c7f9d4eac0cafa8c675`.
Verified merge tree: `8d4fd321116470acb3a2a8e38e4064157b54c770`. The materialized tree equals the independent
`git merge-tree --write-tree` result; there are no merge conflicts.
All commands below ran on that exact merge commit with `TMPDIR=/var/tmp`.
Remote deploy: fork, isolated branch `deploy/ga-y06j2y-gate`, PR base
`gastownhall/gascity:main`. The source is not already on the base, and the
commit-to-PR preflight returned no PR. The reviewed SHA is the deploy source.

`docs/PROJECT_MANIFEST.md` is absent in this checkout. The seven criteria follow
the deployer's release-gate-criteria protocol; command scope and required lanes
come from TESTING.md, Makefile, and .github/workflows/ci.yml.

| Criterion | Result | Evidence |
|---|---|---|
| 1 Review | PASS | Closed ga-6vmdfk records PASS on the exact source; single-pass is permitted while the second pass is disabled. |
| 2 Acceptance | PASS | Requested setting/comment present; regression passes; pinned config verification, lint-changed, full vet pass. TDD red/green commits resolve and green adds only six config lines over red. |
| 3 Tests | PASS (attributed failures) | Primary: all 40 jobs terminal, 36 PASS / 4 FAIL solely on six attributed prefix cases. Supplemental acceptance: raw exit 2 with attributed schema refusal and package-budget timeout; 80 unstarted tests are not counted as passing. All six changed-file functions PASS in both primary lanes. |
| 3a Attribution | PASS | Prefix: ga-4w6d2r, unlanded fix ga-c2atlu. Schema: ga-n75ap3, unlanded fix ga-h8haz6. Budget: ga-rngagz, first-hit same-run tracker escape. Mechanism/path separation verified, with raw failures/incomplete results retained. |
| 3b Policy/lint | PASS | All static/policy, minimum-bd compatibility, generated-artifact, and dashboard commands below exit 0. Acceptance is attributed under 3a; its command did not pass. Pinned lint resolves 2.12.0 without mismatch. |
| 3c Changed CI lane | PASS (not applicable) | Candidate changes no CI job, matrix, timeout, or required-check list. ci_lane_run: n/a. |
| 4 High findings | PASS | Review records no unresolved high-severity findings. |
| 5 Clean branch | PASS | Source and verified merge checkouts clean. Deploy adds only this committed record; publication is guarded on clean status and the reviewed SHA's presence. |
| 6 Merge | PASS | Original tested merge tree and fresh publication-base merge-tree check both clean; build/vet pass on tested tree. No self-rebase needed. |
| 7 Theme/scope | PASS | Concurrent lint config plus its contract regression. Both feature commits cite ga-p37sng; scope accepts ga-y06j2y and ga-p37sng, without forbidden internal paths. |

The .golangci.yml run block has `allow-parallel-runners: true` and the requested
comment. Independently inspected golangci-lint 2.12.0 source confirms
`pkg/commands/run.go` acquire/releaseFileLock bypass the runner lock for this
setting; `internal/go/cache/cache.go` documents separate concurrent-process
cache safety. Lint uses the setting without an ad hoc parallel-runners flag.
Resolved red: `7b59160994c154c95ef2d0b24def8f43a95afac1`; resolved green: `ff7e8c01e4cffe9fdc265a6f8d4c154ea08fb1f3`. Red introduces the assertion
while the flag remains unset; green adds the six config lines. No imports or
dependencies change.

## Full-scope evidence

```text
test_cmd: make test-local-full-parallel LOCAL_TEST_JOBS=4
test_cmd_scope: full-suite
raw_exit: 2
jobs: 40 terminal / 36 PASS / 4 FAIL (attributed condition)
PASS: 53559
FAIL: 6 (all attributed, 0 diff-owned)
SKIP: 235 (0 diff-owned)
including_subtests: 95876 PASS / 6 FAIL / 330 SKIP
diff_tests_executed: all 6 functions in the changed file, PASS in both lanes
waiver_ref: none
```

These are named result records, including repeat executions across lanes,
not unique-function counts. Every documented unit, process, integration,
formula, and end-to-end job completed, alongside Darwin compile and two shell
self-tests. The documented target's own shard selectors were unchanged.
The agent added no test-name filter or package restriction.

| Changed-file function | unit-core | integration-packages-core-2-of-4 |
|---|---|---|
| TestLintUsesReadonlyModuleDownloads | PASS | PASS |
| TestLintAllowsParallelRunners | PASS | PASS |
| TestQualityGateTargetsUseReadonlyModuleDownloads | PASS | PASS |
| TestLintTargetsApplyMemoryLimit | PASS | PASS |
| TestFmtCheckDoesNotModifyGoSumWithAmbientWritableModuleMode | PASS | PASS |
| TestLintChangedFailsClosedWhenReadonlyMetadataIsStale | PASS | PASS |

`skip_justification`: unit-lane real-process omissions are exercised by the
passing process shards. Other skips cover platform/Darwin-only tests, helper
entrypoints, explicit optional tmux/MCP/catalog/persistence/golden opt-ins,
unavailable SSH/br or installed-tool features, host privilege/filesystem/PID
and subreaper conditions, absent legacy prompt/formula assets, and documented
existing characterization or embedded-pack cases. All per-test skip text is
retained in evidence.json and shard logs; no diff-owned test skips.
Rootless Podman socket ping passed before execution; DOCKER_HOST uses it, Ryuk
is disabled per fleet policy, and this change needs no test container image.

The full command used load-gate-run.sh (threshold 15, max wait 1800 seconds)
then isolated-test-run.sh. Its observed five-minute load fields, also saved on the bead:

```text
load_threshold=15
load_waited_seconds=510
load_wait_timed_out=0
load_start=28.08
load_max=54.19
load_mean=27.48
samples=116
read_errors=0
```

## Preserved failure attribution

| Test | Full-sweep log and line |
|---|---|
| TestGastown_MailArchive | integration-rest-full-1-of-8.log:28 |
| TestGastown_PipelineMailChain | integration-rest-full-1-of-8.log:32 |
| TestMail_BashAgent | integration-rest-full-2-of-8.log:62 |
| TestGastown_MailRoundTrip | integration-rest-full-6-of-8.log:24 |
| TestGastown_PipelineMailAndWork | integration-rest-full-6-of-8.log:29 |
| TestGastown_PipelineConvoyTracking | integration-rest-full-8-of-8.log:26 |

failure_attribution: TestGastown_MailArchive, TestGastown_PipelineMailChain, TestMail_BashAgent, TestGastown_MailRoundTrip, TestGastown_PipelineMailAndWork, TestGastown_PipelineConvoyTracking -> ga-4w6d2r | clause 3: a (MECHANISM), landed proof. The candidate changes only .golangci.yml and scripts/lint_readonly_contract_test.go. test/integration and test/agents neither import the scripts package nor execute golangci-lint (git grep against origin/main returned no matches); lint configuration is not runtime configuration. Failing paths do not overlap either changed file. The known fixture-prefix condition is explicit in the opened ga-4w6d2r record (created 2026-09-26T23:55:25Z), whose prior independent investigation reproduced the same tests on untouched main. Current MailArchive output carries ca-16; extractBeadID admits only bd/gc/mc. loop-mail.sh admits only ^gc- inbox rows, explaining the two reply timeouts; TestMail_BashAgent's obsolete gc bead diagnostic is secondary.
fix-carrying: this deploy's build_bead ga-p37sng is gc.fixes_tracker=ga-88dvlm. The blocking condition's designated fix ga-c2atlu is gc.fixes_tracker=ga-4w6d2r with gc.work_outcome=blocked at commit 66b9671a38491c723479ee56d13fc3cb3bfb8732. That commit resolves but is not an ancestor of origin/main 0f41de797ef54adbc18ec7f28b9e5b01e59dc458; main's loop-mail.sh still has both ^gc- filters. Thus the fix has not landed and the cross-fix repeat exception applies. No waiver, no re-run-to-green, no added target/census/sleeps/subprocesses/listeners/parallelism in the candidate.

Additional full-sweep signatures: PipelineMailAndWork prints valid ua-5 but mayor-dispatch.sh only accepts ^gc- rows; PipelineConvoyTracking prints valid jm-19 rejected by the same extractBeadID helper; MailRoundTrip has the same loop-mail.sh ack timeout already confirmed in the opened landing guard. All six are explicitly covered by ga-4w6d2r and its unlanded fix ga-c2atlu.

The complete sighting was appended to the opened ga-4w6d2r and read back as
comment `e0f49ec1-8be7-5904-9376-90a63b6da494`. No fixture was patched or rerun
to obtain green. No waiver was used. That landing guard stays open until its
fix lands; this lint deploy does not claim to fix the prefix condition.

## Required commands

`go build ./...` and `go vet ./...`: PASS. The isolation wrapper ran these
policy targets, all PASS: `make test-ci-policy`, `make check-gomod-replace`,
`make check-native-dependency-surface`, `make check-eventexport-isolation`,
`make check-core-boundary`, `make test-native-doltlite-beads`, `make lint`,
`make fmt-check`, `make vet`, `make check-docs`. Lint/format/config verification
used run-pinned-lint.sh: pin 2.12.0, resolved 2.12.0, mismatch false.
`make lint-changed LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main`
also passed on the same verified tree.

Supplementary CI lanes with exit 0 on that tree: `make test-bd-cli-contract` with
checksum-verified minimum-supported bd v1.0.4 isolated under the artifact
directory, `make spec-ci`, `./scripts/check-generated-docs-drift.sh`,
`make dashboard-ci dashboard-e2e-play dashboard-smoke`, and
`npm run --workspace gas-city-dashboard-frontend test` from the web workspace.
Dashboard source/test/e2e typechecks, Chromium render smoke, Vitest, and local
loopback preview respond successfully. Generated artifacts have no drift.
The separate unfiltered `make test-acceptance ACCEPTANCE_GO_TEST_FLAGS='-count=1 -v'`
returned exit 2. Its output records 63 PASS / 1 FAIL / 3 SKIP at top level,
167 PASS / 2 FAIL / 3 SKIP including subtests. The failing parent and subtest
are `TestBeadsProxiedDefault/legacy-managed-city-unchanged`: `gc rig add` emitted
`bd init --force`, then refused the pending shared-server migration v65 to v66.
`failure_attribution`: that test -> ga-n75ap3 | clause 3(a), MECHANISM.
This candidate changes no runtime code that test/acceptance reaches; the import
and tool-reference search for scripts/golangci under test/acceptance returned
no matches, and no changed-file/package overlap exists. Opened ga-n75ap3
predates this run. Its fix ga-h8haz6 is stamped `gc.fixes_tracker=ga-n75ap3`,
`gc.work_outcome=blocked`; resolved commit
`c98dbc740ee61b86226bf29a686cfa37bb3f3d62` is not reachable from the pinned base.
Own build ga-p37sng fixes ga-88dvlm, so the cross-fix repeat exception applies.
The sighting was appended to ga-n75ap3 and read back.

The acceptance package subsequently exhausted its aggregate 15-minute budget;
TestConvoyLifecycle had been running only 2 seconds, its Land_NotOwned subtest
0 seconds. TestBeadsProxiedDefault had already consumed 560.14 seconds. This
is not evidence of a convoy hang. `failure_attribution`: acceptance package
budget -> ga-rngagz | clause 3(a), MECHANISM,
no changed runtime code, failing-file/path overlap, census change, new suite
target or added parallel/process/listener/sleep load. Search found no existing
tracker for this condition; the new tracker was created, opened and read back.
It records discovery, rather than proving pre-existence. Attribution uses the
full non-diff-owned-gate-failure protocol's explicit same-run tracker escape
when clause 3 has landed and clauses 1/4 are clear; the full file controls the
criteria table's summary. No unmeasured claim of a host-load cause is made.

Eighty main acceptance functions never started, and TestConvoyLifecycle has no
terminal result. These are incomplete, never PASS or SKIP. The full missing-name
inventory is acceptance-evidence.json in the artifact directory. Three actual
acceptance skips are the opted-out topology matrix plus two legacy-migration
cases without a pre-journal gc binary; none belongs to this diff. CI's Tier A lane deliberately omits the live bd/Dolt stack; the
local tools enabled the additional slow cases. No hermetic CI pass is inferred,
no failing case was rerun, and no timeout/filter/tool hiding was used to obtain
green. The completed full-scope primary sweep is separate from this preserved
supplemental incomplete run. There is no waiver.

The acceptance load summary is retained separately:

```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 load_start=14.71 load_max=29.77 load_mean=21.22 samples=31 read_errors=0
```

`make check-hooks` verifies .githooks owns core.hooksPath. The gate commit uses
the active pre-commit hook, and normal push invokes the fast baseline hook.
The supplementary working-tree patch is empty after all generators/checks.

## Earlier run and launch correction

The prior FAIL gate at b0a1c5470c5448ad49d660aa796ca0faba9e7bae preserved Unix
socket failures from an overlong caller TMPDIR. Mayor mail gm-wisp-lpatd0
explicitly authorized one fresh gate with TMPDIR=/var/tmp as an infrastructure
correction, not a waiver. Earlier evidence remains on the bead and in artifacts.

The initial launch of this fresh run omitted the shard-log directory. All 40
commands failed output redirection before executing tests: exit 2, zero test
result records. This was the deployer's launch error. After creating that
directory, the one actual full sweep above ran on the identical verified
commit. The zero-test log remains in suite.log and is not passing evidence.
Its separate load summary was threshold 15, waited 900 seconds, timeout 0,
start 18.88, max 18.90, mean 16.82, samples 31, read errors 0.

The initial acceptance invocation aborted in TestMain before m.Run: host bd
schema v64 did not match the pinned linked library v66. No main acceptance test
body executed. Its raw exit 2 and complete log remain in
supplemental-acceptance.log; it is not passing evidence. An isolated CLI was built
from the exact pinned beads v1.3.0 module, with the existing fleet CLI and store
untouched. GC_ACCEPTANCE_BD_BIN selected it for the corrected unfiltered lane, whose failure and timeout are recorded above.
The separate original acceptance load summary was threshold 15, waited 780
seconds, timeout 0, start 18.21, max 20.18, mean 17.25, 27 samples, 0 read errors.
This startup correction did not rerun any named failing test or repeat the
primary full sweep.

## Artifacts and authority

Artifact directory: `/var/tmp/gc-ga-y06j2y-authorized.kqbaiwm_`. Primary full output: suite-corrected.log; raw test
records: shards/*.log and evidence.json. Policy and supplementary logs are
alongside them. Temporary test worktrees are removed after each supervisor.

Publish the PR, then exact-head deploy clearance, then a peek-verified
MERGE-REQUEST to mayor for mpr. The deployer does not merge. Outcome remains
blocked until the PR lands.
