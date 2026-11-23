# Runtimecontract test sharding release gate

**Verdict:** **PASS**

Evaluated 2026-11-19T11:25:28.648906+00:00. Deploy bead ga-7jshilei; build ga-1mzalylv; review ga-51619jkp.

- deploy_mode: remote; push_remote: fork; base_branch: main
- reviewed_commit: dd6f82055319e64e0167e0d35f1ddfa7cc8564b9
- gate_base: 1feaf7eeb78b24ad578f2ec2cbf343c496656a0c
- gate_head: dd6f82055319e64e0167e0d35f1ddfa7cc8564b9
- merge_head: d5560ff9ea360ca9590d33092f2ecf083b4359da
- tested_tree: c99eb0c51a7956db76f52f2efa0c2a71547b9713 (the reviewed source and materialized merge have identical trees)
- merge_state: clean (origin/main@1feaf7eeb78b24ad578f2ec2cbf343c496656a0c)
- gate_base_kind: current-tip
- merge_validation: MPR owns merge-time freshness/conflict resolution and final CI; this PASS authorizes PR-open.
- evidence_root: /var/tmp/deploy-ga-7jshilei-20261119.0t8kpwcl
- preflight: associated-PR lookup for the reviewed commit returned []; source resolves locally and is team-authored.

| # | Criterion | Result and evidence |
|---|---|---|
| 1 | Review PASS present | PASS — ga-51619jkp records PASS for the exact reviewed commit; no carryover or reviewer-added change. |
| 2 | Acceptance criteria met | PASS — exactly one BUILD attribute plus its explanatory comment; eight fresh shards pass; generated BUILD sync clean; measurement audit below. |
| 3 | Tests pass | PASS — full-suite five-phase fresh remote run and local floor, zero failures; complete required-CI accounting below. |
| 4 | No open HIGH review findings | PASS — reviewer findings are none, no unresolved HIGH item. |
| 5 | Final branch clean | PASS — source and merge scratch clean at audit; only this gate record is added, and publication verifies a clean committed worktree. |
| 6 | Merge state and build/vet | PASS — clean materialized merge; bazel build //... and make lint (nogo) exit 0; logs c6-build.log and c6-vet.log under evidence_root. |
| 7 | Single feature theme | PASS — internal/runtime/runtimecontract/BUILD.bazel only; shard the existing conformance test target. |

## Acceptance evidence

The source adds shard_count = 8 and the measured rationale, retains the existing timeout and assertions, and cites ga-1mzalylv in its commit message. The designated fix metadata remains gc.fixes_tracker=ga-vkhfnj.
measurement-audit.json was checked against the retained raw BEPs under /var/tmp/rcshard-evidence.io5Vn7: m01/m03 controls TIMEOUT at 300.016/300.011 seconds; m02/m04 each have eight first-attempt linux-sandbox passes, maxima 91.404/91.205 seconds (both below 150). The census tool independently reports NAMES_AGREE for m05/m06: 239 names, 59 top-level, 238 PASS and one unchanged real-screen-absence SKIP. No synthetic load was launched by this gate.
runtimecontract-fresh-unit-audit.json records the independent gate's eight fresh remote shard actions, all PASS, slowest 56.726 seconds. All 59 top-level tests execute; the real-screen test runs on the farm. make bazel-sync plus git diff --exit-code passed. make check-hooks confirms .githooks is active; the gate commit and push use its normal hooks.

## Criterion 3 evidence

- test_cmd: gate-criterion3.sh --repo /var/tmp/gc-merge-ga-7jshilei.dyq6u4f8 --base 1feaf7eeb78b24ad578f2ec2cbf343c496656a0c --bead-json /var/tmp/deploy-ga-7jshilei-20261119.0t8kpwcl/bead.json --owned /var/tmp/deploy-ga-7jshilei-20261119.0t8kpwcl/owned3.tsv --run-dir /var/tmp/deploy-ga-7jshilei-20261119.0t8kpwcl/c3-run/evidence
- test_cmd_scope: full-suite
- phases: make -k check (bazel test //... plus shell guards); bazel test --config=acceptance //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests; bazel test --config=integration //test:integration_packages; full //test/integration:integration_test minus the latency manifest; separate manifest-only latency leg.
- phase_flags: --config=remote-exec --config=ci --keep_going --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1; exact commands and BEP paths are retained in execution.json and each phase's stdout.
- test_counts: 157138 PASS, 0 FAIL, 617 SKIP (top-level terminal events across jobs, including the copied floor logs; repeated tests count per job).
- test_log_dir: /var/tmp/deploy-ga-7jshilei-20261119.0t8kpwcl/c3-run/evidence/remote/logs
- event_reader: gate-test-evidence.py --logs <test_log_dir> --owned /var/tmp/deploy-ga-7jshilei-20261119.0t8kpwcl/owned3.tsv; exit 0, remote-test-events.txt.
- diff_tests_executed: none (no diff-owned tests); diff-owned-tests.py reports no test-file hunk, retained in owned3-explain.txt.
- test_exec_mode: remote-exec
- test_exec_bep: the five *.bep.json and *.evidence.json files under /var/tmp/deploy-ga-7jshilei-20261119.0t8kpwcl/c3-run/evidence/remote
- rbe_state: rc marker present, kill switch absent, certificate expires 2027-01-09; worker environment tree and origin/main digests match (rbe-state.json).
- fallback_reason: none
- admission: remote, floor 25 GiB, observed 66.361 GiB, cap 2, waited 0 seconds; no local-heavy/load wait for remote actions.
- farm_test_seconds: 13524.958
- owned_topup: none (no diff-owned tests missing).
- floor_topup: complete, 16/16 steps exit 0; 13 environment-absence package top-ups plus Darwin compile and two runner self-tests. Counts 24 PASS, 0 FAIL, 14 SKIP, already included above; floor-topup.json and floor-test-events.txt retain commands and results.
- latency_leg: measured under a clear farm probe, first attempt, both tests from gate-latency-tests.txt PASS; latency-leg.json retains the probe, load and names. No latency test ran in the main integration leg.
- skip_inventory: execution.json retains 603 farm skip events (513 other, 90 environment-absence); skip-inventory.json has every name, file/line and reason. These include platform/privilege cases, opt-in live probes, provider contract applicability, known characterization tests, subprocess helpers and absent host fixtures. Every environment-absence package was topped up locally. Residual top-up SKIPs require herdr/br, simulation-tag fixtures or particular tmux bindings. No Go source, test body, helper, skip condition or dependency changed, so these unchanged tests cannot be affected by this target's scheduling attribute; the configured target's own farm tests all pass.
- failure_attribution: none; policy_attribution: none; waiver_ref: none (no gascity waiver path).
- heavy_mode: none; heavy_units: n/a; coverage_check: n/a (ordinary full-suite); heavy-classify.txt retained.
- ci_lane_run: n/a (no CI job, matrix, timeout, or required-check configuration changed).
- local_load: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=5.34 run_max_load=7.13 run_mean_load=6.80 run_readings=6 wait_first_load=5.34 wait_max_load=5.34 wait_mean_load=5.34 wait_readings=1 read_errors=0
- isolation: wrappers used; gascity scratch uses the documented PASS-THROUGH mode with pinned bd v1.3.4 and non-interactive credentials. Fresh views isolate all write/build/drift lanes. Rootless Podman socket and _ping were verified before launch. Raw per-job logs remain retained.

| Phase | Result actions | Fresh remote | Cache flags | Retries | Window/verdict |
|---|---|---|---|---|---|
| unit | 422 | 422 | 0 | 0 | PASS |
| acceptance | 25 | 25 | 0 | 0 | PASS |
| integration-packages | 86 | 86 | 0 | 0 | PASS |
| integration-full | 14 | 14 | 0 | 0 | PASS |
| integration-latency | 1 | 1 | 0 | 0 | PASS |

policy_lane: PASS — C6 make lint, fast-policy.sh's shell guards and Bazel format/policy targets; all exit 0 before suite launch.
drift_lane: PASS — fast-drift.sh generated specs/schema/clients/dashboard drift, fast-openapi.sh breaking-change policy, fast-buildsync.sh make bazel-sync plus git diff --exit-code; all exit 0 before suite launch.
lane_isolation: fast-*.stderr retain each FRESH_TREE_RECORD; same merge/tree, modified=0 untracked=0. Fast result times are retained in fast-final.txt (PASS before C3 launch).

## Required CI accounting

ci_required_closure: python3 ci_required_triggers.py /var/tmp/gc-merge-ga-7jshilei.dyq6u4f8 1feaf7eeb78b24ad578f2ec2cbf343c496656a0c dd6f82055319e64e0167e0d35f1ddfa7cc8564b9 fork; merged workflow blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs need a status (accounting/closure.txt). The runner-policy outputs resolve Linux runners to Blacksmith Ubuntu and runner_windows to Blacksmith Windows 2025.

| Leaf job | Status | Evidence |
|---|---|---|
| runner-policy | LOCAL-PASS | EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=<accounting/runner-policy.out> python3 .github/workflows/scripts/runner_policy.py; exit 0; accounting/runner-policy.log and .out. |
| changes | LOCAL-PASS | closure tool exit 0; suite_mode=filtered, all three filters no match; accounting/closure.txt. |
| credential-provider-windows | NOT-TRIGGERED | NON-LINUX; needs.changes.outputs.credential_provider == 'true'; no match, closure tool row. |
| pack-gate | NOT-TRIGGERED | needs.changes.outputs.packs == 'true'; no match, closure tool row. |
| windows-lane | LOCAL-PASS | Exact four run steps extracted from merged CI YAML in ci-windows-lane.sh; exit 0; accounting/windows-lane-output.txt. Fresh view, load-gated, isolated wrapper, detached service. |
| windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | Windows native allowlist tests require the job's Windows runner. Native allowlist test-binary closure computed with GOOS=windows GOARCH=amd64 go list -mod=readonly -test -deps for its three package entries; exit 0, no changed input in closure; native-windows-reach.json. |

windows_reach: none (native Windows allowlist; changed runtimecontract BUILD is outside its closure).
deferred_to_pr_ci: windows-lane-test (non-linux-runner)
not_covered: none
host_gaps: none
windows_lane_load: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=6.27 run_max_load=11.69 run_mean_load=10.35 run_readings=3 wait_first_load=6.27 wait_max_load=6.27 wait_mean_load=6.27 wait_readings=1 read_errors=0
windows_lane_isolation: FRESH_TREE_RECORD from=/var/tmp/gc-merge-ga-7jshilei.dyq6u4f8 head=d5560ff9ea36 tree=c99eb0c51a79 dir=/var/tmp/gc-fresh-tree.23SKNw modified=0 untracked=0 ignored=6 ignored_names=.bazelrc.local,.github/workflows/scripts/__pycache__/,bazel-bin,bazel-gc-merge-ga-7jshilei.dyq6u4f8,bazel-out

Merge-ready requires green CI / required and successful native Windows tests on the published head. The tracker and landing-wait edges remain until the repair lands; opening this PR does not clear them.
