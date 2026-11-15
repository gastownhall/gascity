# Release gate: pinned-clock demand recording

**Verdict:** **PASS**

Bead: `ga-52zc9y50`; build `ga-a6xq8yq5`; review `ga-cpbpui68`.
Reviewed source: `4e723483c8f1e8b711d07631fb991b435a1d0abc`. Canonical merged test commit: `194341e3af96283ca0f1237d859987822bac276c`; tree `7631f497565915cdd16c2321ddb370cdf095d911`.
Gate base: `164658a1a467bfe8bd614eee16ea76a899fcc3f2` (`origin/main`, pinned before testing). Deploy mode: remote; push remote: fork.
Evidence root: `/var/tmp/gc-gate-ga-52zc9y50-d1761nvu`. Evaluated 2026-11-14T04:16:04.379223+00:00.

| # | Result | Evidence |
|---|---|---|
| 1 | PASS | Review verdict PASS is bound to the resolved source SHA. No carryover or reviewer-added-test repin. |
| 2 | PASS | Six exit-contract checks mapped below; all assertions retained and both owned tests PASS by name. |
| 3 | PASS | Fresh full unit, acceptance, integration-package, integration and separate latency phases; mandatory native floor and CI job accounting completed. No failure attribution or waiver used. |
| 4 | PASS | No unresolved HIGH review findings. Deadline length, helper attribution and other pinned-clock tests remain documented advisories. |
| 5 | PASS | Reviewed source and canonical merge are clean; publication requires only this record added, then a clean committed branch and the normal hooks. |
| 6 | PASS | Clean canonical merge; full Bazel build and make vet/nogo passed before fast lanes or runtime. |
| 7 | PASS | Two source commits, three cmd/gc test files, +50/-2, one demand-recording fixture theme; no production or internal-doc paths. |

## Merge and acceptance evidence

merge_state: clean (origin/main@164658a1a467bfe8bd614eee16ea76a899fcc3f2)
gate_base_kind: current-tip
merge_validation: PR merge result is tested at merge time by the merge authority; this gate does not rebase.
Preflight: no PR associated with the reviewed source. C6 evidence: c6-run-output.log, c6-build.bep.json and c6-vet.bep.json under the evidence root.

- AC1/AC6: read() uses a real source timer; collect() measures timeout using the pinned lane clock. The fixture extends its deadline to one hour; production retains ten seconds. Fresh unit and integration tests PASS. Historical host contention and the stale-recording variant were not independently reproduced.
- AC2: all three phases, ReadyGated checks, backing-read checks and demand ID sets remain. The two removed lines are fields re-added with alignment; the slow case also requires an actual stalled read.
- AC3: the resolved red commit contains the slow regression and an empty helper; recorded reviewer RED reproduces the missing-recording symptom. Fresh gate GREEN is independent. No synthetic host load was generated.
- AC4: full-suite and by-name evidence below; normal full pre-push is an additional publication check. The exact historical 58-second pre-push contention remains a limitation.
- AC5: live build metadata gc.fixes_tracker=ga-bxfq42l verified; tracker remains open until landing.

## Tests and required lanes

docs/PROJECT_MANIFEST.md is absent. Commands and required coverage were derived from TESTING.md, Makefile, the merged CI workflow and the release-gate protocol.
test_cmd: gate-criterion3.sh (gate-rbe-run.py: make -k check / bazel test //...; acceptance including solo targets; --config=integration //test:integration_packages; full integration minus latency registry; latency registry alone under worker probe; mandatory native floor)
test_cmd_scope: full-suite
test_exec_mode: remote-exec (BEP); required native top-ups are separately recorded.
test_exec_bep: /var/tmp/gc-gate-ga-52zc9y50-d1761nvu/criterion3-run/evidence/remote/unit.bep.json, /var/tmp/gc-gate-ga-52zc9y50-d1761nvu/criterion3-run/evidence/remote/acceptance.bep.json, /var/tmp/gc-gate-ga-52zc9y50-d1761nvu/criterion3-run/evidence/remote/integration-packages.bep.json, /var/tmp/gc-gate-ga-52zc9y50-d1761nvu/criterion3-run/evidence/remote/integration-full.bep.json, /var/tmp/gc-gate-ga-52zc9y50-d1761nvu/criterion3-run/evidence/remote/integration-latency.bep.json
BEP predicate: five phases; every result strategy remote, uncached locally/remotely, attempt 1, start inside its phase window, terminal verdict; no fallback or mixed suite.
rbe_state: certificate/kill-switch/worker-pin preflight passed (rbe-state.json).
fallback_reason: none
admission: /var/tmp/gc-gate-ga-52zc9y50-d1761nvu/criterion3-run/evidence/remote/admission.json (remote cap 2, provisional floor 25 GiB)
farm_test_seconds: 12040.919
test_log_dir: /var/tmp/gc-gate-ga-52zc9y50-d1761nvu/criterion3-run/evidence/remote/logs
test_counts: PASS=154397 FAIL=0 SKIP=606
diff_tests_executed: TestSplitCityV2DemandIsGatedOffThePass PASS; TestSplitCityV2DemandIsGatedOffThePassUnderSlowReads PASS (unit and integration-package phases, final-by-name.txt).
waiver_ref: none
failure_attribution: none
heavy_mode: none
ci_lane_run: n/a (no CI-config change)
policy_lane: PASS — pinned format check, whole-tree C6 nogo/vet, four make check shell guards and policy Bazel targets; fast-run-output.log.
drift_lane: PASS — make bazel-sync then empty git diff and status; generated schema/spec/client/dashboard/lockfile checks and pinned-base OpenAPI check; each in a separate fresh tree.

| Phase | Fresh remote test results | Verdict |
|---|---:|---|
| unit | 411 | PASS |
| acceptance | 25 | PASS |
| integration-packages | 86 | PASS |
| integration-full | 14 | PASS |
| integration-latency | 1 | PASS |

floor_topup: complete; 16 sequential native jobs, all exit 0; /var/tmp/gc-gate-ga-52zc9y50-d1761nvu/criterion3-run/evidence/remote/floor-topup.json.
owned_topup: see execution.json; both owned tests already PASS in the full remote phases.
latency_leg: measured under clear worker probe, both registry tests PASS, first attempt; no latency test ran in the main integration leg.
skip_inventory: /var/tmp/gc-gate-ga-52zc9y50-d1761nvu/criterion3-run/evidence/remote/skip-inventory.json; final native skips inventoried in floor logs.
skip_justification: no owned skip. Affected-package unit skips: 187/187 identical to pinned base; integration skips: 66/66 identical. All six shared hunks are accounted for: awaitEveryRead and scenario helper serve only the two owned tests; only the slow variant stores a nonzero stall; other demandBacking instances keep zero. All inspected skip sites are UNCHANGED. Other packages cannot import cmd/gc test inputs. Remaining skips are platform/provider capabilities, declared integration prerequisites, environment gaps re-exercised by the native floor and existing scenario exclusions; full reasons and site proof are retained.
floor_skip_inventory: floor-skip-inventory.json has 72 rows including subtests; 45 candidate sites all UNCHANGED (floor-skip-site-proof.txt). Seven native reason triples differ from the farm: one cmd/gc live-herdr journey and six internal/runtime/herdr journeys take the unchanged unit-lane guard after GC selectors are scrubbed, rather than report missing herdr. These are inventoried non-owned native SKIPs; no live-herdr fixture or skip guard was changed.
Skip proofs: skip-base-comparison.txt, skip-integration-base-comparison.txt, skip-all-site-proof-preliminary.txt and skip-shared-source-sites.txt; owned.explain.txt records all six shared hunks.
Environment: pinned bd v1.3.4; ICU74 preflight OK (icu74.txt). Wrapper reported PASS-THROUGH, not isolated; runtime/native command environments scrub live-store selectors. No claim of a cloned wrapper environment.

## Load and CI accounting

native-floor: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=73.46 run_max_load=83.43 run_mean_load=78.98 run_readings=11 wait_first_load=46.98 wait_max_load=73.46 wait_mean_load=34.66 wait_readings=61 read_errors=0`
load_threshold: 15; load_waited_seconds: 1801; load_wait_timed_out: 1 (native-floor).
windows-lane: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=31.89 run_max_load=40.29 run_mean_load=36.72 run_readings=4 wait_first_load=71.17 wait_max_load=80.14 wait_mean_load=57.71 wait_readings=62 read_errors=0 prewait_s=1801`
load_threshold: 15; load_waited_seconds: 1801; load_wait_timed_out: 1 (windows-lane).

Load lines retain threshold, waited_seconds, wait_timed_out, wait_* and run_* observations; any soft timeout is disclosed above.
ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-gate-ga-52zc9y50-d1761nvu/merge 164658a1a467bfe8bd614eee16ea76a899fcc3f2 4e723483c8f1e8b711d07631fb991b435a1d0abc fork
workflow_blob: fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612
ci_required_leaf_count: 6
ci_required_accounting:

| Job | Status | Evidence |
|---|---|---|
| runner-policy | LOCAL-PASS | EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=/var/tmp/gc-gate-ga-52zc9y50-d1761nvu/ci-required/runner-policy.out python3 .github/workflows/scripts/runner_policy.py; exit 0; /var/tmp/gc-gate-ga-52zc9y50-d1761nvu/ci-required/runner-policy.log |
| changes | LOCAL-PASS | ci_required_closure command above; exit 0; /var/tmp/gc-gate-ga-52zc9y50-d1761nvu/ci-required/closure.txt |
| credential-provider-windows | NOT-TRIGGERED |   credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true' |
| pack-gate | NOT-TRIGGERED |   pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true' |
| windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | runner=blacksmith-4vcpu-windows-2025; reach=none |
| windows-lane | LOCAL-PASS | bash /var/tmp/gc-gate-ga-52zc9y50-d1761nvu/ci-windows-lane.sh (fresh-tree + admission + load + wrapper command in accounting.json); exit 0; /var/tmp/gc-gate-ga-52zc9y50-d1761nvu/ci-required/windows-lane-output.log |

windows_reach: {"credential-provider-windows": "not triggered", "packages": ["./internal/credentialprovider", "./internal/genshell/..."], "windows-lane-test": "none"}
deferred_to_pr_ci: windows-lane-test (non-linux-runner)
not_covered: none
CI command detail: windows-lane executes its four workflow run steps exactly, including Windows allowlist builds/test compile, Windows nogo analyzer build/compile and Darwin all-package build/compile. Exit 0; accounting.json and windows-lane-output.log retain commands and output.
Gate PASS authorizes PR-open. Merge requires CI / required green on this head and success (or path-filter skip) for each deferred job. No merge performed.

## Evidence integrity

- /var/tmp/gc-gate-ga-52zc9y50-d1761nvu/criterion3-run/evidence/execution.json: 320479 bytes; SHA256 acf51c66f50883f4e3ab5f46bfe2fa045b48c80ee4c7d7b27457c3326ff1f9f7.
- /var/tmp/gc-gate-ga-52zc9y50-d1761nvu/ci-required/accounting.json: 3768 bytes; SHA256 8fef43039f66fa740c8abc79c7a5d230e13d5fa9faea421d207066d31c70ec05.
- /var/tmp/gc-gate-ga-52zc9y50-d1761nvu/final-by-name.txt: 1007 bytes; SHA256 803c35d9e78d9d3726ae45446a3138f970ca07a68c6023d003b1cfea31725c72.
- /var/tmp/gc-gate-ga-52zc9y50-d1761nvu/acceptance-audit.txt: 2406 bytes; SHA256 e581ff898ffc32a9c32a15371b8cddf1fb801d9159dd2aacddc468c838f7c519.
- /var/tmp/gc-gate-ga-52zc9y50-d1761nvu/gate-audit-inputs.json: 2146 bytes; SHA256 3bc68eb61c4212dec34d8df7a7d0f8248bb2904415b0fcb6dc22dae5e6ad85b6.
- /var/tmp/gc-gate-ga-52zc9y50-d1761nvu/floor-skip-inventory.json: 28839 bytes; SHA256 c53db2a181fc544927256f21db94ce6d8b9853ef82a05f4adc77ca4ec094c491.
- /var/tmp/gc-gate-ga-52zc9y50-d1761nvu/floor-skip-site-proof.txt: 8368 bytes; SHA256 a8509058ffbf400e9d458173c116a29b43e53377798ee8333cc44d070c50d41f.
- /var/tmp/gc-gate-ga-52zc9y50-d1761nvu/ci-windows-lane.sh: 1026 bytes; SHA256 dfb35dc326ebbf262d78077040c4eccde0da392bc915d07c7d4451a1155ca718.
