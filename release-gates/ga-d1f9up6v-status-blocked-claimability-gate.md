# Recorded blocked-status claimability — release gate

**Verdict:** **PASS**

Deploy bead: `ga-d1f9up6v`; build: `ga-5sxxd0w`; review: `ga-086mrpgq`.
Reviewed source: `75f0e61c00ccc1def717bfe2dcc8a98ac2e1ac8c`. Mode: remote; push remote: fork; base branch: main.
Pinned gate base: `37b7b762edba99b21e62b6d73af109e40fd563c8`; gate_base_kind: current-tip.
Tested merge: `24ed6a4254f72be8e85097f578d7ef6d0b265f49`; tree: `5154b91515cff20d8225f6210061adf2288515f2`.
merge_state: clean (origin/main@37b7b762edba99b21e62b6d73af109e40fd563c8; files: none).
merge_validation: merge result built and vetted here at the pinned base. MPR owns merging; a later head requires a fresh gate.

| # | Result | Evidence |
|---|---|---|
| 1 — Review | PASS | Reviewer PASS on the exact resolved source; no carryover or reviewer-added commit. |
| 2 — Acceptance | PASS | Recorded blocked status wins over the blockedness projection, after deferred/held precedence. Divergence maps directly to DemandClaimBlocked, and drain-ack suppresses the assigned-work alarm without reading dependencies. All five owned tests, controls and wire-decode pin passed by name; both production cause-switch consumers were inspected. |
| 3 — Tests and policy | PASS | Complete five-phase uncached remote suite, native floor, policy/drift lanes, unchanged skip-site proof and six-leaf required-CI accounting below. |
| 4 — Severity | PASS | Zero unresolved HIGH findings in the review. |
| 5 — Clean branch | PASS | Role checkout and test scratch pristine before writing this record. The isolated deploy head must contain the reviewed source plus this record only; publication verifies a clean worktree and exact record-only diff after commit. |
| 6 — Merge build/vet | PASS | Exact clean merge tree built and vetted/nogo; both BEPs overallSuccess=true. |
| 7 — Theme | PASS | One recorded blocked-status diagnostic classification theme, seven files plus this record. |

The two checked consumers are classifyDemandTrigger in cmd/gc/demand_divergence.go and openAssignedRowProvablyNonClaimable in cmd/gc/session_reconciler.go. Both explicitly handle demandRowStatusBlocked. Empty raw status preserves existing behavior; deferred and held retain precedence. The internal/events change widens a doc comment for the existing classification, with no wire/schema change.

test_cmd_scope: full-suite; heavy_mode: none; waiver_ref: none (Gas City has no waiver path).
test_cmd: `env GC_GATE_REMOTE_ENABLED=1 GC_REMOTE_GATE_FLOOR_GIB=25 GC_REMOTE_GATE_CAP=2 ISOLATED_TEST_BEAD=ga-d1f9up6v GC_GATE_DELTA=shadow GC_GATE_LOAD_DEFER_MAX=3 /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-criterion3.sh --repo /var/tmp/gc-merge-validate.0b1B0N --base 37b7b762edba99b21e62b6d73af109e40fd563c8 --bead-json /var/tmp/gc-gate-ga-d1f9up6v-m787m8ah/c3-bead.json --owned /var/tmp/gc-gate-ga-d1f9up6v-m787m8ah/owned.tsv --run-dir /var/tmp/gc-gate-ga-d1f9up6v-m787m8ah/c3-run/evidence`.
Unit command: make -k check (whole-tree bazel test //... plus shell guards); acceptance: bazel test --config=acceptance //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests; integration packages: bazel test --config=integration //test:integration_packages; full integration and its separated latency leg: //test/integration:integration_test. Exact per-phase flags, commands and BEPs are retained in the execution proof.
Phases: whole-tree unit/check; acceptance; every integration-tagged package; full integration excluding latency inventory; inventory-only latency under a clear farm probe. All force --nocache_test_results and --noremote_local_fallback, first attempts, remote strategy and phase-window start proven from BEP.
diff_owned_evidence: /var/tmp/gc-gate-ga-d1f9up6v-m787m8ah/merge-owned-explain.txt (owned set identical at source and tested merge).
not_owned_skip_evidence: /var/tmp/gc-gate-ga-d1f9up6v-m787m8ah/skip-source-audit/evidence.json; per-name site/reason/unchanged proof, no shared/helper hunks; base measurement not required.
test_bd: isolated-test-run:   pinned test bd (gm-i3csn7): GASCITY_TEST_BD version=v1.3.4 bin=/home/jaword/.local/bd-versions/v1.3.4/bd reports='bd version 1.3.4 (7ef2fd05e: HEAD@7ef2fd05eb0c)' ref_check=match:7ef2fd05e origin=preexisting pin_source=/var/tmp/gc-merge-validate.0b1B0N/deps.env displaces='bd version 1.3.2 (9b65b9e19)'
gate_base: 37b7b762edba99b21e62b6d73af109e40fd563c8; gate_head: 75f0e61c00ccc1def717bfe2dcc8a98ac2e1ac8c; delta_gate: {"mode":"shadow","decision":"full","reason":"not-gate-pr","detail":"no PR record (pr.json)","applied":false,"base":"37b7b762edba99b21e62b6d73af109e40fd563c8","head":"75f0e61c00ccc1def717bfe2dcc8a98ac2e1ac8c","merge":"24ed6a4254f72be8e85097f578d7ef6d0b265f49","prior":null,"delta":{},"changed_labels":[],"changed_label_count":0,"tests":{},"phases":{},"plan_sha256":"6335ddfb2d8696d0eb1f62d65525966d565eacdd66f6ffc16a730b010f7da69e"}.
failure_attribution: none; policy_attribution: none; post_merge_criteria: none.
rbe_state: {"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"}.
admission: {"class":"remote","floor_gib":25.0,"free_gib":58.01045227050781,"waited_seconds":0.001,"cap":2}.
test_exec_bep: {"unit":{"results":426,"remote_executed":426,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":87,"remote_executed":87,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":15,"remote_executed":15,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}}.
owned_topup: [].
test_exec_mode: remote-exec; fallback_reason: none; farm_test_seconds: 15234.145.
test_log_dir: `/var/tmp/gc-gate-ga-d1f9up6v-m787m8ah/c3-run/evidence/remote/logs` (retained).

```text
test_counts: PASS=158832 FAIL=0 SKIP=618
  (top-level result lines, column 0, summed over the 570 logs; a test that several jobs run counts once per job; sub-tests are not counted)
diff_tests_executed:
  TestBdStatusBlockedRowClassifiesAsStatusBlocked PASS (integration-packages-75af79934639abaae69c.log, unit-fd986cf5bab8709b8d45.log)
  TestClassifyDemandRowClaimabilityStatusBlocked PASS (integration-packages-074fc6c34a1fb2577127.log, unit-d9e667428f972d067bd3.log)
  TestDivergenceStatusBlockedRowIsBlockedWithoutSettlement PASS (integration-packages-e5330b8c981927473156.log, unit-64014bc0bb3833bc198c.log)
  TestOpenAssignedRowStatusBlockedMakesNoDependencyRead PASS (integration-packages-f7fba3a97bc02cffff0b.log, unit-3d6d3036a2f5902472b5.log)
  TestReconcileSessionBeads_DrainAckStatusBlockedAssignedWorkSuppressesEvent PASS (integration-packages-b72a67a2f9444af705e3.log, unit-0e1499ad8e8a0a6b9d81.log)
```

policy_lane: PASS — make check-release-dist-ignore check-routed-test-rows check-split-topology-rows check-residency-boundary check-hooks; explicit nogo/lint; thirteen policy/docsync targets.
drift_lane: PASS — make bazel-sync followed by git diff --exit-code and empty porcelain; genspec/schema/client/SPA/lockfile sync targets; openapi-breaking-check against the pinned gate base.
fast_lane: PASS 2026-11-22T18:44:23.702741+00:00 merge_head=24ed6a4254f72be8e85097f578d7ef6d0b265f49.
known_red_precheck: known-red-precheck: target=check-routed-test-rows verdict=green merge=0 base=- (it passes on the merge tree); known-red-precheck: stale: tracker=ga-1ptv4jey `make check-routed-test-rows` passes on this merge tree; the tracker may be ready to close; known-red-precheck: proceed reason=nothing-known-red (check-routed-test-rows=green).
format_lane: PASS — //:go_format_test. All lanes ran in separate fresh views before the suite. Strict storeclass predicates used trusted base tools and passed.
complexity: zero-growth form (ii); base and merge both exit 2, no merge row worse than its base row, no new/regressed/deleted threshold changes, baseline untouched. Raw outputs retained; this is the documented scoring rule, no waived failure.
ci_lane_run: n/a (no CI-config change).

skip_justification: all printed skip sites are UNCHANGED at the tested merge, with line_in_hunk=no and no SHARED-HUNKS or HELPER-HUNKS in the diff. Platform, lane, missing-fixture/provider and environment-absence reasons are inventoried by test, site and log. The five added tests introduce no shared/helper hunks. Production changes consume recorded status in the claimability predicate and two diagnostics; none of the unchanged platform/lane/fixture/provider skip predicates consumes that classification. No same-command baseline measurement is required under the documented unchanged-site rule. All native environment-absence top-ups and the local floor completed; none of the five owned tests or their subtests skipped.

Native environment: rootless Podman/Dolt image and sweep preflight passed; ICU 74 recorded libraries verdict=OK.

Load observations (wait_* and run_* are separate; timed-out ordinary waits are recorded):

```text
load_threshold: 15
load_waited_seconds: 0
load_wait_timed_out: 0
run_start_load: 19.02
run_max_load: 45.58
run_mean_load: 23.22
run_readings: 92
storeclass-proof/wrapper.stderr: LOAD_GATE_SUMMARY threshold=15 waited_seconds=810 wait_timed_out=0 run_start_load=14.84 run_max_load=18.63 run_mean_load=18.30 run_readings=5 wait_first_load=15.47 wait_max_load=20.40 wait_mean_load=17.06 wait_readings=28 read_errors=0
c3-run/units/gate.w1.stderr.log: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=19.02 run_max_load=45.58 run_mean_load=23.22 run_readings=92 wait_first_load=19.02 wait_max_load=19.02 wait_mean_load=19.02 wait_readings=1 read_errors=0
c3-run/evidence/remote/floor-topup.stdout: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1171 wait_timed_out=0 run_start_load=14.78 run_max_load=18.28 run_mean_load=17.05 run_readings=9 wait_first_load=24.96 wait_max_load=26.50 wait_mean_load=19.50 wait_readings=40 read_errors=0
```

ci_required_closure: `python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-validate.0b1B0N 37b7b762edba99b21e62b6d73af109e40fd563c8 75f0e61c00ccc1def717bfe2dcc8a98ac2e1ac8c fork`; workflow blob `fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612`; 6 LEAF jobs need a status; the rest are rolled up through their needs..

ci_required_accounting:

- runner-policy | LOCAL-PASS | command /home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-merge-validate.0b1B0N -- /home/jaword/projects/gc-management/packs/actual/deployer/scripts/gate-base.sh run -- /home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh --threshold 15 --max-wait 1800 --poll-interval 30 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- timeout --signal=TERM --kill-after=10s 1800 python3 /var/tmp/gc-deployer-required-ci-20261122.py job /var/tmp/gc-gate-ga-d1f9up6v-m787m8ah runner-policy; exit 0; log /var/tmp/gc-gate-ga-d1f9up6v-m787m8ah/required-ci/runner-policy.stdout
- changes | LOCAL-PASS | command python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-validate.0b1B0N 37b7b762edba99b21e62b6d73af109e40fd563c8 75f0e61c00ccc1def717bfe2dcc8a98ac2e1ac8c fork; exit 0; log /var/tmp/gc-gate-ga-d1f9up6v-m787m8ah/required-ci/closure.stdout
- credential-provider-windows | DEFERRED-TO-PR-CI reason=non-linux-runner |   credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'
- pack-gate | LOCAL-PASS | command /home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-merge-validate.0b1B0N -- /home/jaword/projects/gc-management/packs/actual/deployer/scripts/gate-base.sh run -- /home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh --threshold 15 --max-wait 1800 --poll-interval 30 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- timeout --signal=TERM --kill-after=10s 1800 python3 /var/tmp/gc-deployer-required-ci-20261122.py job /var/tmp/gc-gate-ga-d1f9up6v-m787m8ah pack-gate; exit 0; log /var/tmp/gc-gate-ga-d1f9up6v-m787m8ah/required-ci/pack-gate.stdout
- windows-lane | LOCAL-PASS | command /home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-merge-validate.0b1B0N -- /home/jaword/projects/gc-management/packs/actual/deployer/scripts/gate-base.sh run -- /home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh --threshold 15 --max-wait 1800 --poll-interval 30 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- timeout --signal=TERM --kill-after=10s 1800 python3 /var/tmp/gc-deployer-required-ci-20261122.py job /var/tmp/gc-gate-ga-d1f9up6v-m787m8ah windows-lane; exit 0; log /var/tmp/gc-gate-ga-d1f9up6v-m787m8ah/required-ci/windows-lane.stdout
- windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner |   windows-lane-test                      LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} EVERY-PR

windows_reach: none for each native Windows job, each checked by its own test import closure.
deferred_to_pr_ci: credential-provider-windows (non-linux-runner), windows-lane-test (non-linux-runner)
not_covered: none
host_gaps: none. PR-open PASS does not replace green CI / required or successful deferred jobs on this exact head.

Commands and exit statuses (native floor commands supplement the full suite):

```text
bazel build --keep_going --output_groups=nogo_fix --config=remote-exec --noremote_local_fallback --build_event_json_file=/var/tmp/gc-gate-ga-d1f9up6v-m787m8ah/c6-vet.bep.json //... ; exit=0
/home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-merge-validate.0b1B0N -- /home/jaword/projects/gc-management/packs/actual/deployer/scripts/gate-base.sh run -- /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-admission.py --class remote --bead ga-d1f9up6v --label fast-format --record /var/tmp/gc-gate-ga-d1f9up6v-m787m8ah/fast-format-admission.json --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh --rbe -- bash /var/tmp/gc-gate-ga-d1f9up6v-m787m8ah/fast-lane.sh format ; exit=0
/home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-merge-validate.0b1B0N -- /home/jaword/projects/gc-management/packs/actual/deployer/scripts/gate-base.sh run -- /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-admission.py --class remote --bead ga-d1f9up6v --label fast-policy --record /var/tmp/gc-gate-ga-d1f9up6v-m787m8ah/fast-policy-admission.json --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh --rbe -- bash /var/tmp/gc-gate-ga-d1f9up6v-m787m8ah/fast-lane.sh policy ; exit=0
/home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-merge-validate.0b1B0N -- /home/jaword/projects/gc-management/packs/actual/deployer/scripts/gate-base.sh run -- /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-admission.py --class remote --bead ga-d1f9up6v --label fast-drift --record /var/tmp/gc-gate-ga-d1f9up6v-m787m8ah/fast-drift-admission.json --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh --rbe -- bash /var/tmp/gc-gate-ga-d1f9up6v-m787m8ah/fast-lane.sh drift ; exit=0
/home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-merge-validate.0b1B0N -- /home/jaword/projects/gc-management/packs/actual/deployer/scripts/gate-base.sh run -- /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-admission.py --class remote --bead ga-d1f9up6v --label fast-openapi --record /var/tmp/gc-gate-ga-d1f9up6v-m787m8ah/fast-openapi-admission.json --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh --rbe -- bash /var/tmp/gc-gate-ga-d1f9up6v-m787m8ah/fast-lane.sh openapi ; exit=0
/home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-merge-validate.0b1B0N -- /home/jaword/projects/gc-management/packs/actual/deployer/scripts/gate-base.sh run -- /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-admission.py --class remote --bead ga-d1f9up6v --label fast-complexity --record /var/tmp/gc-gate-ga-d1f9up6v-m787m8ah/fast-complexity-admission.json --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh --rbe -- bash /var/tmp/gc-gate-ga-d1f9up6v-m787m8ah/fast-lane.sh complexity ; exit=0
go build -o /var/tmp/gc-storeclass-trusted-pij5x9gy/storeclass-baseline-trusted ./analyzers/storeclass/cmd/storeclass-baseline ; exit=0
/var/tmp/gc-storeclass-trusted-pij5x9gy/storeclass-baseline-trusted -repo /var/tmp/gc-fresh-tree.E5PCCS ; exit=0
/var/tmp/gc-storeclass-trusted-pij5x9gy/storeclass-baseline-trusted -repo /var/tmp/gc-fresh-tree.E5PCCS -growth-since 37b7b762edba99b21e62b6d73af109e40fd563c8 -base-tip 37b7b762edba99b21e62b6d73af109e40fd563c8 -gate-only ; exit=0
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-d1f9up6v-m787m8ah/c3-run/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-d1f9up6v-m787m8ah/c3-run/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-d1f9up6v-m787m8ah/c3-run/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-d1f9up6v-m787m8ah/c3-run/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-d1f9up6v-m787m8ah/c3-run/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
GATE_PHASE phase=unit test_rc=0 predicate_rc=0 start_ms=1795373094143 end_ms=1795373303851
GATE_PHASE phase=acceptance test_rc=0 predicate_rc=0 start_ms=1795373305070 end_ms=1795373447112
GATE_PHASE phase=integration-packages test_rc=0 predicate_rc=0 start_ms=1795373448340 end_ms=1795373769525
GATE_PHASE phase=integration-full test_rc=0 predicate_rc=0 start_ms=1795373770418 end_ms=1795374295675
GATE_PHASE phase=integration-latency test_rc=0 predicate_rc=0 start_ms=1795374296607 end_ms=1795374407573
go test -tags integration -count=1 -v -run '^(TestCollectInputConvoyRootsAgreesWithTheFrozenReference|TestConvoyAutocloseAgreesWithTheFrozenReference|TestMoleculeAutocloseAgreesWithTheFrozenReference|TestReplay|TestSessionEventPumpLiveHerdr|TestSimCoverageGate|TestSweepStaleTmuxServers_KeepsServerWhoseRootExistsInItsOwnMountNamespace|TestSweepStaleTmuxServers_ReapsServerWhoseRootIsGoneInItsOwnMountNamespace)$' ./cmd/gc ; exit=0
go test -tags integration -count=1 -v -run '^(TestAttachedRigScopeShellToken|TestJsonlExportPushRetryDelayIsLocaleIndependent)$' ./examples/gastown ; exit=0
go test -tags integration -count=1 -v -run '^(TestMetadataEmptyStringClearContract|TestProbeVersionsAgainstHostBinaries)$' ./internal/beads ; exit=0
go test -tags integration -count=1 -v -run '^(TestPinnedBeadsModuleDirResolvesThisBuildsOwnCache)$' ./internal/beads/beadstest ; exit=0
go test -tags integration -count=1 -v -run '^(TestMigrateJournalFileMatchesPinnedBeads)$' ./internal/beads/contract ; exit=0
go test -tags integration -count=1 -v -run '^(TestBrProviderConformance)$' ./internal/beads/exec ; exit=0
go test -tags integration -count=1 -v -run '^(TestCustomTypesCheck_ServerBackedStoreIgnoresAmbientEndpoint|TestCustomTypesCheck_TableDrift|TestCustomTypesCheck_TableDriftUsesTestOwnedDoltContext)$' ./internal/doctor ; exit=0
go test -tags integration -count=1 -v -run '^(TestMCPMailBridgeSourceable|TestMCPMailConformance|TestMCPMailCrossPodNameResolution|TestMCPMailProjectKeyIsolation|TestMCPMailReplyMissingOriginalIsNotFound)$' ./internal/mail/exec ; exit=0
go test -tags integration -count=1 -v -run '^(TestActivityLive|TestHerdrConformance|TestProviderLive|TestProviderLiveClaudeKindPath|TestProviderLiveOccupantSwapKeepsLiveness|TestSessionEventsLive)$' ./internal/runtime/herdr ; exit=0
go test -tags integration -count=1 -v -run '^(TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors)$' ./internal/runtime/runtimetest ; exit=0
go test -tags integration -count=1 -v -run '^(TestGetKeyBinding_CapturesUserBinding|TestGetKeyBinding_SkipsGasTownBindings|TestIsGTBinding_DetectsGasTownBindings|TestSetBindings_PreserveFallbackOnRepeatedCalls)$' ./internal/runtime/tmux ; exit=0
go test -tags integration -count=1 -v -run '^(TestBdLatestSchemaVersionIsolatesHOMEFromSharedServerConfig|TestBdLatestSchemaVersionLeavesNothingInItsTempRoot|TestRealBdProvisionLeavesTheHostHomeAlone)$' ./test/acceptance/helpers ; exit=0
go test -tags integration -count=1 -v -run '^(TestStartDrift_SystemdManaged_RestartsToNewBuildID)$' ./test/integration ; exit=0
make test-fsys-darwin-compile ; exit=0
bash scripts/test-push-gate-lock.sh ; exit=0
bash scripts/test-local-concurrency.sh ; exit=0
python3 .github/workflows/scripts/runner_policy.py
 ; exit=0
for attempt in 1 2 3; do
  if scripts/update-bundled-gastown-pack --check; then
    exit 0
  fi
  if [ "$attempt" = "3" ]; then
    exit 1
  fi
  sleep $((attempt * 10))
done
 ; exit=0
for attempt in 1 2 3; do
  if make test-pack-registry-live; then
    exit 0
  fi
  if [ "$attempt" = "3" ]; then
    exit 1
  fi
  sleep $((attempt * 10))
done
 ; exit=0
GOOS=windows go build $(cat .github/windows-lane/build.txt) ; exit=0
GOOS=windows go test -count=1 -vet=off -run '^$' -exec /bin/true $(cat .github/windows-lane/test-build.txt) ; exit=0
GOOS=windows go build ./analyzers/... && GOOS=windows go test -count=1 -vet=off -run '^$' -exec /bin/true ./analyzers/... ; exit=0
GOOS=darwin go build ./... && GOOS=darwin go test -count=1 -vet=off -run '^$' -exec /bin/true ./... ; exit=0
```

Execution metadata and inventories are retained by hash, not pasted into this record:

```json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/gc-gate-ga-d1f9up6v-m787m8ah/c3-run/evidence/remote","gate_base":"37b7b762edba99b21e62b6d73af109e40fd563c8","gate_head":"75f0e61c00ccc1def717bfe2dcc8a98ac2e1ac8c","admission":{"class":"remote","floor_gib":25.0,"free_gib":58.01045227050781,"waited_seconds":0.001,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"},"delta_gate":{"mode":"shadow","decision":"full","reason":"not-gate-pr","detail":"no PR record (pr.json)","applied":false,"base":"37b7b762edba99b21e62b6d73af109e40fd563c8","head":"75f0e61c00ccc1def717bfe2dcc8a98ac2e1ac8c","merge":"24ed6a4254f72be8e85097f578d7ef6d0b265f49","prior":null,"delta":{},"changed_labels":[],"changed_label_count":0,"tests":{},"phases":{},"plan_sha256":"6335ddfb2d8696d0eb1f62d65525966d565eacdd66f6ffc16a730b010f7da69e"},"test_exec_bep":{"unit":{"results":426,"remote_executed":426,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":87,"remote_executed":87,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":15,"remote_executed":15,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":15234.145,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=5fd31cdd75b202be3ab9de14bb2514f059b7969d1f14ab7fdc392c2c723e6b7b"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":108.237,"evidence_errors":[],"probe":["GATE_FARM_PROBE waiting waited=0s load1=77.62 load5=100.10 load15=62.55 cpus=32 threshold=50 max_wait=600s","GATE_FARM_PROBE verdict=clear waited=30s load1=47.04 load5=90.53 load15=60.56 cpus=32 threshold=50 max_wait=600s"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 37.51 83.20 59.20 23/1476 57209"]}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":38722}},"log_dir":"/var/tmp/gc-gate-ga-d1f9up6v-m787m8ah/c3-run/evidence/remote/floor-logs"},"skip_inventory":{"counts_by_reason_class":{"other":514,"environment-absence":90},"top_level_counts_by_reason_class":{"other":256,"environment-absence":43},"tests":{"elided_array":{"items":604,"bytes":217415}}},"test_cmd":{"elided_array":{"items":5,"bytes":2064}},"test_counts":{"PASS":158832,"FAIL":0,"SKIP":618},"diff_tests_executed":"diff_tests_executed:\n  TestBdStatusBlockedRowClassifiesAsStatusBlocked PASS (integration-packages-75af79934639abaae69c.log, unit-fd986cf5bab8709b8d45.log)\n  TestClassifyDemandRowClaimabilityStatusBlocked PASS (integration-packages-074fc6c34a1fb2577127.log, unit-d9e667428f972d067bd3.log)\n  TestDivergenceStatusBlockedRowIsBlockedWithoutSettlement PASS (integration-packages-e5330b8c981927473156.log, unit-64014bc0bb3833bc198c.log)\n  TestOpenAssignedRowStatusBlockedMakesNoDependencyRead PASS (integration-packages-f7fba3a97bc02cffff0b.log, unit-3d6d3036a2f5902472b5.log)\n  TestReconcileSessionBeads_DrainAckStatusBlockedAssignedWorkSuppressesEvent PASS (integration-packages-b72a67a2f9444af705e3.log, unit-0e1499ad8e8a0a6b9d81.log)\nGATE_TEST_EVIDENCE_RESULT ok: 5 diff-owned tests verified by name\n"}
```

Full execution retained: 6059 lines; /var/tmp/gc-gate-ga-d1f9up6v-m787m8ah/c3-run/evidence/execution.json; SHA256 06343d59ac2d6ce0afd9c3d1cf433c0491a777bb55f525126d2519a8f3077987.
Full skip sites retained: 5424 lines; /var/tmp/gc-gate-ga-d1f9up6v-m787m8ah/skip-source-audit/evidence.json; SHA256 cb70b06aba26bee5ad5ed53c1fd10dc5360cf139889490d9d611537f6556a39f.
Full CI accounting retained: 223 lines; /var/tmp/gc-gate-ga-d1f9up6v-m787m8ah/required-ci/accounting.json; SHA256 2dbec48e4b9041c7e1ce3a81fa1b2cf4c10729b20a73b2194dcbdf9d42107ceb.
Full known-red precheck retained: 5 lines; /var/tmp/gc-gate-ga-d1f9up6v-m787m8ah/known-red-precheck.json; SHA256 bab9e01655a9cd66afa59c6dab6e0e37be9aa1b708b95f4648cb82c51430c4af.
Full runtime preflight retained: 113 lines; /var/tmp/gc-gate-ga-d1f9up6v-m787m8ah/runtime-preflight.json; SHA256 fa96f5675edec87a07c5e3cf337af890a5337efc2388725498456fe56857c451.
Full diff-owned explanation retained: 333 lines; /var/tmp/gc-gate-ga-d1f9up6v-m787m8ah/merge-owned-explain.txt; SHA256 567a7c88f87d48894b06add8ef01c3e6e85df3c73571a3a944bae172cfa4cbdf.
Full fast lanes retained: 173 lines; /var/tmp/gc-gate-ga-d1f9up6v-m787m8ah/fast-result.json; SHA256 f29907a720b9495ca35310522d8362e8368cdd6681a8df5ffe18f8e9735cbbda.
Full merge build/vet retained: 59 lines; /var/tmp/gc-gate-ga-d1f9up6v-m787m8ah/c6-audit.json; SHA256 dc35ee2ad07bcbb285136430a6a06e5f481a2386809b3688b98b87c4a4f8d5ff.
Full retained evidence manifest retained: 1691 lines; /var/tmp/gc-gate-ga-d1f9up6v-m787m8ah/gate-evidence-manifest.sha256; SHA256 461666311efaf446a7c67bc315ea70c33e8ade5a01313ed24dde29d80b31e9a8.
