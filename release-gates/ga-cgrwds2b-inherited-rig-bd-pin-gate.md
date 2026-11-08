# Release gate: inherited rig workspace BD_BIN pin

**Verdict:** **PASS**

Bead: ga-cgrwds2b; build: ga-xn1enfv; independent review: ga-9jqzgh39.
Recorded: 2026-11-08T22:17:15.099903+00:00.
Reviewed source: 0d1c9fb62a0897750028808fbf48157afa08800f (resolved in the object store).
Validated merged commit: 9ebf023e6862316a905825bc82642d564d29f794; tree: accea75eeb1bc76f9a398b6d8bcb889b19460835.
DEPLOY_MODE: remote; BASE_REF: origin/main; PUSH_REMOTE: fork.
Evidence root: /var/tmp/deploy-ga-cgrwds2b-r3.5qekx59v. This is the mayor-granted third gate round; earlier HOLD results are historical.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Live review verdict names the exact source; no carryover or reviewer-added commit. |
| 2 | Acceptance criteria met | PASS | All five build criteria independently checked below, including fresh release evidence and zero complexity growth. |
| 3 | Tests pass | PASS | Five uncached remote phases, 16 native floor checks, by-name and SKIP audit, policy/drift and complete CI job accounting. |
| 4 | No unresolved HIGH findings | PASS | Reviewer reports no security findings; no unresolved HIGH finding in the live build/review/deploy records. |
| 5 | Final branch clean | PASS | Only the gate record changes above the reviewed source; clean index/worktree verified before publication. |
| 6 | Merge state recorded; build and vet | PASS | Clean canonical merge; both binaries build and full-tree nogo/vet pass, detached rc=0. |
| 7 | Single feature theme | PASS | Three source paths in cmd/gc cover inherited-city binary selection and its regression proofs; ancestry/citation and denylist guards pass. |

merge_state: clean (origin/main@b32baea298f8; files: none)
gate_base: b32baea298f85b9884d17b3c55a1ff19a773e8eb
gate_base_kind: current-tip
merge_validation: exact git merge-tree result built, vetted and tested; current-head merged-tree verification remains merge-authority work.
criterion6_command: bazel build --config=remote-exec --noremote_local_fallback //cmd/gc:gc //cmd/gc-osc7501:gc-osc7501; make vet NOGO_BUILD_FLAGS='--keep_going --output_groups=nogo_fix --config=remote-exec --noremote_local_fallback'; both rc=0; c6-independent-output.txt.

## Acceptance evidence

1. The inherited-city Dolt endpoint selects the absolute executable workspace pin instead of ambient PATH. TestResolveBdBinaryForScope and its executable/relative/missing-pin subtests PASS in both full unit and integration-package runs. The RED test commit 3a0976cbc9ed85f898aa9618e9fc8e8e5c0d3b7b resolves as the GREEN parent; the validator's retained RED evidence records the selection and refusal failures.
2. TestBdVouchInheritedManagedRigUsesWorkspacePin PASS proves city/rig vouch and allowlisted passthrough use one pinned inventory/executable; the ambient wrapper is neither walked nor executed. Production inventory checks are unchanged.
3. Existing independent-backend/binding isolation, invalid-pin and inventory refusal owners PASS by name, including TestBdTrustBuildInfo (the current owner of the criterion's obsolete TestBdTrustRequiresAGoBuild name).
4. Fresh release evidence comes from this canonical merge and clean fresh views, independently of reviewer evidence. BUILD regeneration leaves no drift; both owned regressions execute with no FAIL/SKIP. No deadline, budget, inventory or waiver exception is used.
5. Complexity zero-growth clause ii: base and merge make complexity-check both rc=2 with byte-identical measured output; pinned COMPLEXITY_BASE_REF make complexity-diff rc=0 prints its measurement header and no threshold changes. No baseline edit. Logs: complexity-base.log, complexity-merge.log, complexity-diff.log.

regression_results:
- TestResolveBdBinaryForScope: PASS (unit and integration-packages).
- TestBdVouchInheritedManagedRigUsesWorkspacePin: PASS (unit and integration-packages).
- TestInvalidWorkspaceBdBinStillFailsClosed: PASS (unit and integration-packages).
- TestWorkspacePathWithoutBdStillFailsClosedWithValidAmbientBdBin: PASS (unit and integration-packages).
- TestBdTrustBuildInfo: PASS (unit and integration-packages).
- TestBdTrustMatchesTheCommandTreeNotTheVersion: PASS (unit and integration-packages).
- TestBdVouchCommand: PASS (unit and integration-packages).
- TestTutorial01: PASS (unit and integration-packages).
Full subtest/result mapping: /var/tmp/deploy-ga-cgrwds2b-r3.5qekx59v/acceptance-by-name.json.

## Test execution

test_cmd: /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-detached-run.sh start --run-dir /var/tmp/deploy-ga-cgrwds2b-r3.5qekx59v/c3-run --key accea75eeb1bc76f9a398b6d8bcb889b19460835 -- env GC_GATE_REMOTE_ENABLED=1 GC_REMOTE_GATE_FLOOR_GIB=25 GC_REMOTE_GATE_CAP=2 ISOLATED_TEST_BEAD=ga-cgrwds2b DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true BEADS_ALLOW_UNREAPED_TESTCONTAINERS=1 /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-criterion3.sh --repo /var/tmp/deploy-ga-cgrwds2b-r3.5qekx59v/merge --base b32baea298f85b9884d17b3c55a1ff19a773e8eb --bead-json /var/tmp/deploy-ga-cgrwds2b-r3.5qekx59v/bead.json --owned /var/tmp/deploy-ga-cgrwds2b-r3.5qekx59v/owned-tests.tsv --run-dir /var/tmp/deploy-ga-cgrwds2b-r3.5qekx59v/c3-run/evidence; terminal rc=0.
test_cmd_scope: full-suite
test_counts: PASS=149300 FAIL=0 SKIP=512
Counts are column-zero terminal Go result lines across 549 job logs, summed per job; repeated tests count once in each job, subtests are excluded. Non-Go floor checks have separate rc=0 evidence.
diff_tests_executed: TestResolveBdBinaryForScope PASS; TestBdVouchInheritedManagedRigUsesWorkspacePin PASS (unit and integration-packages; full by-name/subtest audit in final-test-evidence.txt and acceptance-by-name.json).
test_log_dir: /var/tmp/deploy-ga-cgrwds2b-r3.5qekx59v/audited-result-logs
test_bd: GASCITY_TEST_BD version=v1.3.4 bin=/home/jaword/.local/bd-versions/v1.3.4/bd reports='bd version 1.3.4 (7ef2fd05e: HEAD@7ef2fd05eb0c)' ref_check=match:7ef2fd05e origin=preexisting pin_source=/var/tmp/deploy-ga-cgrwds2b-r3.5qekx59v/merge/deps.env displaces='bd version 1.3.2 (9b65b9e19)'
test_exec_mode: remote-exec (five full farm phases); required native floor is supplemental, with no partial-suite stitching.
test_exec_bep: all 533 results remote, first attempt, inside their own phase windows, terminal PASS; zero local/remote cached test results, retries, transport or predicate errors.
rbe_state: certificate valid; kill switch absent; worker environment hash matches pinned main; execution.json below records exact values.
fallback_reason: none
admission: remote cap=2, floor=25 GiB, observed free=85.941 GiB, waited=0.001s; native top-ups retain the local-heavy load wait.
farm_test_seconds: 12325.737
heavy_mode: none (computed classify.txt)
owned_topup: none (both owned tests already PASS in full-suite logs)
floor_topup: 16/16 commands rc=0; every environment-absence farm SKIP selected by package, plus pinned-beads support, Darwin compile and both runner self-tests.
latency_leg: measured, attempt 1, clear farm probe; TestGcBdLatencyBudget=PASS, TestGcSeatHookLatencyBudget=PASS; main phase excludes these budget tests.
skip_inventory: 498 farm SKIP records (72 environment-absence, 426 other; includes subtests); retained by pointer below. Native remaining SKIPs are separately inventoried.
skip_justification: all 293 unique farm source sites are unchanged, including 42 conformance/interleaved-log mappings; 39 native SKIP rows map to unchanged sites. The sole shared test hunk is the stdlib bytes import in cmd/gc/bd_inventory_trust_test.go. Same-command pinned-base cmd/gc runs independently reproduce all 151 unit and 31 integration SKIPs, with zero new/reason-changed/diff-owned/unexplained skips. The native command separately reproduces TestReplay (GC_V2_REPLAY_DIR unset), TestSimCoverageGate (GC_V2_SIM_COVERAGE_FILES unset) and TestSessionEventPumpLiveHerdr (live-journey opt-in): 3/3 pre-existing, same reasons. Other remaining reasons are unrelated platform/tool/opt-in conditions or intentional conformance/placeholder skips; neither their preconditions nor source/import package changes. The only production change selects a city pin for inherited rigs; no diff-owned result is skipped. Printed reasons and source proofs are retained, not counted as PASS.
failure_attribution: none (zero failures)
waiver_ref: none
ci_lane_run: n/a (no CI-config change)
uncovered_criteria: none
deferred_to_pr_ci: none
not_covered: none

| Phase | Remote results | Cached | Retries | Result |
|---|---|---|---|---|
| unit | 407 | 0 | 0 | PASS |
| acceptance | 25 | 0 | 0 | PASS |
| integration-packages | 86 | 0 | 0 | PASS |
| integration-full | 14 | 0 | 0 | PASS |
| integration-latency | 1 | 0 | 0 | PASS |

Per-phase commands and flags (each terminal test_rc=0 and predicate_rc=0):

```text
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-cgrwds2b-r3.5qekx59v/c3-run/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-cgrwds2b-r3.5qekx59v/c3-run/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-cgrwds2b-r3.5qekx59v/c3-run/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-cgrwds2b-r3.5qekx59v/c3-run/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-cgrwds2b-r3.5qekx59v/c3-run/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```

The unit phase executes make -k check (whole-tree bazel test plus shell guards); acceptance executes the CI acceptance tier; integration-packages executes the complete gating package tier; full integration and its separate latency phase cover the remaining integration target. The native floor is generated by gate-floor-topup.py, not a substitute for these full-scope phases. TestTutorial01 PASS is present in integration-package evidence with GC_FAST_UNIT=0. The previous systemd restart blocker TestStartDrift_SystemdManaged_RestartsToNewBuildID PASSes natively (5.7s restart, 15s budget).

load_threshold: 15
load_waited_seconds: 1801
load_wait_timed_out: 1
run_start_load: 83.75
run_max_load: 80.67
run_mean_load: 76.28
run_readings: 13
load_summary: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=83.75 run_max_load=80.67 run_mean_load=76.28 run_readings=13 wait_first_load=88.68 wait_max_load=92.76 wait_mean_load=58.18 wait_readings=61 read_errors=0
chain_load_summary: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=10.74 run_max_load=101.42 run_mean_load=50.91 run_readings=145 wait_first_load=10.74 wait_max_load=10.74 wait_mean_load=10.74 wait_readings=1 read_errors=0
native_base_skip_load: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=80.36 run_max_load=83.48 run_mean_load=82.12 run_readings=4 wait_first_load=48.85 wait_max_load=80.36 wait_mean_load=39.78 wait_readings=61 read_errors=0
The native floor and its baseline comparison each exhausted the ordinary 1800s bounded wait and proceeded as prescribed; all commands and the comparison rc=0. The load fields above describe the floor; the chain-wide monitor is reported separately.
runtime_setup: rootless Podman socket active, API ping OK, pinned Dolt image 2.2.0 cached, testcontainer sweep enabled, paired Ryuk/unreaped opt-outs; ICU74 OK. gascity is PASS-THROUGH in isolated-test-run.sh, not a cloned environment; gate-criterion3/floor test_environment supplies test isolation. No live production vouch/config/restart/cutover occurred.

## Policy, drift and CI job accounting

fast_lane: PASS before any runtime phase; fast-run-independent-output.txt.
policy_lane: four make shell guards, pinned fmt-check-changed, full nogo lint and 14 Bazel policy/docs/format targets; all rc=0. Exact commands: fast-policy.sh.
policy_attribution: none; complexity uses criterion 2's measured zero-growth rule.
drift_lane: fresh-tree-run.sh -> gate-base.sh run -> make bazel-sync -> git diff --exit-code; rc=0. Exact command: drift.sh.
format_lane: run-pinned-lint.sh -- make fmt-check-changed; rc=0.

```text
FRESH_TREE_RECORD from=/var/tmp/deploy-ga-cgrwds2b-r3.5qekx59v/merge head=9ebf023e6862 tree=accea75eeb1b dir=/var/tmp/gc-fresh-tree.9LxwXo modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-merge,bazel-out,bazel-testlogs
GATE_BASE_RECORD base=b32baea298f85b9884d17b3c55a1ff19a773e8eb knobs=CI_BEADS_DIFF_BASE,BASE_SHA,COMPLEXITY_BASE_REF,LINT_BASE dir=/var/tmp/gc-fresh-tree.9LxwXo
FRESH_TREE_RECORD from=/var/tmp/deploy-ga-cgrwds2b-r3.5qekx59v/merge head=9ebf023e6862 tree=accea75eeb1b dir=/var/tmp/gc-fresh-tree.47xG18 modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-merge,bazel-out,bazel-testlogs
GATE_BASE_RECORD base=b32baea298f85b9884d17b3c55a1ff19a773e8eb knobs=CI_BEADS_DIFF_BASE,BASE_SHA,COMPLEXITY_BASE_REF,LINT_BASE dir=/var/tmp/gc-fresh-tree.47xG18
```

ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/deploy-ga-cgrwds2b-r3.5qekx59v/merge b32baea298f85b9884d17b3c55a1ff19a773e8eb 0d1c9fb62a0897750028808fbf48157afa08800f fork; rc=0; workflow blob e5b11c348505dab28dc223e874bc2c61cbbb33c5; 4 LEAF jobs need a status; ci-required-closure.txt.
ci_required_accounting:
- changes | LOCAL-PASS | python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/deploy-ga-cgrwds2b-r3.5qekx59v/merge b32baea298f85b9884d17b3c55a1ff19a773e8eb 0d1c9fb62a0897750028808fbf48157afa08800f fork; rc=0; /var/tmp/deploy-ga-cgrwds2b-r3.5qekx59v/ci-required-closure.txt
- runner-policy | LOCAL-PASS | python3 .github/workflows/scripts/runner_policy.py; rc=0; /var/tmp/deploy-ga-cgrwds2b-r3.5qekx59v/runner-policy.log
- credential-provider-windows | NOT-TRIGGERED | credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'
- pack-gate | NOT-TRIGGERED | pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'
runner_policy_environment: EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=<retained runner-policy.out>; read-only 3e exception.
windows_reach: none (credential-provider-windows NOT-TRIGGERED by the computed path filters).
Gate PASS authorizes PR-open; merge still requires green CI / required at the exact PR head.

## Retained execution record

```json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/deploy-ga-cgrwds2b-r3.5qekx59v/c3-run/evidence/remote","admission":{"class":"remote","floor_gib":25.0,"free_gib":85.94058227539062,"waited_seconds":0.001,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98","worker_env_origin_main":"sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"},"test_exec_bep":{"unit":{"results":407,"remote_executed":407,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":86,"remote_executed":86,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":14,"remote_executed":14,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":12325.737,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=5fd31cdd75b202be3ab9de14bb2514f059b7969d1f14ab7fdc392c2c723e6b7b"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":72.339,"evidence_errors":[],"probe":["GATE_FARM_PROBE verdict=clear waited=0s load1=10.98 load5=13.29 load15=18.19 cpus=32 threshold=50 max_wait=600s"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 11.66 13.17 17.95 11/1432 55353"]}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":31252}},"log_dir":"/var/tmp/deploy-ga-cgrwds2b-r3.5qekx59v/c3-run/evidence/remote/floor-logs"},"skip_inventory":{"counts_by_reason_class":{"other":426,"environment-absence":72},"top_level_counts_by_reason_class":{"other":233,"environment-absence":43},"tests":{"elided_array":{"items":498,"bytes":178886}}},"test_cmd":{"elided_array":{"items":5,"bytes":2074}},"test_counts":{"PASS":149300,"FAIL":0,"SKIP":512},"diff_tests_executed":"diff_tests_executed:\n  TestBdVouchInheritedManagedRigUsesWorkspacePin PASS (integration-packages-75f7330b9b673df65e4f.log, unit-3d6d3036a2f5902472b5.log)\n  TestResolveBdBinaryForScope PASS (integration-packages-251f1cf8cede4438b2d5.log, unit-493754ea2d5b4d8858c8.log)\nGATE_TEST_EVIDENCE_RESULT ok: 2 diff-owned tests verified by name\n"}
```

Full full execution.json retained: 5044 lines; /var/tmp/deploy-ga-cgrwds2b-r3.5qekx59v/c3-run/evidence/execution.json; SHA256 ec4a90fa82990a547d2ad12335e4324ca8a2e6fca79f3e4c8fd61025d8b0718a.
Full remote skip inventory retained: 3986 lines; /var/tmp/deploy-ga-cgrwds2b-r3.5qekx59v/c3-run/evidence/remote/skip-inventory.json; SHA256 3ed28050945e06eacb87d4db48d7d4bf5bdc4446c98b938080652fad13e727ff.
Full floor command results retained: 237 lines; /var/tmp/deploy-ga-cgrwds2b-r3.5qekx59v/floor-command-results.json; SHA256 99e7ee94b7d09a13683de5d1aae51bff75ff5f3857adc0ae98199ce61105a287.
Full skip source-site proof retained: 280 lines; /var/tmp/deploy-ga-cgrwds2b-r3.5qekx59v/skip-sites-round3.txt; SHA256 9a0b3a6f64cfd3e4642be7f0f81cea8572578e37447bfc85a8b39dd939f2f7da.
Full conformance skip source-site proof retained: 13 lines; /var/tmp/deploy-ga-cgrwds2b-r3.5qekx59v/skip-conformance-sites-round3.txt; SHA256 89d108891a5cc6df94e6179d431add469bca95c1a475c5c68c98e8a72e5076a1.
Full native skip sites and reasons retained: 236 lines; /var/tmp/deploy-ga-cgrwds2b-r3.5qekx59v/floor-skip-sites.json; SHA256 17d497cd6a12d5c92adae67c7b4a556dcf13e1a554a003cac8a3ea5f1c73c928.
Full unit same-command base skip comparison retained: 152 lines; /var/tmp/deploy-ga-cgrwds2b-r3.5qekx59v/compare-skips-unit.txt; SHA256 2132ce6054ea642b6b6e7a21e707683d4c38ad32ad155aa48ed6587942efc5b1.
Full integration same-command base skip comparison retained: 32 lines; /var/tmp/deploy-ga-cgrwds2b-r3.5qekx59v/compare-skips-integration-packages.txt; SHA256 41e5281f2965e65b2a384d2bea20e103bef70186a53cd70d777fd452f1fb28fc.
Full native same-command base skip comparison retained: 4 lines; /var/tmp/deploy-ga-cgrwds2b-r3.5qekx59v/native-skip-comparison.txt; SHA256 274653d6c9614e613c17ef622088624f6004c27630f37d37945390624155b681.
Full final independent runtime audit retained: 375 lines; /var/tmp/deploy-ga-cgrwds2b-r3.5qekx59v/final-audit.json; SHA256 e2ca12455e12abeda4c96807ca64aa4320f3fcfdbdb72bf06d66327c765eeaf2.
Full retained evidence SHA256 manifest retained: 2082 lines; /var/tmp/deploy-ga-cgrwds2b-r3.5qekx59v/evidence-sha256.txt; SHA256 a70daecc205ba6daa24be6a19ec26b1912991914a8fad3a11ac849323a0b7493.
