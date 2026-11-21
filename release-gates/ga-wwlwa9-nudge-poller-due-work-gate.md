# Release gate: nudge-poller due-work gate (`ga-wwlwa9`)

- Deploy bead: `ga-wwlwa9`
- Build bead: `ga-8x82f4`
- Review bead: `ga-79ec1n`
- Reviewed commit: `5ec70a5968ac4cf1b7cd4fcd0b6d994999c46e6c`
- Source branch (provenance only): `builder/ga-8x82f4`
- Base evaluated: `origin/main@378a8ec328a23aa7349222576add20dbdb29a301`
- Merge base: `631929f416c638c24d092738c075b880d270a1fb`
- Deploy mode: remote; push target: `origin`
- Evaluation date: 2026-09-13
- Verdict: **PASS**

The remote pre-flight found no pull request carrying the reviewed commit, so
there is no already-merged or closed-PR disposition to reconcile.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-79ec1n` is closed and records `verdict: pass` plus `deploy_commit: 5ec70a5968ac4cf1b7cd4fcd0b6d994999c46e6c`. The SHA resolves to the reviewed commit in this repository. |
| 2 | Acceptance criteria met | **PASS** | The cheap target-scoped due-work read now precedes the expensive observe/deliver pair; empty-queue ticks skip delivery, idle observation still runs every five ticks to detect dead sessions, due work observes and delivers immediately, and the post-attempt fresh due-work read preserves #5317 skip accounting. The reviewer explicitly accepted the live-strace and shared `bd` runner optimizations as separately scoped follow-ups `ga-yzg1wm` and `ga-wo65hm`, and recorded `uncovered_criteria: none`. |
| 3 | Tests pass | **PASS** | The documented full local CI union completed all 40 jobs with **46,530 PASS / 0 FAIL / 225 SKIP** top-level test executions. Each of the three diff-owned tests passed in both the `cmd/gc` process and integration-package lanes, with zero diff-owned FAIL or SKIP. The required policy/lint lane and additive build/vet checks also passed. Details below. |
| 4 | No high-severity review findings open | **PASS** | The exact-head review reports no security finding, no style/lint finding, no blocker, no major, and no minor. |
| 5 | Final branch clean | **PASS** | `git status --short` was empty in the detached evaluation worktree at the exact reviewed commit after the full suite and additive checks. This checklist is the deployer's only new file for this bead. |
| 6 | Branch diverges cleanly from main | **PASS** | After fetching current main, `git merge-tree --write-tree --messages origin/main 5ec70a5968ac4cf1b7cd4fcd0b6d994999c46e6c` exited 0 and produced tree `d7a6e7046199f5c49534d4ee4f65c71bb530ab1c`; main has 10 exclusive commits and the candidate has 1. No self-rebase was needed. |
| 7 | Single feature theme | **PASS** | One reviewed commit changes only `cmd/gc/cmd_nudge.go` and its dedicated regression-test file. Both concern the single nudge-poller due-work gating and liveness-preservation theme. `assert_deploy_ancestry_scope` passes for `ga-wwlwa9`, `ga-8x82f4`, and `ga-79ec1n`. |

## Acceptance evidence

- `nudgePollTargetHasDueWork` is evaluated before any session observation or
  delivery attempt on each tick.
- Empty-queue ticks bypass `nudgePollDeliverQueued`; a bounded idle counter
  forces `nudgeObserveTarget` every `nudgePollIdleObserveEvery` (five) ticks,
  preserving dead-session exit behavior.
- Due work sets `observeThisTick` immediately, so it reaches delivery on the
  next ordinary queue-check tick rather than waiting for the idle cadence.
- Skip accounting re-reads due-work state after the delivery attempt and only
  counts `not-delivered` when work remains, preserving the #5317 distinction
  between an idle tick and a failed delivery.
- The change does not alter the configured poll interval, queue format, flock
  discipline, or `gc nudge status` path.
- Follow-up `ga-yzg1wm` remains open for a real bd+tmux live-strace comparison;
  follow-up `ga-wo65hm` remains open for the broader shared
  `execCommandRunner` `--db`/`-C` pinning change. Both depend on the source
  build bead and remain independently tracked.

## Criterion 3 evidence

- `test_cmd`: `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true GO_TEST_TIMEOUT=30m LOCAL_TEST_JOBS=4 GOFLAGS=-v LOCAL_TEST_LOG_DIR=/var/tmp/ga-wwlwa9-full-logs make test-local-full-parallel`
- `test_cmd_scope`: `full-suite`
- `test_counts`: 40/40 jobs PASS; 46,530 top-level PASS, 0 FAIL, 225 SKIP
- `diff_tests_executed`:
  - `TestCmdNudgePollSkipsDeliveryAttemptWithEmptyQueue` — PASS twice
  - `TestCmdNudgePollExitsWhenSessionDiesWithEmptyQueue` — PASS twice
  - `TestCmdNudgePollDeliversDueWorkPromptly` — PASS twice
- Existing #5317 guards also passed twice each:
  - `TestCmdNudgePollRecordsDispatchSkipForBusyTarget`
  - `TestCmdNudgePollDoesNotRecordSkipWithoutQueuedWork`
- `skip_justification`: all 225 skips are emitted by unchanged suite code for
  platform-specific paths, subprocess-helper sentinels, explicit live-provider
  opt-ins, or scenarios intentionally routed to another full-suite lane. None
  is diff-owned; the real `cmd/gc` process lane ran and passed.
- `failure_attribution`: n/a — no failures
- `waiver_ref`: none
- `ci_lane_run`: n/a — no CI configuration, matrix, timeout, or required-check
  list changed
- Retained shard logs: `/var/tmp/ga-wwlwa9-full-logs`
- Shard-log manifest SHA-256: `90e68fec0ad16c1b2cd14b8134e9e8b9f2e2faaaaeed9e66cf559fd567d30ddb`

Additional required lanes and checks:

- `make test-ci-policy`: PASS
- `go vet ./...`: PASS
- `go build ./...`: PASS
- `make lint-new`: PASS, `0 issues`
- `git diff --check 631929f416c638c24d092738c075b880d270a1fb..5ec70a5968ac4cf1b7cd4fcd0b6d994999c46e6c`: PASS
- `gofmt -l cmd/gc/cmd_nudge.go cmd/gc/cmd_nudge_poll_due_work_gate_test.go`: PASS, no output
- `make check-hooks`: PASS; `core.hooksPath` is `.githooks`

## Disposition

All seven release criteria pass. Prepare the isolated
`deploy/ga-wwlwa9-gate` branch at the exact reviewed commit, commit this
checklist, push it, open the pull request, and publish deploy clearance before
routing the merge request. The deployer does not merge.
