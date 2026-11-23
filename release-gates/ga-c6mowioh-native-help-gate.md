# Native command help release gate

**Verdict:** **PASS**

- Bead `ga-c6mowioh`; build `ga-f02lce.1`; review `ga-8q4fqytw`, round 1 PASS; no unresolved HIGH findings.
- Reviewed source `4e3b4df6efc40aa392817bf2990bbd5e4595a6b1`; source branch `builder/ga-f02lce.1` is provenance only.
- Gated merge `7849be9d9f00923d9c42c2433d9c335e4966c39a`; tree `a34fbb5849c691963168bab7a749b521e7b7e81b`.
- `merge_state: clean (origin/main@f4627bc5a305450b2cb93967ae631324c9a9ce22)`; `gate_base_kind: current-tip`.
- Publication check: clean merge against `origin/main@a47ce14d1d187fc17fa8f56d12e5d5e59874f655`; pack schema files unchanged since the gate pin. No rebase.
- `merge_validation: merge authority checks the current merge result and current-head CI; this gate authorizes PR-open only`.

| # | Criterion | Result and evidence |
|---|---|---|
| 1 | Review PASS | PASS — reviewed source matches live review metadata and the independently resolved commit. |
| 2 | Acceptance met | PASS — optional native_help propagates through both command discovery paths; eager/lazy help uses the script with exactly --help; bounded execution and error/empty-output fallback; default remains static help. |
| 3 | Tests and required lanes | PASS — complete canonical suite, six owned tests by name, native floor, all required skip comparisons and six-LEAF CI accounting below. |
| 4 | No unresolved HIGH findings | PASS — reviewer security/spec/style notes and acceptance walk contain none; opt-in help execution is the reviewed design. |
| 5 | Final branch clean | PASS — isolated deploy branch; only the administrative gate record is added to the reviewed source; clean status verified before publication. |
| 6 | Merge state and build/vet | PASS — raw merge tree/parents match pin; whole-tree build and explicit lint/nogo BEPs finish successfully. |
| 7 | One feature theme | PASS — opt-in command help with config propagation, generated schema and regression tests. |

## Test evidence

- `test_cmd`: `gate-criterion3.sh --repo /var/tmp/gc-merge-validate.RmX4xT --base f4627bc5a305450b2cb93967ae631324c9a9ce22 --bead-json /var/tmp/gc-gate-ga-c6mowioh-9igbaw2d/c3-bead.json --owned /var/tmp/gc-gate-ga-c6mowioh-9igbaw2d/owned.tsv --run-dir /var/tmp/gc-gate-ga-c6mowioh-9igbaw2d/c3-run/evidence`, detached through `gate-detached-run.sh`, canonical admission/load/isolation wrappers.
- `test_cmd_scope: full-suite`; unit (`make -k check`, including Bazel `//...`), acceptance, integration-packages, full integration without latency tests, then separate latency phase.
- `test_counts: PASS=158724 FAIL=0 SKIP=618`; `waiver_ref: none`; `failure_attribution: none`.
- `test_exec_mode: remote-exec`; `fallback_reason: none`; five BEPs prove remote execution, no cached tests, attempt 1 and starts within phase windows; all retained log/XML artifact hashes verified.
- `test_exec_bep`: `/var/tmp/gc-gate-ga-c6mowioh-9igbaw2d/c3-run/evidence/remote/{unit,acceptance,integration-packages,integration-full,integration-latency}.bep.json`.
- `rbe_state`: enabled, valid certificate and source/base worker-env pins equal. `admission`: remote cap 2, floor 25.0 GiB; `farm_test_seconds: 13863.19`.
- `heavy_mode: none`; `delta_gate: shadow/full (not-gate-pr)`, no narrowing. `owned_topup: []`.
- `floor_topup`: complete; all 16 steps exit 0, including every environment-absence package, beadstest, Darwin compile and runner self-tests.
- `latency_leg`: complete and measured under a clear farm probe; both listed latency tests PASS.
- `skip_inventory`: 604 farm events including subtests; native floor reran all environment-absence packages. Complete output has 676 skip events including subtests across 319 unchanged source sites.
- `skip_justification`: existing lane/platform/provider/fixture/environment skips; no owned test or subtest skipped. The changed shared helpers in cmd/gc serve only the added native-help tests. Named base comparisons prove 4 simulation, 189 unit, 68 integration-package and 36 native-floor SKIP events pre-existing. Other skip files and shared skip helpers are unchanged.
- Site locations are anchored to the exact tested merge because three skipped tests were added on main after the source branch point; the six owned names re-derived at reviewed source and tested merge are identical. All sites have line_in_hunk=no and UNCHANGED verdicts.
- Initial base console reader refusal was an evidence-format issue: original raw artifacts were recovered through gate-rbe-evidence.py and the accepted test invocation was reused, without rerun. Original console and refusal retained.
- `test_log_dir`: `/var/tmp/gc-gate-ga-c6mowioh-9igbaw2d/c3-run/evidence/remote/logs`; base logs/comparisons retained under `/var/tmp/gc-gate-ga-c6mowioh-9igbaw2d/base-skip-evidence`.

```text
diff_tests_executed:
  TestAddDiscoveredCommandsToRoot_NativeHelp PASS (integration-packages-2ab73b52d8fd4dc346df.log, unit-f93a0ca96ccc61d0cae9.log)
  TestDiscoveredCommandFromPackCommandInfo_CarriesNativeHelp PASS (integration-packages-509ef0a6cd81866833cc.log, unit-088cc38d26d094729229.log)
  TestPackCommandTreeCandidateSnapshotFieldCountRatchet PASS (integration-packages-830685c60d0c2dd88567.log, unit-2ef13e1b6898c5544764.log)
  TestTryDiscoveredCommandFallback_NativeHelp PASS (integration-packages-c2ab4e7d4735e1bd66a7.log, unit-912ab643330b35bd0b6b.log)
  TestDiscoverPackCommands_NativeHelpFromManifest PASS (unit-4bbf1267e0c4ccc54042.log)
  TestLoadWithIncludes_LegacyPackTomlCommandNativeHelp PASS (unit-4bbf1267e0c4ccc54042.log)
GATE_TEST_EVIDENCE_RESULT ok: 6 diff-owned tests verified by name
```

```text
COMPARE-NOTE the remote worker's account differs between the runs in 1 SKIP(s), read as the same reason because the account is the only difference (an account is rbe-, a letter and two digits, standing alone; the logs are not changed): TestInfraScopedManagedDoltPIDIdentityUnderRealSystemd (base rbe-a09, head rbe-a05)
```

## Fast lanes and load

- `policy_lane`: make shell/hook guards, explicit Bazel policy targets, whole-tree lint/nogo and strict trusted-base storeclass baseline/growth checks PASS.
- `drift_lane`: separate fresh-view make bazel-sync + clean diff/status; schema/spec/client/dashboard artifact and lockfile sync targets PASS. Fresh-view formatting and OpenAPI breaking checks PASS.
- Complexity zero-growth rule (ii) PASS: pinned base and merge complexity-check exit 2 with no worse merge row; complexity-diff has no threshold changes; baseline untouched (existing condition ga-q7p6is).
- `ci_lane_run: n/a (no CI-config change)`; no API/spec/dashboard source change requiring a preview.

```text
strict-policy: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1802 wait_timed_out=1 run_start_load=24.02 run_max_load=23.40 run_mean_load=22.10 run_readings=3 wait_first_load=46.82 wait_max_load=92.23 wait_mean_load=55.20 wait_readings=61 read_errors=0
native-floor: LOAD_GATE_SUMMARY threshold=15 waited_seconds=450 wait_timed_out=0 run_start_load=14.97 run_max_load=20.16 run_mean_load=16.09 run_readings=7 wait_first_load=36.42 wait_max_load=36.42 wait_mean_load=24.01 wait_readings=16 read_errors=0
runner-policy: LOAD_GATE_SUMMARY threshold=15 waited_seconds=210 wait_timed_out=0 run_start_load=14.72 run_max_load=NA run_mean_load=NA run_readings=0 wait_first_load=20.80 wait_max_load=20.80 wait_mean_load=17.21 wait_readings=8 read_errors=0
windows-lane: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=14.72 run_max_load=19.51 run_mean_load=17.42 run_readings=3 wait_first_load=14.72 wait_max_load=14.72 wait_mean_load=14.72 wait_readings=1 read_errors=0
pack-gate: LOAD_GATE_SUMMARY threshold=15 waited_seconds=120 wait_timed_out=0 run_start_load=14.62 run_max_load=14.88 run_mean_load=14.08 run_readings=4 wait_first_load=19.41 wait_max_load=19.41 wait_mean_load=16.98 wait_readings=5 read_errors=0
base-native: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=13.49 run_max_load=15.63 run_mean_load=15.27 run_readings=2 wait_first_load=13.49 wait_max_load=13.49 wait_mean_load=13.49 wait_readings=1 read_errors=0
```

## Required CI accounting

- `ci_required_closure`: `python3 $GC_CITY_ROOT/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-validate.RmX4xT f4627bc5a305450b2cb93967ae631324c9a9ce22 4e3b4df6efc40aa392817bf2990bbd5e4595a6b1 origin`; merged workflow blob `fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612`; `6 LEAF jobs need a status`. Origin-head and fork-head closures are identical.

| Job | Status | Evidence |
|---|---|---|
| runner-policy | LOCAL-PASS | CI script with EVENT_NAME=pull_request, PR_AUTHOR=quad341, FORCE_BLACKSMITH empty, GITHUB_OUTPUT file; exit 0; `/var/tmp/gc-gate-ga-c6mowioh-9igbaw2d/required-ci/runner-policy.stdout`. |
| changes | LOCAL-PASS | Tool exits 0; packs/shared filters true, suite_mode full; `/var/tmp/gc-gate-ga-c6mowioh-9igbaw2d/required-ci/closure-origin.stdout`. |
| credential-provider-windows | DEFERRED-TO-PR-CI reason=non-linux-runner | Triggered by shared filter; runner_windows resolves to blacksmith-4vcpu-windows-2025; Windows import closure has no changed files. |
| pack-gate | LOCAL-PASS | Original bundled pack pin and live registry smoke commands with GC_TEST_GASCITY_PACKS_REGISTRY=main; both exit 0; `/var/tmp/gc-gate-ga-c6mowioh-9igbaw2d/required-ci/pack-gate.stdout`. |
| windows-lane | LOCAL-PASS | All four original Windows/Darwin CI build/test-compile commands, exact env/arguments, exit 0 sequentially behind fresh view, load gate, isolation and detached runner; `/var/tmp/gc-gate-ga-c6mowioh-9igbaw2d/required-ci/windows-lane.stdout`. |
| windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | Native Windows allowlist import closure has no changed files; actual runner label is Windows. |

- `windows_reach: none` for credential-provider and the native Windows allowlist; both computations exit 0.
- `deferred_to_pr_ci: credential-provider-windows (non-linux-runner), windows-lane-test (non-linux-runner)`; `not_covered: none`.
- Merge requires CI / required and both deferred native Windows jobs successful on the current head.

## Retained audits

- `/var/tmp/gc-gate-ga-c6mowioh-9igbaw2d/suite-completion-audit.json` — SHA256 `ed28dcda54bea9cc1dd10d462d2a3a0a60ae012778eb7727c5a0a7542eaf0533`.
- `/var/tmp/gc-gate-ga-c6mowioh-9igbaw2d/remote-independent-audit.json` — SHA256 `d93b6e7a9abe878fc9ec83925805b7676ca827ab41d8a146fc8a9fa56775fd7d`.
- `/var/tmp/gc-gate-ga-c6mowioh-9igbaw2d/skip-source-audit/evidence.json` — SHA256 `5c73b45d42b073dd7950cc8bd5669ec745c72e46ca9e38cb7dc4965c1e03e75d`.
- `/var/tmp/gc-gate-ga-c6mowioh-9igbaw2d/base-skip-evidence/report.json` — SHA256 `95ae3340e4a7d20e1f800a603d60c81a6b2af864828dc22f7219f8fbb311874b`.
- `/var/tmp/gc-gate-ga-c6mowioh-9igbaw2d/required-ci/evidence.json` — SHA256 `f534eba6c11f73284eb9f63fe44a43e23b234fabcc099e03751305e33f57e38d`.
- `/var/tmp/gc-gate-ga-c6mowioh-9igbaw2d/windows-accounting/reach.json` — SHA256 `4aa615b958e3bdc5f01fd8204ccd184095523ce52cb4b740c62665c461a0c2a2`.
- `/var/tmp/gc-gate-ga-c6mowioh-9igbaw2d/publication-audit.json` — SHA256 `225ab6d9b557eef524adba2baa24835cd378236299bebeeaae8cd13eadc730a9`.

No existing pack command is opted in by this change. Consumer adoption is a separate post-landing decision. Non-blocking prose follow-up: ga-1rwfyg9c (pack-spec field row).
