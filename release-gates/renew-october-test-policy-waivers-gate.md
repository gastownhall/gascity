# Renew October test-policy waivers — deploy gate

**Verdict:** **PASS**

Deploy bead ga-w1tv7kq; build ga-siu9v3; review ga-onmfwnm. Evaluated 2026-10-31 UTC. This PASS authorizes PR publication. Merge belongs to mpr after required CI succeeds on the published head.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | ga-onmfwnm verdict pass names the exact reviewed commit; no review carryover or reviewer-added tests. |
| 2 | Acceptance criteria met | PASS | AC1/3 date-only proof below; AC4 strict-clock targets and full gate pass; AC5 stagger names pass and gap is 39 days; AC2/5/6 mandatory PR text prepared verbatim, with proof output appended separately. Published body must be read back before clearance. AC7 allowed merge window is open; merge deadline is 2026-11-01T00:00Z. |
| 3 | Tests pass | PASS | 502 fresh first-attempt remote actions, five complete phases plus mandatory native floor. 134404 PASS / 0 FAIL / 377 SKIP top-level results. Every required CI leaf accounted below. |
| 4 | No unresolved high review findings | PASS | Reviewer reports no blocking findings; unresolved HIGH = 0. |
| 5 | Final branch clean | PASS | Canonical merge and source worktree clean before gate record; publication checks the isolated branch is clean after committing this record. |
| 6 | Merge state recorded; build/vet pass | PASS | Clean canonical merge of frozen main and reviewed source; materialize_merge_tree and make vet (all-target Bazel build/nogo/vet) exit 0. No rebase or conflict repair. |
| 7 | Single feature theme | PASS | Four production/data/generated-document files, 170 date-only line pairs renewing the same 2026-10-31 expiries. |

## Evidence fields

```text
deploy_mode: remote
base_ref: origin/main
reviewed_commit: 80c5330d1499cd8685d1a813043b888efc54281d
gate_base_sha: 91f4bb2318c9f5008bdc47ea28875a865bddacd2
gate_base_kind: current-tip
gated_merge_head: f23aa909be16f041bdb3903c4ad9b27864f722c0
gated_merge_tree: fbb54f1834c2db1f4228b4ef372db87c146156cd
merge_state: clean (origin/main@91f4bb2318c9f5008bdc47ea28875a865bddacd2)
merge_validation: mpr must validate the merge against its then-current main; exact-head clearance does not replace merge-time tests
tests_green: true
test_cmd_scope: full-suite
test_exec_mode: remote-exec
test_exec_bep: {"unit":{"results":378,"remote_executed":378,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":24,"remote_executed":24,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":85,"remote_executed":85,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":14,"remote_executed":14,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}}
test_counts: 134404 PASS, 0 FAIL, 377 SKIP (top-level result lines; includes native floor once)
diff_tests_executed: none (no diff-owned tests; owned.tsv empty; strict names below are acceptance checks)
test_log_dir: /var/tmp/deploy-ga-w1tv7kq.qqz2qzk2/c3/evidence/remote/logs
strict_acceptance_counts: 183 PASS, 0 FAIL, 0 SKIP; 4 fresh remote actions
test_bd: GASCITY_TEST_BD version=v1.3.2 bin=/home/jaword/.local/bd-versions/v1.3.2/bd reports='bd version 1.3.2 (9b65b9e19: HEAD@9b65b9e198ac)' ref_check=match:9b65b9e19 origin=preexisting pin_source=/var/tmp/gc-merge-validate.w1tv7kq.eq1j9kty/deps.env displaces='bd version 1.3.2 (9b65b9e19)'
rbe_state: {"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98","worker_env_origin_main":"sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"}
fallback_reason: none
admission: {"class":"remote","floor_gib":25.0,"free_gib":47.952796936035156,"waited_seconds":0.001,"cap":2}
farm_test_seconds: 9722.194
owned_topup: none; no diff-owned tests
floor_topup: complete; 16/16 commands exit 0, 21 PASS/0 FAIL/14 SKIP top-level Go results, Darwin compile PASS, runner self-tests 55+25 PASS/0 FAIL
latency_leg: complete; one clear-probe attempt; TestGcBdLatencyBudget PASS, TestGcSeatHookLatencyBudget PASS
heavy_mode: none
heavy_units: n/a
coverage_check: n/a (ordinary five-phase remote full suite plus mandatory native floor)
load_threshold: 15 (native floor; remote phases do not use local load admission)
load_waited_seconds: 1801
load_wait_timed_out: 1 (ordinary soft wait; run proceeded as documented)
run_start_load: 71.14
run_max_load: 80.87
run_mean_load: 74.40
run_readings: 11
policy_lane: PASS; format and make shell guards/check-hooks, Bazel policy/docsync/format/lint targets; fast-lane.sh policy rc=0
policy_attribution: none
drift_lane: PASS; make bazel-sync then git diff --exit-code and empty git status, generated spec/schema/clients/dashboard lockfile tests; fast-lane.sh drift rc=0
openapi_breaking_lane: PASS; make openapi-breaking-check OPENAPI_BREAKING_BASE=91f4bb2318c9f5008bdc47ea28875a865bddacd2; rc=0
failure_attribution: none
waiver_ref: none (gascity has no deploy waiver path; renewed policy expiry data is not a deploy waiver)
ci_lane_run: n/a (no CI-config change)
ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-validate.w1tv7kq.eq1j9kty 91f4bb2318c9f5008bdc47ea28875a865bddacd2 80c5330d1499cd8685d1a813043b888efc54281d fork; exit 0; workflow blob e5b11c348505dab28dc223e874bc2c61cbbb33c5; 4 LEAF jobs need a status; the rest are rolled up through their needs.
windows_reach: internal/testutil/providerledger/ledger.go
deferred_to_pr_ci: credential-provider-windows (non-linux-runner)
not_covered: none
host_gaps: none
high_findings_open: 0
record_only_commit: publication adds only this gate record to the reviewed source; no runtime/test data changes beyond the reviewed four-file diff
```

## Commands and execution

Criterion 6: `criterion6.sh` materialized the pinned merge, checked hooks, and ran `make vet NOGO_BUILD_FLAGS="--keep_going --output_groups=nogo_fix --config=remote-exec --noremote_local_fallback"`; exit 0, `CRITERION6_PASS`. No self-rebase.

Fast lane before runtime/suite: `fresh-tree-run.sh --from <canonical-scratch> -- gate-base.sh run -- bash fast-lane.sh <policy|drift|openapi>`; all three exit 0 in separate views. `policy` includes `make fmt-check-changed`, `make check-release-dist-ignore check-routed-test-rows check-split-topology-rows check-residency-boundary check-hooks` and Bazel policy targets. `drift` runs `make bazel-sync`, `git diff --exit-code`, an empty status check and generated-file tests. Full exact scripts and logs are retained below.

Full suite: `gate-base.sh run --dir <canonical-scratch> -- gate-criterion3.sh --repo <canonical-scratch> --base <gate_base_sha> --bead-json <bead-current.json> --owned <owned.tsv> --run-dir <c3/evidence> -- python3 <local-bazel.py>`; terminal `GATE_RUN_EXIT rc=0 state=complete`. Remote rollout setting inherited unchanged; O1 own-code provenance eligible; heavy mode none. Native floor was load-gated, all 16 steps exit 0, no failed test or latency verdict was retried.

```text
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-w1tv7kq.qqz2qzk2/c3/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-w1tv7kq.qqz2qzk2/c3/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-w1tv7kq.qqz2qzk2/c3/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-w1tv7kq.qqz2qzk2/c3/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-w1tv7kq.qqz2qzk2/c3/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```

The phase commands use the documented full Bazel unit/policy, acceptance, integration-packages, and full integration coverage, with the two latency-budget tests partitioned into the isolated fifth phase. All actions satisfy remote strategy, no cached flags, attempt 1, starts inside their phase window, and terminal verdicts. 1004 retained farm log/XML artifact hashes were independently rechecked. Mandatory native floor logs are copied into the combined log directory and counted once. Floor self-tests report 55 and 25 passes; Darwin compile exits 0.

## Strict-clock acceptance and smoke

`gate-rbe-phase.sh owned-waiver-strict <strict-ac-ready> --test_env=GC_WAIVER_CLOCK=strict //internal/testpolicy/waiverexpiry:waiverexpiry_test //internal/testpolicy/resourcecensus:resourcecensus_test //internal/testutil/providerledger:providerledger_test //test/docsync:docsync_test`; exit 0. The phase label is tooling terminology: these are additional acceptance names, not diff-owned tests. Four fresh remote actions, 183 top-level PASS, 0 FAIL, 0 SKIP; eight log/XML hashes checked. `docsync_test` is the `make check-docs` target.

```text
TestDatedWaiversAgainstToday PASS (owned-waiver-strict-8c99475fe7aea7040e60.log)
TestRepositoryLedgerMatchesCensusAndDocumentation PASS (owned-waiver-strict-ec59c6484af9b652e4d0.log)
TestCatalogMatchesProductionWiringAndDocumentation PASS (owned-waiver-strict-5bc37b3835c4aadfbf91.log)
TestRuntimeWaiverExpiriesAreWholeDaysInOrder PASS (owned-waiver-strict-5bc37b3835c4aadfbf91.log)
TestRuntimeWaiverExpiriesDivergeByGap PASS (owned-waiver-strict-5bc37b3835c4aadfbf91.log)
```

## Skip assessment

`skip_inventory`: 363 farm skip events / 305 unique package-name-reason records, classified as 71 environment-absence and 292 other events. Every environment-absence package was rerun in the mandatory native floor, selecting the parent for subtests. Native floor has 14 top-level skips (39 including subtests): missing replay/simulation fixture inputs, optional Postgres DSN and br executable, opt-in live herdr journeys, tmux binding cases requiring an enclosing tmux session, and intentional unavailable-provider skip-contract subtests. Remaining farm skips are documented platform or opt-in fixture exclusions, subprocess helpers, known guarded suites, and intentional skip-contract cases. The diff changes only expiry data, no test body, helper, skip condition or provisioning. 416 possible farm source sites and 128 native log source sites mechanically read UNCHANGED; shared expiry-data hunks were reviewed. These cannot supply absent runtimes/OS/fixtures, and strict expiry/census/ledger/documentation checks all actually passed. No diff-owned test is skipped or missing. Full inventories and site proofs are retained by hash below.

## Required CI accounting

```text
diff 91f4bb2318c9f5008bdc47ea28875a865bddacd2...80c5330d1499cd8685d1a813043b888efc54281d: 4 changed files
  filter packs                no match
  filter credential_provider  MATCH internal/testutil/providerledger/ledger.go
  filter shared               no match
suite_mode: filtered  (shared filter did not match)

ci-required needs 5 jobs; transitive closure 5 jobs
job key                                  kind       runs-on                    trigger
runner-policy                            LEAF       linux                      EVERY-PR
changes                                  LEAF       linux                      EVERY-PR
check                                    AGGREGATOR linux                      EVERY-PR
  credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'
  pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'

4 LEAF jobs need a status; the rest are rolled up through their needs.
```

- runner-policy | LOCAL-PASS | EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=/var/tmp/deploy-ga-w1tv7kq.qqz2qzk2/ci-required/runner-policy.out python3 .github/workflows/scripts/runner_policy.py; exit 0; /var/tmp/deploy-ga-w1tv7kq.qqz2qzk2/ci-required/runner-policy.log
- changes | LOCAL-PASS | ci_required_triggers.py closure and path-filter evaluation; exit 0; /var/tmp/deploy-ga-w1tv7kq.qqz2qzk2/ci-required/closure.txt
- credential-provider-windows | DEFERRED-TO-PR-CI reason=non-linux-runner | runner blacksmith-4vcpu-windows-2025; reach internal/testutil/providerledger/ledger.go
  GOOS=windows GOARCH=amd64 go vet ./internal/credentialprovider; exit 0; /var/tmp/deploy-ga-w1tv7kq.qqz2qzk2/ci-required/windows-vet.log
  GOOS=windows GOARCH=amd64 go vet -tags=integration ./internal/credentialprovider; exit 0; /var/tmp/deploy-ga-w1tv7kq.qqz2qzk2/ci-required/windows-integration-vet.log
- pack-gate | NOT-TRIGGERED | pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'

Runner outputs were followed to their definitions: Linux Blacksmith labels and Windows Server 2025. The Windows vet checks prove build/vet only. Merge requires `CI / required` green at the published head and the Windows job success (or a skip solely from a false path filter). No host-precondition deferral and nothing NOT-COVERED.

## Acceptance proof and publication constraints

```text
TESTING.md: removed=57 added=57 date_only=PASS
internal/testpolicy/resourcecensus/policy.go: removed=56 added=56 date_only=PASS
internal/testutil/providerledger/ledger.go: removed=1 added=1 date_only=PASS
test/test-resources.toml: removed=56 added=56 date_only=PASS
TOTAL: 4 files, removed=170 added=170; 170 date-only line pairs; PASS
NORMALIZATION: 2026-10-31 -> 2026-12-31; time.October, 31 -> time.December, 31; all other bytes identical.
test/test-resource-counts.toml: unchanged; PASS
old-expiry grep '2026-10-31': empty; PASS
old-expiry grep 'time.October, 31': empty; PASS
```

Herdr expiry is 2026-12-31; nearest other runtime expiry 2026-11-22, gap 39 days. Both stagger tests pass. Mayor decision 4 survives in ga-cp3hwi and ga-siu9v3 notes; original purged mail references are retained in the mandatory PR text. ga-nnzehf is already on the frozen base, its counts file unchanged. The herdr hunk belongs to ga-80po0c.3 waiver owner reviewer. PR title corrects the stale bead count to 56. `PR-DESCRIPTION` is carried verbatim, alongside the explicit ISO/Go-month proof normalization. Historical engdocs and earlier gate records remain history. Merge is outside the excluded 2026-10-29 through 2026-10-31T00:00Z interval, and must land before 2026-11-01T00:00Z; the strict nightly audit first fails then. This is real renewal work, not a no-op.

## Compact execution record

```json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/deploy-ga-w1tv7kq.qqz2qzk2/c3/evidence/remote","admission":{"class":"remote","floor_gib":25.0,"free_gib":47.952796936035156,"waited_seconds":0.001,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98","worker_env_origin_main":"sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"},"test_exec_bep":{"unit":{"results":378,"remote_executed":378,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":24,"remote_executed":24,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":85,"remote_executed":85,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":14,"remote_executed":14,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":9722.194,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=5fd31cdd75b202be3ab9de14bb2514f059b7969d1f14ab7fdc392c2c723e6b7b"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":78.207,"evidence_errors":[],"probe":["GATE_FARM_PROBE verdict=clear waited=0s load1=48.18 load5=34.03 load15=23.27 cpus=32 threshold=50 max_wait=600s"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 31.36 31.45 22.86 9/1026 51994"]}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":30274}},"log_dir":"/var/tmp/deploy-ga-w1tv7kq.qqz2qzk2/c3/evidence/remote/floor-logs"},"skip_inventory":{"counts_by_reason_class":{"other":292,"environment-absence":71},"top_level_counts_by_reason_class":{"other":220,"environment-absence":42},"tests":{"elided_array":{"items":363,"bytes":128133}}},"test_cmd":["test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-w1tv7kq.qqz2qzk2/c3/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec","test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-w1tv7kq.qqz2qzk2/c3/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec","test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-w1tv7kq.qqz2qzk2/c3/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec","test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-w1tv7kq.qqz2qzk2/c3/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec","test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-w1tv7kq.qqz2qzk2/c3/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec"],"test_counts":{"PASS":134404,"FAIL":0,"SKIP":377},"diff_tests_executed":"diff_tests_executed: none (no diff-owned tests)\nGATE_TEST_EVIDENCE_RESULT ok: the diff owns no test\n"}
```

## Retained evidence

Full context.json retained: 19 lines; /var/tmp/deploy-ga-w1tv7kq.qqz2qzk2/context.json; SHA256 8e4c6ae858957fb80ec79fb285e9563427a564e1d85ddef7c52450c731b18062.

Full criterion6.sh retained: 46 lines; /var/tmp/deploy-ga-w1tv7kq.qqz2qzk2/criterion6.sh; SHA256 547d4067aff8c8740090c1a442f73b60621c778227d1a1b215799380465a3d4c.

Full c6-run/units/gate.w1.stdout.log retained: 21 lines; /var/tmp/deploy-ga-w1tv7kq.qqz2qzk2/c6-run/units/gate.w1.stdout.log; SHA256 97225e36a5b96d8434c9db06da9698ca958a26ba2290f7ce4b4f3b3a74afe747.

Full fast-lane.sh retained: 27 lines; /var/tmp/deploy-ga-w1tv7kq.qqz2qzk2/fast-lane.sh; SHA256 e568372d9097e6e62fe7fd64fc8d37347daa9decac3c455188703d05027230fc.

Full fast-policy.log retained: 509 lines; /var/tmp/deploy-ga-w1tv7kq.qqz2qzk2/fast-policy.log; SHA256 40436d494aecd837934d6eabfafe8f7b80e241ff5631965a150525cb224fe133.

Full fast-drift.log retained: 157 lines; /var/tmp/deploy-ga-w1tv7kq.qqz2qzk2/fast-drift.log; SHA256 04b2fb750a1c8d4108f5dfbbb39d6b2cbffec71793f2eb03add869cc1a1dcfeb.

Full fast-openapi.log retained: 153 lines; /var/tmp/deploy-ga-w1tv7kq.qqz2qzk2/fast-openapi.log; SHA256 842ea8fc882006a84f0f4a42b8edcad69c3ef098af1784e86ac63d947b4aef87.

Full runtime-preflight.log retained: 10 lines; /var/tmp/deploy-ga-w1tv7kq.qqz2qzk2/runtime-preflight.log; SHA256 e45e561ec5ce2d0fa736dae565c3413f50a13c01819b02d71577547a46d8212c.

Full icu74-preflight.txt retained: 4 lines; /var/tmp/deploy-ga-w1tv7kq.qqz2qzk2/icu74-preflight.txt; SHA256 dc1ab2e8ccf94bcf9a0c827a085e548134cafef5ec8378acc25ae4c948c21196.

Full c3/evidence/execution.json retained: 3956 lines; /var/tmp/deploy-ga-w1tv7kq.qqz2qzk2/c3/evidence/execution.json; SHA256 a166ec2c73d74b4ce17173f91339b5349a024818caacad88b9ea48212d3572b0.

Full independent-phases.json retained: 97 lines; /var/tmp/deploy-ga-w1tv7kq.qqz2qzk2/independent-phases.json; SHA256 308a3baae0a0053a7fc4ca0f313d1e341b7f1f43859c5f4c415638dfb7b713eb.

Full c3/evidence/remote/skip-inventory.json retained: 2906 lines; /var/tmp/deploy-ga-w1tv7kq.qqz2qzk2/c3/evidence/remote/skip-inventory.json; SHA256 0cc2d187b2461fec278d1720285c5576e0fba8dfb3664bdbd4a873afbb34e782.

Full skip-audit.json retained: 22 lines; /var/tmp/deploy-ga-w1tv7kq.qqz2qzk2/skip-audit.json; SHA256 734ac2df297c621f17ea9e379831361fbc3ab8f4def232f6328677986b8290af.

Full skip-sites-unchanged.txt retained: 419 lines; /var/tmp/deploy-ga-w1tv7kq.qqz2qzk2/skip-sites-unchanged.txt; SHA256 0ff58f97725acdf6411b7ec07f6346d40806194479396ff29da5b533a55ef331.

Full floor-sites-unchanged.txt retained: 131 lines; /var/tmp/deploy-ga-w1tv7kq.qqz2qzk2/floor-sites-unchanged.txt; SHA256 dd4efe33a714b8d8281b3287c2ac233ec4adce55263d9ee1842dfb85ae14a6f9.

Full floor-artifact-manifest.json retained: 82 lines; /var/tmp/deploy-ga-w1tv7kq.qqz2qzk2/floor-artifact-manifest.json; SHA256 787c01319b4c3ad8c63cf25ba75d86f2b1e598bbb69f88b025461c9b4c367b5c.

Full final-test-audit.json retained: 19 lines; /var/tmp/deploy-ga-w1tv7kq.qqz2qzk2/final-test-audit.json; SHA256 04ceaf2ddccbb133911c23030f3e7ef13b8b4f73550caad9cea345f597baa7dd.

Full strict-ac-ready/by-name.txt retained: 10 lines; /var/tmp/deploy-ga-w1tv7kq.qqz2qzk2/strict-ac-ready/by-name.txt; SHA256 7b8beeeb797b7166687468b019726c9e0316d0268b40e86406203d1b8f1c56e4.

Full ci-required/accounting.json retained: 60 lines; /var/tmp/deploy-ga-w1tv7kq.qqz2qzk2/ci-required/accounting.json; SHA256 1311adfc676b4cc0238f67a15bd6726489fee45843af1955aef2c46607d9d218.

Full prove-date-only.py retained: 16 lines; /var/tmp/deploy-ga-w1tv7kq.qqz2qzk2/prove-date-only.py; SHA256 1a2609b890bded9df86fa922fc2699beeb04e41580403225df28da0bfa9d21d3.

Full proof-output.txt retained: 9 lines; /var/tmp/deploy-ga-w1tv7kq.qqz2qzk2/proof-output.txt; SHA256 c969c78a1938760dc411db6a70021c323191dfa37290f44aedb989439fad0bbe.

Full pr-description-extra.txt retained: 33 lines; /var/tmp/deploy-ga-w1tv7kq.qqz2qzk2/pr-description-extra.txt; SHA256 48960d9d70cf84f78c150f790efaae6f5dde9ad82dd94a02649e477e73cbd1de.

Full c3/evidence/remote/unit-artifacts.json retained: 10586 lines; /var/tmp/deploy-ga-w1tv7kq.qqz2qzk2/c3/evidence/remote/unit-artifacts.json; SHA256 87a18d15dc4f15352882c5b2185cac2292f4497ac799f97dd8fbba9303d9e487.

Full c3/evidence/remote/acceptance-artifacts.json retained: 674 lines; /var/tmp/deploy-ga-w1tv7kq.qqz2qzk2/c3/evidence/remote/acceptance-artifacts.json; SHA256 af3b40ec0cab37daa156958caec12be5d00f684e056ce3282943d0838fffa607.

Full c3/evidence/remote/integration-packages-artifacts.json retained: 2382 lines; /var/tmp/deploy-ga-w1tv7kq.qqz2qzk2/c3/evidence/remote/integration-packages-artifacts.json; SHA256 0192e5e8c74849e11270821941258a1a8ac1d638c63a94b93931ffa9b26b0d18.

Full c3/evidence/remote/integration-full-artifacts.json retained: 394 lines; /var/tmp/deploy-ga-w1tv7kq.qqz2qzk2/c3/evidence/remote/integration-full-artifacts.json; SHA256 11ce708d0d8d58eae10b5da4854219b69cbbf5b2340268aee98c8e8ccd6e197d.

Full c3/evidence/remote/integration-latency-artifacts.json retained: 30 lines; /var/tmp/deploy-ga-w1tv7kq.qqz2qzk2/c3/evidence/remote/integration-latency-artifacts.json; SHA256 b3cec8339c364ffcf84b4a3095d786ccb5484621c9ce25e4596e8a213768b243.

