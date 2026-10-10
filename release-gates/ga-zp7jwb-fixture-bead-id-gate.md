**Verdict:** **PASS**

# Fixture bead ID release gate

Date: 2026-09-27T17:07:44.777888+00:00

Deploy bead: ga-zp7jwb. Build: ga-c2atlu. Review: ga-gwnucx.
Reviewed source: `66b9671a38491c723479ee56d13fc3cb3bfb8732`.
Base: `origin/main` at `5bdacc6ac8fcc477bc160a812afe3af6cb064ccf`.
Tested canonical merge: `0c0fc5214497740f1189d360e2e035efab016a0a`; tree `40357046baab7bcb19d069763212af53633c1fc3`.
Deploy mode: remote. Source is pinned to the reviewed commit; the gate record is a separate process-only commit.
Evidence directory: `/var/tmp/gc-ga-zp7jwb-0wpf0f1g`.
Criteria source: deployer release-gate-criteria protocol and `TESTING.md`; this checkout has no `docs/PROJECT_MANIFEST.md`.

## Criteria

| # | Result | Evidence |
| --- | --- | --- |
| 1 | PASS | Review ga-gwnucx explicitly records PASS for the resolved source. Its Bazel coverage gap was closed by actual execution with CC=/usr/bin/gcc. |
| 2 | PASS | Shared shell grammar, all ten consumers, parser anchors/table rows, negative controls, repeated six-test runs, dependent fixtures, both smoke shards, Bazel and vet verified below. No descoped acceptance criterion. |
| 3 | PASS | Full documented 40-job suite completed. All diff-owned tests passed. Two unrelated root conditions satisfy all four attribution clauses and the fix-carrying repeat rule; raw nonzero results are retained below. |
| 4 | PASS | Review reports no blockers/majors/minors and no open HIGH/security findings. |
| 5 | PASS | Reviewed checkout and restored canonical checkout have no tracked changes. Bazel generation left no drift. Gate markdown is the sole deployer addition; final committed branch status is checked before push. |
| 6 | PASS | Clean merge-tree against the pinned origin/main, then independent go build ./... and go vet ./... on that canonical merge, both exit 0. Main remained at the same SHA at final fetch. No self-rebase required. |
| 7 | PASS | One theme: fixture bead ID recognition after configurable prefixes. Changes are limited to test/agents, test/integration and its Bazel definition; diagnostic bd list fix belongs to the same mail fixture. Scope guard passed for ga-zp7jwb/ga-c2atlu/ga-gwnucx; no stack. |

## Full-scope evidence

```text
test_cmd: make test-local-full-parallel LOCAL_TEST_JOBS=4
test_cmd_scope: full-suite
GOFLAGS: -v
TMPDIR: /var/tmp
execution: load-gate-run.sh --threshold 15 --max-wait 1800 -- isolated-test-run.sh -- <test_cmd>
suite_exit: 2
job_counts: 37 PASS, 3 FAIL, 0 unfinished (40 total)
top_level_executions: 53572 PASS, 3 FAIL, 235 SKIP
all_test_and_subtest_executions: 95907 PASS, 5 FAIL, 330 SKIP
waiver_ref: none
load_threshold: 15
load_waited_seconds: 0
load_wait_timed_out: 0
load_start: 10.79
load_max: 51.51
load_mean: 27.59
isolation_tripwire: none
```

Counts include repeated helper/test-binary executions across the documented lanes, rather than distinct test names. The raw logs and audit scripts are retained with `audit.json`, `full-results.json`, `job-status.json` and exit receipts. Both rest-smoke jobs and all eight rest-full jobs passed. The process lane completed all six cmd/gc shards.

Rootless Podman socket and cached `docker.io/dolthub/dolt:2.2.0` were verified before the suite. DOCKER_HOST points to that socket; Ryuk was disabled with BEADS_ALLOW_UNREAPED_TESTCONTAINERS=1 under the host sweep protocol.

### Diff-owned results within the full suite

| Test | Observed PASS jobs |
| --- | --- |
| `TestIsBeadIDToken` | integration-rest-full-2-of-8, unit-core |
| `TestParseBeadID` | integration-rest-full-3-of-8, unit-core |
| `TestAgentScriptsShareBeadIDMatcher` | integration-rest-full-4-of-8, unit-core |
| `TestMail_BashAgent` | integration-rest-full-5-of-8 |


`TestParseBeadID` covers shim-created beads, real bd-created issues, multi-rig fake output, created convoys, inbox table rows, no-unread output and database reinitialization warnings. All seven subtests passed.
diff_tests_executed: all four roots above PASS; no diff-owned FAIL, SKIP or absent result.

### Acceptance checks

All ten fixture scripts source `lib/bead-id.sh`; no hardcoded `^gc-`, `^bd-` or `^mc-` filter remains (`static-acceptance.json`). The grammar supports configured prefixes, multiple dash segments and child segments; the parser reads anchored creation output or the first field of a table row. The mail timeout dump now calls bd list.

Both negative controls failed as required, then were restored: adding a banned prefix filter to loop-mail.sh reported its file/line; changing BEAD_ID_ERE reported grammar drift. Logs, rc=1 receipts and original file hashes are in `negative-controls.json`. Restored files match their original hashes.

Untagged `go test ./test/integration/ -count=1 -v` completed with 4 top-level PASS, 11 total PASS and zero FAIL/SKIP. The focused matcher tests are all included and explicitly PASS.

Two fresh hermetic tmux-mode invocations ran the six acceptance tests with TMPDIR=/var/tmp and no GC_SESSION override:

```text
go test -tags integration -count=1 -timeout 25m -v -run "^(TestGastown_MailArchive|TestGastown_MailRoundTrip|TestGastown_PipelineMailChain|TestGastown_PipelineMailAndWork|TestGastown_PipelineConvoyTracking|TestMail_BashAgent)$" ./test/integration/
```

six-targets-1: 6 PASS, 0 FAIL, 0 SKIP, exit 0.

```text
--- PASS: TestGastown_MailRoundTrip (2.83s)
--- PASS: TestGastown_MailArchive (3.10s)
--- PASS: TestGastown_PipelineMailAndWork (3.97s)
--- PASS: TestGastown_PipelineConvoyTracking (2.68s)
--- PASS: TestGastown_PipelineMailChain (7.82s)
--- PASS: TestMail_BashAgent (2.77s)
```

six-targets-2: 6 PASS, 0 FAIL, 0 SKIP, exit 0.

```text
--- PASS: TestGastown_MailRoundTrip (2.87s)
--- PASS: TestGastown_MailArchive (2.51s)
--- PASS: TestGastown_PipelineMailAndWork (2.42s)
--- PASS: TestGastown_PipelineConvoyTracking (2.63s)
--- PASS: TestGastown_PipelineMailChain (7.61s)
--- PASS: TestMail_BashAgent (2.67s)
```

All required dependent fixture tests had PASS events in the full suite:

| Test | PASS job |
| --- | --- |
| `TestGastown_MailArchive` | integration-rest-full-4-of-8 |
| `TestGastown_MailRoundTrip` | integration-rest-full-1-of-8 |
| `TestGastown_PipelineMailChain` | integration-rest-full-4-of-8 |
| `TestGastown_PipelineMailAndWork` | integration-rest-full-1-of-8 |
| `TestGastown_PipelineConvoyTracking` | integration-rest-full-3-of-8 |
| `TestMail_BashAgent` | integration-rest-full-5-of-8 |
| `TestGastown_PipelineHumanToWorker` | integration-rest-smoke-1-of-2 |
| `TestGastown_PipelinePoolDrain` | integration-rest-full-2-of-8 |
| `TestGastown_PipelineGitCommitMerge` | integration-rest-full-5-of-8 |
| `TestGastown_PolecatHappyPath` | integration-rest-full-6-of-8 |
| `TestGastown_PolecatPoolProcessing` | integration-rest-full-7-of-8 |
| `TestGastown_PoolScaling` | integration-rest-full-8-of-8 |
| `TestGastown_RefineryProcessing` | integration-rest-full-8-of-8 |
| `TestGastown_RefinerySequentialQueue` | integration-rest-full-1-of-8 |
| `TestGastown_ShutdownDogProcessesWarrant` | integration-rest-full-2-of-8 |
| `TestGastown_WitnessOrphanDetection` | integration-rest-full-6-of-8 |
| `TestGastown_WitnessWithNoOrphans` | integration-rest-full-7-of-8 |
| `TestGastown_EventsBeadLifecycle` | integration-rest-full-6-of-8 |
| `TestGastown_EventsFiltering` | integration-rest-full-1-of-8 |
| `TestGastown_SlingToNonexistent` | integration-rest-full-4-of-8 |
| `TestGastown_SlingToSuspended` | integration-rest-full-5-of-8 |
| `TestGastown_MultiRig_BeadIsolation` | integration-rest-full-7-of-8 |
| `TestInitBdAllowsStandaloneCreate` | integration-rest-full-8-of-8 |
| `TestGCLiveContract_BeadsAndEvents` | integration-rest-full-8-of-8 |
| `TestHumaBinary_SessionMessageAsync` | integration-rest-full-2-of-8 |


`make bazel-sync` exit 0 and subsequent `git diff --exit-code` exit 0. `bazel test //test/integration:integration_test` executed one test target and passed (exit 0). Both used --repo_env=CC=/usr/bin/gcc to bypass the host ccache sandbox fault. `go vet ./...` covers the required ./test/... vet. `make check-hooks` confirms .githooks owns core.hooksPath; the gate commit runs its pre-commit check.

### Skip justification

Every top-level SKIP context was inspected in audit.json. None belongs to this diff. Categories: Darwin/unsupported-OS and /private path aliases on Linux; helper-only entry points; fast-lane process tests separately exercised by the full process lane; optional catalog/MCP/k8s/persistence/tmux-dogfood profiles; destructive cleanup tests requiring explicit opt-in; installed bd capability guards (ga-e7z613 and missing --if-revision); absent br/herdr/tooling-formula or embedded-pack fixtures; tests of unavailable host conditions (root/chown/setuid, localhost SSH, no-/proc and no-subreaper paths); existing ambient-scope prohibitions ga-klo4gz; fixed characterization/golden-rewrite tests. The native upstream-beads error-normalization canary skips when its external storage is unavailable at 127.0.0.1:0. Container-backed owned tests and all required acceptance fixtures ran and passed. Per-test diagnostic context remains in audit.json and shard logs.

## Failure attribution

No failure is waived. Criterion 3 passes with these explicitly attributed unrelated conditions under the release protocol. This deploy is fix-carrying: its own build_bead ga-c2atlu is independently verified gc.fixes_tracker=ga-4w6d2r (closed blocked, source not yet landed). The designated fixes for both blocking conditions remain OPEN and have no confirmed-shipped record at verdict time.

| Failure | Tracker and designated unlanded fix | Proof and clauses |
| --- | --- | --- |
| TestRepositoryLedgerMatchesCensusAndDocumentation (unit-core and integration-packages-core-4-of-4) | ga-yihql5; ga-1lt6oc, gc.fixes_tracker=ga-yihql5 | (i) test not diff-owned; (ii) tracker opened and sighting recorded, discovering-run timing accepted by landed proof plus clear i/iv; (iii) proof d: untouched pinned base reproduces identical subprocess calls 705/704 and files 206/205, exit 1; (iv) failing internal/testpolicy/resourcecensus package/ledger has no path overlap with the fixture diff. |
| TestIsAgentRunning/matching_shell_process and /multiple_process_names_with_match (integration-packages-runtime-tmux-3-of-3; parent also FAIL) | ga-sbx0vo; ga-atixjw, gc.fixes_tracker=ga-sbx0vo | (i) test not diff-owned; (ii) pre-existing condition tracker opened and this sighting appended; (iii) proof a: unchanged runtime/tmux login-shell setup samples tput, then observes zsh; runtime/tmux and its unchanged test/tmuxtest dependency execute none of the changed agent scripts or integration parser; (iv) no package/path overlap. |

```text
failure_attribution: TestRepositoryLedgerMatchesCensusAndDocumentation -> ga-yihql5 | clause 3: d — same deterministic condition reproduced on untouched BASE_REF
failure_attribution: TestIsAgentRunning/matching_shell_process -> ga-sbx0vo | clause 3: a — changed fixture/parser code unreachable
failure_attribution: TestIsAgentRunning/multiple_process_names_with_match -> ga-sbx0vo | clause 3: a — changed fixture/parser code unreachable
repeat_exception: fix-carrying ga-c2atlu -> ga-4w6d2r; blocking fixes ga-1lt6oc and ga-atixjw not landed
```

Base reproduction: `isolated-test-run.sh -- go test ./internal/testpolicy/resourcecensus/ -run ^TestRepositoryLedgerMatchesCensusAndDocumentation$ -count=1 -v`; see census-base.log and receipt. Trackers/fix metadata and review evidence are retained in final-lineage.json. Mayor bookkeeping replies were checked; neither is a waiver.

## Policy and CI configuration

```text
policy_lane: make GOLANGCI_LINT=/var/tmp/mpr-toolchains/golangci-lint-2.12.0/gopath/bin/golangci-lint lint-affected fmt-check-changed test-ci-policy check-gomod-replace check-native-dependency-surface check-eventexport-isolation check-core-boundary test-native-doltlite-beads check-docs
policy_result: PASS (exit 0), through isolation wrapper
LINT_CHANGED_SCOPE: tracked
LINT_CHANGED_REF: 5bdacc6ac8fcc477bc160a812afe3af6cb064ccf
policy_attribution: none
ci_lane_run: not applicable — no CI job/matrix/timeout/required-check changes
```
