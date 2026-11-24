**Verdict:** **PASS**

# Darwin lost-port fixture release gate

Evaluated 2026-11-22T07:41:52.773311+00:00. Deploy `ga-4bp80f2s`; build `ga-1ejsnch9`; review `ga-2vhhf2e7`.
Reviewed source: `4d7d8778e12536a20f0286e50e643303a8037197`. Deploy mode: remote; push remote: fork; base branch: main.
Pinned base: `299167a17b7414692e89617ecca9b1ed5b2e12c5`. Tested merge: `5a365a26f04a4e0478d479768aa270b6e3f3b377`; tree: `71d596d83305f3a18ab42602c52facdc1298ab7d`.
`merge_state: clean (origin/main@299167a17b7414692e89617ecca9b1ed5b2e12c5)`; `gate_base_kind: current-tip` at gate start; conflicts: none.
Main advanced after the pin to `1042f51083f93b7b16d3d80cbcd7f3eb73fb374b`. The gate did not rebase or move its source.
`merge_validation:` GitHub main rules currently require a merge queue and checks on its merge group; MPR owns merge-time validation and the merge.
Release criteria: required pack release-gate-criteria fragment; docs/PROJECT_MANIFEST.md is absent.

| # | Result | Evidence |
|---|---|---|
| 1 | PASS | Live review `ga-2vhhf2e7` says verdict: pass on the exact resolved source. No carryover or reviewer-added commit. |
| 2 | PASS | Both helpers match the reference patch: two files, +24/-0. Linux regressions PASS with retry output; exact-source native Mac proof below. Resource census passes. Tracker stamp is present on build, review and deploy; tracker remains open until landing. |
| 3 | PASS | Full canonical gate, static lanes, tagged-source nogo, native floor and all six required-CI leaf accounts audited below. |
| 4 | PASS | No unresolved HIGH finding in style, security or specification review. |
| 5 | PASS | Canonical scratch and publication worktree clean before recording this evidence. Deploy publication verifies clean status after its gate-only commit. |
| 6 | PASS | Clean merge against the pinned base; whole-tree build and explicit lint/nogo BEPs terminate overallSuccess=true. |
| 7 | PASS | One theme: detect whether the client-socket hold actually prevents a listener bind in two existing regression fixtures. |

## Complete test evidence

`test_cmd_scope: full-suite`; `test_exec_mode: remote-exec`; `heavy_mode: none`; `waiver_ref: none`; `failure_attribution: none` for the canonical suite.
`test_cmd:` gate-criterion3.sh selects the complete five-phase Bazel chain: make -k check (//...), acceptance plus solo tests, integration-packages, full integration minus the latency list, then its separate latency phase.
All phases use --config=remote-exec --config=ci --nocache_test_results --noremote_local_fallback --test_env=GO_TEST_WRAP_TESTV=1. The chain ran on the canonical merged tree, independently of reviewer evidence.
`test_counts: 158687 PASS, 0 FAIL, 618 SKIP` (top-level result occurrences summed over retained logs; repeated targets count per execution, subtests do not).
`diff_tests_executed: none (no diff-owned tests)`; helper callers are affected, not owned. The two regression roots are additionally verified as acceptance criteria.
`delta_gate: shadow, decision=full, reason=not-gate-pr, applied=false`; `owned_topup: []`; no delta or curated-list substitution.

| Phase | Fresh remote result actions | Top-level PASS | FAIL | SKIP |
|---|---:|---:|---:|---:|
| unit | 426 | 93638 | 0 | 443 |
| acceptance | 25 | 425 | 0 | 14 |
| integration-packages | 87 | 64394 | 0 | 133 |
| integration-full | 15 | 205 | 0 | 14 |
| integration-latency | 1 | 2 | 0 | 0 |

554/554 result actions: strategy remote, neither cache flag, attempt/run 1, start inside its phase window, terminal PASS. 1,108 retained raw CAS artifacts hash-verified. Parsing strips only byte 0x16; raw artifacts remain intact.
`test_exec_bep:` /var/tmp/gc-gate-ga-4bp80f2s-vnsbjpio/c3-run/evidence/remote/<phase>.bep.json. `farm_test_seconds: 13280.084`. `rbe_state:` marker present, kill switch absent, certificate valid, tree worker-env digest matches origin/main. `fallback_reason: none`.
`admission:` remote cap 2, floor 25.0 GiB, free 60.943 GiB, waited 0.001 seconds.
`latency_leg:` both listed latency tests PASS on attempt 1 under GATE_FARM_PROBE verdict=clear. Neither ran in integration-full.
`floor_topup:` all 16 native commands exit 0, including beadstest coverage, environment-absence packages, Darwin compile and both runner self-tests. Native floor is supplementary to the complete remote phases.
`load_threshold: 15`; `load_waited_seconds: 420`; `load_wait_timed_out: 0`; `run_start_load: 14.35`; `run_max_load: 16.77`; `run_mean_load: 15.65`; `run_readings: 7` (native floor; remote admission has no host-load wait).
`test_log_dir: /var/tmp/gc-gate-ga-4bp80f2s-vnsbjpio/c3-run/evidence/remote/logs`; native floor logs: `/var/tmp/gc-gate-ga-4bp80f2s-vnsbjpio/c3-run/evidence/remote/floor-logs`. Read with gate-test-evidence.py; retained, never deleted.

## Acceptance and skip audit

Linux: TestStartSQLServerRetriesWhenAClientSocketHoldsTheProbedPort and TestStartTestDoltServerSurvivesALostProbedPort each report PASS, not SKIP, with their own lost-probed-port retry line in the unfiltered integration-packages phase. Resource census also passes in the full unit phase.
Native macOS proof: https://github.com/quad341/gascity/actions/runs/44994434838 is SUCCESS on Intel and arm64, checking out the exact reviewed source with no reference patch. Each architecture reports 15 other test/dolttest tests PASS, the dolttest lost-port test SKIP, and its beads twin SKIP, with the printed BSD bind-precondition reason. Those two new native skips satisfy the explicit acceptance contract; they are not described as pre-existing Linux-suite skips.
`skip_justification:` after native follow-ups, 337 unique package/test rows remain SKIP. All source sites are unchanged; none is owned. The 244 rows with shared hunks in their test-binary import closures have pinned-base comparisons below. The remainder's computed closures contain neither changed helper. Per-name reasons, sites, closures and source-log references are retained in skip-audit/plan.json.
Remaining reasons include platform/root prerequisites, deliberate unavailable-provider contract cases, opt-in live services and existing explicit coverage deferrals. The changed helpers are reached only by the two named Linux tests, which pass; no remaining skipped test calls either helper.
- unit: COMPARE-SUMMARY head_skips=310 judged=215 pre-existing=215 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=95  => ALL PRE-EXISTING
- integration-packages: COMPARE-SUMMARY head_skips=105 judged=100 pre-existing=100 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=5  => ALL PRE-EXISTING
- integration-full: COMPARE-SUMMARY head_skips=13 judged=13 pre-existing=13 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING

Supplemental unit base comparison is not a passing suite: its owned-* wrapper supplied -test.v to non-Go targets, causing 15 argument errors. Every Go result passed and the requested skip comparisons matched. Those failures and command exit 1 remain in unit-base-comparison-audit.json; no failed result was re-executed or credited as passing gate evidence. The earlier 0700-directory refusal produced no BEP/tests and is retained separately.

## Static lanes and required CI

`policy_lane: PASS` — shell guards in make check plus explicit policy Bazel targets; format, docs sync, dependency/core-boundary, test-policy, release, security and CI-policy checks. C6 includes make lint, and a separate integration-tagged nogo build covers both changed package test targets.
`drift_lane: PASS` — make bazel-sync, git diff --exit-code and empty complete status, plus schema, OpenAPI, client, dashboard distribution and lockfile drift checks; each static lane uses its own fresh tree. OpenAPI breaking check passes at the frozen base.
`complexity: PASS (zero-growth ii)` — raw complexity-check exits 2 on both pinned base and merge; no merge row worsens, complexity-diff prints no threshold changes, baseline untouched. This is the documented scoring rule, not a waiver.
`storeclass baseline and growth gate: PASS` — trusted base-built analyzer; baseline and strict unapproved growth predicates exit 0, frozen base/tip recorded. No approval label or raise flag.
`ci_lane_run: n/a (no CI-config change)`; API/spec/dashboard manual preview: n/a (two test-only paths). Full unit still covers dashboard Bazel targets.
`ci_required_closure:` ci_required_triggers.py /var/tmp/gc-merge-validate.Hl45H3 299167a17b7414692e89617ecca9b1ed5b2e12c5 4d7d8778e12536a20f0286e50e643303a8037197 fork; workflow blob `fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612`; 6 LEAF jobs need a status. check is an AGGREGATOR.

| Leaf job | Status | Evidence |
|---|---|---|
| changes | LOCAL-PASS | python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-validate.Hl45H3 299167a17b7414692e89617ecca9b1ed5b2e12c5 4d7d8778e12536a20f0286e50e643303a8037197 fork; exit 0; /var/tmp/gc-gate-ga-4bp80f2s-vnsbjpio/ci-required/closure.stdout |
| runner-policy | LOCAL-PASS | python3 .github/workflows/scripts/runner_policy.py; exit 0; /var/tmp/gc-gate-ga-4bp80f2s-vnsbjpio/ci-required/runner-policy.stdout |
| pack-gate | LOCAL-PASS | scripts/update-bundled-gastown-pack --check; GC_TEST_GASCITY_PACKS_REGISTRY=main make test-pack-registry-live; exit 0; /var/tmp/gc-gate-ga-4bp80f2s-vnsbjpio/ci-required/pack-gate-run-output.txt |
| windows-lane | LOCAL-PASS | exact windows-lane run commands: Windows builds/tests-compile, nogo cross-build, whole-tree Darwin build/test-compile; exit 0; /var/tmp/gc-gate-ga-4bp80f2s-vnsbjpio/ci-required/windows-lane-run-output.txt |
| credential-provider-windows | DEFERRED-TO-PR-CI reason=non-linux-runner |   credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} TRIGGERED  if: needs.changes.outputs.credential_provider == 'true' |
| windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner |   windows-lane-test                      LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} EVERY-PR |
`ci_required_job_load: pack-gate` — LOAD_GATE_SUMMARY threshold=15 waited_seconds=30 wait_timed_out=0 run_start_load=14.92 run_max_load=19.63 run_mean_load=18.16 run_readings=4 wait_first_load=15.35 wait_max_load=15.35 wait_mean_load=15.06 wait_readings=3 read_errors=0 prewait_s=30.
`ci_required_job_load: windows-lane` — LOAD_GATE_SUMMARY threshold=15 waited_seconds=1800 wait_timed_out=1 run_start_load=42.04 run_max_load=59.06 run_mean_load=51.02 run_readings=5 wait_first_load=18.23 wait_max_load=78.08 wait_mean_load=49.70 wait_readings=62 read_errors=0 prewait_s=1800.

`windows_reach: none`; windows-lane-test has its own plain/integration Windows test-binary closure calculation; no changed production input is reachable. Actual Windows execution remains deferred.
`deferred_to_pr_ci: credential-provider-windows (non-linux-runner), windows-lane-test (non-linux-runner)`; `not_covered: none`. Every leaf has one status. PR-open PASS authorizes no merge: CI / required and both deferred jobs must be green on the PR head before merge.

## Retained evidence

c3-run/evidence/execution.json: `/var/tmp/gc-gate-ga-4bp80f2s-vnsbjpio/c3-run/evidence/execution.json`; SHA256 `08aee239a2405d767d3fce692b25dfc78a72736cbd01e5286389c191704933b5`.
remote-independent-audit.json: `/var/tmp/gc-gate-ga-4bp80f2s-vnsbjpio/remote-independent-audit.json`; SHA256 `a9fab436a33e6dcf9319228ec75214d4f49727452b48691fcd7b09a1d5d98385`.
fast-independent-audit.json: `/var/tmp/gc-gate-ga-4bp80f2s-vnsbjpio/fast-independent-audit.json`; SHA256 `ea94886dfa2fe69ff287a1f41d776dde3b38a4b88f5d3c7e53231de28c84f0ef`.
storeclass-proof/verified.json: `/var/tmp/gc-gate-ga-4bp80f2s-vnsbjpio/storeclass-proof/verified.json`; SHA256 `87e04b4b454a73484e367fc092c4608ab7f917a7777b8d578f1dff403c8941fe`.
tagged-nogo-verified.json: `/var/tmp/gc-gate-ga-4bp80f2s-vnsbjpio/tagged-nogo-verified.json`; SHA256 `134328996ddd3bfe85b0c934f780e1c56f597fb03f1b044925fc11a51518dd5c`.
linux-port-regressions.json: `/var/tmp/gc-gate-ga-4bp80f2s-vnsbjpio/linux-port-regressions.json`; SHA256 `7e7bb3fa27cd3ff1a01c1f5de18b806134deae2699db4359fc96a60bd841f541`.
mac-evidence/independent-verification.json: `/var/tmp/gc-gate-ga-4bp80f2s-vnsbjpio/mac-evidence/independent-verification.json`; SHA256 `f91f388f3f9621ecaae1adb8a931b1d63bd69be3942c5407cf544185350ac9b3`.
skip-audit/plan.json: `/var/tmp/gc-gate-ga-4bp80f2s-vnsbjpio/skip-audit/plan.json`; SHA256 `d18499c95c66da30cb68116795f8f094d3ac2c1f840113a97800850918d11f77`.
base-skip-comparisons-final.json: `/var/tmp/gc-gate-ga-4bp80f2s-vnsbjpio/base-skip-comparisons-final.json`; SHA256 `d5254d29c129a7122ae0b4ae7ee48633c0faa279e141bae8113e191bfbb005d7`.
unit-base-comparison-audit.json: `/var/tmp/gc-gate-ga-4bp80f2s-vnsbjpio/unit-base-comparison-audit.json`; SHA256 `54f847bccc4419d39751e7aaf750103c8927ce3e7ddb1d33281d3f05ee859295`.
ci-required/summary.json: `/var/tmp/gc-gate-ga-4bp80f2s-vnsbjpio/ci-required/summary.json`; SHA256 `a26348272ede8bf117411fea27aaa68cc293104d13e3bcdee560127595a2d9b7`.
publication-preflight.json: `/var/tmp/gc-gate-ga-4bp80f2s-vnsbjpio/publication-preflight.json`; SHA256 `760e083d2e0e47b1dcb7b275b313fb058fcc288f77f7267e97ff1d35f26a7ccc`.
