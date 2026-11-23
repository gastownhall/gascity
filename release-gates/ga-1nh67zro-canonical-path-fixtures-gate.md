# Canonical city and rig path fixtures — release gate

**Verdict:** **PASS**

Evaluated 2026-11-22T23:35:08.625550+00:00 for deploy bead ga-1nh67zro, build ga-iawy13.2, review ga-9nniijg.

deploy_source: 241bfebb70dad17b91657d6baa665c8a2905929e
gate_base: 7b8d498424f491832facf6941ffe84c2e862b012
merge_head: 23d6e30227adedbbfdbb6633860d33156c53ed5a
merge_state: clean (origin/main@7b8d498424f491832facf6941ffe84c2e862b012)
gate_base_kind: current-tip
merge_validation: merge result built and vetted here; final integration remains a merge-time responsibility.
deploy_mode: remote
push_remote: fork (quad341/gascity)
evaluation_order: 6, 1, 2, fast lane, 3, 4, 5, 7
manifest: docs/PROJECT_MANIFEST.md is absent; current gate protocol, TESTING.md, Makefile and gated CI workflows supplied the checks.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Closed review ga-9nniijg records PASS for the exact resolved deploy source. No carryover or reviewer-added commit is involved. |
| 2 | Acceptance criteria met | PASS | All five fixture files assert canonical discovery/store paths. The explicit linked-city fixture compares the discovered path with the resolved target. Ordinary unit/integration runs and the symlink-rooted run pass; the latter reports 26 PASS, zero FAIL/SKIP. The source comment preserves the cross-platform evidence boundary, without claiming a reproduced macOS failure. Only test files change. |
| 3 | Tests pass | PASS | Canonical full-suite chain, local floor, all six owned tests, split-strict, integration-smoke, policy/drift checks and required CI accounting pass. Remaining skips are accounted for below. |
| 4 | No high-severity findings open | PASS | Review security findings are none; zero unresolved HIGH findings. The recorded style advisory is non-blocking. |
| 5 | Final branch clean | PASS | Packaging adds only this gate record to the reviewed source. A clean worktree/index is required and checked after committing it, before publication. |
| 6 | Merge state and build/vet | PASS | Raw clean merge at the pinned base; make build and full Bazel nogo passed on the materialized merge, hooks disabled only for scratch materialization. |
| 7 | Single feature theme | PASS | Five cmd/gc test files cover one canonical-path fixture invariant. Ancestry scope passed for deploy/build IDs; no stack or unrelated feature is included. |

## Acceptance and test evidence

The symlink-rooted run sets TMPDIR and GOTMPDIR to /tmp/gcln.mBs0cu-link, pointing at /tmp/gcln.mBs0cu. It executes the six owned tests, TestFindCityResolvesSymlinkedCityDir, and TestRigAddLocalAndAPIProduceIdenticalArtifacts (all three subcases). The new TestFindCity/canonical_resolves_symlinked_fixture passes. Static inspection confirms its expected value is the resolved target, distinct from the raw link; builder and reviewer mutation evidence is recorded on their beads.

test_cmd_scope: full-suite
test_exec_mode: remote-exec
fallback_reason: none
test_counts: PASS=158830 FAIL=0 SKIP=618
These are the canonical reader's emitted result-line totals across lanes, not distinct test functions.
test_log_dir: /var/tmp/deploy-ga-1nh67zro-20261122.t4FHpp/criterion3/evidence/remote/logs
test_bd: GASCITY_TEST_BD version=v1.3.4 bin=/home/jaword/.local/bd-versions/v1.3.4/bd reports='bd version 1.3.4 (7ef2fd05e: HEAD@7ef2fd05eb0c)' ref_check=match:7ef2fd05e origin=preexisting
waiver_ref: none
failure_attribution: none in the canonical full-suite chain; the separate publication-source attribution is recorded below.
heavy_mode: none
owned_topup: none required
floor_topup: complete, 16/16 steps exit 0
farm_test_seconds: 15188.726
latency_leg: measured, clear probe; TestGcBdLatencyBudget PASS; TestGcSeatHookLatencyBudget PASS
admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 59.290679931640625, "waited_seconds": 0.001}
rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72", "worker_env_tree": "sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"}
delta_gate: shadow, decision full, reason not-gate-pr; no narrowing applied

The actual suite comprises make -k check, acceptance plus acceptance_solo_tests, integration_packages, full integration with the registered latency names excluded, and those latency names alone with the farm probe. Exact phase flags (including forced fresh results and no local fallback) follow:

test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-1nh67zro-20261122.t4FHpp/criterion3/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-1nh67zro-20261122.t4FHpp/criterion3/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-1nh67zro-20261122.t4FHpp/criterion3/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-1nh67zro-20261122.t4FHpp/criterion3/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-1nh67zro-20261122.t4FHpp/criterion3/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec

test_exec_bep: {"acceptance": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 25, "results": 25, "retried": 0, "window_ok": true}, "integration-full": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 15, "results": 15, "retried": 0, "window_ok": true}, "integration-latency": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 1, "results": 1, "retried": 0, "window_ok": true}, "integration-packages": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 87, "results": 87, "retried": 0, "window_ok": true}, "unit": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 426, "results": 426, "retried": 0, "window_ok": true}}

diff_tests_executed:
  TestConvergeTestGateUsesRigStorePath PASS (integration-packages-514989cc035c0e9a8ff5.log, unit-abd97d2cb9b450225d57.log)
  TestFindCity PASS (integration-packages-9326181a736eb30b71ca.log, unit-5a9df316125cd0aeff2e.log)
  TestOpenStoreAtForCityExecProjectsConfiguredTargets PASS (integration-packages-75f7330b9b673df65e4f.log, unit-1cc8c8ab55e59a9e03a9.log)
  TestResolveFormulaScope_CwdInsideRig PASS (integration-packages-cb110414c557c0928e82.log, unit-f679cd2551fcc64525aa.log)
  TestResolveFormulaScope_RigFallsBackToCityLayers PASS (integration-packages-b72a67a2f9444af705e3.log, unit-64014bc0bb3833bc198c.log)
  TestResolveFormulaScope_RigFlagWins PASS (integration-packages-75f7330b9b673df65e4f.log, unit-10cc7795c0ec199a5530.log)
GATE_TEST_EVIDENCE_RESULT ok: 6 diff-owned tests verified by name

## Fast lane and skip accounting

policy_lane: PASS — make build lint (full nogo), then isolated make -k fmt-check check-release-dist-ignore check-routed-test-rows check-split-topology-rows check-residency-boundary.
GLT_RECORD: pin=2.14.0 resolved=2.14.0 mismatch=false.
drift_lane: PASS — isolated make bazel-sync BAZEL_SYNC_FLAGS=--config=remote-exec; git diff --exit-code.
known_red_precheck: proceed reason=nothing-known-red; check-routed-test-rows passed. Tracker ga-1ptv4jey was reported stale and did not park the gate.
ci_lane_run: n/a (no CI-config change).
skip_justification: all printed skip sites are unchanged. Platform, configured-provider, fixture and lane skips outside cmd/gc cannot reach changed test helpers; no production input changes. Shared helper hunks are assertRigAddArtifactsIdentical and normalizeArtifact in rig_provision_parity_test.go; their affected parity test and all three subcases pass. Every cmd/gc skip is additionally reproduced by name at the pinned base, including the exact native floor command/environment.
skip_inventory: 676 records, 319 source sites, zero unresolved sites.
integration-packages: COMPARE-SUMMARY head_skips=68 judged=68 pre-existing=68 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
topup: COMPARE-SUMMARY head_skips=36 judged=36 pre-existing=36 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
unit: COMPARE-SUMMARY head_skips=193 judged=193 pre-existing=193 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
COMPARE-NOTE the remote worker's account differs between the runs in 1 SKIP(s), read as the same reason because the account is the only difference (an account is rbe-, a letter and two digits, standing alone; the logs are not changed): TestInfraScopedManagedDoltPIDIdentityUnderRealSystemd (base rbe-a05, head rbe-a08)
Go SYN framing bytes are removed using the same normalization as gate-rbe-evidence.py; raw logs remain retained. The first baseline measurement was adopted from its successful 24-result BEP after correcting the reader; no failed test was retried.
load_threshold: 15
load_waited_seconds: 0
load_wait_timed_out: 0
run_start_load: 10.87
run_max_load: 13.15
run_mean_load: 10.68
run_readings: 48

## Required CI accounting

ci_required_closure: ci_required_triggers.py on the gated merge, pinned base and source, head kind fork; workflow blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs.
ci_required_accounting:

| Job | Status | Evidence |
|---|---|---|
| runner-policy | LOCAL-PASS | Prescribed pull_request/quad341 environment, exit 0; runner-policy-result.txt. |
| changes | LOCAL-PASS | Prescribed trigger tool, exit 0; ci-closure.txt. |
| credential-provider-windows | NOT-TRIGGERED | Tool row: credential_provider filter did not match. |
| pack-gate | NOT-TRIGGERED | Tool row: packs filter did not match. |
| windows-lane | LOCAL-PASS | All four gated YAML run steps, including tools/nogo working directory, exit 0 in a fresh view; windows-lane-result.txt. |
| windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | runner_windows=blacksmith-4vcpu-windows-2025; native test.txt import closure computed for Windows, no changed file in reach. |

windows_reach: none (native windows-lane-test closure; native-windows-closure.txt and native-windows-reach.txt)
deferred_to_pr_ci: windows-lane-test (non-linux-runner)
not_covered: none
host_gaps: none

Additional manual Bazel targets passed freshly: gc_split_strict_test, api_split_strict_test, integration_smoke_test. Gate PASS authorizes PR opening; merge requires current-head CI / required and the deferred native Windows job to pass.

## Retained evidence

/var/tmp/deploy-ga-1nh67zro-20261122.t4FHpp/criterion3/evidence/execution.json (SHA256 16d187ab876fbf8f30195945198ebf06627b61757cb1305bb91bfa486fd03c83)
/var/tmp/deploy-ga-1nh67zro-20261122.t4FHpp/criterion3/evidence/remote/skip-inventory.json (SHA256 9f1b70d5c40f2a13a9af8d423baedea235ac580e11f4bf084614db533e463d3d)
/var/tmp/deploy-ga-1nh67zro-20261122.t4FHpp/skip-audit/evidence.json (SHA256 4099e43b62076476a3457e4cca266d1b18547a3470da128d61594a3bc08480c0)
/var/tmp/deploy-ga-1nh67zro-20261122.t4FHpp/skip-audit/comparisons.json (SHA256 67429a761fcb861385d8b3ff939de1d8d32bb6ac7fde1b262ede0e32e55526f9)
/var/tmp/deploy-ga-1nh67zro-20261122.t4FHpp/c6-result.txt (SHA256 2e702cb3db36c913a4add1819489d4eac22179070c29d7d85da2335dc91e9970)
/var/tmp/deploy-ga-1nh67zro-20261122.t4FHpp/fast-result.txt (SHA256 8300a49c6ddc951735b57f00201545d92de305819d808cd29f13dba14e046963)
/var/tmp/deploy-ga-1nh67zro-20261122.t4FHpp/runner-policy-result.txt (SHA256 98022911a77467b17031ee5f6f7e3fe881b4029746f6dc90bc9e2882ea5c8b02)
/var/tmp/deploy-ga-1nh67zro-20261122.t4FHpp/windows-lane-result.txt (SHA256 f0c40f2e2db32a1aa4543e0257ed291ae42eaae554cdbff1f8ed5b082f72abc9)
/var/tmp/deploy-ga-1nh67zro-20261122.t4FHpp/required-bazel-result.txt (SHA256 e2f9d0c67aa59959ef32f5984208010b6883324b707c74a1859d6e22915ce303)
/var/tmp/deploy-ga-1nh67zro-20261122.t4FHpp/symlink-test.log (SHA256 d8021a3aabc6a90e7fc9ee97a76593c922c55e57cb0a1c2da45ca025ca6a046b)
/var/tmp/deploy-ga-1nh67zro-20261122.t4FHpp/symlink-evidence/symlink.log (SHA256 4e6566091daf2fe68ff87f24fe45b90d99519ec6f1d26be2f52fe159918f2021)
/var/tmp/deploy-ga-1nh67zro-20261122.t4FHpp/symlink-topup-result.txt (SHA256 05ec55b05c2932bdb4fbadde54c4ad15bba702a14aa8f0b8a8961e084362b518)
/var/tmp/deploy-ga-1nh67zro-20261122.t4FHpp/base-comparisons-result.txt (SHA256 e818008dede096cdaae3143777aeb3a40ef7e6eaab163c065ebb2e767fd73764)
/var/tmp/deploy-ga-1nh67zro-20261122.t4FHpp/ci-closure.txt (SHA256 cf559da5cd3f2a12b296758280a5c71fce46c2c103551567368ad363877d4dbc)
/var/tmp/deploy-ga-1nh67zro-20261122.t4FHpp/native-windows-closure.txt (SHA256 3fecdddca96fa7539d6b30512dd99dc5468ba4208f396a75b7e8331c7f2e8f20)

## Publication-source validation

The canonical gate above tested the reviewed changes merged with the pinned current base. A separate publication check also ran the complete unit/policy suite on frozen source head 1c2d5955a2c61d20e51b779f983b867f5c725745, before this report-only supplement. Its command was gascity-bazel-legs.sh test --config=fork-cache --keep_going --jobs=4 //...: leg 1 completed 297 targets (296 PASS, one FAIL; 70 executed and 227 cached), and the tokenrender processwrapper leg passed its one target. Its aggregate exit was 3. The failed verdict is retained; no failed test was retried. The normal pre-push hook was not launched after this equivalent suite failed.

failure_attribution: TestDatedWaiversAgainstToday -> ga-5550q2dx
- Clause 1: not diff-owned. The failure is internal/testpolicy/waiverexpiry/waiverexpiry_test.go:41; the entire package is unchanged and has no shared diff hunks.
- Clause 2: tracker ga-5550q2dx was opened in full with its comments. Created 2026-11-05, it predates this run and explicitly documents this test and the historical provider-catalog expiry condition. This is the first occurrence on this deploy bead; its earlier hold was a different check-routed-test-rows condition. The sighting was appended to the existing tracker, without filing another.
- Clause 3(a), MECHANISM: computed module test-binary import closures on the frozen source (23 packages) and canonical merge (24) both exclude cmd/gc. The change contains only five cmd/gc test files and this Markdown record. It cannot change the datedWaivers literal ledger inputs or clock read.
- Clause 4: no path overlap with internal/testpolicy/waiverexpiry.

The frozen source lacks landed renewal bc60286b7564195ad20e2eca1925cbcab555d455, present in the pinned gate base and canonical merge. The failing messages name the t3bridge/exec waivers expired 2026-11-05 and fleet-fatal after 2026-11-19; ACP/k8s/hybrid are warnings only. Earlier renewal 08190f2b9975f9415fd0621483b6a54961f1379d is present in both trees and is not the missing fix. This session's canonical unit log independently records TestDatedWaiversAgainstToday PASS by name. No source rebase, waiver edit or waiver_ref is involved.

The non-diff-owned-gate-failure protocol authorizes a specific-head push with --no-verify after attribution. The final publication head differs from the checked source head only in this gate report; the exact resulting head is bound in bead metadata/notes before that push. The bead ownership and beads-chain pre-push checks are run explicitly. This authorizes branch publication; current-head required CI and the deferred Windows job remain required for merge.

publication_load: threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=14.42 run_max_load=25.61 run_mean_load=20.32 run_readings=58
publication_evidence: /var/tmp/deploy-ga-1nh67zro-20261122.t4FHpp/publish-result.txt
publication_failure_log: /var/tmp/deploy-ga-1nh67zro-20261122.t4FHpp/prepush-waiverexpiry-test.log (SHA256 bbee3e28cc795bfdcd03a9f235c3ad16e5fd49b4ba4e3efa2dd76de13f752ee9)
publication_import_closures: prepush-import-closure.txt, prepush-merge-import-closure.txt under the retained run directory.
