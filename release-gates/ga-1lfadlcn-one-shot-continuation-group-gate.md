**Verdict:** **PASS**

# One-shot continuation-group routing release gate

Bead: ga-1lfadlcn. Build: ga-sj2h8f.1. Review: ga-8tc9px8a.

- Reviewed deploy source: `e8e1c0c5c61c1105d5064d77e6ca676c5006d64d`; provenance branch is never a push target.
- Deploy mode: remote; push remote: fork; base: `origin/main`.
- gate_base: `42f005d69334630c326bb3eac685543a57f47ccf`; gate_head: `e8e1c0c5c61c1105d5064d77e6ca676c5006d64d`.
- merge_state: clean (`origin/main@42f005d69334630c326bb3eac685543a57f47ccf`).
- gate_base_kind: current-tip; tested merge: `0251871e7054404444e310421bd23f1063d748f7`.
- Tested tree: `99801997b0fabbb32d225efa4a6837491f8f4224`.
- Pack at C3 launch: `cc4b57a701a30c4b0453c27aa9386965edbce5c4`; launch: 2026-11-24T19:59:38.768456+00:00.
- merge_validation: clean synthetic merge independently built and tested; merge authority must validate the current merge result and current-head CI at merge time. The gate neither rebases nor merges.
- Evidence root: `/var/tmp/gc-gate.ga-1lfadlcn-20261124.LkFXAJp5`; retained mirror: `/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-1lfadlcn/gate-20261124-LkFXAJp5`. Raw per-job logs are retained, never deleted.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS | PASS | Review ga-8tc9px8a explicitly binds its PASS to the reviewed source, including one reviewer-added test. Independent helper verifies the append-only test commit and exact path; full suite exercises it by name. |
| 2 | Acceptance criteria | PASS | Uniform refusal of any nonempty continuation group on a one-shot route, including drain-stamped groups; typed wrapped error names step/group/lifecycle and both remedies. Shared drain hard-fails with the specified reason and offending step; long-lived grouping remains covered. Engdocs matches the revised architect ruling. |
| 3 | Tests and required CI accounting | PASS | Full independent five-phase uncached remote suite, all16 native-floor commands, fast format/policy/lint/drift lanes, by-name owned-test verification, and complete per-SKIP proof. All six required CI leaves accounted below. |
| 4 | No open high-severity findings | PASS | Reviewer has zero unresolved HIGH/major findings. Wrapper wording and optional upgrade-note observations are nonblocking; neither changes the revised exit contract. |
| 5 | Clean final branch | PASS | Tested source scratch has no tracked or untracked change. Isolated deploy branch contains the exact reviewed source plus only this gate record; clean status is verified after its record commit, before push. |
| 6 | Merge state and build/vet | PASS | Evaluated first after already-merged preflight: no existing target PR. Clean merge into the pinned base builds all1500 targets with nogo:22683 actions,53.268s, final BEP overallSuccess=true. Scratch merge commit used hooks disabled. |
| 7 | Single feature theme | PASS | Routing lifecycle/continuation compatibility only: graphroute, its shared-drain caller, focused tests and matching engineering documentation. Separate long-lived retry defect ga-62zv5cor is excluded. |

reviewer_tests_verified: `1d84048efe1abc20daccd33ea257259a97cf9710` -> `e8e1c0c5c61c1105d5064d77e6ca676c5006d64d` files=internal/graphroute/graphroute_test.go

## Acceptance and scope evidence

The architect's ruling on ga-sj2h8f.1 supersedes the original key-split request. There is no new metadata key and no spelling-based exemption. `IndependentStepsGroupError` wraps `ErrIndependentStepsContinuationGroup`; its remedies are a long-lived agent or drain `context="separate"`. Shared drain failure uses `gc.failure_reason=drain_item_one_shot_continuation_group` and the offending step as `gc.failure_subject`.

The PR body must expose the kept -> error maintainer call, unknown external-pack impact, configured repo/city audit, shared-drain failure contract, out-of-scope retry tracker and merge authority. These five required statements are preserved in the PR draft and checked at publication.

## Independent test evidence

fast_lane: PASS before suite setup and launch. Each mutating/walking lane used an independent `fresh-tree-run.sh` view of the same merge; the remote definitions installer ran in that view before its command. No lane rewrote the suite scratch.

policy_lane: `make check-release-dist-ignore check-routed-test-rows check-split-topology-rows check-residency-boundary check-gomod-replace check-core-boundary check-eventexport-isolation` exit0; `make lint NOGO_BUILD_FLAGS='--keep_going --output_groups=nogo_fix --config=remote-exec --noremote_local_fallback'` exit0. Format: `bazel test --config=remote-exec --noremote_local_fallback //:go_format_test` exit0. policy_attribution: none.

drift_lane: `make bazel-sync BAZEL_SYNC_FLAGS='--config=remote-exec --noremote_local_fallback'` then `git diff --exit-code`, exit0. Gazelle pattern/expression diagnostics produced no tracked drift. `docs/PROJECT_MANIFEST.md` is absent in this checkout and its path history; the release protocol and current Makefile, TESTING and CI define the executed lanes.

heavy_mode: none. known_red_precheck: proceed, nothing-known-red. ICU74 check: OK, soname hashes retained. Own-code provenance selected remote-exec. No container transport is needed for these owned routing tests; containerhost uses the documented fake Docker client. CI-config diff (3c): n/a, no workflow/job/matrix/timeout/check-list change.

test_exec_mode: remote-exec
fallback_reason: none
test_cmd_scope: full-suite
waiver_ref: none (gascity has no waiver path)
local_test_log_dir / test_log_dir: `/var/tmp/gc-gate.ga-1lfadlcn-20261124.LkFXAJp5/criterion3/evidence/remote/logs` (570 retained logs; mirrored under the artifact root).

test_cmd (detached launch, actual NUL-separated argv retained):

```text
env GC_GATE_REMOTE_ENABLED=1 GC_REMOTE_GATE_FLOOR_GIB=25 GC_REMOTE_GATE_CAP=2 ISOLATED_TEST_BEAD=ga-1lfadlcn GC_GATE_DELTA=shadow GC_GATE_LOAD_DEFER_MAX=3 /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-criterion3.sh --repo /var/tmp/gc-gate.ga-1lfadlcn-20261124.LkFXAJp5/merge --base 42f005d69334630c326bb3eac685543a57f47ccf --bead-json /var/tmp/gc-gate.ga-1lfadlcn-20261124.LkFXAJp5/bead-at-c3.json --owned /var/tmp/gc-gate.ga-1lfadlcn-20261124.LkFXAJp5/owned-tests.tsv --run-dir /var/tmp/gc-gate.ga-1lfadlcn-20261124.LkFXAJp5/criterion3/evidence
```

The canonical runner executed unit (`bazel test //...`), acceptance, integration-packages, full integration excluding the dedicated latency list, and the dedicated latency phase, each with CI configuration, verbose named output, forced uncached test results, no local fallback and retained BEP. The command list, phase windows, rbe_state, admission, delta decision, native-floor argv and inventory are in the compact recorder below and its full pointed file.

| Phase | Valid first-attempt uncached remote results | farm_test_seconds |
|---|---:|---:|
| unit | 426 | 4770.185 |
| acceptance | 25 | 1587.001 |
| integration-packages | 87 | 4180.016 |
| integration-full | 15 | 3711.744 |
| integration-latency | 1 | 64.376 |

All554 result events satisfy the remote execution predicate; total farm_test_seconds: 14313.322. All16 native floor jobs exit0, including systemd restart, Darwin compile, push-gate-lock and local-concurrency self-tests; canonical floor selections are computed, not a replacement narrowed suite.

test_counts: PASS=159164 FAIL=0 SKIP=618 (column-zero top-level results summed per job across570 logs, not unique test identities).

diff_tests_executed:

```text
diff_tests_executed:
  TestDecorateDrainItemRecipeSharedContinuationGroupFollowsPoolLifecycle PASS (integration-packages-e4245ddf6a37030111bc.log, unit-1ab526405ace21879f85.log)
  TestProcessDrainSharedFailsClosedForOneShotItemRoute PASS (unit-e7f4116e9a278807bf1e.log)
  TestApplyGraphRouteBinding_IndependentSteps PASS (unit-2b4b2c7a15e403324bb1.log)
  TestApplyGraphRouteBinding_IndependentStepsErrorPointsAtBothWaysOut PASS (unit-2b4b2c7a15e403324bb1.log)
  TestApplyGraphRouteBinding_LongLivedPoolKeepsGroup PASS (unit-2b4b2c7a15e403324bb1.log)
  TestAssignGraphStepRoute_PropagatesIndependentStepsError PASS (unit-2b4b2c7a15e403324bb1.log)
GATE_TEST_EVIDENCE_RESULT ok: 6 diff-owned tests verified by name
```

latency_leg: PASS on attempt1, both named budgets PASS after farm probe clear; waited0s, load1=3.91 load5=2.87 load15=4.46 cpus32 threshold50. No full-target timing result substitutes for that lane.

Native floor load: load_threshold=15; load_waited_seconds=1801; load_wait_timed_out=1; run_start_load=24.77; run_max_load=42.61; run_mean_load=32.74; run_readings=12. wait_first_load=28.43; wait_max_load=28.43; wait_mean_load=20.34; wait_readings=61; read_errors=0. The ordinary bounded wait proceeded by policy, without changing the threshold, rerunning tests or granting a waiver.

SKIP accounting:676 events including subtests/native map to319 unchanged source sites. All51 actual test-binary import closures resolve. Every301 reaching row reproduces at the pinned base (unit197,integration-packages68,native36), new0 reason-changed0 diff-owned0 unresolved0;375 rows cannot reach any changed production/shared-helper package. Every shared hunk and its reach/baseline proof is expanded per row in the retained proof.

COMPARE-NOTE: `TestInfraScopedManagedDoltPIDIdentityUnderRealSystemd` base reports remote account `rbe-a01`, head `rbe-a07`; only that worker-account difference is normalized for comparison. Original logs are unchanged.

Auxiliary unit baseline retention first refused absent XML because the focused command omitted output download; its original4/4 tests had passed. Four exact original raw logs were recovered from existing local files with BEP CAS authority, SHA256 and byte-count verification, and the original execution predicate was independently verified. No test was rerun; the original refusal is retained. These focused comparisons prove SKIPs only and never replace the full suite. Native baseline used the floor's recorded flags/tags/environment and only its selected SKIP names.

execution.json (canonical compact form; large arrays replaced by measured elision markers):

```json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/gc-gate.ga-1lfadlcn-20261124.LkFXAJp5/criterion3/evidence/remote","gate_base":"42f005d69334630c326bb3eac685543a57f47ccf","gate_head":"e8e1c0c5c61c1105d5064d77e6ca676c5006d64d","admission":{"class":"remote","floor_gib":25.0,"free_gib":51.14323425292969,"waited_seconds":0.001,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"},"delta_gate":{"mode":"shadow","decision":"full","reason":"not-gate-pr","detail":"no PR record (pr.json)","applied":false,"base":"42f005d69334630c326bb3eac685543a57f47ccf","head":"e8e1c0c5c61c1105d5064d77e6ca676c5006d64d","merge":"0251871e7054404444e310421bd23f1063d748f7","prior":null,"delta":{},"changed_labels":[],"changed_label_count":0,"tests":{},"phases":{},"plan_sha256":"6fff445d6bd10f4c4b869c02430e2004485bfb00d0ba3000963c3f18f48213ea"},"test_exec_bep":{"unit":{"results":426,"remote_executed":426,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":87,"remote_executed":87,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":15,"remote_executed":15,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":14313.322,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=5fd31cdd75b202be3ab9de14bb2514f059b7969d1f14ab7fdc392c2c723e6b7b"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":64.376,"evidence_errors":[],"probe":["GATE_FARM_PROBE verdict=clear waited=0s load1=3.91 load5=2.87 load15=4.46 cpus=32 threshold=50 max_wait=600s"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 10.90 4.96 5.10 8/1090 56568"]}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":40101}},"log_dir":"/var/tmp/gc-gate.ga-1lfadlcn-20261124.LkFXAJp5/criterion3/evidence/remote/floor-logs"},"skip_inventory":{"counts_by_reason_class":{"other":514,"environment-absence":90},"top_level_counts_by_reason_class":{"other":256,"environment-absence":43},"tests":{"elided_array":{"items":604,"bytes":225268}}},"test_cmd":{"elided_array":{"items":5,"bytes":2129}},"test_counts":{"PASS":159164,"FAIL":0,"SKIP":618},"diff_tests_executed":"diff_tests_executed:\n  TestDecorateDrainItemRecipeSharedContinuationGroupFollowsPoolLifecycle PASS (integration-packages-e4245ddf6a37030111bc.log, unit-1ab526405ace21879f85.log)\n  TestProcessDrainSharedFailsClosedForOneShotItemRoute PASS (unit-e7f4116e9a278807bf1e.log)\n  TestApplyGraphRouteBinding_IndependentSteps PASS (unit-2b4b2c7a15e403324bb1.log)\n  TestApplyGraphRouteBinding_IndependentStepsErrorPointsAtBothWaysOut PASS (unit-2b4b2c7a15e403324bb1.log)\n  TestApplyGraphRouteBinding_LongLivedPoolKeepsGroup PASS (unit-2b4b2c7a15e403324bb1.log)\n  TestAssignGraphStepRoute_PropagatesIndependentStepsError PASS (unit-2b4b2c7a15e403324bb1.log)\nGATE_TEST_EVIDENCE_RESULT ok: 6 diff-owned tests verified by name\n"}
```

Full execution.json retained: 6058 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-1lfadlcn/gate-20261124-LkFXAJp5/criterion3/evidence/execution.json; SHA256 49a94af7feb24f13e827176af1fd8e351665a035d16c074ac5b15b416a030586.

Full skip inventory retained: 4834 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-1lfadlcn/gate-20261124-LkFXAJp5/criterion3/evidence/remote/skip-inventory.json; SHA256 6789ae8a181c554f315a87a2f07616553a9ecf87e08b62cba504b387ba23ad2a.

Full expanded not-owned SKIP proof retained: 103231 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-1lfadlcn/gate-20261124-LkFXAJp5/not-owned-skip-evidence.json; SHA256 4e2570ebd89024b5c5740466d07db1c339a78be6d15f0ec02ffa53898c39d02c.

Full by-name test evidence retained: 12 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-1lfadlcn/gate-20261124-LkFXAJp5/final-test-evidence.txt; SHA256 e0c9c19cab4f66c193ba017980b4142b80225381e267b6d268ce5cda9d2b1b5a.

Full native floor argv/results retained: 1056 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-1lfadlcn/gate-20261124-LkFXAJp5/criterion3/evidence/remote/floor-topup.json; SHA256 ab4b02afdce3f2b7cdef0daeded4e225c93ad4039d940deb792c2cfdc13ceb1f.

## Required CI accounting (3e)

ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-gate.ga-1lfadlcn-20261124.LkFXAJp5/merge 42f005d69334630c326bb3eac685543a57f47ccf e8e1c0c5c61c1105d5064d77e6ca676c5006d64d fork; workflow_blob: fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs need a status.

| Job | Status | Evidence |
|---|---|---|
| runner-policy | LOCAL-PASS | EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=/var/tmp/gc-gate.ga-1lfadlcn-20261124.LkFXAJp5/ci-runner-policy-output.txt python3 .github/workflows/scripts/runner_policy.py; exit0; /var/tmp/gc-gate.ga-1lfadlcn-20261124.LkFXAJp5/ci-runner-policy-terminal-output.txt |
| changes | LOCAL-PASS | Closure tool exit0: /var/tmp/gc-gate.ga-1lfadlcn-20261124.LkFXAJp5/ci-required-closure-final.txt |
| windows-lane | LOCAL-PASS | GOOS=windows go build $(cat .github/windows-lane/build.txt); GOOS=windows go test -count=1 -vet=off -run '^$' -exec /bin/true $(cat .github/windows-lane/test-build.txt); in tools/nogo: GOOS=windows go build ./analyzers/... && GOOS=windows go test -count=1 -vet=off -run '^$' -exec /bin/true ./analyzers/...; at root: GOOS=darwin go build ./... && GOOS=darwin go test -count=1 -vet=off -run '^$' -exec /bin/true ./...; exit0; /var/tmp/gc-gate.ga-1lfadlcn-20261124.LkFXAJp5/ci-windows-lane-terminal-output.txt |
| credential-provider-windows | NOT-TRIGGERED | credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true' |
| pack-gate | NOT-TRIGGERED | pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true' |
| windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | Resolved runner_windows is windows-latest or Blacksmith Windows, not Linux; own GOOS=windows GOARCH=amd64 test-binary closure exit0, reach none: /var/tmp/gc-gate.ga-1lfadlcn-20261124.LkFXAJp5/windows-reach.json |

windows_reach: none for credential-provider-windows and the separately computed native windows-lane-test closure, both exit0. The latter uses GOOS=windows GOARCH=amd64 over its own .github/windows-lane/test.txt; the credential-specific helper was not reused for it.

deferred_to_pr_ci: windows-lane-test (non-linux-runner).
not_covered: none.

CI runner-policy LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=71.01 run_max_load=NA run_mean_load=NA run_readings=0 wait_first_load=42.75 wait_max_load=71.01 wait_mean_load=32.22 wait_readings=61 read_errors=0

CI windows-lane LOAD_GATE_SUMMARY threshold=15 waited_seconds=1802 wait_timed_out=1 run_start_load=32.26 run_max_load=57.47 run_mean_load=43.54 run_readings=7 wait_first_load=71.79 wait_max_load=121.28 wait_mean_load=81.80 wait_readings=61 read_errors=0

Full required CI accounting retained: 54 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-1lfadlcn/gate-20261124-LkFXAJp5/ci-required-accounting.json; SHA256 c82f40c51f63eed47d70bbe7362bd5463a68bb807847521b8abbf2e81bf59c88.

A gate PASS authorizes PR-open only. Merge requires CI / required green on the current PR head and success of the deferred Windows job (or its own path-filter skip). Deployer does not merge; ordinary merge authority is MPR.
