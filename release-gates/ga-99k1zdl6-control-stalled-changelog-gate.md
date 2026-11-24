# control.stalled changelog compatibility notice (ga-99k1zdl6)

**Verdict:** **PASS**

Reviewed commit: 7f05e505d7ba559cfa56f93283c77a834c78d758 (resolved object; build ga-7gvz5n, review ga-d0l38nq).
Source branch builder/ga-7gvz5n is provenance only, never a push target.
DEPLOY_MODE: remote. Base ref: origin/main.
gate_base: d6d5784f9cffbcff48f330dd5a5368e5dd622ccb.
merge_head: d4e4cf4e80ac98db0d70d89ed1243159bf18f177; merge_tree: 390d0b89f03e75b202c8664ce38ac77f4016545d.
merge_state: clean (origin/main@d6d5784f9cffbcff48f330dd5a5368e5dd622ccb; files: none).
gate_base_kind: current-tip.
merge_validation: clean merge built and vetted here; MPR owns the merge-time guarantee and exact-head check if main moves.

| # | Result | Evidence |
|---|---|---|
| 1 | PASS | Review ga-d0l38nq records PASS for the exact reviewed source. No carryover or branch-tip substitution. |
| 2 | PASS | All three acceptance criteria independently verified in acceptance.txt and below. |
| 3 | PASS | Complete fresh chain PASS=159056 FAIL=0 SKIP=618; no diff-owned tests; every remaining SKIP and all six required CI leaves accounted for below. |
| 4 | PASS | Zero unresolved HIGH review findings. The optional Upgrading Notes cross-reference and wording scope observations are non-blocking. |
| 5 | PASS | Role/source and merge scratch clean before this checklist. Publication branch will contain only this gate record over the reviewed source and must be clean after its hooked commit. |
| 6 | PASS | Hook-free clean merge, independently matched to git merge-tree; bazel build --config=fork-cache --repo_env=DO_NOT_TRACK=1 //... rc0, 1500 targets including nogo/vet; c6.output. |
| 7 | PASS | One changelog compatibility entry, CHANGELOG.md only, +15/-0. No independent feature bundle; branch ancestry/citation denylist guard PASS with the verified deploy/build lineage, no stack. |

## Acceptance evidence

Independent acceptance evidence (not a suite substitute):
AC1: merged CHANGELOG.md Unreleased/Added contains exactly the added control.stalled entry, both semantic and pending values, and the requirement for subscribers to branch on error_class. Current emitter pendingControlStall uses errorClass pending without ordersFailed; semantic kind sets ordersFailed true. Current typed event payload docs name the same discriminator.
AC2: entry sits under Unreleased/Added; existing storage.binding.not_configured and proven_beads value/field additions provide the requested precedent. Exactly one Unreleased heading remains; git diff --check rc0.
AC3: authoritative source range faefebb03e9904c5444e9e4cb760fdd575984f81..7f05e505d7ba559cfa56f93283c77a834c78d758 changes CHANGELOG.md only, +15/-0. No source/tests/OpenAPI edit.
Review ga-d0l38nq records PASS for exact reviewed SHA; zero unresolved HIGH findings. No new test appropriate for this documentation-only criterion; diff-owned-tests.py reports none.
Scope is one consumer-compatibility changelog entry; no independent feature theme.
Contributor PR #6528 is already represented by local main history and was not read or touched.

Exact merged entry:

```text
- **`control.stalled` now fires for a second disposition, `error_class`
  `"pending"`, so a subscriber must branch on `error_class` before it treats
  the event as terminal.** Through 1.5.1 the event carried one value,
  `"semantic"`: a refusal the control dispatcher quarantined, which closes the
  control bead and fails its order, so `order.failed` follows. It now also
  fires with `"pending"` when a drift-pending wait (a rig removed from
  `city.toml`, a city renamed mid-flight, or a sibling workflow store that
  cannot be opened) outlasts its loudness horizon. A pending bead stays open
  and keeps retrying, completes the moment the drift is healed, and emits no
  `order.failed`. A subscriber that reads every `control.stalled` as "this
  workflow is dead" now mistakes a healable wait for a dead one: treat only
  `"semantic"` as terminal, and read `"pending"` as a wait that needs the
  drift fixed, not the workflow restarted. A subscriber should also ignore an
  `error_class` it does not know (#6528).
```

## Criterion 3

test_cmd_scope: full-suite.
test_exec_mode: remote-exec (BEP-derived).
test_counts: PASS=159056 FAIL=0 SKIP=618.
test_log_dir: /var/tmp/gc-heavy-gate/runs/ga-99k1zdl6.c3/evidence/remote/logs.
diff_tests_executed: none (no diff-owned tests); official source-diff evidence retained in owned-explain.txt and empty owned.tsv.
waiver_ref: none. failure_attribution: none (zero failures in this chain).
heavy_mode: none (computed by heavy-composite-gate.sh classify; heavy.txt).
test_exec_bep: 554 fresh first-attempt remote results, no test cache hit, no other strategy, all starts within their phase windows. Exact per-phase counters are in the execution record below.
farm_test_seconds: 16011.173.
rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72", "worker_env_tree": "sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"}.
fallback_reason: None.
admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 50.239356994628906, "waited_seconds": 0.001}.
owned_topup: [].
floor_topup: complete=True; 16 prescribed native steps; all exit0, environment-absence packages, beadstest, runner self-tests and Darwin filesystem compile. Complete commands and individual logs in floor-topup.json.
latency_leg: complete, both published budgets PASS in the independent clear-probe lane; statistic and budgets unchanged; exact probe, attempts, results and commands retained in execution.json.
delta_gate: shadow plan chose full gate (not-gate-pr); no narrowed run or stitched evidence.

Fast lane PASS before suite launch, with each lane in its own fresh view: format bazel test --config=fork-cache --repo_env=DO_NOT_TRACK=1 //:go_format_test; policy gate-base.sh run -- isolated-test-run.sh -- make lint check-release-dist-ignore check-routed-test-rows check-split-topology-rows check-residency-boundary NOGO_BUILD_FLAGS='--keep_going --output_groups=nogo_fix --config=fork-cache --repo_env=DO_NOT_TRACK=1'; drift BAZEL_SYNC_FLAGS='--config=fork-cache --repo_env=DO_NOT_TRACK=1' make bazel-sync then git diff --exit-code. Full scripts/output in fast.sh, drift.sh, fast.output. Known-red precheck proceeds; ICU74 OK, sonames/hashes retained. Prior held run is not reused.

Full chain launcher: gate-detached-run.sh -> gate-criterion3.sh --repo /var/tmp/gc-merge-validate.ga-99k1zdl6.oulr7xr7 --base d6d5784f9cffbcff48f330dd5a5368e5dd622ccb --bead-json /var/tmp/gc-gate.ga-99k1zdl6.qlwupvwb/bead-suite.json --owned /var/tmp/gc-gate.ga-99k1zdl6.qlwupvwb/owned.tsv --run-dir /var/tmp/gc-heavy-gate/runs/ga-99k1zdl6.c3/evidence; GC_GATE_REMOTE_ENABLED=1 GC_REMOTE_GATE_FLOOR_GIB=25 GC_REMOTE_GATE_CAP=2 ISOLATED_TEST_BEAD=ga-99k1zdl6 GC_GATE_DELTA=shadow GC_GATE_LOAD_DEFER_MAX=3. Exact executed phase commands:

```text
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-99k1zdl6.c3/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-99k1zdl6.c3/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-99k1zdl6.c3/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-99k1zdl6.c3/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-99k1zdl6.c3/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```

### SKIP integrity

skip_inventory: 676 events (including subtests/repeated jobs); reason classes {"environment-absence": 128, "other": 548}. Counts differ from top-level SKIP lines by definition. 319 distinct source sites are UNCHANGED; unresolved=0. Official --explain/--site evidence retained in skip-proof-batch files; exact reasons and per-event log paths in all-skip-events.json. Merged diff against the pinned base is CHANGELOG.md only: no SHARED-HUNKS or HELPER-HUNKS, no test body/guard changes. No baseline helper comparison is required.

skip_justification: unchanged guards select profiles, optional tools/fixtures and platform/runtime capabilities; their reasons are recorded individually. The native floor exercises every selected environment-absence package. Runtime/profile skip decisions cannot be reached by this plain changelog entry. No diff-owned SKIP is excused.

### Load evidence

criterion 6: LOAD_GATE_SUMMARY threshold=15 waited_seconds=150 wait_timed_out=0 run_start_load=13.91 run_max_load=15.38 run_mean_load=15.38 run_readings=1 wait_first_load=19.43 wait_max_load=19.43 wait_mean_load=16.55 wait_readings=6 read_errors=0
full C3 remote class, host sampling only: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=14.33 run_max_load=15.54 run_mean_load=13.22 run_readings=50 wait_first_load=14.33 wait_max_load=14.33 wait_mean_load=14.33 wait_readings=1 read_errors=0
native floor: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=12.15 run_max_load=14.54 run_mean_load=13.23 run_readings=7 wait_first_load=12.15 wait_max_load=12.15 wait_mean_load=12.15 wait_readings=1 read_errors=0
runner-policy: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=12.72 run_max_load=NA run_mean_load=NA run_readings=0 wait_first_load=12.72 wait_max_load=12.72 wait_mean_load=12.72 wait_readings=1 read_errors=0
windows-lane: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=12.51 run_max_load=18.89 run_mean_load=16.81 run_readings=4 wait_first_load=12.51 wait_max_load=12.51 wait_mean_load=12.51 wait_readings=1 read_errors=0
Remote admission has no host-load wait. Native/CI commands retain threshold15 waits. Every load summary preserves threshold, waited_seconds, timeout and all run readings; wait samples remain separate.

## Required CI leaf accounting (3e)

ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-validate.ga-99k1zdl6.oulr7xr7 d6d5784f9cffbcff48f330dd5a5368e5dd622ccb 7f05e505d7ba559cfa56f93283c77a834c78d758 fork; rc0; merged workflow blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs need a status.

```text
diff d6d5784f9cffbcff48f330dd5a5368e5dd622ccb...7f05e505d7ba559cfa56f93283c77a834c78d758: 1 changed files
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
- runner-policy | LOCAL-PASS | EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=/var/tmp/gc-gate.ga-99k1zdl6.qlwupvwb/accounting/runner-policy.out python3 .github/workflows/scripts/runner_policy.py; rc0; runner-policy.output. Runs-on expressions resolve to explicit Ubuntu/Windows labels.
- changes | LOCAL-PASS | Tool command/row above; closure.txt, rc0.
- credential-provider-windows | NOT-TRIGGERED | Tool row: NON-LINUX; credential_provider filter no match.
- pack-gate | NOT-TRIGGERED | Tool row: packs filter no match.
- windows-lane | LOCAL-PASS | Its four exact run commands below with unchanged empty job/step env, rc0; windows-lane.output; fresh-tree outermost, load-gated, isolated, detached, sequentially after runner-policy completed.
- windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | Its OWN Windows test-binary import closure from .github/windows-lane/test.txt computed rc0, reach=none; windows-native-reach.json and raw closure retained.

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

windows_reach: windows-lane-test=none; own job's Windows test-binary closure, no changed file reached.
deferred_to_pr_ci: windows-lane-test (non-linux-runner).
not_covered: none. host_gaps: none for required CI jobs.
ci_lane_run: n/a (no CI-config change).
This PASS authorizes PR publication. Merge requires current-head CI / required and deferred native Windows success (or skipped solely on its own false path filter). Merge authority is MPR's.

## Retained evidence

Run root /var/tmp/gc-gate.ga-99k1zdl6.qlwupvwb; full suite /var/tmp/gc-heavy-gate/runs/ga-99k1zdl6.c3/evidence. Full raw BEPs, digest-verified per-job logs, artifact manifests and skip reasons are kept. Isolation wrapper reports PASS-THROUGH for Gas City, not a clean ambient-environment claim; Bazel uses its execution sandboxes, native floor scrubs store selectors, CI jobs run fresh committed views. docs/PROJECT_MANIFEST.md is absent; the role's seven release criteria govern. This record does not claim the PR has already been published or merged.

Compact execution record:

```json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/gc-heavy-gate/runs/ga-99k1zdl6.c3/evidence/remote","gate_base":"d6d5784f9cffbcff48f330dd5a5368e5dd622ccb","gate_head":"7f05e505d7ba559cfa56f93283c77a834c78d758","admission":{"class":"remote","floor_gib":25.0,"free_gib":50.239356994628906,"waited_seconds":0.001,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"},"delta_gate":{"mode":"shadow","decision":"full","reason":"not-gate-pr","detail":"no PR record (pr.json)","applied":false,"base":"d6d5784f9cffbcff48f330dd5a5368e5dd622ccb","head":"7f05e505d7ba559cfa56f93283c77a834c78d758","merge":"d4e4cf4e80ac98db0d70d89ed1243159bf18f177","prior":null,"delta":{},"changed_labels":[],"changed_label_count":0,"tests":{},"phases":{},"plan_sha256":"d5c1c2c8313ac195abcb063e2c7bd16bcd8accc769fbcab920d41e4af0133b06"},"test_exec_bep":{"unit":{"results":426,"remote_executed":426,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":87,"remote_executed":87,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":15,"remote_executed":15,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":16011.173,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=5fd31cdd75b202be3ab9de14bb2514f059b7969d1f14ab7fdc392c2c723e6b7b"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":79.388,"evidence_errors":[],"probe":["GATE_FARM_PROBE verdict=clear waited=0s load1=1.90 load5=5.37 load15=8.89 cpus=32 threshold=50 max_wait=600s"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 5.71 5.93 8.97 9/1142 56216"]}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":38510}},"log_dir":"/var/tmp/gc-heavy-gate/runs/ga-99k1zdl6.c3/evidence/remote/floor-logs"},"skip_inventory":{"counts_by_reason_class":{"other":514,"environment-absence":90},"top_level_counts_by_reason_class":{"other":256,"environment-absence":43},"tests":{"elided_array":{"items":604,"bytes":216205}}},"test_cmd":{"elided_array":{"items":5,"bytes":2054}},"test_counts":{"PASS":159056,"FAIL":0,"SKIP":618},"diff_tests_executed":"diff_tests_executed: none (no diff-owned tests)\nGATE_TEST_EVIDENCE_RESULT ok: the diff owns no test\n"}
```

Full execution.json retained: 6058 lines; /var/tmp/gc-heavy-gate/runs/ga-99k1zdl6.c3/evidence/execution.json; SHA256 5fd4129aec099ccb6d77549baf4912a62a7a68fddbc907f8a656ff8b5ec92bdf.

Full named evidence retained: 6 lines; /var/tmp/gc-gate.ga-99k1zdl6.qlwupvwb/final-by-name.txt; SHA256 91ca7f0a06bc5ebbac6f4e82201c7c69ab5102e469382b359c123045d0074520.

Full skip inventory retained: 5410 lines; /var/tmp/gc-gate.ga-99k1zdl6.qlwupvwb/all-skip-events.json; SHA256 7e127f4dd96a2d9842542b3c1338e4169b427ad51138242385663a06e97a41e3.

Full skip sites retained: 321 lines; /var/tmp/gc-gate.ga-99k1zdl6.qlwupvwb/skip-sites-audited.json; SHA256 bdc9baa562ebe4c3cc9feb95ca057c2a2426e691cbff48dd0b8c8dbca380c224.

Full native floor retained: 1056 lines; /var/tmp/gc-heavy-gate/runs/ga-99k1zdl6.c3/evidence/remote/floor-topup.json; SHA256 8fb8b82bbd3b2351307d6553eed483a168164a91561b4f2fc437b2eacce99e77.

Full Windows reach retained: 21 lines; /var/tmp/gc-gate.ga-99k1zdl6.qlwupvwb/accounting/windows-native-reach.json; SHA256 a5d23f73b0ec5051076b6c9999383bf532819550f4043e203e799bc53d4a83e0.
