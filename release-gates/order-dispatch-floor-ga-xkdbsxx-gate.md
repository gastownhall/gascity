**Verdict:** **PASS**

# Doctor order-dispatch estimate

Bead: ga-xkdbsxx; build: ga-c5se4z; reviewer: ga-n1zd0ys.
Completed: 2026-11-07T04:54:59.085089+00:00.

- Reviewed source: `db56e8623bc5cf1b05e5106adf6ddb558c41f96a` (resolved in this repository; provenance builder branch is not a push target).
- Frozen validation base: `6c9663d4ca4c0c32196dcfb435d641fe387084d2`; `gate_base_kind: current-tip`.
- Validation merge: `a5f793faeaada754634be77862fe850c39b99b5f`; tree `bdd10f29795577e9eaff3ac2558ad2a6d569e03f`.
- `merge_state: clean (origin/main@6c9663d4ca4c0c32196dcfb435d641fe387084d2)`.
- `merge_validation: predicted and actual merge trees match; full build/nogo and tests validate this merge; merge authority checks the eventual merge result`.
- Publication snapshot: base `986e25dd2b7b0cacb4e47d29f30c0bbdc8e76e28`, merge state `clean` at 2026-11-07T04:54:39.172343+00:00; no rebase or test credit for this later snapshot.
- Isolated publication branch: `deploy/ga-xkdbsxx-gate`, cut exactly at the reviewed source; only this checklist is added.
- Durable raw evidence: `/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-xkdbsxx/gate-20261107-ol1upa`. Commands, original logs, BEP, XML, hashes and comparison proofs are retained there; no historical gate result is reused.

## Criteria

| # | Result | Evidence |
|---|---|---|
| 1 Review PASS | PASS | ga-n1zd0ys passed the exact reviewed source; full review read. No carryover, reviewer-added commit or substituted branch tip. |
| 2 Acceptance | PASS | All13 added/changed test roots PASS by name: interval boundary, configured budget/patrol, counted/excluded triggers, suspended/inapplicable orders, advisory/nonfixable registration and ordinary doctor output. Existing JSON-success, check-registration and TestTutorial01 checks PASS separately. Tutorial changes accompany the check. Dispatch budget extraction preserves the old positive-value-or-default4 predicate. Ring-size proxy and expected stock-city warning are disclosed in the PR. |
| 3 Tests and CI | PASS | Complete5-phase fresh remote chain and16-command native floor, fast policy/generated lanes and4-leaf CI accounting complete below. All owned tests execute and PASS; Windows job deferred under the documented non-Linux rule. No failure attribution or waiver. |
| 4 Review findings | PASS | Zero unresolved HIGH findings. Five minor review findings remain informational; no deployer source edits. |
| 5 Clean branch | PASS | Source and validation scratch clean; checklist is the sole bookkeeping change. Normal-hook commit and clean readback required before publication and recorded on the bead. |
| 6 Merge state/build | PASS | Normal-hook merge matches predicted tree; full Bazel build/nogo of1456 targets PASS at frozen base. Preliminary normal pre-commit compilation was local; the complete authoritative build explicitly uses remote-exec with no local fallback. Later base advancement is recorded separately. |
| 7 Single theme | PASS | One doctor dispatch estimate plus its budget-default extraction, documentation and tests. Ancestry guard accepts ga-xkdbsxx/ga-c5se4z; no unrelated commit, denied path or stack. |

## Acceptance and limitations

The diagnostic counts enabled cooldown, cron and event orders. Condition/manual/webhook orders, suspended-rig orders and backend-inapplicable orders are excluded. It uses ceil(ring/budget) times the configured patrol interval, reports warnings in ordinary output, is advisory on every path, cannot auto-fix, and is excluded from warmup. It neither changes scheduling behavior nor raises the budget.

Ring size is an upper-bound proxy for the live due ring: the estimate assumes all counted orders are due together and can flag an interval the actual dispatcher meets. A standing warning on a stock city is expected. Read-only bundled-core inventories give16 orders/2m at the reviewed source and18 orders/2m30s at the frozen validation merge with default budget4/patrol30s. The existing30s beads-health/gate-sweep and1m order-tracking-sweep intervals fall below that estimate. Doctor's exit code remains unchanged.

Builder recorded an uncommitted compile-failure RED before GREEN; the normal nogo hook refused committing undefined symbols. No separate RED SHA is claimed or invented. The review accepted this evidence. docs/PROJECT_MANIFEST.md is absent; current TESTING.md/Makefile plus the mandatory pack criteria define the available release checks.

## Criterion3 runtime

`test_cmd_scope: full-suite`; `test_exec_mode: remote-exec`; `heavy_mode: none`.

Actual durable launcher:

```text
env GC_GATE_REMOTE_ENABLED=1 GC_REMOTE_GATE_FLOOR_GIB=25 GC_REMOTE_GATE_CAP=2 ISOLATED_TEST_BEAD=ga-xkdbsxx DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true BEADS_ALLOW_UNREAPED_TESTCONTAINERS=1 /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-criterion3.sh --repo /var/tmp/gc-gate.ga-xkdbsxx._ol1up_a/merge --base 6c9663d4ca4c0c32196dcfb435d641fe387084d2 --bead-json /var/tmp/gc-gate.ga-xkdbsxx._ol1up_a/bead-c3-2.json --owned /var/tmp/gc-gate.ga-xkdbsxx._ol1up_a/owned.tsv --run-dir /var/tmp/gc-gate.ga-xkdbsxx._ol1up_a/c3-2/evidence
```

All5 phases force `--nocache_test_results --noremote_local_fallback`, with remote-exec, ci, keep_going, verbose test-wrapper output, top-level artifact download and a separate BEP per phase. Unit executes the complete make-check chain; acceptance covers acceptance_test and acceptance_solo_tests; integration packages cover integration_packages; full integration covers every integration_test shard except the registered latency tests, measured in their own final leg. Expanded exact commands are retained in execution.json and phase stdout; no focused diagnostic replaces this full chain.

| Phase | Fresh remote result executions | Result |
|---|---:|---|
| unit | 395 | PASS |
| acceptance | 25 | PASS |
| integration-packages | 86 | PASS |
| integration-full | 14 | PASS |
| integration-latency | 1 | PASS |
| Total | 521 | PASS |

`test_exec_bep`: all521 result identities are strategy remote, neither cached flag, attempt1, start inside the phase window, terminal PASSED. Independent audit re-read every result and coupled all1042 log/XML IDs and CAS URI hashes/lengths to their retained bytes. No retry, cache credit, mixed strategy or local fallback. `farm_test_seconds: 12349.09`.

`test_counts: PASS=147990 FAIL=0 SKIP=512`: top-level Go result lines summed across537 logs, once per job, not unique tests. 470 logs contain Go result lines; the other67 are policy/non-Go/self-test logs with their structured result or command exit verified.
`test_log_dir: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-xkdbsxx/gate-20261107-ol1upa/c3-2/evidence/remote/logs` (original directory: `/var/tmp/gc-gate.ga-xkdbsxx._ol1up_a/c3-2/evidence/remote/logs`).

`diff_tests_executed`:

- TestOrdersConfig_EffectiveMaxDispatchesPerTick — PASS
- TestOrderDispatchFloor_EventOrdersOccupyTheRing — PASS
- TestOrderDispatchFloor_FlagsCronBelowFloor — PASS
- TestOrderDispatchFloor_FlagsIntervalBelowFloor — PASS
- TestOrderDispatchFloor_HonorsConfiguredBudget — PASS
- TestOrderDispatchFloor_InapplicableOrdersDoNotOccupyTheRing — PASS
- TestOrderDispatchFloor_IntervalAtOrAboveFloorIsNotFlagged — PASS
- TestOrderDispatchFloor_NeverReadsAsAFixableCheck — PASS
- TestOrderDispatchFloor_NothingToCheckIsOK — PASS
- TestOrderDispatchFloor_RingCountsOnlyBudgetedTriggers — PASS
- TestOrderDispatchFloor_SkipsSuspendedRigOrders — PASS
- TestOrderDispatchFloor_SurfacesInNonVerboseDoctorOutput — PASS
- TestOrderDispatchFloor_UsesConfiguredPatrolInterval — PASS

Required unchanged neighbors, separate from the13 owned roots: TestBuildDoctorChecks_NameSetUnchanged, TestDoctorJSONSuccessIsParseableJSONOnly and TestTutorial01 all PASS by name in the GC_FAST_UNIT=0 integration-package lane.

`latency_leg`: complete, measured attempt1; TestGcBdLatencyBudget and TestGcSeatHookLatencyBudget PASS. Clear probe after40s: load1=47.11, load5=88.45, CPUs32, threshold50. Neither test ran in main integration. No latency retry/deferral.

`admission`: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 45.903953552246094, "waited_seconds": 0.001}.
`rbe_state`: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98", "worker_env_tree": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"}.
`fallback_reason: none`; `owned_topup: none`.

ICU74 check OK with recorded provenance; pinned bd1.3.4/ref7ef2fd05e and rootless Podman socket ping verified before assertions. No actual Go testcontainers/ContainerRequest dependency; native Dolt/tool dependencies checked. Wrapper reports PASS-THROUGH, not isolation; farm is hermetic and the native floor uses canonical environment scrubbing/load admission. Remote rollout enablement was inherited, not enabled by this session.

`floor_topup`: complete,16/16 command exits0:13 package groups plus Darwin fsys compile, push-gate-lock and local-concurrency self-tests. Native systemd restart test PASS, restart6.8s within15s. Actual native load:

```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=360 wait_timed_out=0 run_start_load=14.45 run_max_load=15.79 run_mean_load=14.46 run_readings=8 wait_first_load=28.89 wait_max_load=28.89 wait_mean_load=20.94 wait_readings=13 read_errors=0
```

The wait cleared after360s; it did not time out. The outer remote driver also emitted observational load readings while its farm actions ran; those are not the native floor's admission measurements.

`skip_inventory`: 498 farm and39 native SKIP events retained with names/reasons/logs. Package-aware combined inventory has159 events covered by actual PASS of the same full name and378 residual events; all residual skip sites map,142 sites read line_in_hunk=no/UNCHANGED, and78 source files have identical base/merge blobs. Shared hunk: internal/doctor/checks_order_dispatch_floor_test.go imports plus the new private fixture helpers orderDispatchFloorCity, writeOrderDispatchFloorFillers and runOrderDispatchFloorTest. No HELPER-HUNKS. Test-binary import reach is cmd/gc, test/acceptance and test/integration. Focused probes on the untouched pinned base, with matching phase configurations and root selection, reproduce all80 judged farm SKIPs with identical reasons: unit26, integration-packages27, integration-full13, acceptance14. All11 base result identities are fresh/first-attempt/remote/PASSED; all22 raw log/XML hashes/lengths are coupled to BEP. Remaining native top-up SKIPs are explicitly inventoried (optional tools/corpus/DSN, platform/capability guards and subprocess helpers); none is owned. New private doctor test fixtures cannot change these guards. Other packages do not import doctor and have no shared/helper hunk; unchanged skip source and explicit reasons are retained. No skip is credited as an owned PASS.

Caller setup error preserved: initial c3 input was a JSON object instead of the bd-show array, selecting local mode. Exact process evidence proves it was stopped in admission/load prewait before any assertion, BEP or verdict. The corrected raw array started a new c3-2 evidence directory; no assertion rerun or failure escape. Original input/logs/proof remain retained. An initial CI planning assertion incorrectly expected NOT-TRIGGERED; the actual closure corrected this before any job result was claimed.

`failure_attribution: none`; `waiver_ref: none`; `ci_lane_run: n/a (no CI-config change)`.

## Fast lane

Full build/nogo PASS; four make-check shell guards PASS; pinned formatter2.14.0 equals resolved version, mismatch=false; all21 policy/format/docs/resource targets PASS. Actual make bazel-sync followed by empty tracked/untracked drift PASS; OpenAPI and generated-client sync PASS. Lanes each ran in their own fresh view and recorded the frozen gate base, including LINT_CHANGED_REF for changed-format. Scratch remained clean. No complexity-check acceptance clause applies.

## CI-required accounting

`ci_required_closure`: `python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-gate.ga-xkdbsxx._ol1up_a/merge 6c9663d4ca4c0c32196dcfb435d641fe387084d2 db56e8623bc5cf1b05e5106adf6ddb558c41f96a fork`; exit0; workflow blob `e5b11c348505dab28dc223e874bc2c61cbbb33c5`; `4 LEAF jobs need a status`. Shared config filter matched, so suite_mode=full and both conditional jobs trigger. Runner-policy outputs resolve Linux to Blacksmith Ubuntu24.04 and the Windows expression to blacksmith-4vcpu-windows-2025. check/ci-required are aggregators and receive no separate status.

| LEAF | Status | Evidence |
|---|---|---|
| changes | LOCAL-PASS | Actual closure command exit0, ci-accounting/closure.txt. |
| runner-policy | LOCAL-PASS | Job's Python command in merge scratch, EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH empty and private GITHUB_OUTPUT; exit0, runner-policy.log/out. |
| credential-provider-windows | DEFERRED-TO-PR-CI reason=non-linux-runner | windows_reach.sh with frozen base/source exit0/no stdout; no changed path in that job's build closure. |
| pack-gate | LOCAL-PASS | Exact workflow run scripts/env, pinned-check then GC_TEST_GASCITY_PACKS_REGISTRY=main make test-pack-registry-live; detached fresh view at validation merge, load/isolation wrappers; exit0 and CI_PACK_GATE_PASS. Raw stdout/stderr and command plan in ci-accounting. |

Pack host probe: timeout30 git ls-remote https://github.com/gastownhall/gascity-packs.git HEAD; exit0, stderr empty. Successful probe required running the job locally. Both workflow steps completed on their first attempt; complete logs contain no failed command or test result.

```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=28.34 run_max_load=29.46 run_mean_load=27.32 run_readings=8 wait_first_load=38.27 wait_max_load=74.96 wait_mean_load=59.44 wait_readings=61 read_errors=0
```

`windows_reach: none`; `deferred_to_pr_ci: credential-provider-windows (non-linux-runner)`; `not_covered: none`.
PASS authorizes opening the PR. Merge requires CI / required green on the exact PR head and the deferred Windows job successful (or skipped only when its own path filter is false). The merge authority settles any later base/conflict changes; the deployer does not merge.

## Publication hooks

The pinned reviewed source advertises retired worker-env sha256:7065d0a100bca411f10804c9ea0e2af60a34be153e94f7cb86f81135e1b98b45; the validation merge and current main advertise the served sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98. The first documentation hook was stopped while its build actions queued, before a test action started; exact owned service/PID and source/stage proof are retained. The normal documentation hook uses local Bazel fork-cache after the host-load wait. The normal push hook runs from the frozen validation merge checkout while pushing the explicitly named isolated deploy branch. Review source and its worker pin stay unchanged; no hook bypass, rebase or force push. Hook results and exact final head are verified on the bead.
