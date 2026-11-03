**Verdict:** **PASS**

# Native systemd drift fixture — ga-6dpyh1q

Gate PASS for PR opening, with the unchanged base's routed-row guard attributed under criteria 3a/3b. Raw runner status 79 and `criterion3_valid=false` remain intact: `criterion3_valid_if_attributed=true` proves every other requirement completed. No waiver is used.

- Build ga-m6wgyi7; review ga-ix76oxq PASS; reviewed source `3547d76f90dcf89d3a69e93e1cefa3862cd7985a`. Provenance builder/ga-m6wgyi7 is never a push target.
- deploy_mode: remote; push_remote: origin (fresh dry-run and ordinary push succeeded); base_ref: origin/main; isolated branch: deploy/ga-6dpyh1q-gate.
- gate_base: `2cf1a9d4ed31016ea14d4c436259898b9a5c3baa`; gate_base_kind: current-tip (frozen at the completed criterion-6 evaluation).
- merge_state: clean (origin/main@2cf1a9d4ed31016ea14d4c436259898b9a5c3baa).
- merge_validation: canonical preserved merge `a2d06873cdacc20089ac2c68d14ebbafa8e40f47` / tree `5e0a436f015f8c281293f79c4148822788ef7a66` exactly matches git merge-tree and was built/vetted. No self-rebase.
- Fresh criterion-3 pack: `11c09134c3000621b25264c1219cabaeb58ceb7f`; landing notice gcg-259073 independently verified. Earlier incomplete criterion 3 receives no test credit.
- Fresh evidence: `/var/tmp/deploy-ga-6dpyh1q-c3fresh.nu09_myt`. Retained C6/fast/complexity evidence: `/var/tmp/deploy-ga-6dpyh1q.Pcf3yFnQ`; their digests are in fresh context.json. PROJECT_MANIFEST.md is absent; deployed release criteria, TESTING.md, Makefile and merged CI define this gate.
- Evaluated: 2026-11-03T23:06:19.642494+00:00.

| # | Result | Evidence |
|---|---|---|
| 1 | PASS | Resolved reviewed SHA matches ga-ix76oxq's final corrected PASS. No carryover or reviewer-added tests. |
| 2 | PASS for PR opening | Fixture-only unit mirror uses the same definition in runtime and isolated HOME. Full owning tiers pass; canonical native managed restart PASS in38.29s, restart5.7s within unchanged15s, with OLD/NEW build identities, systemd-managed branch and /health asserted. Existing safety owners run in the full tiers. Complexity has182 identical base/merge discrepancy rows and no threshold change. Criterion8's actual-main landing and native-main PASS remain an explicit post-merge condition below. |
| 3 | PASS with attributed base policy failure | Five complete remote phases:514 fresh PASSED packets; no cached/retried results. All16 native floor steps exit0. Sole pending guard is independently reproduced at the pinned base and attributed below; all four clauses and fix-carrying exception hold. |
| 4 | PASS | No unresolved HIGH/security finding. Review's fixture-write cleanup advisory remains nonblocking; no scope added. |
| 5 | PASS | Isolated branch was checked clean at the reviewed source before adding this record. Publication checks require a clean committed branch. |
| 6 | PASS | Preserved clean merge built with `bazel build --config=remote-exec --noremote_local_fallback //...` including nogo; criterion6-remote-output.log terminal rc0. |
| 7 | PASS | One fixture helper in test/integration/start_drift_test.go, +14/-3. Current ancestry guard accepts only deploy/build IDs; no stack or internal configuration paths. |

## Fast lane and policy attribution

format_lane: PASS — `bazel test --config=remote-exec --noremote_local_fallback //:go_format_test`, the canonical pinned formatter equivalent. Cache here is allowed and is never counted as fresh criterion-3 test evidence.
policy_lane: `make lint check-release-dist-ignore check-routed-test-rows check-split-topology-rows check-residency-boundary check-hooks`. Nogo, ignore guard, remaining guards and hook ownership PASS; only routed rows is attributed. Original fast-lane-bazel-output.log plus fast-remainder-output.log retain the independent fresh views.
drift_lane: PASS — `make bazel-sync BAZEL_SYNC_FLAGS=--config=remote-exec`, `git diff --exit-code` and empty untracked check in a separate fresh view. Warnings retained; no generated drift or baseline edits.

policy_attribution: check-routed-test-rows -> ga-lpaujfv. This FRESH run's candidate and untouched frozen base both exit2 with the identical INCOMPLETE diagnostics for cmd_rig_list_hq_running_test.go and cmd_service_test.go. Checker, manifest and both inputs have matching base/merge blobs; see fresh routed-rows-proof.json and candidate-routed-rows.log/base-routed-rows.log. This static checker scans source; the diff only changes a test/integration fixture helper and cannot alter those inputs. No self-test masks later steps.

Clauses1/4: checker and offending paths untouched, with no census/target/test-body growth. Clause2: opened ga-lpaujfv, predating this run; appended and read back the sighting. Clause3(d): identical same-run untouched-base reproduction. Repeat exception: own build ga-m6wgyi7 carries `gc.fixes_tracker=ga-ltzlhr0`; designated policy repair ga-x8ux906 carries `gc.fixes_tracker=ga-lpaujfv` and a refreshed canonical landing check remains NOT-LANDED. This attribution neither waives a test nor closes the main policy tracker.

The canonical unit command was `make -k check`. It reported make_rc2, failed1, unparsed0, attributable=true; its sole target is check-routed-test-rows/Error1. GATE_PHASE predicate_rc79 preserved the failure and still ran the remaining phases. Final `pending_attribution` has exactly that one row; `criterion3_valid_if_attributed=true`. Agent adjudication accepts that row under the proof above; raw execution.json is unchanged.

## Fresh suite evidence

test_cmd_scope: full-suite. The unmodified canonical gate-criterion3.sh ran in a durable service; full remote suite plus canonical native floor, with no local restart or farm/local stitching. Phase commands are make-k-check, acceptance including solo tests, all integration packages, full integration minus the canonical latency registry, then only that registry. Actual flags as emitted:

~~~text
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-6dpyh1q-c3fresh.nu09_myt/c3-run/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-6dpyh1q-c3fresh.nu09_myt/c3-run/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-6dpyh1q-c3fresh.nu09_myt/c3-run/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-6dpyh1q-c3fresh.nu09_myt/c3-run/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-6dpyh1q-c3fresh.nu09_myt/c3-run/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
~~~

| Phase | Result packets | Remote executed | Cached local / remote / retried | Window valid |
|---|---:|---:|---|---|
| unit | 389 | 389 | 0 / 0 / 0 | yes |
| acceptance | 24 | 24 | 0 / 0 / 0 | yes |
| integration-packages | 86 | 86 | 0 / 0 / 0 | yes |
| integration-full | 14 | 14 | 0 / 0 / 0 | yes |
| integration-latency | 1 | 1 | 0 / 0 / 0 | yes |

Every packet PASSED; each BEP is under `/var/tmp/deploy-ga-6dpyh1q-c3fresh.nu09_myt/c3-run/evidence/remote/<phase>.bep.json`. Raw artifact hashes independently verified:1028 log/XML files across the five manifests; see terminal-verification.json. test_exec_mode: remote-exec; fallback_reason: none; farm_test_seconds: 10157.311.
test_counts:143740 PASS /0 FAIL /482 SKIP — top-level Go result lines over530 retained logs, one count per job execution, excluding subtests. By-name reader: by-name-summary.txt. diff_tests_executed: none (helper/shared hunk only; ownership was recomputed mechanically). Required affected owner `TestStartDrift_SystemdManaged_RestartsToNewBuildID` reports a real native PASS in remote/floor-logs/floor-12.log.
test_log_dir: `/var/tmp/deploy-ga-6dpyh1q-c3fresh.nu09_myt/c3-run/evidence/remote/logs`. All logs retained. Non-Go/compile/self-test logs are distinguished by the reader and BEP records.

skip_inventory: remote/skip-inventory.json,468 rows (71 environment-absence,397 other). These are non-owned conditional skips; canonical floor reruns every environment-absence selection by package/parent. Remaining native conditional/intentional skips are recorded in the floor logs. Unrelated unit fast-tier skips are covered by integration-packages; the affected systemd SKIP is replaced by the actual native PASS. The changed shared helper is reached by that case; it cannot affect unrelated skip conditions.
owned_topup: none. floor_topup: complete,16/16 exit0 —13 native package steps plus Darwin compile, push-gate lock and concurrency self-tests. No floor failure retried. Native fixture descriptions have no remaining runtime unit files or loaded units; its already-stopped cleanup message is retained in the native log.
latency_leg: complete, first attempt; both budget tests PASS under clear farm probe (load1=33.39,32 CPUs, threshold50); no latency test ran in the main leg.
heavy_mode: none. admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 85.44101333618164, "waited_seconds": 0.001}.
rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98", "worker_env_tree": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"}.
load_threshold:15; native floor load_waited_seconds:0; load_wait_timed_out:0; run_start_load:7.79; run_max_load:10.44; run_mean_load:9.26; run_readings:7. Remote phases use remote admission without host load wait. ICU74 loader OK, recorded provenance; rootless Podman/socket and exact deps.env Dolt image confirmed, testcontainer-sweep enabled. Wrapper reported PASS-THROUGH/NOT ISOLATED; no clean-environment claim.
ci_lane_run:n/a (no CI-config change). waiver_ref:none (gascity has no waiver path).

## ci-required accounting

ci_required_closure: ci_required_triggers.py /var/tmp/gc-merge-validate.0b93oy 2cf1a9d4ed31016ea14d4c436259898b9a5c3baa 3547d76f90dcf89d3a69e93e1cefa3862cd7985a origin; workflow blob `e5b11c348505dab28dc223e874bc2c61cbbb33c5`;4 LEAF jobs.

~~~text
diff 2cf1a9d4ed31016ea14d4c436259898b9a5c3baa...3547d76f90dcf89d3a69e93e1cefa3862cd7985a: 1 changed files
  filter packs                no match
  filter credential_provider  no match
  filter shared               no match
suite_mode: filtered  (shared filter did not match)

ci-required needs 5 jobs; transitive closure 5 jobs
job key                                  kind       runs-on                    trigger
runner-policy                            LEAF       linux                      EVERY-PR
changes                                  LEAF       linux                      EVERY-PR
check                                    AGGREGATOR linux                      EVERY-PR
  credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'
  pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'

4 LEAF jobs need a status; the rest are rolled up through their needs.
~~~

- changes | LOCAL-PASS | closure tool exit0, ci-required-closure.txt.
- runner-policy | LOCAL-PASS | exact job command/env EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=<temp> python3 .github/workflows/scripts/runner_policy.py; detached fresh-tree/load-gated/wrapped run exit0, runner-policy-output.log.
- credential-provider-windows | NOT-TRIGGERED | tool row above; windows_reach:none, tool exit0.
- pack-gate | NOT-TRIGGERED | tool row above.

deferred_to_pr_ci:none; not_covered:none. Detailed status records: ci-accounting.json. This PASS authorizes PR opening; required PR CI still governs merge.

## Delivery boundary

ga-9sx8d3m was closed by its owner after gm-40usi4 actually landed on gc-management main. This corrected-runner release does not close any product tracker. The fixture repair must still land on gascity origin/main, then the canonical native owner must independently PASS on that landed main before the tracker owner may resolve ga-ltzlhr0. ga-rsqucqa needs its own fresh gate after that; ga-lpaujfv remains open until its actual repair lands and its guard passes on fresh main. Neither a review/deploy close nor this pre-merge native PASS releases those conditions. Merge authority remains MPR's.
