# Managed startup context release gate

**Verdict:** **PASS**

Bead: ga-7iyznj7r; build: ga-meilzh; review: ga-afoh3y4f
Reviewed commit: db9438a9ab69c5527a05279ae22f3f13c74b1497
Gate base: c7eb6d2f43ca1c0dec508f043b1e75f25470094a
Validated merge: b895799ba8c1553ec1999509d3a56e046d9ec228
Validated tree: 50fe715aa0831452160e041ffc389c7998e00102
Sealed: 2026-11-16T19:33:28.841750+00:00

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS | PASS | Review PASS binds to the resolved reviewed commit. Reviewer-added test carryover independently verified as one append-only test commit. |
| 2 | Acceptance criteria | PASS | Eleven static checks plus ten diff-owned tests pass. Gemini/Kiro startup markers are scoped correctly; Kiro stdin is redirected; OMP v3 parks, reads, clears and injects context once, and upgrades existing hooks. |
| 3 | Tests | PASS | Five fresh remote phases, all native top-ups, by-name owned results, skip accounting, and required CI leaf accounting below. |
| 4 | No open HIGH findings | PASS | Review security findings: none; only one non-blocking comment nit. |
| 5 | Clean final source | PASS | Canonical merge and assigned worktree are clean; publication verifies the isolated deploy branch after the gate record commit. |
| 6 | Merge state and build/vet | PASS | Merge clean against the pinned base; independent merge-tree identity, whole-tree Bazel build and vet/nogo passed with normal hooks active. |
| 7 | Single feature theme | PASS | Three reviewed source commits, eight files: managed-provider startup-context delivery. |

merge_state: clean (origin/main@c7eb6d2f43ca1c0dec508f043b1e75f25470094a)
gate_base_kind: current-tip
gate_base: c7eb6d2f43ca1c0dec508f043b1e75f25470094a
merge_validation: merge result built, vetted and tested here; MPR owns current merge validation and required PR CI before landing.
release_criteria_source: deployer release-gate-criteria.md.tmpl, TESTING.md, and engdocs/contributors/release-gate-criteria-conventions.md; docs/PROJECT_MANIFEST.md is absent.

reviewer_tests_verified: 53f16c23132601b8bb7251f2b334123fe681d86d -> db9438a9ab69c5527a05279ae22f3f13c74b1497 files=cmd/gc/embed_builtin_packs_test.go
policy_lane: PASS; pinned golangci-lint2.14.0 resolved2.14.0, fmt, whole-tree nogo, four make-check shell guards and policy targets.
drift_lane: PASS; CI make bazel-sync, empty git diff/status; generated OpenAPI/schema/dashboard/client/lockfiles/docsync checks passed.
complexity_lane: PASS zero-growth(ii); pinned base and merge absolute rc2 with byte-identical250-line diagnostics; pinned diff reports no threshold changes. No baseline edits.
fast_lane_isolation: separate fresh-tree views, each with recorded gate-base pin; canonical scratch unchanged.
known_red_precheck: proceed nothing-known-red; check-routed-test-rows passes; ga-1ptv4jey reported stale.

heavy_mode: none
test_cmd_scope: full-suite
test_exec_mode: remote-exec
fallback_reason: none
waiver_ref: none
failure_attribution: none
ci_lane_run: n/a (no CI-config change)
farm_test_seconds: 14117.66
test_log_dir: /var/tmp/gate-ga-7iyznj7r-3kbfrlep/criterion3-run/evidence/remote/logs
test_counts: PASS=155089 FAIL=0 SKIP=613
Counts above are top-level per-job result lines; skip inventory includes subtests and is a separate denominator.
diff_tests_executed: all10 PASS by name in retained full-suite logs.
owned_topup: []
floor_topup: all16 steps exit0; 13 package checks, fsys Darwin compile and two runner self-tests.
latency_leg: measured first attempt after clear farm probe; both named budget tests PASS. Main leg excluded both tests.
skip_inventory: 671 events (599 remote+72 native); 669 exact pinned-base matches and two independently justified unchanged filesystem guards. Zero new/diff-owned/missing-reason/unjudged events.
SKIP proof: all source sites unchanged; all six native groups match at the pinned base, including shared-helper cmd/gc cases. Two comparator reason-changed results are retained: per-run paths differ, exact skip guards/files do not, and neither binary imports a SHARED-HUNKS package. No normalization, retry or waiver.

ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gate-ga-7iyznj7r-3kbfrlep/merge c7eb6d2f43ca1c0dec508f043b1e75f25470094a db9438a9ab69c5527a05279ae22f3f13c74b1497 fork; workflow_blob=fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs need a status; the rest are rolled up through their needs.
ci_required_accounting:
- runner-policy | LOCAL-PASS | bash /var/tmp/gate-ga-7iyznj7r-3kbfrlep/ci-runner-policy.sh; exit=0; log=/var/tmp/gate-ga-7iyznj7r-3kbfrlep/ci-required/runner-policy.log
- changes | LOCAL-PASS | python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gate-ga-7iyznj7r-3kbfrlep/merge c7eb6d2f43ca1c0dec508f043b1e75f25470094a db9438a9ab69c5527a05279ae22f3f13c74b1497 fork; exit=0; log=/var/tmp/gate-ga-7iyznj7r-3kbfrlep/ci-required/triggers.txt
- credential-provider-windows | NOT-TRIGGERED |   credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'
- pack-gate | NOT-TRIGGERED |   pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'
- windows-lane | LOCAL-PASS | /home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gate-ga-7iyznj7r-3kbfrlep/merge -- /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-admission.py --class local-heavy --record /var/tmp/gate-ga-7iyznj7r-3kbfrlep/ci-required/windows-lane-admission.json --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh --class local-heavy --threshold 15 --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- bash /var/tmp/gate-ga-7iyznj7r-3kbfrlep/ci-windows-lane.sh; exit=0; log=/var/tmp/gate-ga-7iyznj7r-3kbfrlep/ci-required/windows-lane.log
- windows-lane-test | DEFERRED-TO-PR-CI | reason=non-linux-runner;   windows-lane-test                      LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} EVERY-PR
windows_reach: none, independently computed for both native Windows jobs.
deferred_to_pr_ci: windows-lane-test (non-linux-runner)
not_covered: none
host_gaps: none

This PASS permits PR publication. Merge requires CI / required green on the exact PR head and native Windows success (or its own false path filter). MPR owns CI watch and merge.

Native load evidence:
- criterion3-run/evidence/remote/floor-topup.stdout: LOAD_GATE_SUMMARY threshold=15 waited_seconds=660 wait_timed_out=0 run_start_load=14.27 run_max_load=14.26 run_mean_load=13.48 run_readings=6 wait_first_load=60.19 wait_max_load=60.19 wait_mean_load=29.71 wait_readings=23 read_errors=0
- native-base-floor.log: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=72.50 run_max_load=70.84 run_mean_load=68.05 run_readings=10 wait_first_load=22.47 wait_max_load=76.67 wait_mean_load=45.16 wait_readings=62 read_errors=0 prewait_s=1801
- ci-required/windows-lane.log: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1171 wait_timed_out=0 run_start_load=14.77 run_max_load=19.91 run_mean_load=16.84 run_readings=4 wait_first_load=59.10 wait_max_load=61.54 wait_mean_load=38.63 wait_readings=41 read_errors=0 prewait_s=1171
load_threshold: 15
load_waited_seconds: 660
load_wait_timed_out: 0
run_start_load: 14.27
run_max_load: 14.26
run_mean_load: 13.48
run_readings: 6

test_exec_bep: {"acceptance": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 25, "results": 25, "retried": 0, "window_ok": true}, "integration-full": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 14, "results": 14, "retried": 0, "window_ok": true}, "integration-latency": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 1, "results": 1, "retried": 0, "window_ok": true}, "integration-packages": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 86, "results": 86, "retried": 0, "window_ok": true}, "unit": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 415, "results": 415, "retried": 0, "window_ok": true}}
rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72", "worker_env_tree": "sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"}
admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 48.057029724121094, "waited_seconds": 0.001}

diff_tests_executed by name:
TestBundledOmpHookDrainsParkedPrimeContextOnce PASS (integration-packages-e5330b8c981927473156.log, unit-2ef13e1b6898c5544764.log)
  TestBundledOmpHookPublishesProviderSessionID PASS (integration-packages-f7fba3a97bc02cffff0b.log, unit-4d20b3d28497f844aa39.log)
  TestDoPrimeWithHook_DevNullStdinLetsEnvMarkersRoute PASS (integration-packages-251f1cf8cede4438b2d5.log, unit-cd6930bb0d0da8646500.log)
  TestDoPrimeWithHook_GeminiStdinSessionStartHonorsManagedMarker PASS (integration-packages-c2ab4e7d4735e1bd66a7.log, unit-912ab643330b35bd0b6b.log)
  TestDoPrimeWithHook_ProviderStdinEventOverridesEnvMarker PASS (integration-packages-509ef0a6cd81866833cc.log, unit-088cc38d26d094729229.log)
  TestCopilotAndCursorSessionStartHooksOmitManagedMarkers PASS (unit-18ff77d2ebe2a811ec81.log)
  TestGeminiSessionStartMarkerConvergesOnExistingSettings PASS (unit-18ff77d2ebe2a811ec81.log)
  TestInstallOMPHookUpgradesLegacyObjectExport PASS (unit-18ff77d2ebe2a811ec81.log)
  TestInstallOverlayManagedProviders PASS (unit-18ff77d2ebe2a811ec81.log)
  TestOMPHookNeedsUpgradeComparesParsedVersion PASS (unit-18ff77d2ebe2a811ec81.log)

test_cmd: env GC_GATE_REMOTE_ENABLED=1 GC_REMOTE_GATE_FLOOR_GIB=25 GC_REMOTE_GATE_CAP=2 ISOLATED_TEST_BEAD=ga-7iyznj7r /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-criterion3.sh --repo /var/tmp/gate-ga-7iyznj7r-3kbfrlep/merge --base c7eb6d2f43ca1c0dec508f043b1e75f25470094a --bead-json /var/tmp/gate-ga-7iyznj7r-3kbfrlep/bead.json --owned /var/tmp/gate-ga-7iyznj7r-3kbfrlep/owned-tests.tsv --run-dir /var/tmp/gate-ga-7iyznj7r-3kbfrlep/criterion3-run/evidence
Full context.json retained: 28 lines; /var/tmp/gate-ga-7iyznj7r-3kbfrlep/context.json; SHA256 6348e8e8e8a32cd3f671a36abf7bc69a48973846e1a46dc5fce1350bc27d5982
Full c6-result.json retained: 25 lines; /var/tmp/gate-ga-7iyznj7r-3kbfrlep/c6-result.json; SHA256 fcc758a5b09aa1da0c830408d0a04b3ab416813472d9b1bbf216be73478b9ab4
Full fast-result.json retained: 135 lines; /var/tmp/gate-ga-7iyznj7r-3kbfrlep/fast-result.json; SHA256 6cedb9240d8d3ed94c9a349ebc32a034d2b3c864d0f561ac2ec762f580597ecd
Full criterion3-run/evidence/execution.json retained: 6000 lines; /var/tmp/gate-ga-7iyznj7r-3kbfrlep/criterion3-run/evidence/execution.json; SHA256 1e9c3214e38d39b711a1cd95e50403629f96f39b6f3df7a45f74bb7828fbc888
Full complete-skip-audit-plan.json retained: 6697 lines; /var/tmp/gate-ga-7iyznj7r-3kbfrlep/complete-skip-audit-plan.json; SHA256 3b35eb605bb904bbb23011ca38675316f0ec16bca48da7eefec436d29f89876b
Full skip-comparison-audit.json retained: 2542 lines; /var/tmp/gate-ga-7iyznj7r-3kbfrlep/skip-comparison-audit.json; SHA256 fb9c43e626d892fd0350a2b6de824ee36648494846edfbb02c73ef058ec2c926
Full skip-path-guard-proof.json retained: 71 lines; /var/tmp/gate-ga-7iyznj7r-3kbfrlep/skip-path-guard-proof.json; SHA256 f7d5a556c265f80ae22c2d92f273397e6c4299353d8657d3b4f852251c885fae
Full skip-shared-source-resolution.json retained: 396 lines; /var/tmp/gate-ga-7iyznj7r-3kbfrlep/skip-shared-source-resolution.json; SHA256 a9800c0d3f2335b8cd69e3c9689b39e5f96148aee2a174d56911d5ed77b89a64
Full independent-skip-reason-audit.json retained: 8733 lines; /var/tmp/gate-ga-7iyznj7r-3kbfrlep/independent-skip-reason-audit.json; SHA256 b3ea8baee50f42c28ff86a3555a41909f03dcc58166d70871f62b88a4d27dcf3
Full ci-required/accounting.json retained: 96 lines; /var/tmp/gate-ga-7iyznj7r-3kbfrlep/ci-required/accounting.json; SHA256 84291efaf6bfb6f764818426d1345522e5b6041b3c88bbf8b399f3cf69fe779e
Full remote-phases-independent-audit.json retained: 333 lines; /var/tmp/gate-ga-7iyznj7r-3kbfrlep/remote-phases-independent-audit.json; SHA256 5cf1c100f60ad2763a106d18be26171b754819b2a7d970bac239226466ec081c
Full final-audit-result.json retained: 97 lines; /var/tmp/gate-ga-7iyznj7r-3kbfrlep/final-audit-result.json; SHA256 b88e900b8c8d779ce324de2f1d55a95d42ceb062faa2ed1c638cca449713fe7e
