# Release gate: recent unread mail and stale reminders — ga-9s0ftp

**Verdict:** **PASS**

## Coordinates

- Reviewed source: `db2f097566a986e30735162dcfb1ee870088a1c9`, resolved as a commit; review bead `ga-u6wr0v` is closed with PASS. The underlying Go review `ga-kbkm8u` passed at `40b49a83b9f95a19f3fad02fca3beab1fe2dafaa`; the final reviewed commit adds the Bazel test source entry.
- Captured test base: `origin/main` at `0c5c1411006908867ffc6488d6ab50a31736b013`. Official materialized merge: `863fb6c8644d1fb4cb01d55248255a4d847448e6`, tree `e745d7a7999072685e1e32311b10a93bfb6a6788`, at `/var/tmp/gc-merge-validate.ga-9s0ftp.EGOd4z`.
- Preflight found no associated PR for the reviewed source; this target is not already merged. Deploy mode is remote, with `origin/main` as base and `fork` as push remote. No self-rebase was needed.
- `docs/PROJECT_MANIFEST.md` is absent in this checkout. This evaluation uses the supplied release criteria and the full local suite documented by `TESTING.md` and the Makefile.

## Criteria

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | `ga-u6wr0v` records PASS and pins the resolved reviewed SHA; `ga-kbkm8u` records PASS for the underlying Go work. No review carryover is asserted. |
| 2 | Acceptance criteria met | PASS | Mail preview preserves priority, selects the newest unread messages within the priority window, and displays the selected window chronologically. The injection paths archive selected ephemeral auto-handoff mail. Delivery withdraws stale mail reminders when aggregate unread mail is zero and retains reminders on lookup errors or unresolved targets. The relevant changed/new tests below execute and pass. |
| 3 | Tests pass | PASS | The documented full-suite command completed 40/40 jobs successfully: 55,519 top-level PASS, 0 FAIL, 236 SKIP. All eight changed/new mail regression tests below passed in both cmd/gc process and integration shards. Required policy/lint lane also passed. No waiver. |
| 4 | No unresolved HIGH findings | PASS | The PASS reviews record no open HIGH findings. |
| 5 | Final branch clean | PASS | Source and official merge scratch were clean. The isolated deploy branch contains the reviewed source plus this gate record; post-commit worktree status is clean. |
| 6 | Clean divergence from main | PASS | `git merge-tree --write-tree` and the official merge materialization succeeded without conflict. The later `origin/main` tip `76c5e95e26f20350701239944dd1e3a3f917ae44` also merges cleanly, producing tree `54d9d356ec35fddbce0b922fe3f55767b8a07132`. |
| 7 | One feature theme | PASS | The ancestry scope check accepts this mail-reminder lineage (`ga-9s0ftp`, `ga-ra0ou2`, `ga-vtxkj5`, `ga-vacioq`), with no stack, independent theme, or private agent-setting path. |

## Full test evidence

```text
heavy_mode: none
test_cmd_scope: full-suite
test_cmd: load-gate-run.sh --threshold 15 --max-wait 1800 -- isolated-test-run.sh -- env DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true BEADS_ALLOW_UNREAPED_TESTCONTAINERS=1 GOFLAGS=-v TMPDIR=/var/tmp LOCAL_TEST_LOG_DIR=/var/tmp/ga-9s0ftp-full-shards-r5 make test-local-full-parallel LOCAL_TEST_JOBS=4
full_command_exit: 0
test_counts: 55519 PASS / 0 FAIL / 236 SKIP (top-level terminal rows)
job_counts: 40 PASS / 0 FAIL
waiver_ref: none
ci_lane_run: n/a (no CI job, matrix, timeout, or required-check change)
load_threshold: 15
load_waited_seconds: 1801
load_wait_timed_out: 1
run_start_load: 30.87
run_max_load: 64.28
run_mean_load: 40.77
run_readings: 107
```

The full run is detached and retained at `/var/tmp/gc-heavy-gate/runs/ga-9s0ftp.c4`; its 40 shard logs are at `/var/tmp/ga-9s0ftp-full-shards-r5`. The bounded load wait timed out and proceeded, as the ordinary-gate protocol specifies. The rootless Podman socket and matching cached test image were checked before launch; the isolation wrapper completed without a tripwire. A prior attempt (`ga-9s0ftp.c3`) was invalid setup evidence: the shard log directory did not exist, so no test binary ran. It is excluded from these results.

`diff_tests_executed`: `TestInjectPreviewPriorityStillOutranksRecency`, `TestInjectPreviewSurfacesNewestUnread`, `TestMailCheckInjectArchivesEphemeralAutoHandoffMessages`, `TestMailCheckInjectFloatsPriorityAutoHandoffIntoWindow`, `TestMailCheckInjectLimitsMessageCount`, `TestTryDeliverQueuedNudgesByPollerAcksDeliveredUnobservedInsteadOfRetrying`, `TestTryDeliverQueuedNudgesByPollerDropsStaleMailReminder`, and `TestTryDeliverQueuedNudgesByPollerKeepsFreshMailReminder` each have two PASS rows and no FAIL or SKIP row in the full-suite logs.

`skip_justification`: the 236 SKIP rows concern existing platform, live-provider, helper, or fixture preconditions, including duplicate fast-tier invocations covered by process/integration shards. No changed or new test skipped. No failure attribution was needed.

The required policy/lint lane ran from the clean official merge tree with its base pinned to `0c5c1411006908867ffc6488d6ab50a31736b013`: `fresh-tree-run.sh -- gate-base.sh run -- run-pinned-lint.sh -- env LINT_CHANGED_REF=<captured-base> LINT_CHANGED_SCOPE=tracked make test-ci-policy lint-affected fmt-check-changed`. It exited 0, selected `./cmd/gc`, found zero golangci-lint issues, and passed affected vet and formatting checks. The pinned linter version was 2.12.0. Evidence is `/var/tmp/gc-heavy-gate/runs/ga-9s0ftp.policy2`. An earlier policy attempt selected no changed paths and is excluded. Whole-tree `go build ./...` and `go vet ./...` passed on the same official merge.

`make bazel-sync` exited 0 on the isolated deploy branch, and the following tracked diff was empty. Gazelle reported warnings about existing missing embed paths and expression merges, but produced no tracked BUILD change. Its untracked Bazel output symlink was removed before the gate commit.

After the run, main advanced through PR #5599. Its seven changed paths are confined to `internal/runtime/tmux/` and the runtime-tmux manifest/test scripts; the reviewed candidate's eight paths are in `cmd/gc/` and `release-gates/`. The sets have zero overlap and the current-tip merge is clean. Mayor's verified ruling `gm-wisp-bpiofu` says the captured-base 40-job PASS is final for this non-overlapping move and current merge-ref CI carries that drift; no second full run or scoped recheck is required. Any later overlapping or conflicting move requires a new decision.
