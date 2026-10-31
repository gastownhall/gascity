# Real transport startup deadline release gate

**Verdict:** **PASS**

Deploy bead: ga-5af7bx. Build bead: ga-cv2tf0. Review bead: ga-tltul1.

- Reviewed source: `8d4b18eec561b127844e21fe73c7abb55a7f6d9e` (resolved from the deploy bead's pinned Commit field).
- Base: `origin/main@a158e519c8d25ac476c4750257185bea65d7f3e7`; refreshed after the sweep, unchanged.
- Tested scratch merge: `4f950a1c47acf0def668aceebdae7ba669a9b550`.
- Merge-result tree: `8c9260d348179a59e5a21e86378e64cfff830b54`.
- Deploy target: `deploy/ga-5af7bx-gate`, cut from the reviewed source; only this gate record is added afterward.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Reviewer PASS present | PASS | ga-tltul1 explicitly reviewed and passed the exact pinned source, including the integration-manifest fixup; its older metadata commit is superseded by its recorded commit override. |
| 2 | Acceptance criteria met | PASS | Five concurrent real-transport repetitions: 75 PASS, 0 FAIL, 0 SKIP, including all nine providers and both negative cases. Claude startup passed at 24.61/24.67/24.62/24.64/24.56 seconds during the live full sweep; the existing 60-second hang budget retains rejection of incomplete and over-budget starts. |
| 3 | Full suite and required policy checks | PASS | The original documented 40-job full fan-out completed 40 PASS / 0 FAIL jobs. Its shard logs contain 95392 PASS / 0 FAIL / 327 SKIP test/subtest events. All four diff-owned tests and their eleven subtests PASS by name. Policy/static checks PASS; details below. |
| 4 | No unresolved HIGH findings | PASS | Reviewer style, security, and specification findings report no HIGH blocker. |
| 5 | Final branch clean | PASS | The tested-merge checkout and assigned worktree were clean. This gate is the sole staged addition; the resulting deploy head is verified clean before push. |
| 6 | Clean divergence from main | PASS | git merge-tree completed without conflicts; canonical materialize_merge_tree produced the exact merge-result tree. go build ./... and go vet ./... PASS on that combined tree. |
| 7 | Single feature theme | PASS | Two integration test files plus their shard registration address the same real-transport startup deadline. Ancestry scope passes for the confirmed ga-5af7bx/ga-cv2tf0 lineage; no stack or independent theme. |

## Test evidence

`test_cmd`: load-gate-run.sh --threshold 15 --max-wait 1800 -- isolated-test-run.sh -- make test-local-full-parallel LOCAL_TEST_JOBS=4

`test_cmd_scope`: full-suite. Environment: GOFLAGS=-v, TMPDIR=/var/tmp, short private HOME /var/tmp/gch.XKnGBn (19 bytes), empty .zshrc, normal host module/build caches, hermetic Git configuration. Rootless podman socket and cached image tags were checked before starting.

`test_counts`: 95392 PASS, 0 FAIL, 327 SKIP reported test/subtest events across the full shard logs; these count repeated executions, not distinct test names. `job_counts`: 40 PASS, 0 FAIL, 0 SKIP; each original fan-out job emitted its terminal exit-zero result.

`diff_tests_executed` (resolved within the full-suite output):

- TestPhase2RealTransportBoundsStayAHangDetector: PASS.
- TestPhase2RealTransportResultStillFailsWhenGenuinelyBroken: PASS.
- TestPhase2RealTransportResultToleratesASlowButCorrectStart: PASS.
- TestPhase2WorkerCoreRealTransportProof: PASS.

All nine TestPhase2WorkerCoreRealTransportProof provider subtests PASS, including Claude/tmux-cli (24.65s) and Codex/tmux-cli (25.88s). Both TestPhase2RealTransportResultStillFailsWhenGenuinelyBroken subtests PASS. No diff-owned FAIL or SKIP.

`skip_justification`: guarded skips cover platform/root/filesystem prerequisites, unavailable optional live SSH/MCP/tooling fixtures, opt-in persistence or tmux dogfood checks, golden regeneration, unsupported provider usage/structured capabilities, intentional helper/classified-start-error exercises, and isolated ambient-directory characterizations. Real-process/herdr tests deferred by the fast unit tier execute in the broader process/integration lanes. These unchanged checks do not test the deadline adjustment; every changed real-transport test executed in its integration lane.

`policy_lane`: make test-ci-policy PASS; make lint-affected fmt-check-changed LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=a158e519c8d25ac476c4750257185bea65d7f3e7 PASS (0 lint issues); make check-hooks PASS. Canonical merge materialization ran the active pre-commit hook.

`ci_lane_run`: n/a — this diff registers three tests in an existing local integration shard manifest; it changes no CI job, matrix, timeout, or required-check list.

`failure_attribution`: none needed for this final run (0 FAIL test events). Earlier incomplete attempts are not gate evidence. `waiver_ref`: none.

## Supervisor and load evidence

The full4 outer exec/load wrapper exited 143 at 03:23:49Z. Mayor established the cause in mail gm-wisp-sn40fy and the live bead notes: an immediate gc nudge at 03:20:24Z interrupted the Codex turn, whose outstanding exec was subsequently SIGTERMed. The independent setsid fan-out (process group 415720) continued without interruption and completed all original 40 jobs. No test result, overall make exit status, or lost sampler value is fabricated. Criterion 3 uses those complete terminal job results and their full-scope per-test output, not the interrupted supervisor's exit code.

- load_threshold: 15.
- load_waited_seconds: unavailable — wrapper interrupted before final aggregation.
- load_wait_timed_out: unavailable — final summary was lost; the suite was observed beginning after load cleared around 03:12Z.
- load_start: unavailable — original sampler was removed during wrapper cleanup.
- load_max: unavailable — original sampler was removed during wrapper cleanup.
- load_mean: unavailable — original sampler was removed during wrapper cleanup.

Independent post-interruption observations: five-minute load 35.53 at 03:28:06Z; monitoring then observed it decline to 13.54 at 03:49:19Z. These observations do not replace the missing full-run summary.

Full master log: /var/tmp/gc-gate-ga-5af7bx-full4.log. Complete shard logs: /var/tmp/gc-gate-ga-5af7bx-full4-shards. Five-repeat concurrent acceptance log: /var/tmp/gc-gate-ga-5af7bx-acceptance3.jsonlog (package PASS, 291.830s). Policy/static logs: /var/tmp/gc-gate-ga-5af7bx-policy.log and /var/tmp/gc-gate-ga-5af7bx-static.log.
