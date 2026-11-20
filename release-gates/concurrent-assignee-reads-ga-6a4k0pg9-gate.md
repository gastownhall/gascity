# Release gate: concurrent multi-assignee reads

**Verdict:** **PASS**

Bead: `ga-6a4k0pg9`; build `ga-59slarrd`; review `ga-0g1lebra`.
Reviewed source: `ee024ead00bf89aad76ca7e15d87e57dc62d2a19`. Canonical clean merge tested: `061d0188f46c48e3520bc718c19b00718c77a2f7`; tree `7247ffbbd2e608c47f5d0260306ead5cf188e01c`.
Pinned gate base: `67617358b143bbd00be000d615133fa1aea42da6` (`origin/main`). Deploy mode: remote; push target: fork.
Evidence root: `/var/tmp/gc-gate-ga-6a4k0pg9-ptenagx0`. Evaluated 2026-11-14T06:01:12.052916+00:00.

| # | Result | Evidence |
|---|---|---|
| 1 | PASS | Review ga-0g1lebra binds PASS to resolved source; no carryover or reviewer-added-test repin. |
| 2 | PASS | Mayor gcg-8275969 explicitly replaces build ASK2/4 with deterministic serial-depth RED/GREEN, interleaved improvement and independent clear-probe latency PASS. All accepted asks mapped below. |
| 3 | PASS | Full five-phase fresh remote suite, mandatory native floor, fast/generated lanes and all six CI leaf accounts complete. No runtime failure attribution or waiver. |
| 4 | PASS | No unresolved HIGH review finding; per-List limit, partial diagnostic ordering and panic containment advisories documented. |
| 5 | PASS | Reviewed source and canonical test merge clean. Only this gate record is added for publication; clean committed branch and normal hooks required before push. |
| 6 | PASS | Clean canonical merge, full Bazel build and make vet/nogo PASS before fast lanes/runtime. |
| 7 | PASS | One commit, three internal/beads files, +251/-19; bounded concurrent list reads only. |

## Merge and acceptance evidence

merge_state: clean (origin/main@67617358b143bbd00be000d615133fa1aea42da6)
gate_base_kind: current-tip
merge_validation: MPR validates the merge result at merge time; this gate never rebases.
Preflight found no associated source PR; C6 full build/vet evidence is c6-run-output.log, c6-build.bep.json and c6-vet.bep.json. No manifest file exists at docs/PROJECT_MANIFEST.md; commands derive from TESTING.md, Makefile, the merged CI workflow and the release-gate protocol.

Acceptance scope: mayor message gcg-8275969 was read and retained as acceptance-scope-ruling.txt. Literal baseline one-second overrun reproduction was NOT achieved. This is an explicit revised acceptance criterion; no budget, statistic or probe change and no waiver.

- ASK1: 1s budget, fastest-of-7 and probe threshold 50 unchanged; only internal/beads production/tests differ.
- Revised ASK2/4(a): retained investigator RED logs on serial base report 1 of 3 in flight and cap peak1 instead 4. Fresh full unit and integration-package logs verify all five owned tests PASS, including concurrency, cap 4, input-order merge, predicates and TierBoth.
- Revised ASK2/4(b): investigator interleaved registered-lane B/C evidence has mail fastest median 333 ms -> 218 ms, disjoint ranges 317–471 ms -> 206–227 ms; prime and drain improve too. Baseline 5/5 PASS, so this is improvement evidence and never a reproduced overrun.
- Revised ASK2/4(c): independent phase 5 probe clear after 20 s at worker load1=46.93 on 32 CPUs. Mail fastest/median/slowest 205/226/261 ms; prime 203/245/265 ms; drain 197/235/276 ms. Both registered budget tests PASS first attempt; phase 4 runs neither.
- ASK3: a four-slot semaphore starts per-index reads; WaitGroup joins before ordered merge. Typical three-route inbox removes two serial round trips. First copy/error/partial handling remains in input order. Tests use synctest, with no new sleep/target/census bump.
- ASK5: no budget-relaxation call. Tracker ga-y39d8tx stays OPEN after landing, per mayor: close only once BOTH ga-76grt0lk and ga-ccxarc1h re-gate fresh main containing the fix and clear-probe latency PASS. Record/mail recurrence without retry.
- Residuals: first-partial-wins diagnostic is not pinned; worker panic containment and a global concurrent-List limit remain advisory. Existing runner concurrent-call audit is on review bead; no race lane is claimed. Full acceptance audit retained in acceptance-audit.txt.

## Criterion 3 evidence

test_cmd: /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-criterion3.sh --repo /var/tmp/gc-gate-ga-6a4k0pg9-ptenagx0/merge --base 67617358b143bbd00be000d615133fa1aea42da6 --bead-json /var/tmp/gc-gate-ga-6a4k0pg9-ptenagx0/bead.json --owned /var/tmp/gc-gate-ga-6a4k0pg9-ptenagx0/owned.tsv --run-dir /var/tmp/gc-gate-ga-6a4k0pg9-ptenagx0/criterion3-run/evidence
test_cmd_scope: full-suite
test_exec_mode: remote-exec (BEP-derived; no mixed/local stitching)
test_exec_bep: {"acceptance": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 25, "results": 25, "retried": 0, "window_ok": true}, "integration-full": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 14, "results": 14, "retried": 0, "window_ok": true}, "integration-latency": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 1, "results": 1, "retried": 0, "window_ok": true}, "integration-packages": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 86, "results": 86, "retried": 0, "window_ok": true}, "unit": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 415, "results": 415, "retried": 0, "window_ok": true}}
rbe_state: eligible, mayor-enabled own-code remote opt-in; runtime-mode.txt and city commit in criterion3-start.json.
fallback_reason: none
admission: /var/tmp/gc-gate-ga-6a4k0pg9-ptenagx0/criterion3-run/evidence/remote/admission.json; remote cap 2, provisional 25 GiB; native floor retains bounded soft load wait.
farm_test_seconds: 13079.363
test_log_dir: /var/tmp/gc-gate-ga-6a4k0pg9-ptenagx0/criterion3-run/evidence/remote/logs
test_counts: PASS=154406 FAIL=0 SKIP=606
Counts use top-level verdict lines, repeated per job; subtests excluded. No no-test-match run used as owned evidence.
diff_tests_executed: TestBdStoreListAssigneesFanOutBoundsConcurrentReads PASS (integration-packages-20cfdbabba8f52b16e0b.log, unit-b398920224b769833cd5.log); TestBdStoreListAssigneesFanOutCombinesInAssigneeOrder PASS (integration-packages-a9e2a6fbbc2774327994.log, unit-ea4511196531bb4b8c55.log); TestBdStoreListAssigneesFanOutReadsConcurrently PASS (integration-packages-ceb32a3c147a95796597.log, unit-9a6a09bab414814cab53.log); TestBdStoreListAssigneesMultipleFansOutPerAssignee PASS (integration-packages-a1f48694841e1859d61a.log, unit-a93729df64a652fced43.log); TestBdStoreListBothTiersAssigneesMultipleFansOutTheListLegOnly PASS (integration-packages-8d820cd4c52192de043e.log, unit-3ee1ae2eaa26dfd63a88.log)
waiver_ref: none
failure_attribution: none
heavy_mode: none
ci_lane_run: n/a (no CI-config diff)
policy_lane: PASS — pinned format, full C6 nogo/vet, four make-check guards and all policy Bazel targets; fast-run-output.log.
drift_lane: PASS — make bazel-sync plus empty diff/status; schema/spec/client/dashboard/lockfile drift and pinned-base OpenAPI checks in separate fresh views; precheck-results.json.

| Phase | Fresh remote results | Verdict |
|---|---:|---|
| unit | 415 | PASS |
| acceptance | 25 | PASS |
| integration-packages | 86 | PASS |
| integration-full | 14 | PASS |
| integration-latency | 1 | PASS |

floor_topup: complete; 16 native jobs, each exit 0; /var/tmp/gc-gate-ga-6a4k0pg9-ptenagx0/criterion3-run/evidence/remote/floor-topup.json.
owned_topup: none needed; all five owned tests PASS by name in full unit and integration-package phases.
latency_leg: first attempt measured under clear probe; both registry tests PASS; main integration leg excludes them.
skip_inventory: /var/tmp/gc-gate-ga-6a4k0pg9-ptenagx0/criterion3-run/evidence/remote/skip-inventory.json has 592 rows; /var/tmp/gc-gate-ga-6a4k0pg9-ptenagx0/floor-skip-inventory.json has 72 native rows including subtests.
skip_justification: no owned SKIP. Conservative pinned-base comparisons cover all farm skip phases: COMPARE-SUMMARY head_skips=433 judged=433 pre-existing=433 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING; COMPARE-SUMMARY head_skips=14 judged=14 pre-existing=14 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING; COMPARE-SUMMARY head_skips=131 judged=131 pre-existing=131 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING; COMPARE-SUMMARY head_skips=14 judged=14 pre-existing=14 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING.
All 318 candidate farm skip sites checked UNCHANGED, line_in_hunk=no; ambiguous helper locations checked at every candidate path. Four shared hunks: two imports, new fanOutHoldRunner/listWithHeldReads used only by owned tests, and test-only constant export used by the cap test. No skipped test calls a changed test helper; production reach is supported by pinned-base equality rather than suffix reasoning.
Supplemental baseline caveat: unit proof helper was mistakenly given //..., so its Go -test.v argument caused 15 non-Go runner invocation failures. The entire supplemental command FAILED and receives no passing-suite credit; no failed target was rerun or attributed. Its actual Go verdicts have 0 FAIL and official comparison 433/433 identical SKIPs. Primary full candidate phases independently PASS, and remaining supplemental Go-only phases PASS separately. Raw failed evidence and base-skip-unit-proof.json retained.
Native SKIP reason delta: 7 rows differ from farm reasons: one cmd/gc live-herdr journey and six internal/runtime/herdr journeys take unchanged early unit-lane guards after live GC selectors are scrubbed, instead of the farm missing-herdr reason. All 45 native sites are UNCHANGED. Full messages in floor-skip-reason-delta.json; these are non-owned inventoried native top-up SKIPs, no test failure.
Environment: pinned bd1.3.4; ICU74 check OK. Wrapper explicitly reported PASS-THROUGH, not a cloned isolated environment. Standard runner and native-floor code scrub live GC/BD/BEADS/DOLT store selectors; no wrapper-isolation claim.

## Load and CI job accounts

native-floor: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=56.01 run_max_load=63.04 run_mean_load=58.93 run_readings=9 wait_first_load=61.75 wait_max_load=82.56 wait_mean_load=57.63 wait_readings=61 read_errors=0`
load_threshold: 15; load_waited_seconds: 1801; load_wait_timed_out: 1; run_start_load: 56.01; run_max_load: 63.04; run_mean_load: 58.93; run_readings: 9 (native-floor).
windows-lane: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=60.92 run_max_load=56.68 run_mean_load=52.80 run_readings=3 wait_first_load=49.64 wait_max_load=151.62 wait_mean_load=100.94 wait_readings=62 read_errors=0 prewait_s=1801`
load_threshold: 15; load_waited_seconds: 1801; load_wait_timed_out: 1; run_start_load: 60.92; run_max_load: 56.68; run_mean_load: 52.80; run_readings: 3 (windows-lane).
pack-gate: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=19.05 run_max_load=20.66 run_mean_load=20.06 run_readings=4 wait_first_load=44.75 wait_max_load=85.37 wait_mean_load=49.36 wait_readings=62 read_errors=0 prewait_s=1801`
load_threshold: 15; load_waited_seconds: 1801; load_wait_timed_out: 1; run_start_load: 19.05; run_max_load: 20.66; run_mean_load: 20.06; run_readings: 4 (pack-gate).

ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-gate-ga-6a4k0pg9-ptenagx0/merge 67617358b143bbd00be000d615133fa1aea42da6 ee024ead00bf89aad76ca7e15d87e57dc62d2a19 fork
workflow_blob: fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612
ci_required_leaf_count: 6
ci_required_accounting:

| Job | Status | Evidence |
|---|---|---|
| runner-policy | LOCAL-PASS | command: EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=<recorded file> python3 .github/workflows/scripts/runner_policy.py; exit: 0; log: /var/tmp/gc-gate-ga-6a4k0pg9-ptenagx0/ci-required/runner-policy.log |
| changes | LOCAL-PASS | command: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-gate-ga-6a4k0pg9-ptenagx0/merge 67617358b143bbd00be000d615133fa1aea42da6 ee024ead00bf89aad76ca7e15d87e57dc62d2a19 fork; exit: 0; log: /var/tmp/gc-gate-ga-6a4k0pg9-ptenagx0/ci-required/closure.txt |
| credential-provider-windows | DEFERRED-TO-PR-CI reason=non-linux-runner | {"runner": "blacksmith-4vcpu-windows-2025", "reach": "none"} |
| windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | {"runner": "blacksmith-4vcpu-windows-2025", "reach": {"reach": "none", "packages": ["./internal/credentialprovider", "./internal/genshell/..."]}} |
| windows-lane | LOCAL-PASS | command: /home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-gate-ga-6a4k0pg9-ptenagx0/merge -- /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-admission.py --class local-heavy --record /var/tmp/gc-gate-ga-6a4k0pg9-ptenagx0/ci-required/windows-lane-admission.json --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh --class local-heavy --threshold 15 --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- bash /var/tmp/gc-gate-ga-6a4k0pg9-ptenagx0/ci-windows-lane.sh; exit: 0; log: /var/tmp/gc-gate-ga-6a4k0pg9-ptenagx0/ci-required/windows-lane-output.log |
| pack-gate | LOCAL-PASS | command: /home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-gate-ga-6a4k0pg9-ptenagx0/merge -- /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-admission.py --class local-heavy --record /var/tmp/gc-gate-ga-6a4k0pg9-ptenagx0/ci-required/pack-gate-admission.json --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh --class local-heavy --threshold 15 --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- bash /var/tmp/gc-gate-ga-6a4k0pg9-ptenagx0/ci-pack-gate.sh; exit: 0; log: /var/tmp/gc-gate-ga-6a4k0pg9-ptenagx0/ci-required/pack-gate-output.log |

windows_reach: {"credential-provider-windows": "none", "windows-lane-test": {"packages": ["./internal/credentialprovider", "./internal/genshell/..."], "reach": "none"}}
deferred_to_pr_ci: credential-provider-windows (non-linux-runner), windows-lane-test (non-linux-runner)
not_covered: none
host_gaps: none
CI Linux commands: windows-lane executes all four merged run steps in their declared directories. pack-gate executes each exact run step in a separate subshell, with step env GC_TEST_GASCITY_PACKS_REGISTRY=main for the live smoke. Both behind serial fresh-tree/admission/load/wrapper views. Commands, status and output retained in accounting.json and *-output.log. Network probe command/rc/stdout/stderr retained verbatim in accounting.json.
Gate PASS authorizes PR-open. Merge requires CI / required green on current head and each deferred job success (or its own false path filter). Deployer does not merge.

## Evidence integrity

- /var/tmp/gc-gate-ga-6a4k0pg9-ptenagx0/criterion3-run/evidence/execution.json: 320917 bytes; SHA256 edf4eb8f61fa30ecbce72397f1efb0138257c12e2f974598a1e0e8ebb7834ba4.
- /var/tmp/gc-gate-ga-6a4k0pg9-ptenagx0/ci-required/accounting.json: 5324 bytes; SHA256 3227afd6c857ece07c13b9991c2f2d1517bdb99cd6823d7866f6296ca77ab2ea.
- /var/tmp/gc-gate-ga-6a4k0pg9-ptenagx0/final-by-name.txt: 1441 bytes; SHA256 195525ce6b5ef01ca0c087e770a9d10a0a9e0dfae44594633d1ee70d26dfbba1.
- /var/tmp/gc-gate-ga-6a4k0pg9-ptenagx0/acceptance-audit.txt: 4183 bytes; SHA256 17be0674e24f51d09b90f8ecc9e03b58b51e3bf7eb14242336e823c934c6623b.
- /var/tmp/gc-gate-ga-6a4k0pg9-ptenagx0/acceptance-scope-ruling.txt: 1243 bytes; SHA256 d6d350ec6863ee0ed6add1d20a79ff8c7021e17d2080197b7574778d2481793e.
- /var/tmp/gc-gate-ga-6a4k0pg9-ptenagx0/base-skip-unit-proof.json: 3239 bytes; SHA256 ae1db9259dc58e16d2cd132e19dbe47cf4cbb2c2b8f358661e63b2a32cf34857.
- /var/tmp/gc-gate-ga-6a4k0pg9-ptenagx0/base-skip-remaining-comparison.json: 8252 bytes; SHA256 2ea24d83a3f0561f9bb5020d343d7e0bffaf2717b89f86292c0e952b23f481f4.
- /var/tmp/gc-gate-ga-6a4k0pg9-ptenagx0/remote-skip-site-proof.json: 40380 bytes; SHA256 768a2bbd50716f05ae5ed0a60f0e3e0da389d55f43e4bff36ac1f57787aea086.
- /var/tmp/gc-gate-ga-6a4k0pg9-ptenagx0/floor-skip-site-proof.json: 3444 bytes; SHA256 9de2973c3eae4e418d7e995cf54f28926da8f158789f8e69cd4d3321fb20288c.
- /var/tmp/gc-gate-ga-6a4k0pg9-ptenagx0/floor-skip-inventory.json: 28839 bytes; SHA256 b5a242f3cde2c09d7693b4757e390d3ac5d1c65023c4a1ebb370f227b17503a4.
- /var/tmp/gc-gate-ga-6a4k0pg9-ptenagx0/floor-skip-reason-delta.json: 2795 bytes; SHA256 aee02f3a2ea1f71b5169cbef24e4d1e15be7c4fb3292401bb0aafdc26b921da2.
