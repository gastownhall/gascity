# Closed blocker lookup release gate

**Verdict:** **PASS**

Deploy bead: ga-6x5pnqs. Build: ga-zediynd. Review: ga-5etmf1u.
Reviewed deploy source: `424a5c7f834badd85ca51f219c8dacc7aec81ce9`. Provenance branch: `builder/ga-zediynd`.
Deploy branch target: `deploy/ga-6x5pnqs-gate`; publication retains the reviewed source and adds only this record.

| # | Result | Evidence |
|---|---|---|
| 1 | PASS | Reviewer PASS on the pinned source; reviewer-added test verified independently: `129eb8915a6f9d6a5a3c13af6a5751f78be55d30 -> 424a5c7f834badd85ca51f219c8dacc7aec81ce9`, files `internal/beads/bdstore_test.go`. |
| 2 | PASS | One batched blocker show with the option separator, missing-ID wisp fallback, fail-loud lookup/decoding errors, and main's step-pass outcome at both sites. All 11 changed tests and 3 unchanged regression witnesses PASS by name. The five cmd/gc conflict files match the reviewed main parent; current main is preserved by the tested merge. |
| 3 | PASS (one pre-existing failure attributed) | Full five-phase scope, 502 uncached remote attempt-1 results. The sole failure is the existing mail-hook budget condition, attributed under criterion 3a below. All 16 mandatory native-floor steps pass; every ci-required leaf is accounted for. |
| 4 | PASS | Review contains zero unresolved high-severity findings. |
| 5 | PASS | Source worktree and canonical test scratch are clean before adding this record. Publication requires a clean isolated branch after the normal-hook record commit. |
| 6 | PASS | Normal-hook canonical merge and whole-repository build/nogo vet pass on the recorded base. Its tree equals git's predicted merge. |
| 7 | PASS | Only three internal/beads files differ from the recorded base: one blocker-lookup implementation and its tests. This record is deploy bookkeeping. Ancestry scope passed for the confirmed ready/blocker lineage. |

merge_state: clean (origin/main@86395fe7dae9c989bb899ecb963a28993ae5b640)
gate_base_kind: current-tip
gate_base_sha: 86395fe7dae9c989bb899ecb963a28993ae5b640
tested_merge: dbf766efd24647b9385fc41b431385d9fdd90dc4
tested_tree: e09a3813262553ef485726f01d7cc3db04570b64
merge_validation: the combined tree was built and tested at the frozen base; MPR must validate merge-time changes and pin this PR's exact cleared head.

No wall-clock gc ready benchmark is claimed. The acceptance evidence pins one batched show and zero whole-history blocker reads.

## Test execution

test_cmd_scope: full-suite
test_exec_mode: remote-exec
test_log_dir: /var/tmp/deploy-ga-6x5pnqs-restart.mt1md34u/c3/evidence/remote/logs
test_counts: 134246 PASS / 1 FAIL / 377 SKIP (top-level Go result lines summed across jobs, not distinct tests)
farm_test_seconds: 9080.356
heavy_mode: none
waiver_ref: none
fallback_reason: none in the final run

All results below used strategy remote, neither cache flag, attempt 1 and starts inside their recorded phase window. All 1,004 retained raw artifacts were hash-verified.

| Phase | BEP results | Verdict |
|---|---:|---|
| unit | 378 | PASSED |
| acceptance | 24 | PASSED |
| integration-packages | 85 | PASSED |
| integration-full | 14 | PASSED |
| integration-latency | 1 | FAILED (only ga-y39d8tx attributed assertion) |

test_exec_bep: per-phase JSON files under `/var/tmp/deploy-ga-6x5pnqs-restart.mt1md34u/c3/evidence/remote`; independent proof `/var/tmp/deploy-ga-6x5pnqs-restart.mt1md34u/farm-complete-execution-audit.json` and `/var/tmp/deploy-ga-6x5pnqs-restart.mt1md34u/attributed-full-suite-audit.json`.

Every phase carries `--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1` and its own BEP. Actual recorded flags:

```text
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-6x5pnqs-restart.mt1md34u/c3/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-6x5pnqs-restart.mt1md34u/c3/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-6x5pnqs-restart.mt1md34u/c3/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-6x5pnqs-restart.mt1md34u/c3/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-6x5pnqs-restart.mt1md34u/c3/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```

Commands: unit `make check`; acceptance includes both acceptance targets; integration-packages runs `//test:integration_packages`; full integration runs all `//test/integration:integration_test` shards minus the latency registry; the separate unsharded latency phase runs exactly that registry under the unchanged farm probe. The canonical runner and retained phase stdout hold the full argv.

```text
diff_tests_executed:
  TestBdStoreReadyChecksEphemeralBlockerWorkOutcome PASS (integration-packages-999a89ecc5b3d09abdd4.log, unit-7f2c404ca64cd2da0395.log)
  TestBdStoreReadyExcludesDependentWhenBlockerClosedAsWorkOutcomeBlocked PASS (integration-packages-ad823e3d7eb71467b6af.log, unit-0b74cd4d9bd61207e1fd.log)
  TestBdStoreReadyFailsLoudWhenBlockerLookupHitsInfraNotFound PASS (integration-packages-8272c8197ebafcf5385f.log, unit-364160de6f2f808aa619.log)
  TestBdStoreReadyFailsLoudWhenBlockerShowOutputIsUnparseable PASS (integration-packages-ad823e3d7eb71467b6af.log, unit-0b74cd4d9bd61207e1fd.log)
  TestBdStoreReadyFailsLoudWhenWispFallbackFails PASS (integration-packages-5a4a1a23acc13a6dd98f.log, unit-9a6a09bab414814cab53.log)
  TestBdStoreReadyKeepsDependentWhenEphemeralBlockerStepPassedDespiteWorkOutcomeBlocked PASS (integration-packages-a9e2a6fbbc2774327994.log, unit-3ee1ae2eaa26dfd63a88.log)
  TestBdStoreReadyPassesBlockerIDsToBdShowAfterDoubleDash PASS (integration-packages-8d820cd4c52192de043e.log, unit-363a36f2662d541bc75e.log)
  TestBdStoreReadyResolvesMixedBlockerBatchAcrossBothTiers PASS (integration-packages-20cfdbabba8f52b16e0b.log, unit-ea4511196531bb4b8c55.log)
  TestBdStoreReadyServesDependentWhenBlockerIsAbsentFromBothTiers PASS (integration-packages-ceb32a3c147a95796597.log, unit-b398920224b769833cd5.log)
  TestBlockerLookupsPicksOnlyTheBatchedShow PASS (integration-packages-a9e2a6fbbc2774327994.log, unit-0b74cd4d9bd61207e1fd.log)
  TestCachedReadyOverBdBackingHonorsAnEvictedBlockersWorkOutcome PASS (integration-packages-20cfdbabba8f52b16e0b.log, unit-363a36f2662d541bc75e.log)
```

Additional unchanged witnesses: main's two step-pass tests and `TestTutorial01` PASS by name. They are not claimed as changed tests. The unit-lane tutorial skip is explicitly retained beside its integration PASS.

policy_lane: format plus shell policy, nogo and required policy targets PASS; commands and fresh-view evidence in `/var/tmp/deploy-ga-6x5pnqs.ou8SKUJI/fast-lane.sh` and `fast-run`.
drift_lane: `make bazel-sync`, `git diff --exit-code`, empty tracked/untracked status; generated schema/OpenAPI/client/dashboard/lockfile targets and pinned-base OpenAPI check PASS in independent fresh views. Every fast-lane test target also executed uncached in the final unit lane, proved by `/var/tmp/deploy-ga-6x5pnqs-restart.mt1md34u/fast-targets-current-farm-proof.json`.
ci_lane_run: n/a (no CI-config change)

## Attributed failure

failure_attribution: TestGcSeatHookLatencyBudget -> ga-y39d8tx | clause 3(b) CROSS-PR

The original latency verdict remains FAILED: probe clear at load1=5.93 on32 CPUs; mail fastest-of-seven=1.322s against1s. Prime837ms, drain671ms and TestGcBdLatencyBudget PASS. `latency_leg: result=failed, complete=false` and raw `execution.json`'s `criterion3_valid=false` remain unchanged. This gate's decision applies standing criterion3a and then completes the missing native/CI checks; it does not rewrite the failed result or treat it as a busy deferral.

Clauses1/2/4: the failing function/site183 is UNCHANGED with shared_hunks_in_file=0; existing OPEN tracker predates this run; its package test/integration is disjoint from this internal/beads diff. Clause3(b): the same assertion failed under a clear probe on unrelated PR8314's canonical merge; that PR's changed cmd/gc paths share no file with this diff. The investigator also retained the same assertion failure on pristine main before this source. Both original logs were read and hash-copied into `/var/tmp/deploy-ga-6x5pnqs-restart.mt1md34u/attribution`. The pending remedy is absent from the frozen main's mail-inject path. Proof: `/var/tmp/deploy-ga-6x5pnqs-restart.mt1md34u/failure-attribution.json` and `attribution/pristine-main-provenance.json`.

No failed assertion was retried, no waiver used, and no new tracker or source change was needed. Native-floor continuation selected only the official environment-absence set and compatibility checks, not the failed latency test.

## Native floor, skips and environment

floor_topup: complete;16/16 official steps exit0, including all environment-absence packages, pinned beadstest, Darwin compile and runner self-tests. Exact commands/results: `/var/tmp/deploy-ga-6x5pnqs-restart.mt1md34u/c3/evidence/remote/floor-topup.json`.
owned_topup: none needed (all11 owned roots present and PASS in the farm run)
skip_inventory: 363 farm rows plus39 native rows (subtests included), with literal reasons and logs in `farm-skip-inventory.json` / `native-skip-inventory.json` under the evidence root. All285+36 candidate skip sites are UNCHANGED; no owned test is excused.
skip_justification: unchanged unit/process lane selection, platform/privilege and helper/re-exec conditions, optional live/provenance/replay/fixture/service/tool/locale selectors, provider capability/fencing branches and pre-existing debt guards. Native top-ups completed; remaining native skips are optional herdr activation, fake unavailable/fencing branches, absent br/PG DSN/replay/simulation artifacts, and interactive tmux context. The changed BdStore command stubs and fixture helpers alter none of those guards. The full eight-helper reach review is `/var/tmp/deploy-ga-6x5pnqs-restart.mt1md34u/manual-skip-review.json`. The separate unchanged Python TypeScript-only argv guard is proved in `python-lockfile-skip-proof.json`.

Container runtime/pinned image and pinned bd preflights completed. ICU74 loader/provenance is OK (`icu74-preflight.txt`). The isolation wrapper's scratch mode is PASS-THROUGH, not a private clone: farm execution is proven by BEP; the official native floor scrubs all live store/session selectors before each command. No private library overlay or Bazel shim was used.

```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=81.09 run_max_load=82.89 run_mean_load=78.75 run_readings=10 wait_first_load=20.51 wait_max_load=82.09 wait_mean_load=46.04 wait_readings=62 read_errors=0 prewait_s=1801
```

load_threshold: 15
load_waited_seconds: 1801
load_wait_timed_out: 1
run_start_load: 81.09
run_max_load: 82.89
run_mean_load: 78.75
run_readings: 10

admission: final farm remote25GiB/cap2; native floor local-heavy40GiB/cap3, with its standard soft load wait. Exact records `c3/evidence/remote/admission.json` and `native-floor-admission.json`.
rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98", "worker_env_tree": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"}

## Required CI accounts

ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-validate.6x5pnqs.bkaphjss 86395fe7dae9c989bb899ecb963a28993ae5b640 424a5c7f834badd85ca51f219c8dacc7aec81ce9 fork; exit0; workflow blob e5b11c348505dab28dc223e874bc2c61cbbb33c5; 4 LEAF jobs need a status. The shared filter enables both downstream outputs; runner values followed to the merged policy.

runner-policy | LOCAL-PASS | EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=/var/tmp/deploy-ga-6x5pnqs-restart.mt1md34u/ci-required/runner-policy.out python3 .github/workflows/scripts/runner_policy.py; exit0; /var/tmp/deploy-ga-6x5pnqs-restart.mt1md34u/ci-required/runner-policy.log. Read-only direct recipe from formula3e.
changes | LOCAL-PASS | closure/path matcher command above; exit0; /var/tmp/deploy-ga-6x5pnqs-restart.mt1md34u/ci-required/closure.txt.
credential-provider-windows | DEFERRED-TO-PR-CI reason=non-linux-runner | credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'; windows_reach exit0, empty output.
pack-gate | LOCAL-PASS | workflow run blocks, verbatim in /var/tmp/deploy-ga-6x5pnqs-restart.mt1md34u/ci-required/pack-gate.sh; detached/fresh-view/load15/max1800/isolation; exit0; /var/tmp/deploy-ga-6x5pnqs-restart.mt1md34u/ci-required/pack-gate-output.txt.

check is AGGREGATOR and gets no separate status.
windows_reach: none (exit0, empty output)
deferred_to_pr_ci: credential-provider-windows (non-linux-runner)
not_covered: none

```text
FRESH_TREE_RECORD from=/var/tmp/gc-merge-validate.6x5pnqs.bkaphjss head=dbf766efd246 tree=e09a38132625 dir=/var/tmp/gc-fresh-tree.uesJpS modified=0 untracked=0 ignored=6 ignored_names=.bazelrc.local,.github/workflows/scripts/__pycache__/,bazel-bin,bazel-gc-merge-validate.6x5pnqs.bkaphjss,bazel-out
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1800 wait_timed_out=1 run_start_load=101.15 run_max_load=103.12 run_mean_load=101.10 run_readings=5 wait_first_load=65.15 wait_max_load=120.26 wait_mean_load=82.60 wait_readings=61 read_errors=0
```

The Windows job must succeed on the PR head, and CI / required must be green before MPR merges. Gate PASS permits PR-open, not merge.

## Retention and provenance

Final evidence: `/var/tmp/deploy-ga-6x5pnqs-restart.mt1md34u` (retention-pinned). C6/fast prerequisites: `/var/tmp/deploy-ga-6x5pnqs.ou8SKUJI`. Original abandoned farm/local attempts remain there and receive zero result credit. Restart authority gm-wisp-fthdu0d required the same pinned source/tree, remote first and new evidence; that is the run above. Every failed raw log/BEP/XML remains intact.

Complete check/audit receipts: `attributed-full-suite-audit.json`, `manual-skip-review.json`, `failure-attribution.json`, `ci-required/accounting.md`. Test/skip inventories and commands are referenced rather than inlined in this release record.
