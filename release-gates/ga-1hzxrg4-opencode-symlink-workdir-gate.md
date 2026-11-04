# OpenCode session lookup through symlinked work directories

**Verdict:** **PASS**

Deploy bead: ga-1hzxrg4; build: ga-bpqfyy; review: ga-g5e9p4g.
Assessed: 2026-11-01T03:03:53.412180+00:00.
Reviewed commit: `d8735915568f33d97f4b0493f8b987fbc75c5d34`.
Deploy branch: `deploy/ga-1hzxrg4-gate`; source branch is provenance only.
Gate base: `1e2224a99aa9ffcb5354cec3bf6862441ac05145`.
Merge scratch commit: `7878066f2a787113345e47283baa88299381b37f`; tested tree: `4023a8aa01413520f601a6a6ce759e0c8edf1540`.
The PR adds this record to the reviewed commit. Production and test changes are unchanged.

| # | Criterion | Result and evidence |
|---|---|---|
| 1 | Review PASS | PASS — ga-g5e9p4g is closed with PASS on the resolved reviewed commit. No review carryover or reviewer-added test amendment. |
| 2 | Acceptance criteria | PASS — OpenCode discovery compares through pathutil.SamePath; the manufactured-symlink regression passed by name; the ZCode by-ID site already uses SamePath; the complete sessionlog target passed in the unit suite. |
| 3 | Tests pass | PASS — all five forced-execution Bazel phases and all 16 local-floor steps passed. 137091 PASS, 0 FAIL, 485 SKIP result lines; owned test PASS with no skipped subtest. |
| 4 | No unresolved high-severity findings | PASS — reviewer reports no open HIGH findings. |
| 5 | Final branch clean | PASS — source checkout clean; this gate record is the sole deployment addition. A clean committed worktree is checked again before push. |
| 6 | Merge state and build/vet | PASS — reviewed source merged cleanly with the recorded base; full Bazel build including nogo succeeded on that merge. |
| 7 | Single feature theme | PASS — OpenCode session discovery only: opencode_reader.go and opencode_reader_test.go, plus this record. |

Evaluation order: preflight, 6, 1, 2, fast lane, full suite, 4, 5, 7. Preflight commit/pulls returned no existing PR. DEPLOY_MODE=remote; PUSH_REMOTE=fork (origin dry-run exited 1). The source ancestry scope guard passed for ga-1hzxrg4 and build ga-bpqfyy; no stack was found. The source was not rebased or force-pushed.

```text
merge_state: clean (origin/main@1e2224a99aa9)
merge_validation: merge result built and vetted here
gate_base_kind: current-tip
gate_base: 1e2224a99aa9ffcb5354cec3bf6862441ac05145
criterion-6-merge-validated: PASS
build_vet_cmd: bazel build --config=remote-exec --noremote_local_fallback //...
build_vet_result: 1448 targets; 22426 successful actions; 267.820 seconds
build_vet_log: /var/tmp/gc-deploy-ga-1hzxrg4.QhtLvM/c6.log
fast_lane: PASS before test environment setup and suite launch
policy_lane: PASS; log=/var/tmp/gc-deploy-ga-1hzxrg4.QhtLvM/policy.log (sha256=04525578c7e2451bf357d286783509441d390a3245b4fa97812521da771b1836, bytes=18529, lines=109)
FRESH_TREE_RECORD from=/var/tmp/gc-merge-validate.ga-1hzxrg4.QakfRB head=7878066f2a78 tree=4023a8aa0141 dir=/var/tmp/gc-fresh-tree.3HyHR0 modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-gc-merge-validate.ga-1hzxrg4.QakfRB,bazel-out,bazel-testlogs
GATE_BASE_RECORD base=1e2224a99aa9ffcb5354cec3bf6862441ac05145 knobs=CI_BEADS_DIFF_BASE,BASE_SHA,COMPLEXITY_BASE_REF,LINT_BASE dir=/var/tmp/gc-fresh-tree.3HyHR0
format_lane: PASS; log=/var/tmp/gc-deploy-ga-1hzxrg4.QhtLvM/format.log (sha256=30f030df9b0b8c44f44e02d3477e72f1824537c21527682f5235c0242f05a89d, bytes=5753, lines=50)
FRESH_TREE_RECORD from=/var/tmp/gc-merge-validate.ga-1hzxrg4.QakfRB head=7878066f2a78 tree=4023a8aa0141 dir=/var/tmp/gc-fresh-tree.s1Lrl9 modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-gc-merge-validate.ga-1hzxrg4.QakfRB,bazel-out,bazel-testlogs
drift_lane: PASS; log=/var/tmp/gc-deploy-ga-1hzxrg4.QhtLvM/drift.log (sha256=9da9bf5e8e7b14bd4526003115c95a6720d8447eb99046a08986f16b103c64fa, bytes=7054, lines=56)
FRESH_TREE_RECORD from=/var/tmp/gc-merge-validate.ga-1hzxrg4.QakfRB head=7878066f2a78 tree=4023a8aa0141 dir=/var/tmp/gc-fresh-tree.wi44C1 modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-gc-merge-validate.ga-1hzxrg4.QakfRB,bazel-out,bazel-testlogs
3c ci_lane_run: n/a (no CI configuration change)
heavy_mode: none
waiver_ref: none
failure_attribution: none (zero failures)
```

The first C6 build was stopped before a verdict to add missing generated RBE definitions to the scratch. Its log is retained as `c6-local-setup-aborted.log`; it is not test evidence. The corrected service is `fast-rbe`, completed with rc=0. Each post-C6 fast lane used its own fresh view; no lane wrote to the suite scratch. Build-file regeneration completed with an empty git diff. The repository has no docs/PROJECT_MANIFEST.md; the supplied release criteria and TESTING.md's Bazel lanes are the gate sources.

```text
test_cmd_scope: full-suite
test_counts: PASS=137091 FAIL=0 SKIP=485
test_log_dir: /var/tmp/gc-deploy-ga-1hzxrg4.QhtLvM/c3/evidence/remote/logs
diff_tests_executed: TestFindOpenCodeSessionFileResolvesSymlinkedWorkDir PASS (unit-eb757c3339d13fe4ad40.log)
test_bd: GASCITY_TEST_BD version=v1.3.2 bin=/home/jaword/.local/bd-versions/v1.3.2/bd reports='bd version 1.3.2 (9b65b9e19: HEAD@9b65b9e198ac)' ref_check=match:9b65b9e19 origin=preexisting pin_source=/var/tmp/gc-merge-validate.ga-1hzxrg4.QakfRB/deps.env displaces='bd version 1.3.2 (9b65b9e19)'
test_exec_mode: remote-exec
fallback_reason: None
admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 52.283485412597656, "waited_seconds": 0.001}
farm_test_seconds: 9511.274
rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98", "worker_env_tree": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"}
owned_topup: [] (owned test verified in the full suite)
floor_topup: complete=true; 16/16 steps exit 0
latency_leg: measured, first attempt, both listed tests PASS
GATE_FARM_PROBE verdict=clear waited=0s load1=47.42 load5=30.05 load15=18.49 cpus=32 threshold=50 max_wait=600s
remote_launch_load: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=45.23 run_max_load=42.90 run_mean_load=21.52 run_readings=43 wait_first_load=45.23 wait_max_load=45.23 wait_mean_load=45.23 wait_readings=1 read_errors=0
local_floor_load: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=13.96 run_max_load=18.90 run_mean_load=17.64 run_readings=7 wait_first_load=13.96 wait_max_load=13.96 wait_mean_load=13.96 wait_readings=1 read_errors=0
load_threshold: 15 (remote class bypasses host wait; applies to local floor)
load_waited_seconds: 0
load_wait_timed_out: 0
run_start_load: 45.23 (remote gate's submitting host)
run_max_load: 42.90
run_mean_load: 21.52
run_readings: 43
local_floor_run_start_load: 13.96
local_floor_run_max_load: 18.90
local_floor_run_mean_load: 17.64
local_floor_run_readings: 7
```

Counts are top-level result lines across 530 retained logs, not distinct test functions; repeated jobs count separately. The latency tests were excluded from integration-full and both passed in their dedicated clear-probe lane.

| Phase | BEP results | Fresh remote | Cached local/remote | Retries | Window valid |
|---|---:|---:|---|---:|---|
| unit | 389 | 389 | 0/0 | 0 | True |
| acceptance | 24 | 24 | 0/0 | 0 | True |
| integration-packages | 86 | 86 | 0/0 | 0 | True |
| integration-full | 14 | 14 | 0/0 | 0 | True |
| integration-latency | 1 | 1 | 0/0 | 0 | True |

Actual commands and flags:

```text
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-deploy-ga-1hzxrg4.QhtLvM/c3/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-deploy-ga-1hzxrg4.QhtLvM/c3/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-deploy-ga-1hzxrg4.QhtLvM/c3/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-deploy-ga-1hzxrg4.QhtLvM/c3/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-deploy-ga-1hzxrg4.QhtLvM/c3/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
unit: make check (bazel test $BAZEL_FLAGS --keep_going //...)
acceptance: bazel test $BAZEL_FLAGS --config=acceptance //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests
integration-packages: bazel test $BAZEL_FLAGS --config=integration //test:integration_packages
integration-full: bazel test $BAZEL_FLAGS --config=integration --test_arg=-test.v --test_arg=-test.skip=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$ //test/integration:integration_test
integration-latency: bazel test $BAZEL_FLAGS --config=integration --test_arg=-test.v --test_filter=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$ --test_sharding_strategy=disabled --run_under=<gate-farm-probe.sh threshold=50 max_wait=600 poll=10, full text sha256 in phase log> //test/integration:integration_test
```

Skip justification: the change owns exactly one test and it passed without a skip. The 485 SKIP result lines include inherited fast-unit exclusions (exercised again by the integration phase), macOS-only cases, unsupported provider contracts, subprocess helper cases, optional live/fixture checks and absent external backends. The remote inventory has 471 unique package/test rows (400 other, 71 environment-absence); every row retains its exact reason. All selected environment-absence top-ups ran by package/parent and all 16 floor steps exited 0. Remaining native skips name absent replay/coverage fixtures, live-herdr opt-ins, an ambient tmux server, br, optional Postgres DSN, and intentional conformance error fixtures; these are not changed tests. No new test was skipped or excused.

The seven skipped sessionlog tests are unchanged macOS Claude/Codex/Gemini alias tests, not OpenCode discovery. For each, diff-owned-tests --site reports line_in_hunk=no, func_touched=no, shared_hunks_in_file=0 and UNCHANGED. The complete --explain output has only the new OpenCode test's body hunk, no shared/helper hunk. The new Linux symlink regression directly exercises the changed path.

CI-required accounting:

```text
ci_required_closure: ci_required_triggers.py /var/tmp/gc-merge-validate.ga-1hzxrg4.QakfRB 1e2224a99aa9ffcb5354cec3bf6862441ac05145 d8735915568f33d97f4b0493f8b987fbc75c5d34 fork
workflow_blob: e5b11c348505dab28dc223e874bc2c61cbbb33c5
closure: 4 LEAF jobs need a status; check is an AGGREGATOR
runner-policy | LOCAL-PASS | EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=/var/tmp/gc-deploy-ga-1hzxrg4.QhtLvM/ci/runner-policy.out python3 .github/workflows/scripts/runner_policy.py; rc=0; log=/var/tmp/gc-deploy-ga-1hzxrg4.QhtLvM/ci/runner-policy.log
changes | LOCAL-PASS | ci_required_triggers.py above; rc=0; log=/var/tmp/gc-deploy-ga-1hzxrg4.QhtLvM/ci/closure.txt
credential-provider-windows | NOT-TRIGGERED | credential_provider filter no match; shared filter no match
pack-gate | NOT-TRIGGERED | packs filter no match; shared filter no match
windows_reach: none (windows_reach.sh exited 0, empty stdout)
deferred_to_pr_ci: none
not_covered: none
```

A gate PASS authorizes PR opening. Merge still requires CI on the current PR head and merge authority's decision.

Durable evidence (retained under a .gc-no-reap run root; inventories stay outside this record):

- /var/tmp/gc-deploy-ga-1hzxrg4.QhtLvM/c3/evidence/execution.json (sha256=eb402817730daf21b05e16b36ca36db7a74fecbe25a2a861814ae6f2bba77d57, bytes=249204, lines=4820)
- /var/tmp/gc-deploy-ga-1hzxrg4.QhtLvM/execution-compact.json (sha256=63a0b03eab7b38086d71c0702a330121b46cd63a35f8efec879a50e1caf360a5, bytes=4117, lines=147)
- /var/tmp/gc-deploy-ga-1hzxrg4.QhtLvM/c3/evidence/remote/skip-inventory.json (sha256=4b28be81c578f36752270393ad79accc65502334e64570fa10a074815f910dee, bytes=186541, lines=3770)
- /var/tmp/gc-deploy-ga-1hzxrg4.QhtLvM/c3/evidence/remote/floor-topup.json (sha256=9a52cccbfb568c6171b1428895040d3ce4e28ba7b1a7a5a084a7653d5b6b0cc2, bytes=39522, lines=901)
- /var/tmp/gc-deploy-ga-1hzxrg4.QhtLvM/c3/evidence/remote/latency-leg.json (sha256=adff30d6a2f90a03eade914e74344104d5f745f0ae3336e2f9dbe13da779dd2c, bytes=1695, lines=55)
- /var/tmp/gc-deploy-ga-1hzxrg4.QhtLvM/test-evidence.txt (sha256=c7de2d6136376f41b074cab2f3d20986eef1cf64a1624d5d194d95825bba6421, bytes=821, lines=7)
- /var/tmp/gc-deploy-ga-1hzxrg4.QhtLvM/owned-explain.txt (sha256=a36112087d437cb1e7907ce0c0cad3d4bf7ae3ed8e1b18ad8ee8c3cdf8e55008, bytes=974, lines=11)
- /var/tmp/gc-deploy-ga-1hzxrg4.QhtLvM/skip-safety.txt (sha256=d12bffbdc7d22fe2009dc00af398c4af35799b617ecc584ebdedb408d0f1dae7, bytes=2404, lines=28)
- /var/tmp/gc-deploy-ga-1hzxrg4.QhtLvM/icu74.txt (sha256=dc1ab2e8ccf94bcf9a0c827a085e548134cafef5ec8378acc25ae4c948c21196, bytes=616, lines=4)
- /var/tmp/gc-deploy-ga-1hzxrg4.QhtLvM/ci/closure.txt (sha256=6a139cf7241f848181956e5786ea9294efa322cf9fd6a1b42003c1d77272af38, bytes=1070, lines=15)
- /var/tmp/gc-deploy-ga-1hzxrg4.QhtLvM/evidence-manifest.tsv (sha256=a4e5439606eff5c50d976b3f1712f67887dfdaca05f4fe0a213770fcb25f0896, bytes=283441, lines=1629)
