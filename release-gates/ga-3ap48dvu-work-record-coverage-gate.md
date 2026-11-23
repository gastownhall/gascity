# Work-record coverage report release gate

**Verdict:** **PASS**

- Bead: `ga-3ap48dvu`; build lineage `ga-387c0b`, `ga-lom03n`; review `ga-5tah9urc` (round 2 PASS, no unresolved HIGH finding).
- Reviewed source: `3cb4aff6fa79b4d4ed5cc9b6956192a9f7d7ccb0`; branch `builder/ga-387c0b` is provenance only.
- Gated merge: `197d7bfaabd401497aa6becdbdb20a404c026dbc`; tree `c8fbef12da9d8d0576b507d43d55fc13d967d034`.
- `merge_state: clean (origin/main@a175518ea6436c3b64c702cc70e7f4de2fa4f754)`; `gate_base_kind: current-tip`.
- Publication freshness: merge-tree remains clean against `origin/main@a47ce14d1d187fc17fa8f56d12e5d5e59874f655`; command-census files unchanged since the gate pin. No rebase performed.
- `merge_validation: merge authority must validate current merge result and current-head CI; this gate authorizes PR-open only`.

| # | Criterion | Result and evidence |
|---|---|---|
| 1 | Review PASS present | PASS — live review metadata names the reviewed source; round 2 PASS. |
| 2 | Acceptance met | PASS — read-only closed-bead coverage report, shared Gated/ValidOutcome rules, table and JSON output, city/limit flags, negative-limit rejection, schema and generated census checked against source and named tests. |
| 3 | Tests and required lanes | PASS — full canonical five-phase remote chain, native floor, unchanged skip audit and six-job CI accounting below. No waiver. |
| 4 | No unresolved HIGH findings | PASS — reviewer security/spec/style notes and acceptance walk contain no unresolved HIGH finding. |
| 5 | Final branch clean | PASS — isolated deploy branch carries only the reviewed source plus this administrative gate record; clean status verified before publication. |
| 6 | Merge state and build/vet | PASS — raw merge tree and parents match the pin; whole-tree build and lint/nogo BEPs finish successfully. |
| 7 | One feature theme | PASS — one work-record reporting feature; schema, generated command census and documentation support that command. |

## Test evidence

- `test_cmd`: `gate-criterion3.sh --repo /var/tmp/gc-merge-validate.Z2X64n --base a175518ea6436c3b64c702cc70e7f4de2fa4f754 --bead-json /var/tmp/gc-gate-ga-3ap48dvu-svqr7144/c3-bead.json --owned /var/tmp/gc-gate-ga-3ap48dvu-svqr7144/owned.tsv --run-dir /var/tmp/gc-gate-ga-3ap48dvu-svqr7144/c3-run/evidence`, launched through `gate-detached-run.sh`; remote admission, load gate and isolation wrapper supplied by the canonical chain.
- `test_cmd_scope: full-suite`; unit (`make -k check`, including Bazel `//...`), acceptance, integration-packages, full integration without latency tests, then the isolated latency phase.
- `test_counts: PASS=158711 FAIL=0 SKIP=618`; `waiver_ref: none`; `failure_attribution: none`.
- `test_exec_mode: remote-exec`; `fallback_reason: none`; five BEPs: remote results 426/25/87/15/1, no cached test results, attempt 1 and valid phase windows; 1108 retained artifact hashes verified.
- `test_exec_bep`: `/var/tmp/gc-gate-ga-3ap48dvu-svqr7144/c3-run/evidence/remote/{unit,acceptance,integration-packages,integration-full,integration-latency}.bep.json`.
- `rbe_state`: enabled and valid certificate; source/base worker environment hashes equal. `admission`: remote, floor 25 GiB, cap 2, admitted with 48.507 GiB free. `farm_test_seconds: 13891.25`.
- `heavy_mode: none`; `delta_gate: shadow/full (not-gate-pr)`, no narrowing.
- `floor_topup`: complete, all 16 steps exit 0 (every environment-absence package, beadstest, Darwin compile and runner self-tests). `owned_topup: []`.
- `latency_leg`: measured, probe clear; TestGcBdLatencyBudget and TestGcSeatHookLatencyBudget PASS.
- `skip_inventory`: 604 farm events including subtests (514 other, 90 environment-absence); all environment-absence packages received the native floor. Across the complete logs, 676 skip events including subtests resolve to 319 UNCHANGED sites; every printed source file is byte-identical to the pinned base, and SHARED-HUNKS/HELPER-HUNKS are absent.
- `skip_justification`: existing unit/process-lane separation, platform/provider capabilities, explicitly pending fixtures and optional runtime/environment requirements. No owned test or subtest skipped. The report changes no skip predicate or shared skip helper; no base skip measurement required by the unchanged-site rule.
- `test_log_dir`: `/var/tmp/gc-gate-ga-3ap48dvu-svqr7144/c3-run/evidence/remote/logs`; logs retained.

```text
diff_tests_executed:
  TestAnalyzeWorkRecordFromStoreJSON PASS (integration-packages-12697e2ea0d32c278115.log, unit-27b23e708b477fce4dad.log)
  TestAnalyzeWorkRecordFromStoreOnlyScansClosed PASS (integration-packages-9326181a736eb30b71ca.log, unit-637c74c578a2b17fc654.log)
  TestAnalyzeWorkRecordFromStoreTable PASS (integration-packages-aa6cf5c138f0b61b866a.log, unit-3d6d3036a2f5902472b5.log)
  TestNewAnalyzeCmdRegistersWorkRecordSubcommand PASS (integration-packages-56f04454dd9158a9ee8a.log, unit-423c6a0da1986b90e9a7.log)
  TestRunAnalyzeWorkRecordRejectsNegativeLimit PASS (integration-packages-e4245ddf6a37030111bc.log, unit-1ab526405ace21879f85.log)
  TestAnalyzeCoverage PASS (unit-8faef004ddcd08190515.log)
  TestAnalyzeCoverageAllCovered PASS (unit-8faef004ddcd08190515.log)
  TestAnalyzeCoverageEmpty PASS (unit-8faef004ddcd08190515.log)
  TestFormatTable PASS (unit-8faef004ddcd08190515.log)
  TestFormatTableNoMissing PASS (unit-8faef004ddcd08190515.log)
  TestFormatTableZeroTotal PASS (unit-8faef004ddcd08190515.log)
  TestValidOutcome PASS (unit-8faef004ddcd08190515.log)
GATE_TEST_EVIDENCE_RESULT ok: 12 diff-owned tests verified by name
```

## Fast lanes and load

- `policy_lane`: whole-tree lint/nogo, make shell/hook guards and required Bazel policy targets PASS; strict storeclass predicates built from the trusted base and both baseline/growth checks exit 0.
- `drift_lane`: fresh-view `make bazel-sync` + clean git diff/status; schema/spec/client/dashboard lockfile and artifact sync targets PASS. Fresh-view formatting and OpenAPI breaking checks PASS.
- Complexity zero growth rule (ii): base and merge `make complexity-check` exit 2 with no worse merge row; pinned-base complexity-diff has no threshold changes. Baseline untouched; existing condition is ga-q7p6is.
- `ci_lane_run: n/a (no CI-config change)`; no API/spec/dashboard change requiring a local preview.

```text
strict-policy: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=50.56 run_max_load=49.61 run_mean_load=48.16 run_readings=5 wait_first_load=30.19 wait_max_load=78.55 wait_mean_load=49.39 wait_readings=61 read_errors=0
native-floor: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=60.35 run_max_load=65.40 run_mean_load=56.73 run_readings=8 wait_first_load=68.32 wait_max_load=68.32 wait_mean_load=35.34 wait_readings=61 read_errors=0
runner-policy: LOAD_GATE_SUMMARY threshold=15 waited_seconds=660 wait_timed_out=0 run_start_load=14.41 run_max_load=NA run_mean_load=NA run_readings=0 wait_first_load=44.15 wait_max_load=44.15 wait_mean_load=25.23 wait_readings=23 read_errors=0
windows-lane: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=14.99 run_max_load=21.39 run_mean_load=18.52 run_readings=4 wait_first_load=14.99 wait_max_load=14.99 wait_mean_load=14.99 wait_readings=1 read_errors=0
```

## Required CI accounting

- `ci_required_closure`: `python3 $GC_CITY_ROOT/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-validate.Z2X64n a175518ea6436c3b64c702cc70e7f4de2fa4f754 3cb4aff6fa79b4d4ed5cc9b6956192a9f7d7ccb0 origin`; merged workflow blob `fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612`; `6 LEAF jobs need a status` (the aggregate itself takes none).

| Job | Status | Evidence |
|---|---|---|
| runner-policy | LOCAL-PASS | CI script with EVENT_NAME=pull_request, PR_AUTHOR=quad341, FORCE_BLACKSMITH empty, GITHUB_OUTPUT file; exit 0; `/var/tmp/gc-gate-ga-3ap48dvu-svqr7144/required-ci/runner-policy.stdout`. |
| changes | LOCAL-PASS | Closure tool exits 0: packs/credential_provider/shared filters false; suite_mode filtered; `/var/tmp/gc-gate-ga-3ap48dvu-svqr7144/required-ci/closure.stdout`. |
| credential-provider-windows | NOT-TRIGGERED | Tool row: NON-LINUX; credential_provider filter false. |
| pack-gate | NOT-TRIGGERED | Tool row: packs filter false. |
| windows-lane | LOCAL-PASS | All four original Windows/Darwin CI build/test-compile commands, unchanged env and arguments, exit 0 sequentially through fresh view, load gate, isolation and detached runner; `/var/tmp/gc-gate-ga-3ap48dvu-svqr7144/required-ci/windows-lane.stdout`; exact commands in `windows-lane-steps.json` beside it. |
| windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | Runner-policy resolves runner_windows to blacksmith-4vcpu-windows-2025. Native test allowlist import closure computed with GOOS=windows: no changed files. |

- `windows_reach: none` for both credential-provider and the native Windows allowlist; both closure computations exit 0.
- Publication target: origin (the guarded dry-run succeeded); origin-head closure recomputed and identical to the earlier fork-head accounting.
- `deferred_to_pr_ci: windows-lane-test (non-linux-runner)`; `not_covered: none`.
- Merge requires CI / required green on the current PR head and the deferred native Windows job successful.

## Retained audits

- `/var/tmp/gc-gate-ga-3ap48dvu-svqr7144/suite-completion-audit.json` — SHA256 `56d5b0a8a16f5dd441e083595ae70b92589dd3848496f712afc311df6a64e845`.
- `/var/tmp/gc-gate-ga-3ap48dvu-svqr7144/skip-site-audit/full-evidence.json` — SHA256 `42732ce3f4e03072714d4bb4bb3a20a86154bf1eae62176621a523448d29a6d6`.
- `/var/tmp/gc-gate-ga-3ap48dvu-svqr7144/required-ci/evidence.json` — SHA256 `5546678d08a6171c08b6c565d8a782ab5ea836278071b2677fe7e731beb92f6e`.
- `/var/tmp/gc-gate-ga-3ap48dvu-svqr7144/windows-accounting/reach.json` — SHA256 `8822c785642929b5c0af629ca6f5010a7835b7ab620a2189a3d91cc35536eca6`.
- `/var/tmp/gc-gate-ga-3ap48dvu-svqr7144/publication-audit.json` — SHA256 `7546d4d08ef0c1005b0952a851e5bfee3edeb23245cdc77c2a73a1ed02b95ec0`.
- `/var/tmp/gc-gate-ga-3ap48dvu-svqr7144/source-acceptance-walk.json` — SHA256 `cdd609c10df78cbe8131364f3c8c1e6ec605ab9fb87a264f5da2abd84d9c6bd7`.

Known non-blocking store-layer issue: bounded bd-backed reads select priority-first rows until the separate ga-43kqzeix fix lands; `--limit 0` scans all closed beads. This report does not change the close enforcement seam. Supersedes delivery PR #5647.
