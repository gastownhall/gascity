**Verdict:** **PASS**

# Release gate: separate heavy workladder simulations

Deploy bead: ga-7qaelp4i. Build: ga-56ib08y5. Review: ga-90439e21.

| # | Criterion | Result | Evidence |
| --- | --- | --- | --- |
| 1 | Review PASS present | PASS | Reviewer verdict and deploy source agree; full SHA resolved against git and current review notes verified. |
| 2 | Acceptance criteria met | PASS | Exact four-file spec tree verified. Placement guard rejects six shards and passes fresh at nine; all 77 original tests pass with seven heavy simulations alone. Historical and fresh timing text is prepared; delivery is checked before clearance. |
| 3 | Tests and required CI | PASS | Five fresh remote phases, complete native floor, all skips justified and all six required CI leaves accounted below. |
| 4 | No open HIGH findings | PASS | Reviewer style/security/spec verdicts have no open HIGH finding. |
| 5 | Final branch clean | PASS | Source and synthetic merge clean; publication requires and verifies a clean record-only commit above the reviewed source. |
| 6 | Merge state/build/vet | PASS | Clean synthetic merge at the frozen current-main base; whole-tree build and explicit nogo/vet terminal SUCCESS, with raw BEPs independently audited. |
| 7 | Single feature theme | PASS | One workladder sharding theme across its target, heavy-test manifest and scripts placement guard. No production logic or test-budget change. |

The pinned merge has no docs/PROJECT_MANIFEST.md. TESTING.md, current CI workflows, bead acceptance and the release protocol define the checks.

deploy_mode: remote
push_remote: fork
gate_head: f49577714c1ebc33448e810f6966c990755a1a82
gate_base: f2bb5cdc01d827f2de1275acdb81e7890a010813
synthetic_merge: c0b679e629fcebe1957a6157de581433dfbd7231
merge_tree: 1cc5c88cb88fbca9b7bbe6b14ea9c6f68c5041f2
merge_state: clean (origin/main@f2bb5cdc01d827f2de1275acdb81e7890a010813; files: none)
gate_base_kind: current-tip
merge_validation: MPR pins the cleared PR head and verifies required CI and merge-time checks
waiver_ref: none
failure_attribution: none

## Criterion 3 evidence

- test_cmd: /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-criterion3.sh --repo /var/tmp/gc-merge-validate.ns65io --base f2bb5cdc01d827f2de1275acdb81e7890a010813 --bead-json /var/tmp/gc-gate-ga-7qaelp4i-hb13qdir/bead-criterion3.json --owned /var/tmp/gc-gate-ga-7qaelp4i-hb13qdir/owned.tsv --run-dir /var/tmp/gc-gate-ga-7qaelp4i-hb13qdir/c3-run/evidence
- test_cmd_scope: full-suite
- phase_command_receipts: ["test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-7qaelp4i-hb13qdir/c3-run/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec","test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-7qaelp4i-hb13qdir/c3-run/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec","test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-7qaelp4i-hb13qdir/c3-run/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec","test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-7qaelp4i-hb13qdir/c3-run/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec","test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-7qaelp4i-hb13qdir/c3-run/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec"]
- test_counts: {"PASS":158179,"FAIL":0,"SKIP":617}
- diff_tests_executed: TestWorkladderShardsKeepHeavyTestsApart PASS (unit-81d8a9af827e251f6b12.log)
- test_exec_mode: remote-exec
- heavy_mode: none
- test_exec_bep: {"audit":"/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-7qaelp4i/gate-20261121-kjdv5skc/evidence/five-phase-raw-audit.json (SHA256 fe307517084049546a5b40d02e3950b1b4d2fece48273d60a52ffdc4969b5399)","bep_paths":{"unit":"/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-7qaelp4i/gate-20261121-kjdv5skc/evidence/c3-run/evidence/remote/unit.bep.json","acceptance":"/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-7qaelp4i/gate-20261121-kjdv5skc/evidence/c3-run/evidence/remote/acceptance.bep.json","integration-packages":"/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-7qaelp4i/gate-20261121-kjdv5skc/evidence/c3-run/evidence/remote/integration-packages.bep.json","integration-full":"/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-7qaelp4i/gate-20261121-kjdv5skc/evidence/c3-run/evidence/remote/integration-full.bep.json","integration-latency":"/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-7qaelp4i/gate-20261121-kjdv5skc/evidence/c3-run/evidence/remote/integration-latency.bep.json"}}
- rbe_state: {"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"}
- fallback_reason: none
- admission: {"class":"remote","floor_gib":25.0,"free_gib":67.56633758544922,"waited_seconds":0.001,"cap":2}
- farm_test_seconds: 15177.795
- delta_gate: {"mode":"shadow","decision":"full","reason":"not-gate-pr","applied":false}
- owned_topup: []
- floor_topup: 16 steps, all exit0; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-7qaelp4i/gate-20261121-kjdv5skc/evidence/c3-run/evidence/remote/floor-topup.json (SHA256 a11cfce9573fcc9d330d3733e30026ee0d46a00dfb89bf1a33e7238c36b15aee)
- skip_inventory: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-7qaelp4i/gate-20261121-kjdv5skc/evidence/skip-independent-audit.json (SHA256 b3d7adf0bf1de8f089d8e8efc565b77cc5001d4c86173dc3d4d45843e7913932)
- latency_leg: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-7qaelp4i/gate-20261121-kjdv5skc/evidence/c3-run/evidence/remote/latency-leg.json (SHA256 d2279cba668a41cc5b5a44087d5f4364d20329317a53f7864971a9e3335e5576)
- test_log_dir: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-7qaelp4i/gate-20261121-kjdv5skc/evidence/c3-run/evidence/remote/logs
- policy_lane: Format, lint/nogo, policy guards and docsync PASS in independent fresh views; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-7qaelp4i/gate-20261121-kjdv5skc/evidence/fast-independent-audit.json (SHA256 3cf74f4e85ce3e08504186a4259c4b3211f6227f2f4790e31db2062f586d1d87)
- drift_lane: make bazel-sync, git diff --exit-code and empty untracked status; generated artifacts and OpenAPI checks PASS
- complexity: Zero growth at the pinned base, baseline untouched; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-7qaelp4i/gate-20261121-kjdv5skc/evidence/complexity-zero-growth.json (SHA256 caba154ed4c77b3c89e4e11694eaa9127413b69736504b29cd52324ec6150ab1)
- ci_lane_run: n/a (no workflow job/matrix/timeout/required-list change); changed Bazel shard layout executed in the fresh full unit phase

| Phase | Fresh remote actions | Cached/local/retried |
| --- | ---: | --- |
| unit | 425 | 0/0/0 |
| acceptance | 25 | 0/0/0 |
| integration-packages | 87 | 0/0/0 |
| integration-full | 14 | 0/0/0 |
| integration-latency | 1 | 0/0/0 |

552 fresh remote actions, 1104 raw log/XML artifact digests and lengths verified. Every result ran on attempt1 inside its phase window. Counts are top-level results summed over jobs, not distinct test names. Latency budgets were measured only after a clear farm-load probe.

The native floor attempted every environment-absence case, beadstest, Darwin compilation and runner self-tests. Podman socket, pinned image, active container sweep, ICU74 loader and disk were probed before launch. Complete execution: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-7qaelp4i/gate-20261121-kjdv5skc/evidence/c3-run/evidence/execution.json (SHA256 33fc9e7cba23907b01f03894cc183e3227166b4087c9caba198431864b4dba57).

- load_threshold: 15
- load_waited_seconds: 1801
- load_wait_timed_out: 1
- run_start_load: 24.15
- run_max_load: 26.49
- run_mean_load: 25.61
- run_readings: 8

LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=24.15 run_max_load=26.49 run_mean_load=25.61 run_readings=8 wait_first_load=21.50 wait_max_load=33.41 wait_mean_load=25.30 wait_readings=61 read_errors=0

## Skip and supplemental-lane evidence

675 all-depth SKIP events at 317 sites, owned SKIP0 and unresolved0. Every site is UNCHANGED and every guard-file blob is identical at BASE and the merge. The exact four-file diff changes workladder test sharding/data and adds a scripts_test placement guard with a passive test-only descriptor; production Go and existing helpers are unchanged. No SKIP belongs to either affected test target. Docs sync passed without SKIPs. The remaining guards select platform/privilege, opt-in environments, unit/process tiers or deliberate fixtures; environment-absence cases were attempted natively.

base_comparison_required: false — clause 3 has no shared/helper-hunk trigger; unchanged-site and identical-blob proofs are retained.
skip_guard_matrix_sha256: 22e0e891a053295e09585375caf2d1dfef1203f990940e05b22980396c1f3579
manual_source_review_pending: false

The actual bazel.yml split-strict path case matches none of the four source paths or this gate record, so its supplemental manual lane is not required. Scope: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-7qaelp4i/gate-20261121-kjdv5skc/evidence/supplemental-lane-scope.json (SHA256 40c388d26c62e5b42aa3e92dfb20727ffc4503a3efb81dc24a5a23574ef4ccf0).

## Required CI accounting

ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-validate.ns65io f2bb5cdc01d827f2de1275acdb81e7890a010813 f49577714c1ebc33448e810f6966c990755a1a82 fork; workflow blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs need a status.

ci_required_accounting:

- runner-policy | LOCAL-PASS | /home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-merge-validate.ns65io -- /home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh --threshold 15 --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- bash /var/tmp/gc-gate-ga-7qaelp4i-hb13qdir/ci-runner-policy-v3.sh; exit0; /var/tmp/gc-gate-ga-7qaelp4i-hb13qdir/ci-required-v3/runner-policy-output.txt
  LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=41.84 run_max_load=NA run_mean_load=NA run_readings=0 wait_first_load=25.53 wait_max_load=79.07 wait_mean_load=55.97 wait_readings=61 read_errors=0
- windows-lane | LOCAL-PASS | /home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-merge-validate.ns65io -- /home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh --threshold 15 --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- bash /var/tmp/gc-gate-ga-7qaelp4i-hb13qdir/ci-windows-lane.sh; exit0; /var/tmp/gc-gate-ga-7qaelp4i-hb13qdir/ci-required-v3/windows-lane-output.txt
  LOAD_GATE_SUMMARY threshold=15 waited_seconds=660 wait_timed_out=0 run_start_load=14.66 run_max_load=15.42 run_mean_load=15.06 run_readings=3 wait_first_load=42.55 wait_max_load=53.69 wait_mean_load=33.28 wait_readings=23 read_errors=0
- credential-provider-windows | NOT-TRIGGERED |   credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'
- pack-gate | NOT-TRIGGERED |   pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'
- windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner |   windows-lane-test                      LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} EVERY-PR
- changes | LOCAL-PASS | python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-validate.ns65io f2bb5cdc01d827f2de1275acdb81e7890a010813 f49577714c1ebc33448e810f6966c990755a1a82 fork; exit0; /var/tmp/gc-gate-ga-7qaelp4i-hb13qdir/ci-required-v3/closure.txt

windows_reach: credential-provider-windows none; windows-lane-test none (its own allowlist and ordinary/integration Windows import closures).
deferred_to_pr_ci: [{"job": "windows-lane-test", "reason": "non-linux-runner"}]
not_covered: []
host_gaps: []

PASS authorizes opening the PR. MPR must verify CI / required and each deferred native job on the current PR head before merging.

## Retained evidence

Verified archive: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-7qaelp4i/gate-20261121-kjdv5skc; manifest SHA256 831004c0cb71521f9ca9699e83e0e6391e313ea71e34c057ee5440cc24910f81; tested merge bundle SHA256 a11fd2872760aacf2f58581cc82b9aee46817103519697fd2675145f43a80346.
Source, review, acceptance, raw build/vet/test receipts, per-job logs and policy/drift proofs are retained there. The publication archive separately records the final record-only commit, pushed PR head, clearance and verified handoff.


## Workladder acceptance evidence

Exact spec application and source-order proof: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-7qaelp4i/gate-20261121-kjdv5skc/evidence/acceptance-code-audit.json (SHA256 7ed93dc0b123f546d2abd7687fd64beebde7760fc3307ff1ca327a79044970db).
Fresh canonical unit results: 77 original top-level tests passed once each across nine shards. Maximum shard action duration 119.698 seconds. Seven heavy simulations occupy seven distinct predicted shards; medium timeout stays 300 seconds.
Guard at nine: canonical unit PASS by name. Guard at six: independent expected RED, Bazel exit3, exact 488-byte log digest verified against its BEP. The first post-run verifier mishandled an omitted false protobuf field; raw BEP/log re-verification resolved it without any test rerun. The original verifier and error are retained. This expected mutant failure is outside the canonical suite: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-7qaelp4i/gate-20261121-kjdv5skc/evidence/guard-red-verified.json (SHA256 40f639f11a4ce442b1677fbc7ca3c3b60ee4481a6b28de56fa267cabd17a8375).

| Shard | Heavy simulation | Simulation seconds | Action seconds |
| --- | --- | ---: | ---: |
| 1 | TestNoDrainWave | 10.62 | 10.778 |
| 2 | TestPropertyAWaitIsOnlyNudged | 39.02 | 39.203 |
| 3 | TestPropertyNoNudgeWhileMarked | 119.49 | 119.698 |
| 4 | TestRateLimitInvariants | 61.46 | 61.59 |
| 5 | TestRateLimitInvariantsAcrossSeats | 63.45 | 63.587 |
| 6 | none of the seven listed simulations | - | 8.845 |
| 7 | none of the seven listed simulations | - | 0.231 |
| 8 | TestPropertyProgressNeedsService | 115.22 | 115.385 |
| 9 | TestLadderInvariants | 114.31 | 114.609 |

Detailed fresh placement/log proof: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-7qaelp4i/gate-20261121-kjdv5skc/evidence/workladder-independent-audit.json (SHA256 f1dbd5bf0b2c661b70f6a6451e05f07dbbdbf610574ca4125ab0725f231b194e).
Historical remote sample: 99 complete6-shard runs, 2026-11-04 through2026-11-20, heaviest-shard median189s/p90 223s/max294s, plus two300s timeouts. Replaying the same per-test timings at9 shards gives median107s/p90 130s/max223s; these are estimates, not fresh executions. Eleven local6-shard runs: median229s/max259s by per-test sums, with BEP durations294/297s. An earlier measured9-shard remote run had max151.3s. The PR body distinguishes these historical/replay measurements from THIS gate fresh per-shard times.
PR timing body prepared and checked before publication (SHA256 825a924b79488c9344ea487ecd86483feebcfadb9554c65008cadefa78253b60); actual GitHub body delivery is verified before clearance/handoff.
Tracker ga-9yad1bw0 stays open until this fix lands. Then ga-5lydhrdn and ga-4d9tvafd need fresh re-gates; no tracker close is claimed here.
