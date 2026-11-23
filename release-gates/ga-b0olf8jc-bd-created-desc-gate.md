# BdStore created-desc selection release gate

**Verdict:** **PASS**

- Bead `ga-b0olf8jc`; build `ga-43kqzeix`; review `ga-bowwwylv` PASS; no unresolved HIGH findings.
- Reviewed source `1d693ecc3c765f3dd26f5a54ec27eb05e6e99df6`; builder branch is provenance only. Deploy branch is `deploy/ga-b0olf8jc-gate`, with one administrative gate-record commit above that exact source.
- Gated merge `eacbfa07ab2d2586715f6caf183e0f52c238a519`; tree `de1c03c3f7bc3f7a9d01361c90d633b9a1c90d82`.
- `merge_state: clean (origin/main@f4627bc5a305450b2cb93967ae631324c9a9ce22)`; `gate_base_kind: current-tip`.
- Publication check: clean merge against `origin/main@a47ce14d1d187fc17fa8f56d12e5d5e59874f655`. No rebase or source change.
- `merge_validation: merge authority checks the current merge result and current-head CI; this gate authorizes PR-open only`.

| # | Criterion | Result and evidence |
|---|---|---|
| 1 | Review PASS | PASS — live review pins the independently resolved source; spec, security and style reviewed. |
| 2 | Acceptance met | PASS — exact created-desc reads select the newest matching population and id-DESC ties before cutting the page; opted-in backing limits explicitly request created order; guard paths and default/unlimited controls preserved. Delivery PR must include Closes #9415. |
| 3 | Tests and required lanes | PASS — canonical full suite, seven owned tests by name, complete native floor, 35 base comparison groups and six-LEAF CI accounting below. |
| 4 | No unresolved HIGH findings | PASS — live reviewer notes and independent source acceptance walk have none. |
| 5 | Final branch clean | PASS — isolated branch, only the gate record above reviewed source; clean status checked before push. |
| 6 | Merge state and build/vet | PASS — pinned raw merge tree and both parents verified; whole-tree build and explicit lint/nogo BEPs overallSuccess=true. |
| 7 | One feature theme | PASS — three source/test files covering BdStore created-desc population and limit behavior; no unrelated feature, internal-document contamination or CI configuration change. |

## Test evidence

- `test_cmd`: `gate-criterion3.sh --repo /var/tmp/gc-merge-validate.rNf9Ua --base f4627bc5a305450b2cb93967ae631324c9a9ce22 --bead-json /var/tmp/gc-gate-ga-b0olf8jc-2q25i8a8/c3-bead.json --owned /var/tmp/gc-gate-ga-b0olf8jc-2q25i8a8/owned.tsv --run-dir /var/tmp/gc-gate-ga-b0olf8jc-2q25i8a8/c3-run/evidence`, detached through gate-detached-run.sh with canonical admission/load/isolation wrappers.
- `test_cmd_scope: full-suite`: unit (`make -k check`, including Bazel `//...`), acceptance, integration-packages, full integration excluding the dedicated latency pair, then the latency phase.
- `test_counts: PASS=158712 FAIL=0 SKIP=618`; `waiver_ref: none`; `failure_attribution: none`.
- `test_exec_mode: remote-exec`; `fallback_reason: none`; all five BEPs prove uncached remote execution, attempt 1 and phase-window starts. All 1108 retained farm log/XML hashes independently verified.
- `test_exec_bep`: `/var/tmp/gc-gate-ga-b0olf8jc-2q25i8a8/c3-run/evidence/remote/{unit,acceptance,integration-packages,integration-full,integration-latency}.bep.json`.
- `rbe_state`: enabled, valid certificate and equal source/base worker-env pins; `admission: remote cap=2 floor=25 GiB`; `farm_test_seconds: 13233.689`.
- `heavy_mode: none`; `delta_gate: shadow/full (not-gate-pr)`; no narrowing; `owned_topup: []`.
- `floor_topup`: complete, all 16 steps exit 0, including all environment-absence packages, beadstest, Darwin compile and runner self-tests.
- `latency_leg`: complete; TestGcBdLatencyBudget and TestGcSeatHookLatencyBudget PASS on attempt 1 under a measured clear farm probe.
- `skip_inventory`: 604 farm events including subtests; native floor reruns all environment-absence packages. Complete retained output has 676 SKIP events including subtests, mapped to 319 unchanged source sites, zero unknown.
- `skip_justification`: existing platform/lane/provider/fixture/environment skips; no owned test or subtest skipped. The changed mixedPriorityCreatedRows/assertBdStoreCreatedPage test helpers are shared hunks in internal/beads. All test binaries whose import closure includes that package were measured at the pinned base: 51 closures checked, 31 remote groups (481 SKIP events) and four native groups (45 events); every comparison exit 0, ALL PRE-EXISTING. All 270 retained base log/XML hashes and remote uncached/attempt/window predicates verified.
- Remaining skip sources and shared skip helpers are unchanged and outside the changed package's import closure. Site locations use the exact tested merge to include main-added tests; source and tested merge own the identical seven tests.
- Initial baseline integration command used the unit configuration and executed no named tests; comparator refused exit 3. It is retained as an invalid measurement under base-skip-evidence. Corrected measurements mirror each canonical phase, including integration tags and latency exclusion. No failed test was retried and no full-suite evidence was replaced.
- `test_log_dir`: `/var/tmp/gc-gate-ga-b0olf8jc-2q25i8a8/c3-run/evidence/remote/logs`; base logs and comparisons: `/var/tmp/gc-gate-ga-b0olf8jc-2q25i8a8/base-skip-evidence-corrected`.

```text
diff_tests_executed:
  TestBdStoreListByLabel PASS (integration-packages-8d820cd4c52192de043e.log, unit-ea4511196531bb4b8c55.log)
  TestBdStoreListByLabelEmpty PASS (integration-packages-b6a233fef6b5b347dcfa.log, unit-363a36f2662d541bc75e.log)
  TestBdStoreListCreatedDescKeepsLargestIDsAtBoundary PASS (integration-packages-b6a233fef6b5b347dcfa.log, unit-363a36f2662d541bc75e.log)
  TestBdStoreListCreatedDescSelectsNewestPopulation PASS (integration-packages-ad823e3d7eb71467b6af.log, unit-3ee1ae2eaa26dfd63a88.log)
  TestBdStoreListCreatedPopulationControls PASS (integration-packages-a1f48694841e1859d61a.log, unit-0b74cd4d9bd61207e1fd.log)
  TestBdStoreListPushesCallerLimitForSortCreatedDesc PASS (integration-packages-8d820cd4c52192de043e.log, unit-ea4511196531bb4b8c55.log)
  TestSeekGatesForceClientSideLimit PASS (integration-packages-8d820cd4c52192de043e.log, unit-b398920224b769833cd5.log)
GATE_TEST_EVIDENCE_RESULT ok: 7 diff-owned tests verified by name
```

```text
COMPARE-NOTE the remote worker's account differs between the runs in 1 SKIP(s), read as the same reason because the account is the only difference (an account is rbe-, a letter and two digits, standing alone; the logs are not changed): TestInfraScopedManagedDoltPIDIdentityUnderRealSystemd (base rbe-a07, head rbe-a04)
COMPARE-NOTE a generated path name differs between the runs in 1 SKIP(s), read as the same reason because it is the only difference and the same file and line were printed (a generated name is the digits Go appends to a temp directory's name, the process id in the name of gascity's temp root gct<pid>-<digits>, or a UUID right after a /work/ directory, which is how a Bazel remote worker names its work directory; the logs are not changed): TestProxyProcessReapRegistryMatchesAnEquivalentSpelling (base 1331572455, head 812565320)
```

## Fast lanes and load

- `policy_lane`: shell/hook guards, explicit policy targets, whole-tree lint/nogo and strict trusted-base storeclass baseline/growth predicates PASS.
- `drift_lane`: separate fresh-view make bazel-sync and clean diff/status, generated schema/spec/client/dashboard/lockfile targets PASS. Fresh-view format and OpenAPI breaking checks PASS.
- Complexity zero-growth rule (ii) PASS: base and merge complexity-check exit 2 with no worse merge row; complexity-diff reports no threshold changes; baseline untouched (existing condition ga-q7p6is).
- `ci_lane_run: n/a (no CI-config change)`; API/spec/dashboard preview not required by this diff.

```text
native-floor: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=12.04 run_max_load=15.85 run_mean_load=14.94 run_readings=7 wait_first_load=12.04 wait_max_load=12.04 wait_mean_load=12.04 wait_readings=1 read_errors=0
runner-policy: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=14.70 run_max_load=NA run_mean_load=NA run_readings=0 wait_first_load=14.70 wait_max_load=14.70 wait_mean_load=14.70 wait_readings=1 read_errors=0
windows-lane: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=14.38 run_max_load=14.45 run_mean_load=14.18 run_readings=3 wait_first_load=14.38 wait_max_load=14.38 wait_mean_load=14.38 wait_readings=1 read_errors=0
pack-gate: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=13.88 run_max_load=18.94 run_mean_load=17.60 run_readings=4 wait_first_load=13.88 wait_max_load=13.88 wait_mean_load=13.88 wait_readings=1 read_errors=0
```

## Required CI accounting

- `ci_required_closure`: `python3 $GC_CITY_ROOT/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-validate.rNf9Ua f4627bc5a305450b2cb93967ae631324c9a9ce22 1d693ecc3c765f3dd26f5a54ec27eb05e6e99df6 fork`; merged workflow blob `fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612`; six LEAF jobs require a status. Origin/fork closures are identical; shared filter selects full coverage.

| Job | Status | Evidence |
|---|---|---|
| runner-policy | LOCAL-PASS | Original CI script and pull_request author environment; exit 0; required-ci/runner-policy.stdout. |
| changes | LOCAL-PASS | Closure tool exit 0, shared filter and suite_mode full; required-ci/closure-fork.stdout. |
| credential-provider-windows | DEFERRED-TO-PR-CI reason=non-linux-runner | Actual Windows runner; import closure has no changed files. |
| pack-gate | LOCAL-PASS | Original bundled pack pin plus live-registry smoke commands, both exit 0 with GC_TEST_GASCITY_PACKS_REGISTRY=main; required-ci/pack-gate.stdout. |
| windows-lane | LOCAL-PASS | All four original Windows/Darwin CI cross-build/test-compile commands and exact environments, sequential in fresh views through load/isolation/detached wrappers; required-ci/windows-lane.stdout. |
| windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | Native Windows allowlist closure has no changed files; actual runner is Windows. |

- Paths above are under `/var/tmp/gc-gate-ga-b0olf8jc-2q25i8a8`. All three raw CI-job output hashes and terminal command results verified.
- `windows_reach: none`, both computations exit 0; `deferred_to_pr_ci: credential-provider-windows (non-linux-runner), windows-lane-test (non-linux-runner)`; `not_covered: none`.
- Merge requires CI / required and both deferred Windows jobs successful on the current head.

## Retained audits

- `/var/tmp/gc-gate-ga-b0olf8jc-2q25i8a8/suite-completion-audit.json` — SHA256 `4df090dd2b1f83a7e5798ba240537b0adc4b49fcadb399254133b6ad0d92e46f`.
- `/var/tmp/gc-gate-ga-b0olf8jc-2q25i8a8/remote-independent-audit.json` — SHA256 `b41216d66a0d5ce4fbd0afcfa04c5d4cec4f09da88bbdb75edc112b5cb326eba`.
- `/var/tmp/gc-gate-ga-b0olf8jc-2q25i8a8/skip-source-audit/evidence.json` — SHA256 `e9121f558bfb748ccc93284b4ca26c65e323d005da1f1c95ef3487458ea46eca`.
- `/var/tmp/gc-gate-ga-b0olf8jc-2q25i8a8/base-skip-evidence-corrected/report.json` — SHA256 `aa06be575cc72bd00b02d189ef0d5afe22e47645016814b930954e492493f13b`.
- `/var/tmp/gc-gate-ga-b0olf8jc-2q25i8a8/required-ci/evidence.json` — SHA256 `358db31001553f43786037c0b7331c434aa6e73f369e602047c1fd70b8f893a1`.
- `/var/tmp/gc-gate-ga-b0olf8jc-2q25i8a8/windows-accounting/reach.json` — SHA256 `69c97aa429dea1a7bf1a5f40b3cabe85ad123a02b0e0ffe08ce7effda69b3caf`.
- `/var/tmp/gc-gate-ga-b0olf8jc-2q25i8a8/publication-audit.json` — SHA256 `4df090dd2b1f83a7e5798ba240537b0adc4b49fcadb399254133b6ad0d92e46f`.

Exact bounded BdStore reads fetch the full matching candidate set before the canonical cut; this correctness cost is intentional. Callers accepting backing ties retain the bounded opt-in. Other backends and consumer implementations are unchanged.
