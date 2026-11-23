# Drift budget scheduling tolerance release gate

**Verdict:** **PASS**

Evaluated: 2026-11-23T14:23:35.447659+00:00
Bead: ga-2wc6aiod; build: ga-bofb21z7; review: ga-23zc0vd9.
Reviewed source: `ca8edba465a82bda10f050b5af231d12c635bbc3`.
gate_base: `cb9a2621ae17eb4b09b1e4d3c71079623486ff8b`
gate_head: `ca8edba465a82bda10f050b5af231d12c635bbc3`
gate_merge: `4ffec7a75ea29e463e5e51eb4388551d8f1a5b59`
gate_tree: `aaff21636d4f8de18bdd0451aeacd5b85af10889`
gate_base_kind: current-tip
merge_state: clean (origin/main@cb9a2621ae17eb4b09b1e4d3c71079623486ff8b)
merge_validation: raw merge build, nogo/vet and full suite; MPR must validate the current merge result and required PR CI before merging.
deploy_mode: remote; base_branch: main; push_remote: origin

| # | Criterion | Result and evidence |
|---|---|---|
| 1 | Review PASS | PASS. ga-23zc0vd9 records PASS on the exact reviewed source; no carryover or reviewer-added patch. |
| 2 | Acceptance | PASS. Minimum statistic tolerates 29 delayed samples plus one 2 ms sample; all three owned tests pass. Independent 150 ms per-call detector mutation fails at 150.938866 ms. Scope is one existing test file; NFR-4 and per-call correctness assertions stay intact. Landing/hold cleanup belongs to mayor after merge. |
| 3 | Tests | PASS. Full-scope forced remote gate and native floor pass; owned tests pass by name; unchanged skips independently checked; six required CI leaf jobs accounted below. |
| 4 | Review findings | PASS. Zero unresolved HIGH/security findings. Low stale p95-comment finding is tracked separately as ga-apes59ph. |
| 5 | Clean branch | PASS. Canonical and assigned worktrees clean before this record; isolated deploy commits only this record above the reviewed source, with clean status verified before publication. |
| 6 | Merge/build | PASS. Raw merge parents and merge-tree independently verified. Build and nogo/vet terminal BEPs report success. No rebase or conflict resolution. |
| 7 | Single theme | PASS. Two TDD commits for ga-bofb21z7; one cmd/gc drift-test timing theme, one changed test file (+42/-8). Ancestry citation and denylist guards pass for deploy and build bead IDs. |

## Acceptance and execution evidence

AC1: `TestDriftBudgetStatistic_IgnoresLoadSpikes` PASS. A 30-sample minimum accepts 29 x 500 ms plus 1 x 2 ms; this is a best-case bound, not a p95 guarantee.
AC2: `TestDriftBudgetStatistic_FlagsUniformSlowdown` PASS; `TestDriftDetect_WithRealisticPacks_NFR1` PASS, 514.008 us in unit and 482.9 us in integration-packages against 100 ms. Independent injected 150 ms sleep makes NFR-1 FAIL (Bazel rc3); fresh mutation view restored and wrapper rc0.
AC3: one existing `_test.go` file only; NFR-4 remains unchanged and passes in both unit and integration-packages.
AC4: post-landing coordination remains with mayor: ga-bofb21z7 is stamped `gc.fixes_tracker=ga-241asnq`; close the tracker and re-gate ga-0xbn6b14 only after this fix lands.

test_cmd: gate-detached-run.sh -> gate-criterion3.sh --repo /var/tmp/gc-merge-ga-2wc6aiod.yjt6507d --base cb9a2621ae17eb4b09b1e4d3c71079623486ff8b --bead-json /var/tmp/gc-deploy-ga-2wc6aiod.a4agstn6/bead.json --owned /var/tmp/gc-deploy-ga-2wc6aiod.a4agstn6/owned.tsv --run-dir /var/tmp/gc-deploy-ga-2wc6aiod.a4agstn6/c3/evidence
test_cmd_scope: full-suite
test_exec_mode: remote-exec
fallback_reason: none
farm_test_seconds: 14910.706
test_exec_bep: /var/tmp/gc-deploy-ga-2wc6aiod.a4agstn6/c3/evidence/remote/<phase>.bep.json; 554 results, all remote, uncached, attempt 1 and inside each phase window; 1108 test.log/test.xml hashes verified against CAS digests.
rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72", "worker_env_tree": "sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"}
admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 63.41572189331055, "waited_seconds": 0.001}
heavy_mode: none (heavy-composite-gate.sh classify exit0)
delta_gate: shadow; full run; not-gate-pr; applied=false
waiver_ref: none
failure_attribution: none
ci_lane_run: n/a (no CI configuration change)

| Phase | Targets | Uncached remote result actions | Top-level PASS | FAIL | SKIP |
|---|---:|---:|---:|---:|---:|
| unit | 309 | 426 | 93768 | 0 | 443 |
| acceptance | 8 | 25 | 425 | 0 | 14 |
| integration-packages | 38 | 87 | 64502 | 0 | 133 |
| integration-full | 1 | 15 | 205 | 0 | 14 |
| integration-latency | 1 | 1 | 2 | 0 | 0 |

test_counts: 158926 PASS, 0 FAIL, 618 SKIP over 570 retained logs; top-level events, repeated jobs count repeatedly. This includes native floor results beyond the remote rows above.
test_log_dir: /var/tmp/gc-deploy-ga-2wc6aiod.a4agstn6/c3/evidence/remote/logs
floor_topup: 16/16 rc0; /var/tmp/gc-deploy-ga-2wc6aiod.a4agstn6/c3/evidence/remote/floor-topup.json (environment-absence packages, beadstest, Darwin compile and runner self-tests). Each child scrubs live GC/BD/BEADS/DOLT selectors; pinned bd v1.3.4.
owned_topup: none required; all three owned names present in the full-suite logs.
diff_tests_executed:
```text
diff_tests_executed:
  TestDriftBudgetStatistic_FlagsUniformSlowdown PASS (integration-packages-56f04454dd9158a9ee8a.log, unit-637c74c578a2b17fc654.log)
  TestDriftBudgetStatistic_IgnoresLoadSpikes PASS (integration-packages-9326181a736eb30b71ca.log, unit-27b23e708b477fce4dad.log)
  TestDriftDetect_WithRealisticPacks_NFR1 PASS (integration-packages-12697e2ea0d32c278115.log, unit-3d6d3036a2f5902472b5.log)
GATE_TEST_EVIDENCE_RESULT ok: 3 diff-owned tests verified by name
```
latency_leg: complete, first attempt probe clear after 30 s; both named budget tests PASS; neither ran in integration-full.

## Skips, policy and load

skip_inventory: /var/tmp/gc-deploy-ga-2wc6aiod.a4agstn6/c3/evidence/remote/skip-inventory.json; 604 deduplicated remote skip records (514 other, 90 environment-absence); native floor attempted every environment-absence package.
skip_justification: /var/tmp/gc-deploy-ga-2wc6aiod.a4agstn6/skip-source-audit/evidence.json records 676 raw skip observations including subtests, 319 unchanged source sites, no unknown locations. Every skip source file is identical at the pinned base. Production code is unchanged. Shared import/helper hunks occur only in cmd/gc/cmd_start_drift_bench_test.go; computed test-binary import closures select all affected skip groups for same-command base comparison. Other skip binaries cannot import cmd/gc; platform, opt-in, subprocess-helper and capability skips keep their printed reasons.
base_skip_evidence: /var/tmp/gc-deploy-ga-2wc6aiod.a4agstn6/base-skip-evidence/report.json; all three remote groups and one native cmd/gc group complete rc0, comparison rc0. These targeted comparisons establish skip provenance only; full-suite scope comes from the canonical run above.
COMPARE-NOTE the remote worker's account differs between the runs in 1 SKIP(s), read as the same reason because the account is the only difference (an account is rbe-, a letter and two digits, standing alone; the logs are not changed): TestInfraScopedManagedDoltPIDIdentityUnderRealSystemd (base rbe-a13, head rbe-a01)
COMPARE-SUMMARY head_skips=68 judged=68 pre-existing=68 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
COMPARE-SUMMARY head_skips=4 judged=4 pre-existing=4 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
COMPARE-SUMMARY head_skips=189 judged=189 pre-existing=189 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
COMPARE-SUMMARY head_skips=36 judged=36 pre-existing=36 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING

policy_lane: make lint (nogo/vet), shell policy guards, complexity-diff, and strict trusted-base storeclass baseline/growth PASS; evidence /var/tmp/gc-deploy-ga-2wc6aiod.a4agstn6/c6-vet.bep.json, guards.*.log and storeclass-proof/verified.json.
drift_lane: make bazel-sync then git diff --exit-code and empty git status PASS; schema, spec, CLI/docs and command census generated-artifact Bazel targets PASS. Evidence /var/tmp/gc-deploy-ga-2wc6aiod.a4agstn6/drift.*.log and generated.bep.json.
lane_isolation: each fast lane runs through fresh-tree-run.sh from the exact clean canonical merge; no lane rewrites the canonical tree. Storeclass analyzer is built from the pinned trusted base; no raise approval or allow-raise flag.
build_evidence: /var/tmp/gc-deploy-ga-2wc6aiod.a4agstn6/c6-build.bep.json and c6-vet.bep.json; both successful terminal BEPs.

storeclass.stderr.log: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=55.05 run_max_load=54.66 run_mean_load=49.15 run_readings=4 wait_first_load=19.30 wait_max_load=71.63 wait_mean_load=52.44 wait_readings=61 read_errors=0
c3-recovery-terminal.stderr: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=40.05 run_max_load=40.75 run_mean_load=13.92 run_readings=48 wait_first_load=40.05 wait_max_load=40.05 wait_mean_load=40.05 wait_readings=1 read_errors=0
Native floor is inside the canonical wrapper; remote-class admission does not impose a host-heavy slot/load wait. Base native comparison and CI job commands use local load gating. Per-command load summaries remain beside their retained logs.

## Required CI accounting

ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-ga-2wc6aiod.yjt6507d cb9a2621ae17eb4b09b1e4d3c71079623486ff8b ca8edba465a82bda10f050b5af231d12c635bbc3 origin; exit0; merged workflow blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs need a status; the rest are rolled up through their needs.
| Job | Status | Evidence |
|---|---|---|
| runner-policy | LOCAL-PASS | /var/tmp/gc-deploy-ga-2wc6aiod.a4agstn6/required-ci/runner-policy.stdout; exact workflow run commands rc0, steps in /var/tmp/gc-deploy-ga-2wc6aiod.a4agstn6/required-ci/runner-policy-steps.json |
| windows-lane | LOCAL-PASS | /var/tmp/gc-deploy-ga-2wc6aiod.a4agstn6/required-ci/windows-lane.stdout; exact workflow run commands rc0, steps in /var/tmp/gc-deploy-ga-2wc6aiod.a4agstn6/required-ci/windows-lane-steps.json |
| credential-provider-windows | NOT-TRIGGERED |   credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true' |
| pack-gate | NOT-TRIGGERED |   pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true' |
| changes | LOCAL-PASS | /var/tmp/gc-deploy-ga-2wc6aiod.a4agstn6/required-ci/closure.stdout |
| windows-lane-test | DEFERRED-TO-PR-CI | reason=non-linux-runner; GOOS=windows GOARCH=amd64 go list -mod=readonly -test -deps over .github/windows-lane/test.txt, exit0, changed-file reach none; /var/tmp/gc-deploy-ga-2wc6aiod.a4agstn6/windows-native-reach.json |

windows_reach: none for windows-lane-test, independently computed over its own native test allowlist; credential-provider-windows is NOT-TRIGGERED.
deferred_to_pr_ci: windows-lane-test (non-linux-runner)
not_covered: none
host_gaps: none
A gate PASS authorizes PR-open only. MPR must require CI / required green and the deferred native Windows job successful on the current PR head.

CI collector bookkeeping recovery: /var/tmp/gc-deploy-ga-2wc6aiod.a4agstn6/ci-accounting-reviewed.json independently accounts the unflushed final NOT-TRIGGERED pack row. Raw evidence remains unchanged; no suite or CI job was re-run.

All per-job logs and artifact receipts are retained under /var/tmp/gc-deploy-ga-2wc6aiod.a4agstn6.

Publication accounting: a fresh origin dry run succeeded. The same-repo ci_required_triggers.py invocation returned byte-identical leaf jobs and triggers to the original fork invocation; both outputs are retained in required-ci/closure{,-origin}.stdout under the evidence directory. Gate source, pinned merge tree and runtime evidence are unchanged.
