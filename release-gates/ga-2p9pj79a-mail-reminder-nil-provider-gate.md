# Mail-reminder unknown-unread regression test — release gate

**Verdict:** **PASS**

Deploy bead: `ga-2p9pj79a`; build: `ga-2qa02l`; review: `ga-084c89ry`.
Reviewed source: `f46bd68e3e53d39ac953dce3cca9ff9b1c114798`. Mode: remote; push remote: fork; base branch: main.
Pinned gate base: `78fa5a1e2a799a1fbbafd18303f892935a2b0f96`; gate_base_kind: current-tip.
Tested merge: `28b87b4d62a85c0f760e48498050b7b355aa8325`; tree: `de9da1ff79b47f9204e4ed8586727edb5e617f07`.
merge_state: clean (origin/main@78fa5a1e2a799a1fbbafd18303f892935a2b0f96; files: none).
merge_validation: merge result built and vetted here at the pinned base. MPR owns merging; a later head requires a fresh gate.

| # | Result | Evidence |
|---|---|---|
| 1 — Review | PASS | Reviewer PASS on the exact resolved source; no carryover or reviewer-added commit. |
| 2 — Acceptance | PASS | Nil provider, empty session ID and mailbox-read error each retain reminders in order; errors surface. A readable empty-mailbox control removes only the reminder. One owned test and all three subtests passed by name; independent reviewer mutations proved the branches fail when broken. Historical gate description already matches the guard and needed no correction. |
| 3 — Tests and policy | PASS | Complete five-phase uncached remote suite, native floor, policy/drift lanes, unchanged skip-site proof and six-leaf required-CI accounting below. |
| 4 — Severity | PASS | Zero unresolved HIGH findings in the review. |
| 5 — Clean branch | PASS | Role checkout and test scratch pristine before writing this record. The isolated deploy head must contain the reviewed source plus this record only; publication verifies a clean worktree and exact record-only diff after commit. |
| 6 — Merge build/vet | PASS | Exact clean merge tree built and vetted/nogo; both BEPs overallSuccess=true. |
| 7 — Theme | PASS | One test-only mail-reminder guard coverage theme, one Go test file plus this record. |

One added whole test function; no production, helper, import, shared-fixture, API/schema/dashboard/CI/config/dependency changes. The existing keep-on-unknown behavior remains the contract being tested.

test_cmd_scope: full-suite; heavy_mode: none; waiver_ref: none (Gas City has no waiver path).
test_cmd: `env GC_GATE_REMOTE_ENABLED=1 GC_REMOTE_GATE_FLOOR_GIB=25 GC_REMOTE_GATE_CAP=2 ISOLATED_TEST_BEAD=ga-2p9pj79a GC_GATE_DELTA=shadow GC_GATE_LOAD_DEFER_MAX=3 /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-criterion3.sh --repo /var/tmp/gc-merge-validate.jrhEnZ --base 78fa5a1e2a799a1fbbafd18303f892935a2b0f96 --bead-json /var/tmp/gc-gate-ga-2p9pj79a-t8lomufl/c3-bead.json --owned /var/tmp/gc-gate-ga-2p9pj79a-t8lomufl/owned.tsv --run-dir /var/tmp/gc-gate-ga-2p9pj79a-t8lomufl/c3-run/evidence`.
Unit command: make -k check (whole-tree bazel test //... plus shell guards); acceptance: bazel test --config=acceptance //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests; integration packages: bazel test --config=integration //test:integration_packages; full integration and its separated latency leg: //test/integration:integration_test. Exact per-phase flags, commands and BEPs are retained in the execution proof.
Phases: whole-tree unit/check; acceptance; every integration-tagged package; full integration excluding latency inventory; inventory-only latency under a clear farm probe. All force --nocache_test_results and --noremote_local_fallback, first attempts, remote strategy and phase-window start proven from BEP.
diff_owned_evidence: /var/tmp/gc-gate-ga-2p9pj79a-t8lomufl/merge-owned-explain.txt (owned set identical at source and tested merge).
not_owned_skip_evidence: /var/tmp/gc-gate-ga-2p9pj79a-t8lomufl/skip-source-audit/evidence.json; per-name site/reason/unchanged proof, no shared/helper hunks; base measurement not required.
test_bd: isolated-test-run:   pinned test bd (gm-i3csn7): GASCITY_TEST_BD version=v1.3.4 bin=/home/jaword/.local/bd-versions/v1.3.4/bd reports='bd version 1.3.4 (7ef2fd05e: HEAD@7ef2fd05eb0c)' ref_check=match:7ef2fd05e origin=preexisting pin_source=/var/tmp/gc-merge-validate.jrhEnZ/deps.env displaces='bd version 1.3.2 (9b65b9e19)'
gate_base: 78fa5a1e2a799a1fbbafd18303f892935a2b0f96; gate_head: f46bd68e3e53d39ac953dce3cca9ff9b1c114798; delta_gate: {"mode":"shadow","decision":"full","reason":"not-gate-pr","detail":"no PR record (pr.json)","applied":false,"base":"78fa5a1e2a799a1fbbafd18303f892935a2b0f96","head":"f46bd68e3e53d39ac953dce3cca9ff9b1c114798","merge":"28b87b4d62a85c0f760e48498050b7b355aa8325","prior":null,"delta":{},"changed_labels":[],"changed_label_count":0,"tests":{},"phases":{},"plan_sha256":"bf3cc5afafd67d5ca5d0c975bbb829a0a21da749817f22ddbc0d46c014c5edcf"}.
failure_attribution: none; policy_attribution: none; post_merge_criteria: none.
rbe_state: {"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"}.
admission: {"class":"remote","floor_gib":25.0,"free_gib":57.50169372558594,"waited_seconds":0.001,"cap":2}.
test_exec_bep: {"unit":{"results":426,"remote_executed":426,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":87,"remote_executed":87,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":15,"remote_executed":15,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}}.
owned_topup: [].
test_exec_mode: remote-exec; fallback_reason: none; farm_test_seconds: 14152.503.
test_log_dir: `/var/tmp/gc-gate-ga-2p9pj79a-t8lomufl/c3-run/evidence/remote/logs` (retained).

```text
test_counts: PASS=158800 FAIL=0 SKIP=618
  (top-level result lines, column 0, summed over the 570 logs; a test that several jobs run counts once per job; sub-tests are not counted)
diff_tests_executed:
  TestSplitStaleMailQueuedNudgesKeepsMailReminderWhenUnreadStateUnknown PASS (integration-packages-e4245ddf6a37030111bc.log, unit-1ab526405ace21879f85.log)
diff_subtest_executed: TestSplitStaleMailQueuedNudgesKeepsMailReminderWhenUnreadStateUnknown/nil-provider PASS
diff_subtest_executed: TestSplitStaleMailQueuedNudgesKeepsMailReminderWhenUnreadStateUnknown/empty-session-id PASS
diff_subtest_executed: TestSplitStaleMailQueuedNudgesKeepsMailReminderWhenUnreadStateUnknown/mailbox-read-error PASS
```

policy_lane: PASS — make check-release-dist-ignore check-routed-test-rows check-split-topology-rows check-residency-boundary check-hooks; explicit nogo/lint; thirteen policy/docsync targets.
drift_lane: PASS — make bazel-sync followed by git diff --exit-code and empty porcelain; genspec/schema/client/SPA/lockfile sync targets; openapi-breaking-check against the pinned gate base.
fast_lane: PASS 2026-11-22T19:20:27.854179+00:00 merge_head=28b87b4d62a85c0f760e48498050b7b355aa8325.
known_red_precheck: known-red-precheck: target=check-routed-test-rows verdict=green merge=0 base=- (it passes on the merge tree); known-red-precheck: stale: tracker=ga-1ptv4jey `make check-routed-test-rows` passes on this merge tree; the tracker may be ready to close; known-red-precheck: proceed reason=nothing-known-red (check-routed-test-rows=green).
format_lane: PASS — //:go_format_test. All lanes ran in separate fresh views before the suite. Strict storeclass predicates used trusted base tools and passed.
complexity: zero-growth form (ii); base and merge both exit 2, no merge row worse than its base row, no new/regressed/deleted threshold changes, baseline untouched. Raw outputs retained; this is the documented scoring rule, no waived failure.
ci_lane_run: n/a (no CI-config change).

skip_justification: all printed skip sites are UNCHANGED at the tested merge, with line_in_hunk=no and no SHARED-HUNKS or HELPER-HUNKS in the diff. Platform, lane, missing-fixture/provider and environment-absence reasons are inventoried by test, site and log. The only change adds a whole in-memory test function and reaches none of those existing predicates. No same-command baseline measurement is required under the documented unchanged-site rule. All native environment-absence top-ups and the local floor completed; none of the owned test or subtests skipped.

Native environment: rootless Podman/Dolt image and sweep preflight passed; ICU 74 recorded libraries verdict=OK.

Load observations (wait_* and run_* are separate; timed-out ordinary waits are recorded):

```text
load_threshold: 15
load_waited_seconds: 0
load_wait_timed_out: 0
run_start_load: 16.33
run_max_load: 19.67
run_mean_load: 16.43
run_readings: 48
storeclass-proof/wrapper.stderr: LOAD_GATE_SUMMARY threshold=15 waited_seconds=480 wait_timed_out=0 run_start_load=14.82 run_max_load=16.80 run_mean_load=15.77 run_readings=5 wait_first_load=25.25 wait_max_load=25.25 wait_mean_load=20.22 wait_readings=17 read_errors=0
c3-run/units/gate.w1.stderr.log: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=16.33 run_max_load=19.67 run_mean_load=16.43 run_readings=48 wait_first_load=16.33 wait_max_load=16.33 wait_mean_load=16.33 wait_readings=1 read_errors=0
c3-run/evidence/remote/floor-topup.stdout: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=14.78 run_max_load=17.35 run_mean_load=16.60 run_readings=9 wait_first_load=14.78 wait_max_load=14.78 wait_mean_load=14.78 wait_readings=1 read_errors=0
```

ci_required_closure: `python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-validate.jrhEnZ 78fa5a1e2a799a1fbbafd18303f892935a2b0f96 f46bd68e3e53d39ac953dce3cca9ff9b1c114798 fork`; workflow blob `fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612`; 6 LEAF jobs need a status; the rest are rolled up through their needs..

ci_required_accounting:

- runner-policy | LOCAL-PASS | command /home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-merge-validate.jrhEnZ -- /home/jaword/projects/gc-management/packs/actual/deployer/scripts/gate-base.sh run -- /home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh --threshold 15 --max-wait 1800 --poll-interval 30 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- timeout --signal=TERM --kill-after=10s 1800 python3 /var/tmp/gc-deployer-required-ci-20261122.py job /var/tmp/gc-gate-ga-2p9pj79a-t8lomufl runner-policy; exit 0; log /var/tmp/gc-gate-ga-2p9pj79a-t8lomufl/required-ci/runner-policy.stdout
- changes | LOCAL-PASS | command python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-validate.jrhEnZ 78fa5a1e2a799a1fbbafd18303f892935a2b0f96 f46bd68e3e53d39ac953dce3cca9ff9b1c114798 fork; exit 0; log /var/tmp/gc-gate-ga-2p9pj79a-t8lomufl/required-ci/closure.stdout
- credential-provider-windows | NOT-TRIGGERED |   credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'
- pack-gate | NOT-TRIGGERED |   pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'
- windows-lane | LOCAL-PASS | command /home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-merge-validate.jrhEnZ -- /home/jaword/projects/gc-management/packs/actual/deployer/scripts/gate-base.sh run -- /home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh --threshold 15 --max-wait 1800 --poll-interval 30 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- timeout --signal=TERM --kill-after=10s 1800 python3 /var/tmp/gc-deployer-required-ci-20261122.py job /var/tmp/gc-gate-ga-2p9pj79a-t8lomufl windows-lane; exit 0; log /var/tmp/gc-gate-ga-2p9pj79a-t8lomufl/required-ci/windows-lane.stdout
- windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner |   windows-lane-test                      LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} EVERY-PR

windows_reach: none for each native Windows job, each checked by its own test import closure.
deferred_to_pr_ci: windows-lane-test (non-linux-runner)
not_covered: none
host_gaps: none. PR-open PASS does not replace green CI / required or successful deferred jobs on this exact head.

Commands and exit statuses (native floor commands supplement the full suite):

```text
bazel build --keep_going --output_groups=nogo_fix --config=remote-exec --noremote_local_fallback --build_event_json_file=/var/tmp/gc-gate-ga-2p9pj79a-t8lomufl/c6-vet.bep.json //... ; exit=0
/home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-merge-validate.jrhEnZ -- /home/jaword/projects/gc-management/packs/actual/deployer/scripts/gate-base.sh run -- /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-admission.py --class remote --bead ga-2p9pj79a --label fast-format --record /var/tmp/gc-gate-ga-2p9pj79a-t8lomufl/fast-format-admission.json --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh --rbe -- bash /var/tmp/gc-gate-ga-2p9pj79a-t8lomufl/fast-lane.sh format ; exit=0
/home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-merge-validate.jrhEnZ -- /home/jaword/projects/gc-management/packs/actual/deployer/scripts/gate-base.sh run -- /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-admission.py --class remote --bead ga-2p9pj79a --label fast-policy --record /var/tmp/gc-gate-ga-2p9pj79a-t8lomufl/fast-policy-admission.json --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh --rbe -- bash /var/tmp/gc-gate-ga-2p9pj79a-t8lomufl/fast-lane.sh policy ; exit=0
/home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-merge-validate.jrhEnZ -- /home/jaword/projects/gc-management/packs/actual/deployer/scripts/gate-base.sh run -- /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-admission.py --class remote --bead ga-2p9pj79a --label fast-drift --record /var/tmp/gc-gate-ga-2p9pj79a-t8lomufl/fast-drift-admission.json --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh --rbe -- bash /var/tmp/gc-gate-ga-2p9pj79a-t8lomufl/fast-lane.sh drift ; exit=0
/home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-merge-validate.jrhEnZ -- /home/jaword/projects/gc-management/packs/actual/deployer/scripts/gate-base.sh run -- /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-admission.py --class remote --bead ga-2p9pj79a --label fast-openapi --record /var/tmp/gc-gate-ga-2p9pj79a-t8lomufl/fast-openapi-admission.json --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh --rbe -- bash /var/tmp/gc-gate-ga-2p9pj79a-t8lomufl/fast-lane.sh openapi ; exit=0
/home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-merge-validate.jrhEnZ -- /home/jaword/projects/gc-management/packs/actual/deployer/scripts/gate-base.sh run -- /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-admission.py --class remote --bead ga-2p9pj79a --label fast-complexity --record /var/tmp/gc-gate-ga-2p9pj79a-t8lomufl/fast-complexity-admission.json --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh --rbe -- bash /var/tmp/gc-gate-ga-2p9pj79a-t8lomufl/fast-lane.sh complexity ; exit=0
go build -o /var/tmp/gc-storeclass-trusted-_g05rqdd/storeclass-baseline-trusted ./analyzers/storeclass/cmd/storeclass-baseline ; exit=0
/var/tmp/gc-storeclass-trusted-_g05rqdd/storeclass-baseline-trusted -repo /var/tmp/gc-fresh-tree.MDOnhH ; exit=0
/var/tmp/gc-storeclass-trusted-_g05rqdd/storeclass-baseline-trusted -repo /var/tmp/gc-fresh-tree.MDOnhH -growth-since 78fa5a1e2a799a1fbbafd18303f892935a2b0f96 -base-tip 78fa5a1e2a799a1fbbafd18303f892935a2b0f96 -gate-only ; exit=0
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-2p9pj79a-t8lomufl/c3-run/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-2p9pj79a-t8lomufl/c3-run/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-2p9pj79a-t8lomufl/c3-run/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-2p9pj79a-t8lomufl/c3-run/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-2p9pj79a-t8lomufl/c3-run/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
GATE_PHASE phase=unit test_rc=0 predicate_rc=0 start_ms=1795375253089 end_ms=1795375395994
GATE_PHASE phase=acceptance test_rc=0 predicate_rc=0 start_ms=1795375396851 end_ms=1795375516689
GATE_PHASE phase=integration-packages test_rc=0 predicate_rc=0 start_ms=1795375517378 end_ms=1795375770127
GATE_PHASE phase=integration-full test_rc=0 predicate_rc=0 start_ms=1795375771031 end_ms=1795376343414
GATE_PHASE phase=integration-latency test_rc=0 predicate_rc=0 start_ms=1795376344159 end_ms=1795376441501
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
GOOS=windows go build $(cat .github/windows-lane/build.txt) ; exit=0
GOOS=windows go test -count=1 -vet=off -run '^$' -exec /bin/true $(cat .github/windows-lane/test-build.txt) ; exit=0
GOOS=windows go build ./analyzers/... && GOOS=windows go test -count=1 -vet=off -run '^$' -exec /bin/true ./analyzers/... ; exit=0
GOOS=darwin go build ./... && GOOS=darwin go test -count=1 -vet=off -run '^$' -exec /bin/true ./... ; exit=0
```

Execution metadata and inventories are retained by hash, not pasted into this record:

```json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/gc-gate-ga-2p9pj79a-t8lomufl/c3-run/evidence/remote","gate_base":"78fa5a1e2a799a1fbbafd18303f892935a2b0f96","gate_head":"f46bd68e3e53d39ac953dce3cca9ff9b1c114798","admission":{"class":"remote","floor_gib":25.0,"free_gib":57.50169372558594,"waited_seconds":0.001,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"},"delta_gate":{"mode":"shadow","decision":"full","reason":"not-gate-pr","detail":"no PR record (pr.json)","applied":false,"base":"78fa5a1e2a799a1fbbafd18303f892935a2b0f96","head":"f46bd68e3e53d39ac953dce3cca9ff9b1c114798","merge":"28b87b4d62a85c0f760e48498050b7b355aa8325","prior":null,"delta":{},"changed_labels":[],"changed_label_count":0,"tests":{},"phases":{},"plan_sha256":"bf3cc5afafd67d5ca5d0c975bbb829a0a21da749817f22ddbc0d46c014c5edcf"},"test_exec_bep":{"unit":{"results":426,"remote_executed":426,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":87,"remote_executed":87,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":15,"remote_executed":15,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":14152.503,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=5fd31cdd75b202be3ab9de14bb2514f059b7969d1f14ab7fdc392c2c723e6b7b"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":94.82,"evidence_errors":[],"probe":["GATE_FARM_PROBE verdict=clear waited=0s load1=9.73 load5=7.98 load15=10.05 cpus=32 threshold=50 max_wait=600s"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 15.15 9.58 10.49 18/1747 57257"]}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":38722}},"log_dir":"/var/tmp/gc-gate-ga-2p9pj79a-t8lomufl/c3-run/evidence/remote/floor-logs"},"skip_inventory":{"counts_by_reason_class":{"other":514,"environment-absence":90},"top_level_counts_by_reason_class":{"other":256,"environment-absence":43},"tests":{"elided_array":{"items":604,"bytes":217413}}},"test_cmd":{"elided_array":{"items":5,"bytes":2064}},"test_counts":{"PASS":158800,"FAIL":0,"SKIP":618},"diff_tests_executed":"diff_tests_executed:\n  TestSplitStaleMailQueuedNudgesKeepsMailReminderWhenUnreadStateUnknown PASS (integration-packages-e4245ddf6a37030111bc.log, unit-1ab526405ace21879f85.log)\nGATE_TEST_EVIDENCE_RESULT ok: 1 diff-owned tests verified by name\n"}
```

Full execution retained: 6058 lines; /var/tmp/gc-gate-ga-2p9pj79a-t8lomufl/c3-run/evidence/execution.json; SHA256 7c4611ae837fcabc4cefb68e47f1ebc0a9bfaeb8fc25a781af18ecdc9fb6d5d3.
Full skip sites retained: 5424 lines; /var/tmp/gc-gate-ga-2p9pj79a-t8lomufl/skip-source-audit/evidence.json; SHA256 d6196893e0de214ac4728e9553110b39739fc31c0232ebf1023481005db00c52.
Full reviewer mutation inspection retained: 34 lines; /var/tmp/gc-gate-ga-2p9pj79a-t8lomufl/reviewer-mutation-audit.json; SHA256 c2105c4d39850a3e4cb4c50955e2f265ee041ae4a8e9cf0705033efc8144604b.
Full CI accounting retained: 172 lines; /var/tmp/gc-gate-ga-2p9pj79a-t8lomufl/required-ci/accounting.json; SHA256 2bccfad9e61677837ee4528696f2fbb428b60c5170378469625a53d994b62d3a.
Full known-red precheck retained: 5 lines; /var/tmp/gc-gate-ga-2p9pj79a-t8lomufl/known-red-precheck.json; SHA256 bab9e01655a9cd66afa59c6dab6e0e37be9aa1b708b95f4648cb82c51430c4af.
Full runtime preflight retained: 113 lines; /var/tmp/gc-gate-ga-2p9pj79a-t8lomufl/runtime-preflight.json; SHA256 519ec06955d5fe1f5db3d114d2dd959b01e587a01f6c9b43db3c339e48912d8c.
Full diff-owned explanation retained: 184 lines; /var/tmp/gc-gate-ga-2p9pj79a-t8lomufl/merge-owned-explain.txt; SHA256 4f7d0cd30ba3e2d063c89ccc91983f917e872f777437db3524e546a6e3e89810.
Full fast lanes retained: 173 lines; /var/tmp/gc-gate-ga-2p9pj79a-t8lomufl/fast-result.json; SHA256 3d1d8327553ac0d110a080dc89d8ff599cae7b40cf3fb1ec545e09749c4a5a53.
Full merge build/vet retained: 61 lines; /var/tmp/gc-gate-ga-2p9pj79a-t8lomufl/c6-audit.json; SHA256 53a6ca855114c7f21bdf2ed668bd4e764d82a5f94fc3feb7e11b2b273e577d77.
Full retained evidence manifest retained: 1692 lines; /var/tmp/gc-gate-ga-2p9pj79a-t8lomufl/gate-evidence-manifest.sha256; SHA256 1dd391c14dfe727a81b9d072760e80b0bc986e7d47858c168dcd3ca279165a07.
