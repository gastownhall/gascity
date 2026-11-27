# Mail replies go to the other party (ga-bzlhlcnw)

**Verdict:** **PASS**

Feature slug: ga-bzlhlcnw-mail-reply-other-party.
Reviewed source: 1bdb7d78d9f0928806dac5bc44d47107cd482679 (resolved commit; build ga-9qno41, review ga-oypfs0c).
Deploy mode: remote. Source branch builder/ga-9qno41 is provenance only.
Gate base: 9285cfb3ed95f20bb4dea3ec085b4f36ff6b3a6c; base ref: origin/main.
Merge validation commit: 1266d12513ce6119f9d58136abfde1e64940c5ec; merge tree: e2c9c7eef08df4886f5b9382ecfc8dae35c2a9f7.
merge_state: clean (origin/main@9285cfb3ed95f20bb4dea3ec085b4f36ff6b3a6c).
gate_base_kind: current-tip (at the time the gate began).
merge_validation: the clean merge result built, vetted and passed the full gate here; MPR owns the merge-time guarantee and exact-head check if the base changes.

| # | Result | Evidence |
|---|---|---|
| 1 | PASS | Review ga-oypfs0c closed pass and names the exact resolved source. No carryover or branch-tip substitution. |
| 2 | PASS | Both original repro tests pass, including the session-shaped sender case; both providers pass the shared self-reply contract. All six caller shapes pass, including the four storebinding bridges. Ordinary reply tests remain covered by the full suite. Reply doc comments describe recipient selection. |
| 3 | PASS | Full fresh remote chain and native floor: PASS=159061 FAIL=0 SKIP=618. All four diff-owned tests PASS by name. Every SKIP is accounted for below; all six ci-required leaf jobs have a status. |
| 4 | PASS | No unresolved HIGH review findings. Review observations about exec-backed adapters, help text and additional edge-case tests remain outside this accepted fix. |
| 5 | PASS | The reviewed source and merge scratch were clean before this checklist was written. This checklist is the publication artifact; the isolated deploy branch is checked clean after its commit and before push. |
| 6 | PASS | Hook-free scratch merge; isolated, load-gated bazel build --config=fork-cache --repo_env=DO_NOT_TRACK=1 //... rc0, 1500 targets including nogo/vet. /var/tmp/gc-gate.ga-bzlhlcnw.CP5EqE/c6.output. |
| 7 | PASS | One mail-reply feature across the bead-backed and in-memory providers and their shared contract/front-door tests: eight files, +184/-18. Ancestry denylist/citation guard passed with this deploy and its verified build bead; no stack. |

## Acceptance and scope

When a sender replies to its own message, the reply goes to the parent's recipient. The bead-backed provider resolves the replier's session identity first; ordinary replies retain the stored sender session route. Both in-process providers reject self-addressed targets. The shared contract is opt-in for in-process providers because exec-backed adapters delegate their reply target to scripts. That limitation is explicitly disclosed in the PR summary. No endpoint, schema, module dependency or role behavior configuration is added.

Independent named evidence:

```text
diff_tests_executed:
  TestFakeReplyToOwnMessage PASS (unit-6639d289bc8a89b5416c.log)
  TestBeadmailReplyToOwnMessage PASS (unit-b061b554055944af2e43.log)
  TestReplyBySenderBareTargetsParentRecipient PASS (unit-b061b554055944af2e43.log)
  TestReplyBySenderWithSessionsDoesNotSelfAddress PASS (unit-b061b554055944af2e43.log)
GATE_TEST_EVIDENCE_RESULT ok: 4 diff-owned tests verified by name
```

The self-reply subtest also passed through TestReferenceMailProviderConforms, TestBeadsOverSQLiteRunsTheMailProviderTable, and the memory/SQLite mail-provider rows of TestWrappedClassFrontsRunTheUnchangedCorpus. Full instance evidence is retained in shared-self-reply-evidence.txt. The original red-before-green evidence remains on the review/build beads; this gate independently ran the full current merged tree.

## Criterion 3

test_cmd_scope: full-suite.
test_exec_mode: remote-exec (BEP-derived).
test_counts: PASS=159061 FAIL=0 SKIP=618.
test_log_dir: /var/tmp/gc-heavy-gate/runs/ga-bzlhlcnw.c3/evidence/remote/logs.
diff_tests_executed: all four owned tests PASS; exact names/logs above.
waiver_ref: none. failure_attribution: none (zero failed tests).
heavy_mode: none, computed by heavy-composite-gate.sh classify.
test_exec_bep: 554 first-attempt remote results, no remote/local test cache hits, no other strategy, all starts inside their phase windows. Phases: unit 426, acceptance 25, integration-packages 87, integration-full 15, integration-latency 1.
rbe_state: recorded in execution.json; worker environment matches the pinned gate base. fallback_reason: none.
admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 52.24791717529297, "waited_seconds": 0.001}.
farm_test_seconds: 14137.287.
owned_topup: none needed.
floor_topup: complete=true, all 16 prescribed native steps rc0, including environment-absence packages, beadstest, Darwin filesystem compile and runner self-tests.
latency_leg: both budgets PASS on the first measured attempt after the farm probe cleared (50 seconds waiting, clear load1=47.45 against threshold 50). The published budget and fastest-of-7 statistic are unchanged.
delta_gate: shadow plan chose the full gate (not-gate-pr); no narrowed run or stitched evidence.

Fast lane ran first, with every lane in its own fresh view, all rc0: format bazel test --config=fork-cache --repo_env=DO_NOT_TRACK=1 //:go_format_test; policy gate-base.sh run -- isolated-test-run.sh -- make lint check-release-dist-ignore check-routed-test-rows check-split-topology-rows check-residency-boundary NOGO_BUILD_FLAGS='--keep_going --output_groups=nogo_fix --config=fork-cache --repo_env=DO_NOT_TRACK=1'; generated drift BAZEL_SYNC_FLAGS='--config=fork-cache --repo_env=DO_NOT_TRACK=1' make bazel-sync then git diff --exit-code. Scripts and full output: /var/tmp/gc-gate.ga-bzlhlcnw.CP5EqE/fast.sh, drift.sh, fast.output.
known_red_precheck: proceed, nothing-known-red; check-routed-test-rows is green and tracker ga-1ptv4jey is stale. The prior hold cleared after its stamped fix landed; this is a fresh gate, not reuse of the old evidence.
ICU 74: OK, recorded sonames/hashes retained in /var/tmp/gc-gate.ga-bzlhlcnw.CP5EqE/icu74.txt.

Launcher: gate-detached-run.sh -> gate-criterion3.sh --repo /var/tmp/gc-merge-validate.ga-bzlhlcnw.MNlMtg --base 9285cfb3ed95f20bb4dea3ec085b4f36ff6b3a6c --bead-json /var/tmp/gc-gate.ga-bzlhlcnw.CP5EqE/bead.json --owned /var/tmp/gc-gate.ga-bzlhlcnw.CP5EqE/owned.tsv --run-dir /var/tmp/gc-heavy-gate/runs/ga-bzlhlcnw.c3/evidence; GC_GATE_REMOTE_ENABLED=1 GC_REMOTE_GATE_FLOOR_GIB=25 GC_REMOTE_GATE_CAP=2 ISOLATED_TEST_BEAD=ga-bzlhlcnw GC_GATE_DELTA=shadow GC_GATE_LOAD_DEFER_MAX=3. Complete recorded phase commands and flags:

```text
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-bzlhlcnw.c3/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-bzlhlcnw.c3/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-bzlhlcnw.c3/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-bzlhlcnw.c3/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-bzlhlcnw.c3/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```

### SKIP integrity

skip_inventory: 676 events including subtests/repeats; 548 other and 128 environment-absence. This differs from the 618 top-level SKIP result lines by design. All 319 distinct source sites are UNCHANGED; no unresolved site or diff-owned SKIP. The two HELPER-HUNKS are the additive RunReplyToOwnMessageTests in internal/mail/mailtest/conformance.go and RunMailProviderTests in internal/storebinding/storebindingtest/messaging.go. Module test-binary import closures were computed for every skipping package under unit, acceptance_a, integration and native-integration tag contexts.

303 HEAD skip events import a changed helper. They all reproduce at the pinned base: 267 remote events over four exact-target/tag groups and 36 native events using the identical published native-floor command/environment. These are focused baseline comparisons only; they never replace or contribute to the full-suite counts. Baseline results and logs are separate from HEAD gate evidence. All 16 baseline native-floor commands also completed rc0 and match the HEAD floor command list.

```text
1-integration-cmd_gc_gc_test: COMPARE-SUMMARY head_skips=68 judged=68 pre-existing=68 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
2-unit-cmd_gc_gc_sim_exec_test: COMPARE-SUMMARY head_skips=4 judged=4 pre-existing=4 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
3-unit-cmd_gc_gc_test: COMPARE-SUMMARY head_skips=189 judged=189 pre-existing=189 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
4-unit-internal_mail_exec_exec_test: COMPARE-SUMMARY head_skips=6 judged=6 pre-existing=6 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
COMPARE-SUMMARY head_skips=36 judged=36 pre-existing=36 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
COMPARE-NOTE the remote worker's account differs between the runs in 1 SKIP(s), read as the same reason because the account is the only difference (an account is rbe-, a letter and two digits, standing alone; the logs are not changed): TestInfraScopedManagedDoltPIDIdentityUnderRealSystemd (base rbe-a10, head rbe-a03)
```

skip_justification: unchanged guards cover fast-unit/profile selection, optional tools and fixtures, platform/runtime capabilities and cases the frozen reference cannot model. The added self-reply helper contains no skip and is invoked by the passing contract callers listed above; its old neighbors' skip decisions are unchanged. The helper-reached groups have measured identical reasons at the pinned base. The other events' source sites are unchanged and their test binaries import neither changed helper. The native floor exercised every selected environment-absence package; remaining reasons and guard proofs are retained in the full inventory.

Baseline setup audit: the first diagnostic attempt lacked generated RBE config and ran locally; its test verdict was PASSED, but the evidence predicate refused the strategy/config. It is uncredited. Raw BEP/stdout/report remain under base-skip-comparisons; the corrected fresh-view run installed and validated the official RBE config and produced the comparisons above. No failed test verdict or full-suite phase was retried.

### Load evidence

criterion 6: load_threshold=15 load_waited_seconds=0 load_wait_timed_out=0 run_start_load=9.38 run_max_load=39.34 run_mean_load=24.55 run_readings=8; LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=9.38 run_max_load=39.34 run_mean_load=24.55 run_readings=8 wait_first_load=9.38 wait_max_load=9.38 wait_mean_load=9.38 wait_readings=1 read_errors=0
full C3, remote class (host sampling only; no local-heavy wait): load_threshold=15 load_waited_seconds=0 load_wait_timed_out=0 run_start_load=30.81 run_max_load=30.89 run_mean_load=16.08 run_readings=52; LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=30.81 run_max_load=30.89 run_mean_load=16.08 run_readings=52 wait_first_load=30.81 wait_max_load=30.81 wait_mean_load=30.81 wait_readings=1 read_errors=0
HEAD native floor: load_threshold=15 load_waited_seconds=0 load_wait_timed_out=0 run_start_load=11.22 run_max_load=12.85 run_mean_load=12.42 run_readings=7; LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=11.22 run_max_load=12.85 run_mean_load=12.42 run_readings=7 wait_first_load=11.22 wait_max_load=11.22 wait_mean_load=11.22 wait_readings=1 read_errors=0
baseline comparison lanes: load_threshold=15 load_waited_seconds=150 load_wait_timed_out=0 run_start_load=14.80 run_max_load=16.30 run_mean_load=14.55 run_readings=7; LOAD_GATE_SUMMARY threshold=15 waited_seconds=150 wait_timed_out=0 run_start_load=14.80 run_max_load=16.30 run_mean_load=14.55 run_readings=7 wait_first_load=18.28 wait_max_load=18.28 wait_mean_load=16.32 wait_readings=6 read_errors=0
baseline comparison lanes: load_threshold=15 load_waited_seconds=0 load_wait_timed_out=0 run_start_load=13.37 run_max_load=17.20 run_mean_load=15.84 run_readings=7; LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=13.37 run_max_load=17.20 run_mean_load=15.84 run_readings=7 wait_first_load=13.37 wait_max_load=13.37 wait_mean_load=13.37 wait_readings=1 read_errors=0
CI commands: load_threshold=15 load_waited_seconds=210 load_wait_timed_out=0 run_start_load=14.34 run_max_load=15.86 run_mean_load=15.15 run_readings=3; LOAD_GATE_SUMMARY threshold=15 waited_seconds=210 wait_timed_out=0 run_start_load=14.34 run_max_load=15.86 run_mean_load=15.15 run_readings=3 wait_first_load=16.91 wait_max_load=16.91 wait_mean_load=15.88 wait_readings=8 read_errors=0
Remote admission uses the remote profile without a host-load wait. Native lanes retain threshold 15. Wait readings are separate from run readings; no wait timed out.

## Criterion 3e: required CI closure

ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-validate.ga-bzlhlcnw.MNlMtg 9285cfb3ed95f20bb4dea3ec085b4f36ff6b3a6c 1bdb7d78d9f0928806dac5bc44d47107cd482679 fork; rc0; merged workflow blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs need a status.

```text
diff 9285cfb3ed95f20bb4dea3ec085b4f36ff6b3a6c...1bdb7d78d9f0928806dac5bc44d47107cd482679: 8 changed files
  filter packs                no match
  filter credential_provider  no match
  filter shared               no match
suite_mode: filtered  (shared filter did not match)

ci-required needs 7 jobs; transitive closure 7 jobs
job key                                  kind       runs-on                    trigger
runner-policy                            LEAF       linux                      EVERY-PR
changes                                  LEAF       linux                      EVERY-PR
check                                    AGGREGATOR linux                      EVERY-PR
  credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'
  pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'
  windows-lane                           LEAF       linux                      EVERY-PR
  windows-lane-test                      LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} EVERY-PR

6 LEAF jobs need a status; the rest are rolled up through their needs.
```

ci_required_accounting:
- runner-policy | LOCAL-PASS | EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=/var/tmp/gc-gate.ga-bzlhlcnw.CP5EqE/accounting/runner-policy.out python3 .github/workflows/scripts/runner_policy.py; rc0; runner-policy.log/out. Runner expressions resolve to explicit Ubuntu/Windows labels.
- changes | LOCAL-PASS | Closure tool above, rc0; accounting/closure.txt.
- credential-provider-windows | NOT-TRIGGERED | Tool row: NON-LINUX, NOT-TRIGGERED, if needs.changes.outputs.credential_provider == 'true'; no filter match.
- pack-gate | NOT-TRIGGERED | Tool row: NOT-TRIGGERED, if needs.changes.outputs.packs == 'true'; no filter match.
- windows-lane | LOCAL-PASS | Exact Windows allowlist and nogo build/test-compile commands, then all-package Darwin build/test-compile below. Each command ran sequentially, fresh view outermost, load-gated, isolated and detached; rc0; accounting/jobs.output. No Windows binary was run on Linux.
- windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | Its own complete Windows test-binary import closure over .github/windows-lane/test.txt computed rc0, ten module package directories, no changed path reached; accounting/windows-native-reach.json.

```bash
#!/usr/bin/env bash
set -euo pipefail
printf 'CI_JOB_STEP Windows allowlist build\n'
GOOS=windows go build $(cat .github/windows-lane/build.txt)
printf 'CI_JOB_STEP Windows allowlist test compile\n'
GOOS=windows go test -count=1 -vet=off -run '^$' -exec /bin/true $(cat .github/windows-lane/test-build.txt)
printf 'CI_JOB_STEP Windows nogo analyzers\n'
(cd tools/nogo && GOOS=windows go build ./analyzers/... && GOOS=windows go test -count=1 -vet=off -run '^$' -exec /bin/true ./analyzers/...)
printf 'CI_JOB_STEP Darwin compile all packages and tests\n'
GOOS=darwin go build ./... && GOOS=darwin go test -count=1 -vet=off -run '^$' -exec /bin/true ./...
```

windows_reach: windows-lane-test=none (its own test-binary closure, not the credential-provider-only probe); credential-provider-windows NOT-TRIGGERED, separate reach probe also rc0/none.
deferred_to_pr_ci: windows-lane-test (non-linux-runner).
not_covered: none. host_gaps: none for required CI jobs.
ci_lane_run: n/a (no CI-config diff).
This PASS authorizes PR publication only. Merge requires CI / required green on the current PR head and the deferred native Windows job successful, or skipped solely by its own false path filter. MPR owns that decision and the exact-head merge.

## Retained evidence

Run root: /var/tmp/gc-gate.ga-bzlhlcnw.CP5EqE; full suite root: /var/tmp/gc-heavy-gate/runs/ga-bzlhlcnw.c3/evidence. All per-job logs, raw BEP, manifests and complete source/skip proofs are retained. Isolation wrapper reports PASS-THROUGH for Gas City, not a clean ambient environment claim; Bazel actions use their execution sandboxes, native floor scrubs store selectors, and the CI commands use a fresh committed view. PROJECT_MANIFEST.md is absent; the seven role criteria govern.

Compact execution record:

```json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/gc-heavy-gate/runs/ga-bzlhlcnw.c3/evidence/remote","gate_base":"9285cfb3ed95f20bb4dea3ec085b4f36ff6b3a6c","gate_head":"1bdb7d78d9f0928806dac5bc44d47107cd482679","admission":{"class":"remote","floor_gib":25.0,"free_gib":52.24791717529297,"waited_seconds":0.001,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"},"delta_gate":{"mode":"shadow","decision":"full","reason":"not-gate-pr","detail":"no PR record (pr.json)","applied":false,"base":"9285cfb3ed95f20bb4dea3ec085b4f36ff6b3a6c","head":"1bdb7d78d9f0928806dac5bc44d47107cd482679","merge":"1266d12513ce6119f9d58136abfde1e64940c5ec","prior":null,"delta":{},"changed_labels":[],"changed_label_count":0,"tests":{},"phases":{},"plan_sha256":"c6ba2cd3ce1a819e5966a002b4944e863eff4c045f6f390b52f8ebc5c7554282"},"test_exec_bep":{"unit":{"results":426,"remote_executed":426,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":87,"remote_executed":87,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":15,"remote_executed":15,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":14137.287,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=5fd31cdd75b202be3ab9de14bb2514f059b7969d1f14ab7fdc392c2c723e6b7b"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":126.965,"evidence_errors":[],"probe":["GATE_FARM_PROBE waiting waited=0s load1=74.10 load5=104.47 load15=61.81 cpus=32 threshold=50 max_wait=600s","GATE_FARM_PROBE verdict=clear waited=50s load1=47.45 load5=92.59 load15=60.01 cpus=32 threshold=50 max_wait=600s"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 31.56 84.45 58.33 9/1039 55905"]}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":38510}},"log_dir":"/var/tmp/gc-heavy-gate/runs/ga-bzlhlcnw.c3/evidence/remote/floor-logs"},"skip_inventory":{"counts_by_reason_class":{"other":514,"environment-absence":90},"top_level_counts_by_reason_class":{"other":256,"environment-absence":43},"tests":{"elided_array":{"items":604,"bytes":216205}}},"test_cmd":{"elided_array":{"items":5,"bytes":2054}},"test_counts":{"PASS":159061,"FAIL":0,"SKIP":618},"diff_tests_executed":"diff_tests_executed:\n  TestFakeReplyToOwnMessage PASS (unit-6639d289bc8a89b5416c.log)\n  TestBeadmailReplyToOwnMessage PASS (unit-b061b554055944af2e43.log)\n  TestReplyBySenderBareTargetsParentRecipient PASS (unit-b061b554055944af2e43.log)\n  TestReplyBySenderWithSessionsDoesNotSelfAddress PASS (unit-b061b554055944af2e43.log)\nGATE_TEST_EVIDENCE_RESULT ok: 4 diff-owned tests verified by name\n"}
```
Full execution.json retained: 6059 lines; /var/tmp/gc-heavy-gate/runs/ga-bzlhlcnw.c3/evidence/execution.json; SHA256 a645dd5075d1f771c462a3e9da627b23b44b9c390cabbd1c37797903e0e4aec1.
Full full named-test evidence retained: 10 lines; /var/tmp/gc-gate.ga-bzlhlcnw.CP5EqE/final-by-name.txt; SHA256 a7b669e57922547fe962132ce830ea61c9e26f9873c6e3da288de5adb4a1a731.
Full shared self-reply contract retained: 9 lines; /var/tmp/gc-gate.ga-bzlhlcnw.CP5EqE/shared-self-reply-evidence.txt; SHA256 b87264c6d17c3b11082a3fbf68ead66adea5254cecbc2e203864cb7b5b99197b.
Full skip events retained: 5410 lines; /var/tmp/gc-gate.ga-bzlhlcnw.CP5EqE/all-skip-events.json; SHA256 5be0417240f75242019a6daeb55cd1017b8383c9ddf50cb68e3a95142eb5ce2f.
Full skip sites retained: 321 lines; /var/tmp/gc-gate.ga-bzlhlcnw.CP5EqE/skip-sites-audited.json; SHA256 bdc9baa562ebe4c3cc9feb95ca057c2a2426e691cbff48dd0b8c8dbca380c224.
Full helper import closures retained: 184 lines; /var/tmp/gc-gate.ga-bzlhlcnw.CP5EqE/shared-helper-import-closures.json; SHA256 6da9b132c93313db990f60670427d6bf95ea67121ef923ba3bee1ce2b4dc2495.
Full remote baseline comparisons retained: 846 lines; /var/tmp/gc-gate.ga-bzlhlcnw.CP5EqE/base-skip-comparisons-rbe/results.json; SHA256 30c56238f6699d1fae8c479d15a279e8cd8f3d9beddfa643ff89aba93e11699f.
Full native baseline comparison retained: 41 lines; /var/tmp/gc-gate.ga-bzlhlcnw.CP5EqE/native-base-skip-proof.json; SHA256 eba141f0d7e7894d072c6c5850ba869bd63547cd50fcbd4e8a9f61a7990912c1.
Full HEAD native floor retained: 1056 lines; /var/tmp/gc-heavy-gate/runs/ga-bzlhlcnw.c3/evidence/remote/floor-topup.json; SHA256 e7fbd25fe0fa86d8e407ce9064c0dcc727396ec5a5c557d0d256fe99fa70a833.
Full BASE native floor retained: 1056 lines; /var/tmp/gc-gate.ga-bzlhlcnw.CP5EqE/base-native-floor-evidence/floor-topup.json; SHA256 901b9c01f311a4ef9baf4cb919d244cd22fdc74f48f0f10a0364568f5b1fcacc.
Full CI output retained: 361 lines; /var/tmp/gc-gate.ga-bzlhlcnw.CP5EqE/accounting/jobs.output; SHA256 4bde9fd1c857f1060d3662fe432c32953c873e025d9655746447e6d877e2e672.
Full source-site proof skip-proof-batch-1 retained: 297 lines; /var/tmp/gc-gate.ga-bzlhlcnw.CP5EqE/skip-proof-batch-1.txt; SHA256 83204358d9af2dd32ad14a88c0987d8f191b91917a3ff14b67c60dbfea3dec27.
Full source-site proof skip-proof-batch-2 retained: 50 lines; /var/tmp/gc-gate.ga-bzlhlcnw.CP5EqE/skip-proof-batch-2.txt; SHA256 b62c4504d2388b8bd3b583e41f8817b6de227d5958e32dd69ac810cdc72fb896.
Full source-site proof skip-proof-batch-3 retained: 35 lines; /var/tmp/gc-gate.ga-bzlhlcnw.CP5EqE/skip-proof-batch-3.txt; SHA256 fc903bfd721aa14b4e7c8978a8ab067c8011771f9c0f76b6292d46dac9c49df1.
Full Bazel artifacts acceptance-artifacts retained: 702 lines; /var/tmp/gc-heavy-gate/runs/ga-bzlhlcnw.c3/evidence/remote/acceptance-artifacts.json; SHA256 f08596dbd621564a6607c4f1a44cd0acbd33124c65c3874b6616b770ebd5484d.
Full Bazel artifacts integration-full-artifacts retained: 422 lines; /var/tmp/gc-heavy-gate/runs/ga-bzlhlcnw.c3/evidence/remote/integration-full-artifacts.json; SHA256 be69757442ad653c7c32c644d93711c3c2b35b3ebd2fc8539634177d47969c08.
Full Bazel artifacts integration-latency-artifacts retained: 30 lines; /var/tmp/gc-heavy-gate/runs/ga-bzlhlcnw.c3/evidence/remote/integration-latency-artifacts.json; SHA256 7db7c662f678a2edce2972136bfdb16b5d2bd7c500c02d6669896fbc9b1c6000.
Full Bazel artifacts integration-packages-artifacts retained: 2438 lines; /var/tmp/gc-heavy-gate/runs/ga-bzlhlcnw.c3/evidence/remote/integration-packages-artifacts.json; SHA256 4bb6aa5acaf3d31cd388a6cdfb60cf289e3c6da781ba21adbe1280234befac9e.
Full Bazel artifacts unit-artifacts retained: 11930 lines; /var/tmp/gc-heavy-gate/runs/ga-bzlhlcnw.c3/evidence/remote/unit-artifacts.json; SHA256 d34e402f2908b0b782fcc5f1fb802ccaa94109ac85b67a88066b68458c16b4a0.
