# Release gate: bd stdout JSON parsing

**Verdict:** **PASS**

- Deploy bead: ga-2o1pf535; build: ga-hc527n; review: ga-pnoi3s5.
- Reviewed source: d1aee4e46e8356293260b2bf26bed7c692243f44.
- Deploy mode: remote; push remote: fork; base ref: origin/main.
- Gate base: e8547dc572836c1a96793b94632bbaeab0de0cbf; canonical merge: 015f473d54b41382c43b0e951e312501034b0ccf.
- Canonical tree: bc9aa40d466f7916b3fc8962e1469f8b96693d47; evaluated 2026-11-17 UTC.
- Evidence root: /var/tmp/gate-ga-2o1pf535-c6_v92o3. All raw per-test logs and XML are retained there.

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS | PASS | ga-pnoi3s5 binds its PASS to the resolved reviewed source. No carryover or reviewer-added tests. |
| 2 | Acceptance criteria | PASS | The existing delegated-write test uses Output(), retains stderr through errors.As and exec.ExitError, and keeps strict JSON/ID/title assertions. No import, subprocess call site, helper, production code or test target added. Owned test and resource-census acceptance test passed by name. Full nogo/vet passed. |
| 3 | Tests and CI coverage | PASS | Complete fresh five-phase remote suite, native floor, measured latency and six-leaf CI accounting below. No attributed test failure or waiver. |
| 4 | Unresolved HIGH findings | PASS | Reviewer reports no blocking style, security or specification findings. Reviewed diff unchanged. |
| 5 | Clean final branch | PASS | The isolated deploy branch was cut at the reviewed source and its checkout was clean. This gate record is the sole deployer addition, committed separately; publication requires a clean post-commit checkout. |
| 6 | Merge state and build/vet | PASS | Clean materialized merge of the two recorded parents; remote build and whole-tree nogo/vet exit 0. |
| 7 | One feature theme | PASS | Exactly one existing cmd/gc test body changes, +8/-1. Ancestry/denylist/citation checks pass for deploy, build and review bead IDs; no stack. |

merge_state: clean (origin/main@e8547dc57283)
gate_base_kind: current-tip
gate_base: e8547dc572836c1a96793b94632bbaeab0de0cbf
merge_validation: merge result built, vetted and tested here; merge authority revalidates the eventual merged tree.

Criterion 6 ran isolated through remote admission, from the canonical merge:

```text
bazel build --config=remote-exec --noremote_local_fallback //cmd/gc:gc
make lint NOGO_BUILD_FLAGS='--keep_going --output_groups=nogo_fix --config=remote-exec --noremote_local_fallback'
```

The complete command/BEP arguments are in criterion6.sh; c6-result.json independently verifies both successful build events and merge parents/tree.

fast_lane: PASS before runtime setup. Each lane used a fresh view and gate-base.sh's pinned base.
policy_lane: PASS; pinned fmt-check-changed, release-dist, routed/split rows, residency boundary, active hooks, 13 Bazel policy/format/docs targets; whole-tree lint/vet from criterion 6.
drift_lane: PASS; make bazel-sync, git diff --exit-code and empty status including untracked files, plus seven generated-artifact targets; OpenAPI breaking check PASS.
complexity: zero growth (ii); base and merge both exit 2 with the same 251 rows; no worsened row and no complexity-diff change. Baseline untouched. This is the prescribed zero-growth score, not an attributed failure.
Static cache hits are disclosed in fast-independent-audit.json and count only for static lanes, never as runtime evidence.

test_cmd: GC_GATE_REMOTE_ENABLED=1 GC_REMOTE_GATE_FLOOR_GIB=25 GC_REMOTE_GATE_CAP=2 ISOLATED_TEST_BEAD=ga-2o1pf535 gate-criterion3.sh --repo /var/tmp/gate-ga-2o1pf535-c6_v92o3/merge --base e8547dc572836c1a96793b94632bbaeab0de0cbf --bead-json /var/tmp/gate-ga-2o1pf535-c6_v92o3/bead-criterion3.json --owned /var/tmp/gate-ga-2o1pf535-c6_v92o3/owned.tsv --run-dir /var/tmp/gate-ga-2o1pf535-c6_v92o3/criterion3/evidence
test_cmd_scope: full-suite
test_counts: 155334 PASS, 0 FAIL, 613 SKIP (column-zero result lines across 557 logs; repeated tests count per job).
diff_tests_executed: TestGcBeadsBdInitDoltliteInitializesDelegatedBdWrites PASS in unit and integration-packages.
acceptance_topup: TestRepositoryLedgerMatchesCensusAndDocumentation PASS in the fresh unit log; ci-required/acceptance-by-name.txt verifies both named requirements.
test_log_dir: /var/tmp/gate-ga-2o1pf535-c6_v92o3/criterion3/evidence/remote/logs
test_exec_mode: remote-exec
test_exec_bep: criterion3/evidence/remote/{unit,acceptance,integration-packages,integration-full,integration-latency}.bep.json
rbe_state: enabled selector, no kill switch, valid certificate; recorded worker-env digest matches pinned tree/main.
fallback_reason: none
admission: remote cap 2, 25 GiB floor; 59.624 GiB free, 0.001s wait.
farm_test_seconds: 17504.68
heavy_mode: none (classifier verified).
owned_topup: none needed; owned root really ran in the complete suite.
floor_topup: all 16 canonical steps exit 0, including environment-sensitive packages, beadstest, Darwin compile and runner self-tests.
latency_leg: measured; clear farm probe after 60s, first attempt, both configured latency tests PASS. No latency test ran in integration-full.
ci_lane_run: n/a (no CI-config changes).
waiver_ref: none
failure_attribution: none

| Phase | Uncached remote actions | PASS roots | SKIP roots |
|---|---:|---:|---:|
| unit | 415 | 91741 | 440 |
| acceptance | 25 | 425 | 14 |
| integration-packages | 86 | 62943 | 131 |
| integration-full | 14 | 199 | 14 |
| integration-latency | 1 | 2 | 0 |

All 541 remote actions used remote strategy, attempt 1, no cache flags and starts inside their phase windows. Independent audit verified all 1,082 raw test.log/XML hashes, sizes and normalized log equivalence against BEP CAS URIs. remote-independent-audit.json and remote-artifact-sha256.json retain the proof.

skip_inventory: 671 root/subtest skip events, each with its printed reason in skip-independent-audit.json (128 environment-absence, 543 other). These differ from the 613 column-zero count above because nested subtests are included. No owned test skipped. Only the separately passing owned test body changes; helpers/production/resource policy/targets are unchanged. All 13 skip sites in the edited file report UNCHANGED, line_in_hunk=no and zero shared hunks. Environment-absence cases received the canonical native-floor run; non-owned native skips retain their explicit platform, fixture, provider-capability or opt-in reason. No missing environment was excused for the owned test.

load_threshold: 15
load_waited_seconds: 0
load_wait_timed_out: 0
run_start_load: 12.73
run_max_load: 16.22
run_mean_load: 15.29
run_readings: 7

Local-floor telemetry above; exact remote-chain host and compile-job telemetry follows. Remote admission intentionally has no host-load wait; its host readings are not farm readings.

```text
remote-chain host: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=111.64 run_max_load=111.93 run_mean_load=31.60 run_readings=58 wait_first_load=111.64 wait_max_load=111.64 wait_mean_load=111.64 wait_readings=1 read_errors=0
native floor: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=12.73 run_max_load=16.22 run_mean_load=15.29 run_readings=7 wait_first_load=12.73 wait_max_load=12.73 wait_mean_load=12.73 wait_readings=1 read_errors=0
CI compile job: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=14.68 run_max_load=19.16 run_mean_load=17.42 run_readings=5 wait_first_load=14.68 wait_max_load=14.68 wait_mean_load=14.68 wait_readings=1 read_errors=0
```

ci_required_closure: python3 ci_required_triggers.py <canonical-merge> <gate-base> <reviewed-source> fork; workflow blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs. Exact command and output: ci-required-accounting.json and ci-required/closure.txt.

| Job | Account | Command/result/log or trigger |
|---|---|---|
| changes | LOCAL-PASS | ci-required/closure.txt; exit 0 |
| runner-policy | LOCAL-PASS | ci-required/runner-policy.log; exit 0 |
| credential-provider-windows | NOT-TRIGGERED |   credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true' |
| pack-gate | NOT-TRIGGERED |   pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true' |
| windows-lane | LOCAL-PASS | ci-required/windows-output.log; exit 0 |
| windows-lane-test | DEFERRED-TO-PR-CI | non-linux-runner; Windows test closure has no changed input |

The runner-policy command used EVENT_NAME=pull_request, PR_AUTHOR=quad341 and empty FORCE_BLACKSMITH, from the canonical merge. Its output resolves the compile job to Ubuntu and native tests to Blacksmith Windows. The compile job ran its own four run steps, in order, via fresh-tree-run.sh -> load-gate-run.sh -> isolated-test-run.sh -> ci-required/windows-lane.sh:

```sh
GOOS=windows go build $(cat .github/windows-lane/build.txt)
GOOS=windows go test -count=1 -vet=off -run '^$' -exec /bin/true $(cat .github/windows-lane/test-build.txt)
# From tools/nogo:
GOOS=windows go build ./analyzers/... && GOOS=windows go test -count=1 -vet=off -run '^$' -exec /bin/true ./analyzers/...
# From repository root:
GOOS=darwin go build ./... && GOOS=darwin go test -count=1 -vet=off -run '^$' -exec /bin/true ./...
```

These are compile checks, not native Windows/macOS test execution. windows_reach: none for windows-lane-test; its own allowlist's GOOS=windows test dependency closure has 10 module directories and contains no changed input. Credential-provider-windows is NOT-TRIGGERED, not deferred.
deferred_to_pr_ci: windows-lane-test (non-linux-runner)
not_covered: none

The continuation initially searched stdout for wrapper metadata written to stderr. Independent inspection of its original combined log and successful RESULT corrected only that bookkeeping assertion; no job was rerun. ci-required-accounting.json is the final account, retaining the original error as provenance.

This PASS authorizes PR-open only. Merge requires CI / required and the deferred Windows job to succeed at the PR's current head, plus the required Bazel checks. No merge is performed here. docs/PROJECT_MANIFEST.md is absent; the supplied release-gate protocol and repository testing/criteria conventions govern this record.
