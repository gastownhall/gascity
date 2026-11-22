# zcode idle-read TERM regression coverage — ga-36k4u7sz

**Verdict:** **PASS**

Evaluated 2026-11-20T23:41:18.961035+00:00. This PASS authorizes opening the PR. Merge requires `CI / required` green on its head and success of the deferred native Windows test job.

- Deploy mode: remote; push target: fork (origin dry-run failed; exit status and remote check retained).
- Exact reviewed source: `fe8a991f8c379fd349ae6620157b776a7a934744`.
- Pinned base: `origin/main@e6e47235027b758b9a86b0ddb99fb5d3a69c59e4`.
- Canonical merge: `505927bcf1e6b485ac6893af19f17d4538b9f048`; tree: `8619d1785b74ac786ac2ccd9f21a6aeea89b4c57`.
- `merge_state: clean (origin/main@e6e47235027b758b9a86b0ddb99fb5d3a69c59e4)`
- `gate_base_kind: current-tip`
- `gate_base_sha: e6e47235027b758b9a86b0ddb99fb5d3a69c59e4`
- `merge_validation: merge result built and vetted here; merge authority validates the eventual merge against its then-current base`.
- Durable evidence: `/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-36k4u7sz/gate-20261120-abe2gt_f/evidence`. Original run: `/var/tmp/gc-gate-ga-36k4u7sz-aoqeivc6`. Immutable source, base, tree and artifact proofs are retained.

| # | Criterion | Result and evidence |
|---|-----------|---------------------|
| 1 | Review PASS present | PASS — ga-290hd2nr explicitly passes the resolved source above; no carryover or reviewer-added-test exception. Original review and build records retained. |
| 2 | Acceptance criteria met | PASS — new parent and both piped/TTY cases PASS by name in the fresh canonical unit run. Independent disposable-view negative control removed only the idle wake timer: both cases FAIL at their 20s bound with the TERM marker present; expected Bazel exit3. Original script restored; canonical scratch unchanged. The source diff changes only adapter_test.go (+65); zcode-repl is unchanged. |
| 3 | Tests pass | PASS — complete canonical full suite, all five phases remote/uncached/first-attempt with passing window predicates; 549 PASS actions, 1098 raw artifact digests/lengths verified. 16/16 mandatory native checks exit0. Combined Go top-level counts: 157943 PASS, 0 FAIL, 617 SKIP; every skip accounted for outside the changed test. Six-leaf required-CI accounting below is complete. |
| 4 | No unresolved HIGH findings | PASS — original review has no security/style HIGH finding; no HIGH issue remains open for this test-only change. |
| 5 | Final branch clean | PASS — exact evaluated scratch HEAD/tree and empty git status verified. The release record is the only addition above the reviewed source on the isolated deploy branch; normal hooks run and a clean post-commit tree is required before push. |
| 6 | Merge state; build and vet | PASS — clean canonical merge on the pinned current tip. Whole-tree Bazel build and explicit lint/nogo BEPs each have terminal overallSuccess=true; merge parents/tree and gate-base marker independently verified. |
| 7 | Single feature theme | PASS — one new deterministic TERM/idle-read regression test, one source file, one behavior. Source ancestry accepts only this deploy and its build bead ga-4pk9s9o4. |

## Runtime and static evidence

`test_cmd: bash /var/tmp/gc-gate-ga-36k4u7sz-aoqeivc6/criterion3.sh` — canonical entry point is `gate-criterion3.sh --repo /var/tmp/gc-merge-validate.Hjirl9 --base e6e47235027b758b9a86b0ddb99fb5d3a69c59e4 --bead-json /var/tmp/gc-gate-ga-36k4u7sz-aoqeivc6/bead-criterion3.json --owned /var/tmp/gc-gate-ga-36k4u7sz-aoqeivc6/owned.tsv --run-dir /var/tmp/gc-gate-ga-36k4u7sz-aoqeivc6/c3-run/evidence`. No custom local command or narrowed suite was supplied. All five phase commands force `--nocache_test_results --noremote_local_fallback`; the canonical latency split is the only test filter.

- `test_cmd_scope: full-suite`
- `test_exec_mode: remote-exec` (derived from all five BEPs, not selector intent).
- `test_exec_bep`: unit 422, acceptance 25, integration-packages 87, integration-full 14, integration-latency 1; every result PASS, remote, uncached, attempt1, inside its phase window.
- `test_counts: 157943 PASS, 0 FAIL, 617 SKIP` (Go top-level result lines across565 per-job logs; subtests counted separately only by the inventory). Non-Go/compile-only logs have terminal BEP success or native-step exit0.
- `diff_tests_executed: TestTermBeforeTheIdleReadBlocksStillEndsTheSession PASS; /piped PASS; /tty PASS`.
- `test_log_dir: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-36k4u7sz/gate-20261120-abe2gt_f/evidence/c3-run/evidence/remote/logs` — all565 logs retained.
- `waiver_ref: none`; `failure_attribution: none`. The expected negative-control failure is separate from the passing suite.
- `heavy_mode: none`; `owned_topup: none`.
- `floor_topup: 16/16 exit0` — environment-absence cases, pinned beadstest, Darwin compile and runner self-tests. No failed step was retried.
- `latency_leg: measured` — first attempt, probe clear after20s; both budget tests PASS; neither ran in integration-full. Budgets/statistic unchanged.
- `farm_test_seconds: 14597.694`; `fallback_reason: none`.
- `admission: remote, floor25GiB, cap2, waited0s`; worker pins match, certificate valid and kill switch absent (execution.json).
- `delta_gate: shadow, not applied, full suite (not-gate-pr)`.
- `ci_lane_run: n/a (no CI-config change)`.

`skip_justification`: 603 farm inventory rows plus 72 native rows (including subtests), 318 unique sites. Every site is UNCHANGED/line_in_hunk=no, outside the sole changed file; no shared/helper hunk exists and no skip is in the zcode package. The changed test body cannot be imported by these other test binaries. Printed reasons are retained (lane/environment/capability/pending-bead cases); native environment-absence repairs completed. One interleaved dispatch subtest reason omitted by the inventory parser was independently recovered from its immediately preceding raw Go log line; its original inventory remains intact. Zero unresolved or diff-owned skips; no base comparison is required without reachable shared/helper hunks.

`policy_lane`: explicit `make lint` plus the required shell guards and13 policy targets, all PASS. `drift_lane`: `make bazel-sync` + empty tracked/untracked diff and7 generated-file checks, all PASS. Format and OpenAPI compatibility also PASS in separate fresh views. Complexity uses prescribed zero-growth form(ii): base and merge checks both exit2, no worse merge row, complexity-diff prints no change rows and the checked-in baseline is untouched. This is zero-growth scoring, not a waiver or citation to the retired complexity tracker. Full commands/exit statuses are in fast-result.json and fast-independent-audit.json. Dashboard preview is not required for the sole zcode test-file diff.

## Required CI closure

`ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-validate.Hjirl9 e6e47235027b758b9a86b0ddb99fb5d3a69c59e4 fe8a991f8c379fd349ae6620157b776a7a934744 fork`; merged workflow blob `fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612`; **6 LEAF jobs**. The check aggregator gets no separate status.

| Job | Status | Command / evidence |
|-----|--------|--------------------|
| runner-policy | LOCAL-PASS | /home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-merge-validate.Hjirl9 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh --threshold 15 --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- bash /var/tmp/gc-gate-ga-36k4u7sz-aoqeivc6/ci-runner-policy.sh; exit0; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-36k4u7sz/gate-20261120-abe2gt_f/evidence/ci-required/runner-policy-output.txt |
| changes | LOCAL-PASS | python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-validate.Hjirl9 e6e47235027b758b9a86b0ddb99fb5d3a69c59e4 fe8a991f8c379fd349ae6620157b776a7a934744 fork; exit0; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-36k4u7sz/gate-20261120-abe2gt_f/evidence/ci-required/closure.txt |
| credential-provider-windows | NOT-TRIGGERED | credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true' |
| pack-gate | NOT-TRIGGERED | pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true' |
| windows-lane | LOCAL-PASS | /home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-merge-validate.Hjirl9 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh --threshold 15 --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- bash /var/tmp/gc-gate-ga-36k4u7sz-aoqeivc6/ci-windows-lane.sh; exit0; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-36k4u7sz/gate-20261120-abe2gt_f/evidence/ci-required/windows-lane-output.txt |
| windows-lane-test | DEFERRED-TO-PR-CI | reason=non-linux-runner; runner_windows=blacksmith-4vcpu-windows-2025; Windows test-binary closure reach=none |

`windows_reach: windows-lane-test = none` (ordinary and integration Windows module test-binary closures computed from its own allowlist). Runner policy resolves to Blacksmith Windows2025. Credential-provider job is NOT-TRIGGERED, not deferred.

`deferred_to_pr_ci: windows-lane-test (non-linux-runner)`

`not_covered: none`; `host_gaps: none`.

The windows-lane job ran every own workflow command: Windows allowlist build/test compilation, tools/nogo Windows build/test compilation, and all-package Darwin build/test compilation. Runner-policy ran with the actual future PR author quad341. The pack and credential-provider rows copy the canonical trigger tool's NOT-TRIGGERED results.

## Load and retained records

The canonical remote run has no host-load prewait by policy. The wrapper's host telemetry (not farm load) is recorded verbatim:

`LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=18.43 run_max_load=24.16 run_mean_load=13.41 run_readings=46 wait_first_load=18.43 wait_max_load=18.43 wait_mean_load=18.43 wait_readings=1 read_errors=0`

- `load_threshold: 15`
- `load_waited_seconds: 0`
- `load_wait_timed_out: 0`
- `run_start_load: 18.43`
- `run_max_load: 24.16`
- `run_mean_load: 13.41`
- `run_readings: 46`

CI jobs each had their own local load wait, ran one at a time, and exited0 from a clean fresh view. Their exact load summaries are retained in ci-independent-audit.json. Native floor/preflight records preserve the pinned runtime/image, ICU74 provenance and environment checks.

Policy sources are the supplied release-gate-criteria protocol, canonical TESTING.md and original build/review acceptance criteria; the prompt's named docs/PROJECT_MANIFEST.md is absent from this checkout.

Full runtime execution retained: 6051 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-36k4u7sz/gate-20261120-abe2gt_f/evidence/c3-run/evidence/execution.json; SHA256 a6bb0f291695a0e2d04dc29e0a594bd4c092500e0931818490f6761c8f770fa6.

Full raw five-phase audit retained: 81 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-36k4u7sz/gate-20261120-abe2gt_f/evidence/five-phase-raw-audit.json; SHA256 d65af452ece89554dc06ad9c49870ae0324333057bde6007c721e572e75bfb6c.

Full skip inventory retained: 4826 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-36k4u7sz/gate-20261120-abe2gt_f/evidence/c3-run/evidence/remote/skip-inventory.json; SHA256 1ff4090e7bc255a57be0482b36381617c1a6ff5e06065f612673ab38657cb048.

Full skip-site proof retained: 7392 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-36k4u7sz/gate-20261120-abe2gt_f/evidence/skip-independent-audit.json; SHA256 3ccebf96a2cc3b10f3ee46dd2b3e2bf3b00cade81d2e00c0e40d6db985367a09.

Full native floor retained: 1056 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-36k4u7sz/gate-20261120-abe2gt_f/evidence/c3-run/evidence/remote/floor-topup.json; SHA256 5d9c9fd576fbbb18cf98d73926b4ffd7f122a200d27e1448f1ef4c47672b9295.

Full by-name verification retained: 7 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-36k4u7sz/gate-20261120-abe2gt_f/evidence/by-name-independent.txt; SHA256 46d96f35581a70493f25c28e7bbee0ba898eea897c5998df47ac7fc92b4ad653.

Full CI accounting retained: 105 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-36k4u7sz/gate-20261120-abe2gt_f/evidence/ci-independent-audit.json; SHA256 0e60ca1cda208589767a198336e1052dd7f4678c088f01d17fe36686b99a5805.

Full negative control retained: 35 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-36k4u7sz/gate-20261120-abe2gt_f/evidence/c2-independent-audit.json; SHA256 5027371bdd09e0e86102f0926191edb73a8569fcdce9ca31c5c2e02adabc6bb6.

Compact canonical execution record:

```json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/gc-gate-ga-36k4u7sz-aoqeivc6/c3-run/evidence/remote","gate_base":"e6e47235027b758b9a86b0ddb99fb5d3a69c59e4","gate_head":"fe8a991f8c379fd349ae6620157b776a7a934744","admission":{"class":"remote","floor_gib":25.0,"free_gib":64.7712631225586,"waited_seconds":0.0,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"},"delta_gate":{"mode":"shadow","decision":"full","reason":"not-gate-pr","detail":"no PR record (pr.json)","applied":false,"base":"e6e47235027b758b9a86b0ddb99fb5d3a69c59e4","head":"fe8a991f8c379fd349ae6620157b776a7a934744","merge":"505927bcf1e6b485ac6893af19f17d4538b9f048","prior":null,"delta":{},"changed_labels":[],"changed_label_count":0,"tests":{},"phases":{},"plan_sha256":"ebba83a43fcc0c36aebe99ee515962eca4772142324e7a3fd6b186ae7e022897"},"test_exec_bep":{"unit":{"results":422,"remote_executed":422,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":87,"remote_executed":87,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":14,"remote_executed":14,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":14597.694,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=5fd31cdd75b202be3ab9de14bb2514f059b7969d1f14ab7fdc392c2c723e6b7b"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":127.003,"evidence_errors":[],"probe":["GATE_FARM_PROBE waiting waited=0s load1=63.62 load5=79.53 load15=39.51 cpus=32 threshold=50 max_wait=600s","GATE_FARM_PROBE verdict=clear waited=20s load1=46.52 load5=74.59 load15=38.73 cpus=32 threshold=50 max_wait=600s"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 33.63 66.39 38.00 40/1546 56192"]}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":38722}},"log_dir":"/var/tmp/gc-gate-ga-36k4u7sz-aoqeivc6/c3-run/evidence/remote/floor-logs"},"skip_inventory":{"counts_by_reason_class":{"other":513,"environment-absence":90},"top_level_counts_by_reason_class":{"other":255,"environment-absence":43},"tests":{"elided_array":{"items":603,"bytes":216937}}},"test_cmd":{"elided_array":{"items":5,"bytes":2064}},"test_counts":{"PASS":157943,"FAIL":0,"SKIP":617},"diff_tests_executed":"diff_tests_executed:\n  TestTermBeforeTheIdleReadBlocksStillEndsTheSession PASS (unit-88afabc17a014d82f49d.log)\nGATE_TEST_EVIDENCE_RESULT ok: 1 diff-owned tests verified by name\n"}
```
