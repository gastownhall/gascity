# Shell pipefail early-exit reader gate

**Verdict:** **PASS**

Deploy bead: ga-7p928g2u. Build: ga-1gn23y. Review: ga-45fkph31.
Reviewed source: `93b8f6df140e6ec64c95e7b3742086a4b75798f4`. Base: `origin/main@2192eee2a1598a1ef1799f77ad8287f537a99a9f`.
Canonical merge: `c049eaaaf5d3121a3820c7aa0f229fc23f4507a3`; tree `1b80811de5ad269bf8f8468a628387c9b30c12fc`.
The reviewed source, canonical merge and GitHub PR merge have identical trees.
PR: https://github.com/gastownhall/gascity/pull/9246. This record is the only added deploy commit.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS | PASS | Closed review ga-45fkph31 names the resolved reviewed source; verdict pass. |
| 2 | Acceptance | PASS | 16 new tests pass; guard 0.12 s; negative control finds 102 sites; existing script wrappers pass; native selftests 67/67 and 25/25; docs/canaries/drift pass. |
| 3 | Full tests, policy, generated files and CI | PASS | Complete canonical five-phase remote run, native floor and latency measured; 155384 PASS, 0 FAIL, 613 SKIP; owned tests all PASS. All four real CI runs success; six required CI leaf jobs success. |
| 4 | No open high-severity review finding | PASS | Reviewer records no blocker/major; remaining observations are minor/info. |
| 5 | Clean final branch | PASS | Source branch and canonical checkout pristine before this record; final deploy commit contains only this file. |
| 6 | Merge state and build/vet | PASS | Clean canonical merge; whole-tree `bazel build --config=remote-exec //...` including nogo/vet succeeds. |
| 7 | Single feature theme | PASS | AST guard plus the mechanical rewrites and contributor convention for early pipe readers under pipefail; seven source commits cite ga-1gn23y. |

merge_state: clean (origin/main@2192eee2a1598a1ef1799f77ad8287f537a99a9f)
gate_base_kind: current-tip
merge_validation: recorded canonical merge built and tested; MPR owns the current merge result and head pin.
DEPLOY_MODE: remote; PUSH_REMOTE: origin; heavy_mode: none.

## Full-scope execution

test_cmd: `gate-criterion3.sh --repo /var/tmp/gate-ga-7p928g2u-yyrq3xtr/runtime-merge.zbjaqmb4 --base 2192eee2a1598a1ef1799f77ad8287f537a99a9f --bead-json /var/tmp/gate-ga-7p928g2u-yyrq3xtr/bead-criterion3.json --owned /var/tmp/gate-ga-7p928g2u-yyrq3xtr/owned.txt --run-dir /var/tmp/gate-ga-7p928g2u-yyrq3xtr/criterion3/evidence`.
The exact argv/environment are retained in `criterion3/cmd.argv`.
The supported recipe runs unit/check (`bazel test //...`, policy/nogo/format included), acceptance, integration-packages, full integration excluding the designated latency tests, then the dedicated latency lane. Its native floor follows as specified by the runner.

test_cmd_scope: full-suite
test_counts: PASS=155384 FAIL=0 SKIP=613 (top-level result occurrences, summed over 557 retained logs; repeated jobs count separately).
test_exec_mode: remote-exec; fallback_reason: none; farm_test_seconds: 14188.125.
test_exec_bep: `criterion3/evidence/remote/{unit,acceptance,integration-packages,integration-full,integration-latency}.bep.json`.
All 541 farm results have remote strategy, no cache flag, attempt 1, a start inside their phase window and a terminal PASS. All 1082 retained farm artifacts match their digests; originals remain unchanged. By-name views remove only rules_go framing bytes.

| Phase | Targets | Results | Verdict |
|---|---:|---:|---|
| unit | 309 | 415 | PASS; remote, uncached, attempt 1 |
| acceptance | 8 | 25 | PASS; remote, uncached, attempt 1 |
| integration-packages | 37 | 86 | PASS; remote, uncached, attempt 1 |
| integration-full | 1 | 14 | PASS; remote, uncached, attempt 1 |
| integration-latency | 1 | 1 | PASS; remote, uncached, attempt 1 |

admission: remote, floor25 GiB, free58.2722 GiB, cap2, wait0.001 s.
rbe_state: enabled/credentials current; tree/main worker-environment pins match; see `criterion3-tooling-version.json` for unchanged consumed helpers at city `ca0ff850c351ce1bc79d5d4f50d655bfff4e1437`.
owned_topup: none needed; all owned names passed in the full run.
floor_topup: complete; all16 commands exit0, including beadstest own-cache resolution, all environment-absence top-ups, Darwin compile, push-gate-lock67/67 and local-concurrency25/25. Native top-ups preserve their skips; no failed result was retried.
latency_leg: complete, first attempt, clear farm probe, both designated tests PASS; no main-leg latency credit and no fallback.
test_log_dir: `/var/tmp/gate-ga-7p928g2u-yyrq3xtr/criterion3/evidence/remote/logs` (557 logs).
waiver_ref: none; failure_attribution: none (no runtime failure).

## Skip ownership and load

skip_inventory: 599 farm skip-event rows and72 native skip-event rows,488 unique names; this inventory counts subtests while test_counts counts only top-level occurrences.
diff_tests_executed: all16 roots PASS by name, with no owned SKIP/FAIL at any nesting level.
not_owned_skip_evidence: 324 printed skip sites plus the recovered raw CONT-context site are UNCHANGED; the entire candidate source files match the pinned base. SHARED-HUNKS are confined to the two newly added scripts test files; HELPER-HUNKS none. Union import closures under unit/integration/acceptance tags exclude both scripts and scripts/cipolicy for every skipped test binary.
skip_justification: existing provider opt-ins, optional tool/fixture absence, platform requirements, intentional conformance skips and tracked gates. Every farm environment absence received its canonical native top-up; both affected examples/gastown environment cases pass there. Remaining optional/native skips are inventoried. No skip was credited as a test PASS.
The population reader missed the reason after `=== CONT` for dispatch's existing fail subtest; raw log and unchanged call_budget_test.go:537 independently show its ga-vnycm2.27 gate. No raw output was changed.

load_threshold: 15; load_waited_seconds: 1801; load_wait_timed_out: 1 (native floor's prescribed soft wait).
run_start_load: 21.09; run_max_load: 21.87; run_mean_load: 19.88; run_readings: 7 (native floor).
Native floor: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=21.09 run_max_load=21.87 run_mean_load=19.88 run_readings=7 wait_first_load=74.74 wait_max_load=74.74 wait_mean_load=53.09 wait_readings=61 read_errors=0`.
Outer remote-class run (no host prewait; farm execution): `LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=71.85 run_max_load=74.82 run_mean_load=51.64 run_readings=107 wait_first_load=71.85 wait_max_load=71.85 wait_mean_load=71.85 wait_readings=1 read_errors=0`.
The timed-out soft wait is disclosed; no threshold, cache, scope or isolation override was used.

## Fast lane and acceptance

policy_lane: separate fresh views;13 policy/format targets PASS; see fast-lane.sh and fast-independent-audit.json.
drift_lane: separate fresh views;7 generated-file checks and OpenAPI drift check PASS; bazel-sync leaves no difference. Static cached results disclosed: policy7/13 and drift4/7; runtime cache credit is zero.
complexity: zero-growth clause(ii): base and merge each exit2 against the rolling baseline,251 rows each, no merge row worse, no new/regressed/deleted-live change against the pinned base. Baseline untouched. The retired literal-command condition is documented by historical tracker ga-q7p6is; this uses the current zero-growth rule.
Acceptance control: final analyzer with only55 swept paths restored to base produces exactly102 findings and the expected FAIL; one uncached remote control run, never candidate-suite credit.
Existing rebase-resolve, ownership-guard and core-boundary test wrappers PASS by name. The guard runs in0.12 s, below2 s. The full new test inventory includes discovery parity omitted by the guide's narrow example filter. Orphan/pre-push prerequisites are on the pinned base.

## Real CI and required-job accounting

ci_lane_run: all four first real runs completed success at the reviewed source:
- [PR Bazel44120715773](https://github.com/gastownhall/gascity/actions/runs/44120715773).
- [Bazel dispatch44120870922](https://github.com/gastownhall/gascity/actions/runs/44120870922), rbe=on; rrc-verify cold-fetch and compare steps PASS.
- [Nightly dispatch44120902006](https://github.com/gastownhall/gascity/actions/runs/44120902006); changed beads-proxied-perf steps/job and whole workflow PASS.
- [Bazel nightly dispatch44120932287](https://github.com/gastownhall/gascity/actions/runs/44120932287); decide and called workflow PASS.

ci_required_closure: `ci_required_triggers.py <canonical-scratch> 2192eee2a1598a1ef1799f77ad8287f537a99a9f 93b8f6df140e6ec64c95e7b3742086a4b75798f4 origin`; workflow blob `fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612`;6 LEAF jobs. The GitHub PR synthetic merge tree is byte-identical to the canonical tree.
ci_required_accounting:
- `runner-policy`: CI-PASS [134171015931](https://github.com/gastownhall/gascity/actions/runs/44120715606/job/134171015931).
- `changes`: CI-PASS [134171272781](https://github.com/gastownhall/gascity/actions/runs/44120715606/job/134171272781).
- `credential-provider-windows`: CI-PASS [134171483097](https://github.com/gastownhall/gascity/actions/runs/44120715606/job/134171483097).
- `pack-gate`: CI-PASS [134171482970](https://github.com/gastownhall/gascity/actions/runs/44120715606/job/134171482970).
- `windows-lane`: CI-PASS [134171270386](https://github.com/gastownhall/gascity/actions/runs/44120715606/job/134171270386).
- `windows-lane-test`: CI-PASS [134171270809](https://github.com/gastownhall/gascity/actions/runs/44120715606/job/134171270809).

deferred_to_pr_ci: none; not_covered: none (ci-required closure).
windows_reach: actual Windows CI-PASS for both native Windows jobs; no local deferral.

Deferred to post-merge observation under MAYOR RULING15:3xZ11-17 / gcg-11419649, not met and not waived:
- rrc-seed: first push-to-main run after merge;3-hour window.
- split-strict-alert and decide's schedule/cache branch: first scheduled nightly after merge;30-hour window.
Two explicit-flag observation records must be minted at complete, before closing the deploy bead; the exact criteria/check/baseline/pass rules are retained in post-merge-observation-plan.json. Static AST/policy checks are their pre-merge evidence only.

Ancillary CI: Image vulnerabilities run44120715684 failed building the untouched base-image apt layer after a Launchpad503, before scanning. Investigator ga-9s4mp2n5 records the complete failing-layer input proof and independent503 probes. The separate pre-existing main dnspython finding is tracked on repo-wide scanner advisory fix ga-9d74sbdn. This check is outside ci-required; MPR applies its scanner policy. No runtime failure or waiver is inferred from it, and no rerun was requested by this gate.

## Owned test names

- `TestFormatPipefailFinding`: PASS.
- `TestGitAndWalkDiscoveryAgree`: PASS.
- `TestPipefailGuardCatchesTheHistoricalIncidents`: PASS.
- `TestPipefailGuardDoesNotFlagSafeShapes`: PASS.
- `TestPipefailGuardFailsOnAParseError`: PASS.
- `TestPipefailGuardFindsReadersAcrossLines`: PASS.
- `TestPipefailGuardFlagsEarlyExitReaders`: PASS.
- `TestPipefailGuardFlagsMidPipelineStage`: PASS.
- `TestPipefailGuardIgnoresCommentsHeredocsAndStrings`: PASS.
- `TestPipefailGuardReportsProducerAndRewrite`: PASS.
- `TestPipefailGuardSortsFindingsByFileThenLine`: PASS.
- `TestPipefailScopeRules`: PASS.
- `TestPipefailWorkflowRunBlocks`: PASS.
- `TestShellNoEarlyExitReaderUnderPipefail`: PASS.
- `TestShellSourceDiscoveryByContent`: PASS.
- `TestWalkShellSourcesFindsShellByContentAndPrunes`: PASS.

## Retained evidence

Raw evidence: `/var/tmp/gate-ga-7p928g2u-yyrq3xtr`; source checkouts and all original logs are preserved.
Verified durable packet: `/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-7p928g2u/gate-pass-20261117T163201Z`; 2065 files / 176873006 bytes.
Packet manifest: `/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-7p928g2u/gate-pass-20261117T163201Z/SHA256MANIFEST.json`; SHA256 `2735912dc72ef79b8bbd16e0eaafbfe6abfe006c2414aad08116f2e1f1436ba5`; every copied file independently verified.
Full audits remain in that packet and the original root: runtime-independent-audit.json, farm-independent-audit.json, skip-independent-audit.json, skip-site-evidence.txt, skip-import-closure-proof.json, floor-topup.json, latency-leg.json, ci-lanes-independent-audit.json, ci-required-independent-audit.json, acceptance-independent-audit.json and negative-control-audit.json. Large inventories/logs are deliberately kept out of this PR.
