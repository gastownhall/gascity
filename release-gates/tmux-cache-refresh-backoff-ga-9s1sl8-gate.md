# Tmux cache refresh backoff — ga-9s1sl8

**Verdict:** **PASS**

Disposition: **PASS**. The fast lane, all 40 full local jobs, all 10 normal Go push-hook jobs and the exact-source draft CI run passed. All 23 owned tests independently PASS by name. The actual workflow closure accounts every one of its 22 leaf jobs: 13 CI-PASS, one original host-gap deferral whose CI succeeded, four NOT-TRIGGERED, three PUSH-ONLY and K8s NOT-COVERED. PASS permits release PR-open; MPR must verify CI / required and the deferred lane on the current published head before merge. This record is the only publication commit added to the reviewed source; it changes no implementation or test code.

Reviewed source: `2acc8d4cb33878180ddc966784e86c70a2dff447`.
Pinned base: `5d2fbd170b0d00bc86409cb66150feddb395e03a`.
Materialized merge: `66e635a2889ca5f2c4e563e12a80afa6a5ed7c7e`.
Merge tree: `927864709107cc8d629d238baa08841b669edd6c`.
Scratch: `/var/tmp/gc-merge-ga-9s1sl8.jOoyTU`.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | ga-js44zp records PASS on the resolved source; the accepted delayed-recovery residual and prior finding are disclosed. |
| 2 | Acceptance criteria met | PASS | Independent code inspection and all 23 owned tests confirm 2/4/8/15s refresh holds bounded by staleTTL/2, the stale-cliff clamp, one permit per newer generation, success reset, overlapping-fetch guards, failure/recovery logging and manifest coverage. The caller audit and accepted residual are preserved verbatim in the prepared PR body. |
| 3 | Tests pass | PASS | Fast lane and all 40 full local jobs PASS; PASS=57971 FAIL=0 SKIP=235, all 23 owned tests PASS. Exact-source CI run37572755795 completed success, including all 14 integration rows, all 12 process shards and both acceptance lanes. Every actual required leaf is accounted below; CI / required success. |
| 4 | No high-severity review findings open | PASS | ga-js44zp security_findings is none; accepted residual remains explicit. |
| 5 | Final branch is clean | PASS | No source/index changes beyond this gate artifact. The two other records are preserved in a verified exact-path stash. Normal commit is followed by a mandatory empty git-status check before push and clearance; only this file enters the publication commit. |
| 6 | Branch diverges cleanly from main | PASS | Textual merge and materialized tree agree against the pinned base. Fresh-view compile/vet PASS. Final current-main merge-tree check passed against b72bd3f7859d62560c247fede2d37d0ca2dcbb4d, tree9a8851299346b16a95d1cc18f75039fd8a4ef698. Actual PR-merge CI compile/static/generated jobs passed on its merge tree. A last fetch/check precedes clearance. |
| 7 | Single feature theme | PASS | Six files implement one cache-backoff feature, tests, manifest assignments and requirement evidence. Ancestry scope passed for ga-9s1sl8 and its same-feature build/rework IDs ga-aj1hax/ga-xn97xp; no stack. |

```text
fast_lane: PASS — completed rc=0, FAST_LANE_PASS
fast_run_dir: /var/tmp/gc-heavy-gate/runs/ga-9s1sl8.fast
fast_service: gc-heavy-ga-9s1sl8.fast-1791337572-661191.service
fast_key: 927864709107cc8d629d238baa08841b669edd6c-fast-v1-0aaa3db36f
fast_command: /var/tmp/ga-9s1sl8-fast-lane.sh
policy_lane: PASS — make lint-affected fmt-check-changed test-ci-policy check-gomod-replace check-native-dependency-surface check-eventexport-isolation check-core-boundary check-docs, through gate-base.sh and run-pinned-lint.sh in a fresh view
drift_lane: PASS — Bazel sync+diff, dashboard-ci+diff, exact genspec/genclient/genschema targets 3/3 PASS with pinned private ICU-74 runtime, base-pinned OpenAPI check with no unwaived breaking changes; independent fresh views
fast_output: /var/tmp/ga-9s1sl8-fast-result.log
test_cmd: load-gate-run --threshold 15 --max-wait 1800, then isolated-test-run -- env private HOME TMPDIR=/var/tmp/gotmp Podman socket TESTCONTAINERS_RYUK_DISABLED=true BEADS_ALLOW_UNREAPED_TESTCONTAINERS=1 GOFLAGS=-v LOCAL_TEST_LOG_DIR retained make test-local-full-parallel
test_cmd_scope: full-suite
test_exit: 0; all 40 jobs passed; detached unit complete at 2026-10-07T04:27:21Z
test_counts: PASS=57971 FAIL=0 SKIP=235
test_counts_definition: top-level Go result lines, column 0, summed across 40 logs; repeated tests count per job; subtests excluded
test_logs_with_results: 36/40; Darwin compile, two shell selftests and productmetrics-testhook have no Go text result lines and passed as jobs
test_run_dir: /var/tmp/gc-heavy-gate/runs/ga-9s1sl8.c3-envfixed
test_service: gc-heavy-ga-9s1sl8.c3-envfixed-1791340010-3055995.service; inactive/dead, MainPID=0, Result=success
test_key: 927864709107cc8d629d238baa08841b669edd6c-052f77b3eb
test_log_dir: /var/tmp/gc-heavy-gate/runs/ga-9s1sl8.c3-envfixed/logs
test_command_file: /var/tmp/ga-9s1sl8-full-suite.sh
test_evidence: /var/tmp/ga-9s1sl8-final-test-evidence.txt
ci_lane_run: n/a (no CI-config change); supplemental CI is separately authorized by gm-wisp-ty7n5ds
heavy_mode: none
waiver_ref: none (gascity has no waiver path)
load_threshold: 15
load_waited_seconds: 1802
load_wait_timed_out: 1
run_start_load: 40.03
run_max_load: 133.60
run_mean_load: 64.36
run_readings: 180
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1802 wait_timed_out=1 run_start_load=40.03 run_max_load=133.60 run_mean_load=64.36 run_readings=180 wait_first_load=33.51 wait_max_load=68.65 wait_mean_load=38.80 wait_readings=61 read_errors=0
diff_tests_executed:
  TestStateCacheBackoff_ChangeDuringAFailingFetchPermitsOneMoreFetch PASS (integration-packages-runtime-tmux-2-of-3.log, unit-core.log)
  TestStateCacheBackoff_DirtyFlagFromAnOlderChangeDoesNotDefeatTheHold PASS (integration-packages-runtime-tmux-2-of-3.log, unit-core.log)
  TestStateCacheBackoff_EachNewerChangePermitsItsOwnFetch PASS (integration-packages-runtime-tmux-3-of-3.log, unit-core.log)
  TestStateCacheBackoff_EvictionInsideAHoldIsImmediate PASS (integration-packages-runtime-tmux-1-of-3.log, unit-core.log)
  TestStateCacheBackoff_FetchScheduleFollowsConsecutiveFailures PASS (integration-packages-runtime-tmux-2-of-3.log, unit-core.log)
  TestStateCacheBackoff_HeldReadsAnswerFromTheCurrentObservation PASS (integration-packages-runtime-tmux-3-of-3.log, unit-core.log)
  TestStateCacheBackoff_HeldReadsClassifyLikeTheFetchingRead PASS (integration-packages-runtime-tmux-3-of-3.log, unit-core.log)
  TestStateCacheBackoff_HoldDoesNotScaleWithTheTTL PASS (integration-packages-runtime-tmux-1-of-3.log, unit-core.log)
  TestStateCacheBackoff_HoldNeverExceedsHalfTheStaleTTL PASS (integration-packages-runtime-tmux-1-of-3.log, unit-core.log)
  TestStateCacheBackoff_HoldNeverOutlivesTheStaleCliff PASS (integration-packages-runtime-tmux-2-of-3.log, unit-core.log)
  TestStateCacheBackoff_HoldNeverOutlivesTheStaleCliffWhenFetchesTimeOut PASS (integration-packages-runtime-tmux-3-of-3.log, unit-core.log)
  TestStateCacheBackoff_LogsOncePerHoldAndOnRecovery PASS (integration-packages-runtime-tmux-1-of-3.log, unit-core.log)
  TestStateCacheBackoff_NewerChangePermitsExactlyOneFetch PASS (integration-packages-runtime-tmux-3-of-3.log, unit-core.log)
  TestStateCacheBackoff_OverlappingFailuresAreOneOutage PASS (integration-packages-runtime-tmux-1-of-3.log, unit-core.log)
  TestStateCacheBackoff_RecoveryWaitsForTheHoldNoLongerThanTheCap PASS (integration-packages-runtime-tmux-2-of-3.log, unit-core.log)
  TestStateCacheBackoff_StaleCliffInstantIsUnchanged PASS (integration-packages-runtime-tmux-1-of-3.log, unit-core.log)
  TestStateCacheBackoff_StaleFailureAfterANewerPublishOpensNoHold PASS (integration-packages-runtime-tmux-1-of-3.log, unit-core.log)
  TestStateCacheBackoff_StaleSuccessAfterANewerPublishEndsNoStreak PASS (integration-packages-runtime-tmux-2-of-3.log, unit-core.log)
  TestStateCacheBackoff_SuccessRestartsTheSchedule PASS (integration-packages-runtime-tmux-3-of-3.log, unit-core.log)
  TestStateCacheBackoff_SupersededRecoveryEndsTheStreak PASS (integration-packages-runtime-tmux-3-of-3.log, unit-core.log)
  TestStateCacheBackoff_UnprimedFailuresBackOff PASS (integration-packages-runtime-tmux-2-of-3.log, unit-core.log)
  TestRuntimeTmuxManifestMatchesCanonicalLinuxIntegrationInventory PASS (integration-packages-core-1-of-4.log, unit-core.log)
  TestRuntimeTmuxManifestSixShardsPartitionInventoryExactlyOnce PASS (integration-packages-core-1-of-4.log, unit-core.log)
```

Runtime preflight through the isolation wrapper and the runner's env-i/bash-lc shape verified pinned bd 1.3.1 (c1c4b642a), Dolt 2.2.0, Go, tmux, cached container images and the enabled 30-minute testcontainer sweep. Private HOME isolates user config. The wrapper pins bd; the suite scrubs its environment and receives container settings only inside the wrapped chain, with no suite allowlist change. Evidence: /var/tmp/ga-9s1sl8-runtime-preflight.log. This does not imply Docker or K8s lane coverage.

## Skip evidence

The 235 top-level SKIP lines correspond to 336 observed skip events including subtests. Every event resolved to one of 218 source sites: DOT --site says UNCHANGED, shared_hunks_in_file=0; every containing file's blob is identical to the pinned base. None of the 23 owned tests or their subtests is skipped or failed. Full printed reasons and per-site ownership evidence are retained in /var/tmp/ga-9s1sl8-observed-skips.json, /var/tmp/ga-9s1sl8-skip-ownership.log and /var/tmp/ga-9s1sl8-skip-justifications.json; category counts are /var/tmp/ga-9s1sl8-skip-justification-summary.json (problem_count=0).

Platform, root/filesystem, readable-procfs and child-subreaper guards reflect this Linux user environment. Subprocess helpers, golden regeneration and deliberate skip fixtures are not standalone behavior tests. Optional live Herdr, MCP, SSH, PostgreSQL, pack-catalog and legacy tooling fixtures are absent or explicitly opt-in. Existing ledgered/provider-profile exclusions are unchanged. The backoff diff changes none of these condition inputs or fixture capabilities; shared helpers in their files are also unchanged.

Ninety package-lane skip events independently PASS by name in the cmd/gc process logs. Two integration-tag-only storage tests — TestManagedBeadsNativeCLICompatibility and TestManagedBdRigProviderStoreRecoversAfterHardKillPortRebind — do not: the existing package lane sets GC_FAST_UNIT=1, while the process lane has no integration tag. They skip in the unchanged setupManagedBdWaitTestCity/skipSlowCmdGCTest environment guard before storage setup; this gate does not claim their execution. Those tests concern CLI/library schema compatibility and Dolt port rebinding, which this cache feature does not change. Unit-lane signal tests and live-provider exclusions remain explicit; no blanket all-tests-executed claim is made.

The old bd 1.0.4 silent-fallback exclusion (ga-e7z613), older-bd --if-revision guard (beads#4682), and nine opt-in persistence cases remain gaps, not PASS results. The persistence source explicitly waits until every bd pin exceeds 1cf8337. The optional real-upstream Dolt fixture at 127.0.0.1:0 is unconfigured; its skip precedes storage operations and is not a missing-testcontainer skip. K8s remains NOT-COVERED by the ga-1zgega ruling. Unscoped live API rows, ambient tmux binding/cleanup opt-ins, and the existing unconditional supervisor-shutdown flake exclusion (#2090) are also recorded. Their skip predicates are unchanged and cannot be altered by this diff's refresh schedule.

## Required CI accounting — ga-1zgega

The actual GitHub PR merge workflow is authoritative for this draft's CI evidence. Its base advanced while the pinned local gate ran. The current workflow removed eight worker leaves and combined fork integration into one 14-row matrix; all 12 process shards and all 14 integration rows must independently succeed. This changes the derived dependency closure, not the attribution or full-suite rules. The original 31-leaf pinned-base derivation remains at /var/tmp/ga-9s1sl8-ci-required-closure.log; its record is preserved separately. No removed job is claimed executed.

ci_required_closure: python3 /var/tmp/ga-1zgega-gate-tools/ci_required_triggers.py /var/tmp/ga-9s1sl8-ci-view.J93vKk b72bd3f7859d62560c247fede2d37d0ca2dcbb4d 2acc8d4cb33878180ddc966784e86c70a2dff447 fork; GitHub merge 7ad9df2f8d8d50f45d7ef2d2d9cf78b9ad90ff30; ci.yml blob fc62c1c7a50cffcc2f25dee9a42a5a84401d92d9; 22 LEAF jobs. Tool hashes match the architect's recorded hashes. Evidence: /var/tmp/ga-9s1sl8-ci-actual-closure.log.

ci_run: https://github.com/gastownhall/gascity/actions/runs/37572755795; actual head_sha matches the reviewed source. Expected matrix names and concrete API job records: /var/tmp/ga-9s1sl8-ci-actual-expanded-jobs.json and /var/tmp/ga-9s1sl8-ci-actual-accounting-current.json. CI-PASS means every job instance has status completed and conclusion success, never an inferred pass from one shard.

windows_reach: none; both pinned-merge build closures computed successfully; actual Windows filter remains NOT-TRIGGERED. deferred_to_pr_ci: release-config (host-precondition-failed: command -v goreleaser rc1, stdout and stderr empty). Actual draft Release config succeeded; the original local host gap remains explicit. not_covered: k8s-session (a green setup-only job does not execute its secret-gated test step). Docker's real host availability probe passed, and its exact-source draft job succeeded. Probe evidence: /var/tmp/ga-9s1sl8-ci-preflight/probes.json.

PASS authorizes release PR-open, never merge. MPR must explicitly verify CI / required and every deferred job on the current PR head; the repository ruleset does not enforce them.

- runner-policy | CI-PASS | [Exact-source CI run](https://github.com/gastownhall/gascity/actions/runs/37572755795), 1/1 job instances success; [Runner policy](https://github.com/gastownhall/gascity/actions/runs/37572755795/job/112634851049)
- changes | CI-PASS | [Exact-source CI run](https://github.com/gastownhall/gascity/actions/runs/37572755795), 1/1 job instances success; [Detect changes](https://github.com/gastownhall/gascity/actions/runs/37572755795/job/112634915005)
- preflight-static | CI-PASS | [Exact-source CI run](https://github.com/gastownhall/gascity/actions/runs/37572755795), 1/1 job instances success; [Preflight / static checks](https://github.com/gastownhall/gascity/actions/runs/37572755795/job/112634915009)
- preflight-acceptance | CI-PASS | [Exact-source CI run](https://github.com/gastownhall/gascity/actions/runs/37572755795), 1/1 job instances success; [Preflight / acceptance A](https://github.com/gastownhall/gascity/actions/runs/37572755795/job/112634915059)
- preflight-generated | CI-PASS | [Exact-source CI run](https://github.com/gastownhall/gascity/actions/runs/37572755795), 1/1 job instances success; [Preflight / generated artifacts](https://github.com/gastownhall/gascity/actions/runs/37572755795/job/112634915585)
- contract-acceptance-previous | CI-PASS | [Exact-source CI run](https://github.com/gastownhall/gascity/actions/runs/37572755795), 1/1 job instances success; [Contract / bd CLI (minimum supported)](https://github.com/gastownhall/gascity/actions/runs/37572755795/job/112634915014)
- contract-acceptance-current | NOT-TRIGGERED |   contract-acceptance-current            LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.beads == 'true'
- release-config | DEFERRED-TO-PR-CI reason=host-precondition-failed | command -v goreleaser rc1, stdout/stderr empty; /var/tmp/ga-9s1sl8-ci-preflight/probes.json; actual ordered draft CI Release config already success but original host gap retained
- dashboard | CI-PASS | [Exact-source CI run](https://github.com/gastownhall/gascity/actions/runs/37572755795), 1/1 job instances success; [Dashboard SPA](https://github.com/gastownhall/gascity/actions/runs/37572755795/job/112634915047)
- integration-shards | CI-PASS | [Exact-source CI run](https://github.com/gastownhall/gascity/actions/runs/37572755795), 14/14 job instances success; [Integration (fork PR) / packages-core-1-of-4](https://github.com/gastownhall/gascity/actions/runs/37572755795/job/112634979196); [Integration (fork PR) / packages-core-2-of-4](https://github.com/gastownhall/gascity/actions/runs/37572755795/job/112634979067); [Integration (fork PR) / packages-core-3-of-4](https://github.com/gastownhall/gascity/actions/runs/37572755795/job/112634979082); [Integration (fork PR) / packages-core-4-of-4](https://github.com/gastownhall/gascity/actions/runs/37572755795/job/112634979219); [Integration (fork PR) / packages-cmd-gc-integration](https://github.com/gastownhall/gascity/actions/runs/37572755795/job/112634979203); [Integration (fork PR) / packages-runtime-tmux-1-of-6](https://github.com/gastownhall/gascity/actions/runs/37572755795/job/112634979102); [Integration (fork PR) / packages-runtime-tmux-2-of-6](https://github.com/gastownhall/gascity/actions/runs/37572755795/job/112634979161); [Integration (fork PR) / packages-runtime-tmux-3-of-6](https://github.com/gastownhall/gascity/actions/runs/37572755795/job/112634979127); [Integration (fork PR) / packages-runtime-tmux-4-of-6](https://github.com/gastownhall/gascity/actions/runs/37572755795/job/112634979136); [Integration (fork PR) / packages-runtime-tmux-5-of-6](https://github.com/gastownhall/gascity/actions/runs/37572755795/job/112634979181); [Integration (fork PR) / packages-runtime-tmux-6-of-6](https://github.com/gastownhall/gascity/actions/runs/37572755795/job/112634979200); [Integration (fork PR) / bdstore](https://github.com/gastownhall/gascity/actions/runs/37572755795/job/112634979262); [Integration (fork PR) / rest-smoke-1-of-2](https://github.com/gastownhall/gascity/actions/runs/37572755795/job/112634979186); [Integration (fork PR) / rest-smoke-2-of-2](https://github.com/gastownhall/gascity/actions/runs/37572755795/job/112634979149)
- integration-rest-full | PUSH-ONLY |   integration-rest-full                  LEAF       linux                      PUSH-ONLY (never runs on a pull request)
- beads-topology-acceptance | CI-PASS | [Exact-source CI run](https://github.com/gastownhall/gascity/actions/runs/37572755795), 1/1 job instances success; [Beads / topology acceptance](https://github.com/gastownhall/gascity/actions/runs/37572755795/job/112634978841)
- beads-proxied-native-acceptance | CI-PASS | [Exact-source CI run](https://github.com/gastownhall/gascity/actions/runs/37572755795), 1/1 job instances success; [Beads / proxied-native acceptance](https://github.com/gastownhall/gascity/actions/runs/37572755795/job/112634978848)
- cmd-gc-process | CI-PASS | [Exact-source CI run](https://github.com/gastownhall/gascity/actions/runs/37572755795), 12/12 job instances success; [cmd/gc process (fork PR) / shard 1 of 12](https://github.com/gastownhall/gascity/actions/runs/37572755795/job/112634978976); [cmd/gc process (fork PR) / shard 2 of 12](https://github.com/gastownhall/gascity/actions/runs/37572755795/job/112634978989); [cmd/gc process (fork PR) / shard 3 of 12](https://github.com/gastownhall/gascity/actions/runs/37572755795/job/112634979109); [cmd/gc process (fork PR) / shard 4 of 12](https://github.com/gastownhall/gascity/actions/runs/37572755795/job/112634979046); [cmd/gc process (fork PR) / shard 5 of 12](https://github.com/gastownhall/gascity/actions/runs/37572755795/job/112634979023); [cmd/gc process (fork PR) / shard 6 of 12](https://github.com/gastownhall/gascity/actions/runs/37572755795/job/112634979090); [cmd/gc process (fork PR) / shard 7 of 12](https://github.com/gastownhall/gascity/actions/runs/37572755795/job/112634979124); [cmd/gc process (fork PR) / shard 8 of 12](https://github.com/gastownhall/gascity/actions/runs/37572755795/job/112634978991); [cmd/gc process (fork PR) / shard 9 of 12](https://github.com/gastownhall/gascity/actions/runs/37572755795/job/112634979087); [cmd/gc process (fork PR) / shard 10 of 12](https://github.com/gastownhall/gascity/actions/runs/37572755795/job/112634979042); [cmd/gc process (fork PR) / shard 11 of 12](https://github.com/gastownhall/gascity/actions/runs/37572755795/job/112634979072); [cmd/gc process (fork PR) / shard 12 of 12](https://github.com/gastownhall/gascity/actions/runs/37572755795/job/112634979077)
- cmd-gc-productmetrics-testhook | CI-PASS | [Exact-source CI run](https://github.com/gastownhall/gascity/actions/runs/37572755795), 1/1 job instances success; [cmd/gc product metrics testhook](https://github.com/gastownhall/gascity/actions/runs/37572755795/job/112634978857)
- credential-provider-windows | NOT-TRIGGERED | credential-provider-windows              LEAF       NON-LINUX windows-latest   NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'
- preflight-unit-cover-noncmdgc | PUSH-ONLY | preflight-unit-cover-noncmdgc            LEAF       linux                      PUSH-ONLY (never runs on a pull request)
- preflight-unit-cover-cmdgc | PUSH-ONLY | preflight-unit-cover-cmdgc               LEAF       linux                      PUSH-ONLY (never runs on a pull request)
- pack-gate | NOT-TRIGGERED | pack-gate                                LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'
- docker-session | CI-PASS | [Exact-source CI run](https://github.com/gastownhall/gascity/actions/runs/37572755795), 1/1 job instances success; [Docker session](https://github.com/gastownhall/gascity/actions/runs/37572755795/job/112634978918)
- k8s-session | NOT-COVERED | ga-1zgega: secret-gated test step does not run; green setup-only job is not test coverage
- openclaw-bridge | NOT-TRIGGERED | openclaw-bridge                          LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.openclaw_bridge == 'true'

## Preserved failed attempts and publication state

The first full attempt failed at module-fetch setup (libopenapi TLS timeout/EOF), before those packages reached tests. Exact private-cache repair was checked against go.sum; source, module pins and shared caches were unchanged. The terminated partial run and its logs remain at ga-9s1sl8.c3 and /var/tmp/ga-9s1sl8-c3-environment-failure.log; repair proof /var/tmp/ga-9s1sl8-module-repair.json. Only the complete c3-envfixed run above is full-suite evidence.

The first normal Bazel push hook refused publication: 119/193 targets passed, 73 binaries aborted in the ICU74 loader and productmetrics timed out in two shards (one in fsync, one in openat/WriteFile). Those logs remain at /var/tmp/ga-9s1sl8-prepush-original-testlogs and its failure inventory. Sightings were read back in ga-sdon3o and ga-vkhfnj; none is attributed as a release PASS. The supported GC_PREPUSH_SUITE=go hook mode runs without --no-verify, force push, host installation or Bazel rc changes. Its first launch failed before tests because I omitted the retained log-directory creation; /var/tmp/ga-9s1sl8-prepush-go-setup-failure.log preserves it. The corrected run /var/tmp/gc-heavy-gate/runs/ga-9s1sl8.prepush-go-logfixed completed successfully: all 10 jobs PASS, command rc0, source counts 28701 PASS/0 FAIL/158 SKIP and all 23 owned tests PASS. Service inactive/dead MainPID0 Result=success. Remote fork head readback matched the reviewed source; full output /var/tmp/ga-9s1sl8-prepush-final-output.log. This hook evidence is separate from the full release counts above.

The isolated branch deploy/ga-9s1sl8-gate remains at the reviewed source, pushed normally to the fork. Draft https://github.com/gastownhall/gascity/pull/7270 is read-back verified at that exact head, author quad341, base main, isDraft true; prepared body verified verbatim. Mayor notification gm-wisp-u1ll7k7 was peek-verified. The full gate is PASS, with all actual source-CI accounts complete and CI / required success. Publish this gate record as the sole new commit, verify clean status and PR head equality, then publish success clearance before the exact-head MPR handoff. The gate record changes the PR head; MPR must wait for CI / required and Release config success on that new head. Do not freshen/reset the deploy branch. Restore the two preserved gate files after the handoff using the exact stash recorded in the bead.
