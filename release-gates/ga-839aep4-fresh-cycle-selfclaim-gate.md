# Fresh-cycle self-claim release gate

**Verdict:** **PASS**

Evaluated: 2026-10-16T17:27:52.143396+00:00

Deploy bead: ga-839aep4. Review: ga-zesnh7z.

- Reviewed source: `3e06a42279f4bf932eff3a41f03d91caa2f722de`
- Pinned base: `d53530d63fabc668cd0025d51268bb546d7f83a7`
- Tested scratch merge: `c5cb31f22f90204a87114e4540aaffde4b6ef3a8`
- Tested tree: `1100687f04181d276e6e26b79e421aef73d3c908`
- `merge_state: clean (origin/main@d53530d63fab)`
- `gate_base_kind: current-tip`
- `gate_base: d53530d63fabc668cd0025d51268bb546d7f83a7`
- `merge_validation: merge result built and vetted here`
- Evidence directory: `/var/tmp/deploy-ga-839aep4.8zfjb1ri`

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS | PASS | ga-zesnh7z records security/style/spec PASS at the exact resolved reviewed source; no carryover or source substitution. |
| 2 | Acceptance criteria | PASS | Architect ruling ga-pvjbx3 Q1/Q2/Q3 checked against code; all 14 changed tests PASS in the full suite; retained deployment probes 5 PASS, 0 FAIL, 0 SKIP. |
| 3 | Full CI-equivalent suite | PASS | All four forced remote phases and the mandatory native floor completed. Raw counts and by-name results below; no waiver. |
| 4 | Open high-severity review findings | PASS | Review decision records zero blockers/majors; O1-O7 are explicitly non-blocking follow-ups. |
| 5 | Clean final branch | PASS | The reviewed source and tested merge have no uncommitted tracked changes. This gate is the only deploy addition; publication requires a clean post-commit status. |
| 6 | Merge state and build/vet | PASS | Canonical merge tree matches merge-tree exactly; full Bazel build and nogo/vet PASS with ordinary hooks. |
| 7 | One feature theme | PASS | Fresh-cycle retention using live claims and uncached bead reads: five files under cmd/gc, eleven reviewed commits. Ancestry and internal-path guards PASS. |

Acceptance: Q1 uses a fresh session-scoped existence scan and retains on scan error; Q2 rereads the owning anchor store without cache; Q3 reads the previous assignee live and defers only while the session still owns it. The deployment probes exercise the previous failed primed-cache handoff and closed-anchor cases, including an uncached control. Full inspection is retained in acceptance-inspection.md.

Fast lane preceded suite launch: full build and nogo/vet; isolated pinned format (golangci-lint 2.12.0, mismatch=false); pinned-base changed-package nogo and four shell guards; nine uncached remote static policy targets; make bazel-sync plus empty diff/status; check-hooks. Logs carry FRESH_TREE_RECORD, GATE_BASE_RECORD and GLT_RECORD. The source changes no API, OpenAPI, dashboard, documentation or CI workflow.

```text
fast_lane: PASS 2026-10-16T16:36:41Z
heavy_mode: none
ci_lane_run: n/a (no CI-config change)
waiver_ref: none (gascity has no waiver path)
test_cmd_scope: full-suite
test_recipe: gate-criterion3.sh via gate-detached-run.sh / gate-admission.py / load-gate-run.sh / isolated-test-run.sh; make check; acceptance; integration-packages; integration-full (no smoke filter); required native floor
test_log_dir: /var/tmp/deploy-ga-839aep4.8zfjb1ri/c3-run/evidence/remote/logs
test_runner: /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-criterion3.sh --repo /var/tmp/gc-merge-validate.vhimppyt --base d53530d63fabc668cd0025d51268bb546d7f83a7 --bead-json /var/tmp/deploy-ga-839aep4.8zfjb1ri/bead.json --owned /var/tmp/deploy-ga-839aep4.8zfjb1ri/owned.tsv --run-dir /var/tmp/deploy-ga-839aep4.8zfjb1ri/c3-run/evidence
suite_targets: unit=make check; acceptance=bazel test --config=acceptance //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests; integration-packages=bazel test --config=integration //test:integration_packages; integration-full=bazel test --config=integration --test_arg=-test.v //test/integration:integration_test
remote_flags: --config=ci --keep_going --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1
test_cmd: ["test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-839aep4.8zfjb1ri/c3-run/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec", "test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-839aep4.8zfjb1ri/c3-run/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec", "test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-839aep4.8zfjb1ri/c3-run/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec", "test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-839aep4.8zfjb1ri/c3-run/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec"]
test_exec_mode: "remote-exec"
test_exec_bep: {"acceptance": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 24, "results": 24, "retried": 0, "window_ok": true}, "integration-full": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 14, "results": 14, "retried": 0, "window_ok": true}, "integration-packages": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 81, "results": 81, "retried": 0, "window_ok": true}, "unit": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 371, "results": 371, "retried": 0, "window_ok": true}}
rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:0c6c05c2196c246dba37e106a3d028e2f47f2e3d7eae0e41c2fe2bd9b2e4f6f0", "worker_env_tree": "sha256:0c6c05c2196c246dba37e106a3d028e2f47f2e3d7eae0e41c2fe2bd9b2e4f6f0"}
fallback_reason: null
admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 105.17004013061523, "waited_seconds": 0.001}
farm_test_seconds: 8917.811
owned_topup: []
skip_inventory: /var/tmp/deploy-ga-839aep4.8zfjb1ri/c3-run/evidence/remote/skip-inventory.json (355 farm skip rows; none diff-owned)
floor_topup: complete, 16/16 steps, all exit 0; /var/tmp/deploy-ga-839aep4.8zfjb1ri/c3-run/evidence/remote/floor-topup.json
load_threshold: 15
load_waited_seconds: 1800
load_wait_timed_out: 1
run_start_load: 23.18
run_max_load: 23.69
run_mean_load: 22.85
run_readings: 7
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1800 wait_timed_out=1 run_start_load=23.18 run_max_load=23.69 run_mean_load=22.85 run_readings=7 wait_first_load=26.71 wait_max_load=58.54 wait_mean_load=33.89 wait_readings=61 read_errors=0
```

Raw terminal evidence (top-level results summed per job; subtests excluded from counts):

```text
GATE_TEST_EVIDENCE logs_dir=/var/tmp/deploy-ga-839aep4.8zfjb1ri/c3-run/evidence/remote/logs logs=506 logs_with_results=440
test_counts: PASS=115239 FAIL=0 SKIP=369
  (top-level result lines, column 0, summed over the 506 logs; a test that several jobs run counts once per job; sub-tests are not counted)
logs_without_results: floor-13.log floor-14.log floor-15.log unit-02e0324f1ef05430d7e4.log unit-04c4f9e4335a6f1c2dfa.log unit-0787047af2c4dad6aebc.log unit-0eb92a6bd00590d91db6.log unit-0f39cb7e4c03399a72c4.log unit-1a34395a607c0208a005.log unit-1e67f201b300dc5f9e78.log unit-21372f682c4033d14a80.log unit-22d5121476fcaa40b586.log (+54 more)
diff_tests_executed:
  TestFreshAnchorBeadTerminal_BypassesPrimedCacheOnExternalClose PASS (integration-packages-514989cc035c0e9a8ff5.log, unit-637c74c578a2b17fc654.log)
  TestFreshCycleRepro_AnchorAlreadyClosedDoesNotCycle PASS (integration-packages-e4245ddf6a37030111bc.log, unit-64014bc0bb3833bc198c.log)
  TestFreshCycleRepro_BdClaimedNextBeadIsSelfClaim PASS (integration-packages-aa6cf5c138f0b61b866a.log, unit-23e7d9fc763496dd8095.log)
  TestFreshCycleRepro_ClaimAfterTickReadRetainsSession PASS (integration-packages-75af79934639abaae69c.log, unit-27b23e708b477fce4dad.log)
  TestFreshCycleRepro_ExternalHandoffThroughPrimedCacheDoesNotDefer PASS (integration-packages-c2ab4e7d4735e1bd66a7.log, unit-1ab526405ace21879f85.log)
  TestFreshCycleRepro_IncarnationStartedAfterCloseUnderOtherAssigneeDefers PASS (integration-packages-bf8ed8ba46004086b3b9.log, unit-0e1499ad8e8a0a6b9d81.log)
  TestFreshCycleRepro_IncarnationStartedAfterRealCloseInRigStoreDefers PASS (integration-packages-830685c60d0c2dd88567.log, unit-cd6930bb0d0da8646500.log)
  TestFreshCycleRepro_IncarnationStartedBeforeRealCloseInRigStoreCycles PASS (integration-packages-f7fba3a97bc02cffff0b.log, unit-10cc7795c0ec199a5530.log)
  TestFreshCycleRepro_OpenPreviousBeadNotOwnedBySessionCycles PASS (integration-packages-074fc6c34a1fb2577127.log, unit-3d6d3036a2f5902472b5.log)
  TestFreshCycleRepro_SelfClaimOfRootWhileAnchorIsStep PASS (integration-packages-9326181a736eb30b71ca.log, unit-4d20b3d28497f844aa39.log)
  TestFreshCycleRepro_SelfClaimOfStepWhileAnchorIsRoot PASS (integration-packages-56f04454dd9158a9ee8a.log, unit-2ef13e1b6898c5544764.log)
  TestFreshCycleRepro_StaleSelfClaimStampDoesNotHideRealClaim PASS (integration-packages-12697e2ea0d32c278115.log, unit-2bde55fab377f50b4739.log)
  TestFreshCycleRepro_UnreadableClaimScanRetainsSession PASS (integration-packages-821a75e4a503601af052.log, unit-493754ea2d5b4d8858c8.log)
  TestPrevAssignedBeadStatus_BypassesPrimedCacheOnExternalHandoff PASS (integration-packages-2ab73b52d8fd4dc346df.log, unit-423c6a0da1986b90e9a7.log)
GATE_TEST_EVIDENCE_RESULT ok: 14 diff-owned tests verified by name
```

Skip verification: 228 directly located farm skip sites are UNCHANGED; the remaining 67 rows point to five helpers whose full bytes match the pinned base. They cover backend capability/ledgered conformance exclusions, unsupported runtime fencing and classified-start fixtures, and the absent optional legacy binary. Native environment-absence top-ups completed; their remaining skip sites are independently proven unchanged in native-skip-proof.json. No changed test skipped or failed. New fresh-cycle logic cannot change these pre-test environment/capability guards. Detailed reasons and proofs are retained in skip-site-proof.json, skip-supplemental-proof.json, native-skip-proof.json and the raw logs. All 980 farm artifacts were independently length/SHA-256 verified.

Review inputs and limits: the committed regressions kill the stale-read mutations; secondary owner-error/status/trim test-strength gaps are tracked separately. The review records the accepted complexity increase from the carried lineage and the separate v2 A13 follow-up. These observations are not represented as new independent gate tests or as waived failures.

Reviewed commit lineage (resolved by git, with RED/GREEN history):

```text
6acb698b03973b4cde27b196d48ef1426d3958c1 test(session-reconciler): red — fresh-cycle guard misses self-claim + stale anchor (ga-pvjbx3)
b9e659dd6c52f4872490196c1f638db0a0d25282 fix(reconciler): fresh-cycle guard checks live claim state, not anchor equality
7b0a3a534238c979c616682df2a7548b8efeac0e chore(bazel): sync cmd/gc BUILD.bazel with new test (refs ga-3wsaxf)
f9745850cb14f51f26d64f968c527fad59f78eee test(session-reconciler): red — fresh-cycle guard fails open on a dark claim scan and gates row C on ownership (refs ga-m27s8y)
51a40a7351884c35524b1b894108d1911d882268 fix(reconciler): fresh-cycle claim scan fails closed; Q3 narrows row A only (refs ga-m27s8y)
c2591b8ffeca8064e433ecc3182c954d96aca56e test(session-reconciler): red — fresh-cycle claim scan reads the tick's read, not the store, on a later claim (refs ga-67v46hf)
78eb6e34d38e993bd83942c0a3539c1d917ab8d0 fix(reconciler): fresh-cycle claim scan reads the store now, not the tick's read (refs ga-67v46hf)
2a5b9aefc0bb346925dc0322050e06346527e507 test(feat): red — needs-deploy: Architecture: fresh-cycle self-claim signal (current_claim_bead_id) is never written by the claim paths builder/investigator use; 5 of 6 kills on 2026-09-25 are "correct" under ga-3pocz7 (from:ga-bjrml9l) (refs ga-1rax433)
e14909c2ae746d08bbbc93e71b497eb958ed1717 feat: green — needs-deploy: Architecture: fresh-cycle self-claim signal (current_claim_bead_id) is never written by the claim paths builder/investigator use; 5 of 6 kills on 2026-09-25 are "correct" under ga-3pocz7 (from:ga-bjrml9l) (refs ga-1rax433)
a5e0ed3d32daeac6e9468fb3b83b2d698846dd87 test(feat): red — needs-deploy: Architecture: fresh-cycle self-claim signal (current_claim_bead_id) is never written by the claim paths builder/investigator use; 5 of 6 kills on 2026-09-25 are "correct" under ga-3pocz7 (from:ga-ornbhgw) (refs ga-2wc69nx)
3e06a42279f4bf932eff3a41f03d91caa2f722de feat: green — needs-deploy: Architecture: fresh-cycle self-claim signal (current_claim_bead_id) is never written by the claim paths builder/investigator use; 5 of 6 kills on 2026-09-25 are "correct" under ga-3pocz7 (from:ga-ornbhgw) (refs ga-2wc69nx)
```
