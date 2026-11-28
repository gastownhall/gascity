**Verdict:** **PASS**

# Release gate: routing targets and routing-event metadata

Deploy bead: ga-8vybo4rz. Build: ga-vjvpgj. Review: ga-288l8gw0.

| # | Criterion | Result | Evidence |
| --- | --- | --- | --- |
| 1 | Review PASS present | PASS | Reviewer verdict and deploy source agree; full SHA resolved against git and current review notes verified. |
| 2 | Acceptance criteria met | PASS | The architect’s sentence follows the sole-routing-key rule verbatim modulo code spans and wrapping. It distinguishes event metadata from routing targets and preserves the narrow execution-route guard. One engineering-doc hunk only. |
| 3 | Tests and required CI | PASS | Five fresh remote phases, complete native floor, all skips justified and all six required CI leaves accounted below. |
| 4 | No open HIGH findings | PASS | Reviewer style/security/spec verdicts have no open HIGH finding. |
| 5 | Final branch clean | PASS | Source and synthetic merge clean; publication requires and verifies a clean record-only commit above the reviewed source. |
| 6 | Merge state/build/vet | PASS | Clean synthetic merge at the frozen current-main base; whole-tree build and explicit nogo/vet terminal SUCCESS, with raw BEPs independently audited. |
| 7 | Single feature theme | PASS | One clarification in engdocs/design/session-model-unification.md; no code, config, build or dependency change. |

The pinned merge has no docs/PROJECT_MANIFEST.md. TESTING.md, current CI workflows, bead acceptance and the release protocol define the checks.

deploy_mode: remote
push_remote: fork
gate_head: 2678e5edbbaa59c7899ceb11b5fb3fa7034546d5
gate_base: 86032f5860542dd1f40e54772efa9ecac05e0601
synthetic_merge: 4cbd4d184424d1dd31309ff8f399882a977c7b0a
merge_tree: 02fcc82a9833ec01c77fae273818327a1be51385
merge_state: clean (origin/main@86032f5860542dd1f40e54772efa9ecac05e0601; files: none)
gate_base_kind: current-tip
merge_validation: MPR pins the cleared PR head and verifies required CI and merge-time checks
waiver_ref: none
failure_attribution: none

## Criterion 3 evidence

- test_cmd: /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-criterion3.sh --repo /var/tmp/gc-merge-validate.YlTogY --base 86032f5860542dd1f40e54772efa9ecac05e0601 --bead-json /var/tmp/gc-gate-ga-8vybo4rz-3ibjruy8/bead-criterion3.json --owned /var/tmp/gc-gate-ga-8vybo4rz-3ibjruy8/owned.tsv --run-dir /var/tmp/gc-gate-ga-8vybo4rz-3ibjruy8/c3-run/evidence
- test_cmd_scope: full-suite
- phase_command_receipts: ["test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-8vybo4rz-3ibjruy8/c3-run/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec","test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-8vybo4rz-3ibjruy8/c3-run/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec","test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-8vybo4rz-3ibjruy8/c3-run/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec","test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-8vybo4rz-3ibjruy8/c3-run/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec","test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-8vybo4rz-3ibjruy8/c3-run/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec"]
- test_counts: {"PASS":158073,"FAIL":0,"SKIP":617}
- diff_tests_executed: none (no diff-owned tests; tool exits0 and reports no Go/shared/helper hunk)
- test_exec_mode: remote-exec
- heavy_mode: none
- test_exec_bep: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-8vybo4rz/gate-20261121-mu58_tcr/evidence/five-phase-raw-audit.json (SHA256 8c47da9a157e1a28b4431c00ee583d66152084d3a8bf02c24c845083c3c8a9b1)
- rbe_state: {"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"}
- fallback_reason: none
- admission: {"class":"remote","floor_gib":25.0,"free_gib":62.046241760253906,"waited_seconds":0.001,"cap":2}
- farm_test_seconds: 13910.015
- delta_gate: {"mode":"shadow","decision":"full","reason":"not-gate-pr","applied":false}
- owned_topup: []
- floor_topup: 16 steps, all exit0; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-8vybo4rz/gate-20261121-mu58_tcr/evidence/c3-run/evidence/remote/floor-topup.json (SHA256 b06650467a74b7ac216c136331c21ac1da63966770dd24b4d49f1a3490581145)
- skip_inventory: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-8vybo4rz/gate-20261121-mu58_tcr/evidence/skip-independent-audit.json (SHA256 b7f0637bc2268a367e4b84745992d899e8c93ff607842bc1e78bb49cfd177a8a)
- latency_leg: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-8vybo4rz/gate-20261121-mu58_tcr/evidence/c3-run/evidence/remote/latency-leg.json (SHA256 a7cff9097d2a9efd4e57b16ebbdb165d7a49217e34024348d3dcd3f1fd1b0b48)
- test_log_dir: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-8vybo4rz/gate-20261121-mu58_tcr/evidence/c3-run/evidence/remote/logs
- policy_lane: Format, lint/nogo, policy guards and docsync PASS in independent fresh views; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-8vybo4rz/gate-20261121-mu58_tcr/evidence/fast-independent-audit.json (SHA256 888a0c558d0a0dbad56d316feff4ab892d696adece06b33b464280937b83c201)
- drift_lane: make bazel-sync, git diff --exit-code and empty untracked status; generated artifacts and OpenAPI checks PASS
- complexity: Zero growth at the pinned base, baseline untouched; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-8vybo4rz/gate-20261121-mu58_tcr/evidence/complexity-zero-growth.json (SHA256 ccedff2a9861156ecb1ceeb32d8287032bad9d1d39a5b15e3cc19265a3c09d7e)
- ci_lane_run: n/a (no CI-config change in this diff)

| Phase | Fresh remote actions | Cached/local/retried |
| --- | ---: | --- |
| unit | 422 | 0/0/0 |
| acceptance | 25 | 0/0/0 |
| integration-packages | 87 | 0/0/0 |
| integration-full | 14 | 0/0/0 |
| integration-latency | 1 | 0/0/0 |

549 fresh remote actions, 1098 raw log/XML artifact digests and lengths verified. Every result ran on attempt1 inside its phase window. Counts are top-level results summed over jobs, not distinct test names. Latency budgets were measured only after a clear farm-load probe.

The native floor attempted every environment-absence case, beadstest, Darwin compilation and runner self-tests. Podman socket, pinned image, active container sweep, ICU74 loader and disk were probed before launch. Complete execution: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-8vybo4rz/gate-20261121-mu58_tcr/evidence/c3-run/evidence/execution.json (SHA256 eb0ba2089341e2944757f3606431fbe0dffeb25cd147195bacfb265788632086).

- load_threshold: 15
- load_waited_seconds: 0
- load_wait_timed_out: 0
- run_start_load: 8.69
- run_max_load: 11.79
- run_mean_load: 11.35
- run_readings: 7

LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=8.69 run_max_load=11.79 run_mean_load=11.35 run_readings=7 wait_first_load=8.69 wait_max_load=8.69 wait_mean_load=8.69 wait_readings=1 read_errors=0

## Skip and supplemental-lane evidence

675 all-depth SKIP events at 317 sites, owned SKIP0 and unresolved0. Every site is UNCHANGED and every guard-file blob is identical at BASE and the merge. The diff has no Go, shared/helper, initializer, build or dependency hunk. Direct Go referrers to the document are comments, and the generic docsync consumer passed without SKIPs. The remaining guards select platform/privilege, opt-in environments, unit/process tiers or deliberate fixtures; environment-absence cases were attempted natively.

base_comparison_required: false — clause 3 has no shared/helper-hunk trigger; unchanged-site and identical-blob proofs are retained.
skip_guard_matrix_sha256: dc5945f27b59e895e34658ebf6329977abc7595f8726b53b0292702235f41c4b
manual_source_review_pending: false

The actual bazel.yml split-strict path case matches neither the engineering-doc path nor this gate record, so its supplemental manual lane is not required. Scope: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-8vybo4rz/gate-20261121-mu58_tcr/evidence/supplemental-lane-scope.json (SHA256 ed50d3ff9f61c7ea7fde2acb9275eb12e202491c4d2bf6ffc03bf7d570605252).

## Required CI accounting

ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-validate.YlTogY 86032f5860542dd1f40e54772efa9ecac05e0601 2678e5edbbaa59c7899ceb11b5fb3fa7034546d5 fork; workflow blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs need a status.

ci_required_accounting:

- runner-policy | LOCAL-PASS | /home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-merge-validate.YlTogY -- /home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh --threshold 15 --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- bash /var/tmp/gc-gate-ga-8vybo4rz-3ibjruy8/ci-runner-policy-v3.sh; exit0; /var/tmp/gc-gate-ga-8vybo4rz-3ibjruy8/ci-required-v3/runner-policy-output.txt
  LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=11.24 run_max_load=NA run_mean_load=NA run_readings=0 wait_first_load=11.24 wait_max_load=11.24 wait_mean_load=11.24 wait_readings=1 read_errors=0
- windows-lane | LOCAL-PASS | /home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-merge-validate.YlTogY -- /home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh --threshold 15 --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- bash /var/tmp/gc-gate-ga-8vybo4rz-3ibjruy8/ci-windows-lane.sh; exit0; /var/tmp/gc-gate-ga-8vybo4rz-3ibjruy8/ci-required-v3/windows-lane-output.txt
  LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=10.89 run_max_load=15.74 run_mean_load=14.03 run_readings=3 wait_first_load=10.89 wait_max_load=10.89 wait_mean_load=10.89 wait_readings=1 read_errors=0
- credential-provider-windows | NOT-TRIGGERED |   credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'
- pack-gate | NOT-TRIGGERED |   pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'
- windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner |   windows-lane-test                      LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} EVERY-PR
- changes | LOCAL-PASS | python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-validate.YlTogY 86032f5860542dd1f40e54772efa9ecac05e0601 2678e5edbbaa59c7899ceb11b5fb3fa7034546d5 fork; exit0; /var/tmp/gc-gate-ga-8vybo4rz-3ibjruy8/ci-required-v3/closure.txt

windows_reach: credential-provider-windows none; windows-lane-test none (its own allowlist and ordinary/integration Windows import closures).
deferred_to_pr_ci: [{"job": "windows-lane-test", "reason": "non-linux-runner"}]
not_covered: []
host_gaps: []

PASS authorizes opening the PR. MPR must verify CI / required and each deferred native job on the current PR head before merging.

## Retained evidence

Verified archive: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-8vybo4rz/gate-20261121-mu58_tcr; manifest SHA256 380d5c0360d1415fc0f5f3c21b6c2aed671cc02b2717173bd1114656ea02d2ea; tested merge bundle SHA256 ff007d9410f4a80723f21d730508ada22275458eaaab15fb9651d415225aacfb.
Source, review, acceptance, raw build/vet/test receipts, per-job logs and policy/drift proofs are retained there. The publication archive separately records the final record-only commit, pushed PR head, clearance and verified handoff.
