# Supervisor identity after a drift restart — release gate

**Verdict:** **PASS**

Gate for `ga-5lydhrdn`; build `ga-bzo6emce`; review `ga-aa6dq312`; designated condition `ga-ai1gn9p5`. Completed 2026-11-21T17:05:39.745820+00:00.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS | PASS | Reviewer verdict names the resolved source below; no carryover or reviewer-added test exception. No blocking findings. |
| 2 | Acceptance criteria | PASS | Three deterministic regressions pass by name; whole cmd/gc and integration coverage pass. Direct and systemd restart regressions pass. RED source/test identity, command references and scope checked; see acceptance evidence below. |
| 3 | Tests and required lanes | PASS | Canonical five-phase full suite plus all 16 native floor steps; 158224 PASS, 0 FAIL, 618 SKIP top-level events. All owned tests pass. Fast lanes, skip provenance and all six ci-required leaf jobs accounted below. |
| 4 | Unresolved HIGH findings | PASS | Zero; reviewer style/security/spec notes read in full. The HTTP test-server ledger increase 1→2 was explicitly reviewed. |
| 5 | Clean final branch | PASS | Source and canonical merge views are clean; publication requires a clean isolated deploy branch containing only the gate commit above the reviewed source. |
| 6 | Merge state and build/vet | PASS | Clean frozen merge; whole-tree Bazel build and nogo succeed, raw terminal events audited. |
| 7 | Single feature theme | PASS | Supervisor identity after drift restart; two cmd/gc files plus their reviewed resource ledger. Full ancestry denylist/citation check passed for the deploy/build/tracker IDs. |

## Immutable source and merge

- deploy_mode: remote; base_branch: main; push_remote: fork.
- reviewed_source: `66e93e5e857daad8ae63399091d3601e0c94ae87`.
- gate_base: `76985b6f3671f158a2af747d4f0d0da86190d5ec`.
- gate_base_kind: current-tip (frozen when criterion 6 ran).
- gate_head: `d605c6c295d5fbe02b624baf505e81827416208e`; gate_tree: `384c933d7abb41e71434ddb4362985d23678b340`.
- merge_state: clean (origin/main@76985b6f3671f158a2af747d4f0d0da86190d5ec; files: none).
- merge_validation: canonical raw merge-tree, resolved parents and tree identity audited; later base movement/conflict is settled and validated by the merge authority under the merge-time guarantee. No rebase or merge is performed here.
- evidence_root: `/var/tmp/gc-gate-ga-5lydhrdn-fresh-20261121-3q926ik4`. Raw BEP, CAS logs, telemetry and comparison logs are retained. Earlier incomplete TIMEOUT evidence remains failed historical evidence; none is credited to this run.

## Acceptance evidence

`acceptance-integrity.json` resolves RED, original base and GREEN; RED production equals its original base, while RED tests and resource ledger equal GREEN. Independent reviewer RED receipts show the three expected behavioral failures with a passing existing control. This gate independently verifies GREEN in the canonical full suite.

- `TestRunStartDriftCheck_RestartRePrintsIdentityAfterTransientStatusFailure`: PASS in the full unit and integration-package logs.
- `TestRunStartDriftCheck_RestartRePrintsIdentityWhenControlSocketLagsHealth`: PASS in the full unit and integration-package logs.
- `TestRunStartDriftCheck_RestartWarnsWhenReplacementNeverAnswersSocket`: PASS in the full unit and integration-package logs.

`TestStartDrift_DirectLaunch_RestartsToNewBuildID` passes in full integration; `TestStartDrift_SystemdManaged_RestartsToNewBuildID` passes in native floor step 12 (30.00s; package exits 0). Whole cmd/gc coverage and nogo pass; `make check` and its shell guards are covered by the unit/fast policy lanes. Source commit message cites both build and condition IDs. `awaitRestartedSupervisorIdentity` retains a confirmed PID, polls socket/status and reports its final obstacle; the restart continues normal registration. The resource ledger adds one HTTP test server. No supervisor start-order, delegated restart, PollReady, integration assertion or complexity-baseline change.

## Criterion 3: complete test evidence

- test_cmd: `/home/jaword/projects/gc-management/packs/actual/all/scripts/gate-criterion3.sh --repo /var/tmp/gc-gate-ga-5lydhrdn-fresh-20261121-3q926ik4/merge --base 76985b6f3671f158a2af747d4f0d0da86190d5ec --bead-json /var/tmp/gc-gate-ga-5lydhrdn-fresh-20261121-3q926ik4/bead-criterion3.stdout --owned /var/tmp/gc-gate-ga-5lydhrdn-fresh-20261121-3q926ik4/owned.tsv --run-dir /var/tmp/gc-gate-ga-5lydhrdn-fresh-20261121-3q926ik4/c3-run/evidence` (cwd `/var/tmp/gc-gate-ga-5lydhrdn-fresh-20261121-3q926ik4/merge`); exact argv/cwd retained in `c3-command.json`. Unit `make -k check` (`bazel test //...` plus shell guards); acceptance and integration-package Bazel targets; full integration excluding the dedicated latency names, followed by the dedicated latency leg. All phase argv retained with the BEPs.
- test_cmd_scope: full-suite.
- diff_tests_executed: all three tests listed in Acceptance evidence PASS in both full unit and integration-package phases.
- test_exec_mode: remote-exec (derived from BEP); all five phases have remote strategy, no cached flags, attempt 1 and starts inside their phase windows. The documented native floor is additive, not a stitched replacement.
- test_counts: 158224 PASS, 0 FAIL, 618 SKIP (column-zero top-level events summed across retained job logs; repeated execution counts once per job).
- test_log_dir: `/var/tmp/gc-gate-ga-5lydhrdn-fresh-20261121-3q926ik4/c3-run/evidence/remote/logs`; `gate-test-evidence.py` independently verifies all three diff-owned names; `final-by-name.stdout`.
- test_exec_bep: `/var/tmp/gc-gate-ga-5lydhrdn-fresh-20261121-3q926ik4/c3-run/evidence/remote/<phase>.bep.json`; `remote-independent-audit.json` independently verifies all 1104 retained test.log/test.xml CAS hashes.
- farm_test_seconds: 14114.943.
- rbe_state: eligible own-code; worker environment matches frozen origin/main; client certificate valid through Jan 9 2027. Runtime ICU74, pinned bd1.3.4, rootless Podman socket/cached pinned image, sweep and disk checks passed.
- admission: remote class, cap 2, floor 25 GiB, free 52.4177 GiB, wait 0.002s; released after completion.
- fallback_reason: none; heavy_mode: none; owned_topup: empty (all owned names already pass).
- floor_topup: all 16 steps exit 0; beadstest, Darwin compile, runner self-tests and every detected environment-absence package included. Per-step commands/logs retained in `remote/floor-topup.json`.
- latency_leg: measured, probe clear on first attempt; both configured latency names PASS; main integration leg ran neither. `remote/latency-leg.json`.
- delta_gate: shadow only; decision full, not-gate-pr; no affected-only credit.
- waiver_ref: none; failure_attribution: none; ci_lane_run: n/a (no CI configuration changed).

| Phase | Executed shards | Result |
|---|---:|---|
| unit | 425 | PASS, uncached remote attempt 1 |
| acceptance | 25 | PASS, uncached remote attempt 1 |
| integration-packages | 87 | PASS, uncached remote attempt 1 |
| integration-full | 14 | PASS, uncached remote attempt 1 |
| integration-latency | 1 | PASS, uncached remote attempt 1 |

### Fast lanes and host load

- policy_lane: `fast-lane.sh policy` through fresh-tree/gate-base/isolation wrappers; 13 Bazel policy targets and every `make check` shell guard PASS. Build/nogo also PASS. Trusted-base storeclass baseline/growth predicates PASS with no raise flag or label.
- drift_lane: `fast-lane.sh drift`; 7 generated targets, `make bazel-sync`, empty diff/status PASS in its own fresh view. Format 1 and OpenAPI 1 target PASS. Exact commands, BEP hashes and fresh-view identities are in `fast-verified.json`; baseline remains untouched.
- The fast lane completed before runtime setup and the suite. Each write-capable lane used a separate fresh view.
- Native floor load: `threshold=15 waited_seconds=540 wait_timed_out=0 run_start_load=14.81 run_max_load=16.94 run_mean_load=15.92 run_readings=8 wait_first_load=18.13 wait_max_load=21.63 wait_mean_load=18.89 wait_readings=19 read_errors=0`. Thus load_threshold=15, load_waited_seconds=540, load_wait_timed_out=0, run_start_load=14.81, run_max_load=16.94, run_mean_load=15.92, run_readings=8. Remote phases use remote admission, with no local heavy slot/load wait. Supplemental base comparisons keep their own load records.

### SKIP provenance

`skip_inventory`: `remote/skip-inventory.json` and `native-skip-proof/all-native-skips.json`; every reason/log is retained. No owned test skipped or failed. The native floor repaired environment gaps including the systemd drift regression; remaining native non-owned skips are inventoried.

All 604 farm SKIP events were checked at 319 unchanged source sites (`remote-skip-sites-plan.json`, `remote-skip-sites.stdout`). The dispatch subtest's blank parser reason was recovered from its unchanged raw interleaved log (call_budget_test.go:537). Shared/import/helper hunks exist only in cmd/gc: the sync/atomic import, restartedSupervisorFixture and assertPostRestartIdentity. Their existing unit 193 events/157 roots and integration 68 events/36 roots match same-command frozen-base comparisons; the native cmd/gc comparison also passes. These helpers are used only by the new regressions. The 41 other skipped packages' combined test import closure (164 module directories) excludes cmd/gc; the new test helpers never enter gc binaries spawned by those tests. Their unchanged skip branches cover platform exclusions, helper-only processes, unsupported provider operations, opt-in live/legacy scenarios and existing tracked restrictions. Printed reasons and per-site proofs are retained in the inventory; none hides this change's restart behavior.

The native cmd/gc proof compares all 36 SKIP events under six roots at the same pinned base, using the native floor's unchanged command; all 36 are pre-existing. Supplemental base comparison load: `threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=56.67 run_max_load=80.96 run_mean_load=70.55 run_readings=4 wait_first_load=15.57 wait_max_load=68.61 wait_mean_load=37.92 wait_readings=61 read_errors=0`. Its ordinary bounded wait expired, then the comparison completed with exit 0. This is SKIP provenance only and does not replace any phase of the canonical suite.

The unit proof compares exact Bazel-label partitions: gc_test 189/189 and gc_sim_exec_test 4/4, covering all 193 events. The first comparator returned 1 because it mixed these variants; its raw logs remain preserved. The corrected comparison changed no test verdict and retried no failed test. A separate evidence-directory preflight refused before tests and was corrected; that diagnostic is also retained.

Comparison notes (raw logs unchanged):

```text
COMPARE-NOTE the remote worker's account differs between the runs in 1 SKIP(s), read as the same reason because the account is the only difference (an account is rbe-, a letter and two digits, standing alone; the logs are not changed): TestInfraScopedManagedDoltPIDIdentityUnderRealSystemd (base rbe-a00, head rbe-a10)
```

### ci-required accounting

ci_required_closure: `python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-gate-ga-5lydhrdn-fresh-20261121-3q926ik4/merge 76985b6f3671f158a2af747d4f0d0da86190d5ec 66e93e5e857daad8ae63399091d3601e0c94ae87 fork`; merged workflow blob `fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612`; 6 LEAF jobs need a status. `check` is an aggregator. Linux windows-lane commands run sequentially in a fresh isolated load-gated view; exact steps, environment and terminal exit 0 retained in ci-required/windows-output.stdout. Trigger rows and runner-policy outputs are retained.

| Job | Status | Evidence |
|---|---|---|
| runner-policy | LOCAL-PASS | `EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=/var/tmp/gc-gate-ga-5lydhrdn-fresh-20261121-3q926ik4/ci-required/runner-policy.out python3 .github/workflows/scripts/runner_policy.py`; exit 0; `/var/tmp/gc-gate-ga-5lydhrdn-fresh-20261121-3q926ik4/ci-required/runner-policy.stdout`. |
| changes | LOCAL-PASS | `python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-gate-ga-5lydhrdn-fresh-20261121-3q926ik4/merge 76985b6f3671f158a2af747d4f0d0da86190d5ec 66e93e5e857daad8ae63399091d3601e0c94ae87 fork`; exit 0; `/var/tmp/gc-gate-ga-5lydhrdn-fresh-20261121-3q926ik4/ci-required/closure.stdout`. |
| credential-provider-windows | NOT-TRIGGERED | credential_provider filter no match; tool row retained in closure.stdout |
| pack-gate | NOT-TRIGGERED | packs filter no match; tool row retained in closure.stdout |
| windows-lane | LOCAL-PASS | Job commands in `ci-required/windows-lane.sh` (Windows builds/test compilation, nogo analyzers, Darwin build/test compilation); exit 0; `/var/tmp/gc-gate-ga-5lydhrdn-fresh-20261121-3q926ik4/ci-required/windows-output.stdout`. |
| windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | /var/tmp/gc-gate-ga-5lydhrdn-fresh-20261121-3q926ik4/ci-required/windows-reach.json |

- windows_reach: none, rc0; computed from this windows-lane-test job's own Windows allowlist/test import closure (`ci-required/windows-reach.json`), not the credential-provider-only helper.
- deferred_to_pr_ci: windows-lane-test (non-linux-runner).
- not_covered: none; host_gaps: none.
- This PASS authorizes PR opening. Merge requires `CI / required` green on the current PR head and the deferred native Windows job successful (or legitimately path-filter skipped). Clearance is published only on the exact verified published head before the merge-request.
