# Release gate: wake drained sessions on sling nudge

**Verdict:** **PASS**
> Values in the table below are superseded by the "Head refresh" section at the end of this file.

- **Deploy bead:** ga-406x21
- **Build bead:** ga-qj4ids
- **Review bead:** ga-n56iko
- **Reviewed commit:** 21c6862b313d83a8dab44d0d27f5fc19b9b9034f
- **Gated commit after bounded self-rebase:** 2e6cfb66fd6de87e8602511c86f9e1e7a59625cf
- **Base:** origin/main at 2eb08ff80dfbe312ebdebbc7e091a5a36945c8d0
- **Merge validation commit:** 3d898e45f7481c98ecf43f81ddf17431d50afbde
- **Deploy mode:** remote
- **Gate run:** 2026-09-24 to 2026-09-25 UTC

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Review bead ga-n56iko is closed with `verdict: pass` for the original commit. The original and recorded rebased commits have the same stable patch ID, `a96141d283f8fa42bc93bc864e431a4ad45d22ac`; the rebase is recorded on the deploy bead. |
| 2 | Acceptance criteria met | PASS | `deliverSlingNudge` requests a managed wake for a non-running target when `canRequestManagedNudgeWake` is true, retains the queued nudge and controller poke, and retains the existing path when the gate is false. `TestDeliverSlingNudgeRequestsManagedWakeForDrainedSession` verifies the wake metadata and queued nudge; its independent verbose run passed. |
| 3 | Tests pass | PASS | `test_cmd: make test-local-full-parallel`; `test_cmd_scope: full-suite`. Run through `isolated-test-run.sh` on the materialized merge tree in `/var/tmp/gc-merge-validate.yhHL19`. `test_counts: 40 PASS jobs, 0 FAIL jobs, 0 SKIP jobs`; no test SKIP lines were reported in preserved shard logs. Full log: `/var/tmp/ga-406x21-full.log`; shard logs: `/var/tmp/ga-406x21-shards`. `diff_tests_executed: TestDeliverSlingNudgeRequestsManagedWakeForDrainedSession PASS` (1 PASS, 0 FAIL, 0 SKIP in an additional verbose run; `/var/tmp/ga-406x21-diff-test.log`). `waiver_ref: none`. |
| 3a | Failure attribution | PASS | No failures or reported skips required attribution. |
| 3b | Policy and lint lane | PASS | `make test-ci-policy` passed through the isolation wrapper (`/var/tmp/ga-406x21-policy.log`). `make lint-affected && make fmt-check-changed` passed with 0 issues (`/var/tmp/ga-406x21-static.log`). `go vet ./...` passed on the merge tree. |
| 3c | CI configuration lane | PASS | `ci_lane_run: n/a`. The diff changes only `cmd/gc/cmd_sling.go` and `cmd/gc/cmd_nudge_test.go`; it changes no CI job, matrix, timeout, or required check. |
| 4 | No high-severity review findings open | PASS | The review bead records no style or security findings and a PASS verdict; unresolved HIGH findings: 0. |
| 5 | Final branch is clean | PASS | The source worktree and merge-validation worktree both had empty `git status --short` output before this gate file was written. |
| 6 | Branch diverges cleanly from main | PASS | The rebased source merged into the pinned base with no conflicts. `go build ./...` and `go vet ./...` passed on that merged tree. Its file tree matches the same patch rebased onto the current main tip. |
| 7 | Single feature theme | PASS | The two changed files add one behavior and its regression test: explicit managed wake from `gc sling --nudge` for a drained target. Diff: 2 files, 86 insertions, 0 deletions. Ancestry scope check passed for the deploy and build bead IDs; no denied paths were present. |

Preflight found no PR for either the reviewed or rebased source commit. The project's `docs/PROJECT_MANIFEST.md` is absent in this checkout; the deployed release-gate criteria fragment supplied the checklist.

## Pre-push hook attribution

The origin push-target dry run at the first gate commit, `486214bc27080448c19e223fa8075a434fcd40d8`, triggered the pre-push fast suite. Its `unit-cmd-gc-2-of-6` job failed in unchanged `TestPoolSessionCreate_TerminalProviderErrorTearsDownBeforeRollback`: `pool_session_name_churn_test.go:213` found row `gc-2` open after confirmed teardown. This branch predates the terminal-provider-error fix already on `origin/main` at `2eb08ff80dfbe312ebdebbc7e091a5a36945c8d0`; the same named test passed on the merge-validation tree (`/var/tmp/ga-406x21-merge-pool-test.log`).

`failure_attribution: TestPoolSessionCreate_TerminalProviderErrorTearsDownBeforeRollback -> ga-3wy6dw`. Clause 1: its test file is unchanged by this diff. Clause 3(c), COVERAGE: the changed `deliverSlingNudge` function was at 0.0% in the failing named test (`/var/tmp/ga-406x21-prepush-coverage.out`). Clause 4: same-package overlap is covered by that measured proof; there is no resource-census bump, new test target, or new test file. The tracker records this first sighting and the pre-push log `/var/tmp/gc-local-tests.kGE9rm/unit-cmd-gc-2-of-6.log`. The pre-push no-verify exception is scoped to this gate's head and this attributed failure; the full merged-tree release suite passed.

## Head refresh

The table above was recorded before two follow-up commits landed on this branch. This addendum updates it for the current reviewed head and supersedes the corresponding values above.

- **Reviewed/gated commit:** c1bb88e94331cbff1bd676d7878256f93597a65c. It includes `487b520bf` (unmanaged-reconciler test) and the observe-error guard commit `c1bb88e9` itself. The branch tip after that commit is only a merge of `origin/main` and does not change the diff below.
- **Criterion 3 (`diff_tests_executed`):** `TestDeliverSlingNudgeRequestsManagedWakeForDrainedSession PASS`, `TestDeliverSlingNudgeSkipsManagedWakeWhenReconcilerUnmanaged PASS`, `TestDeliverSlingNudgeSkipsManagedWakeWhenObservationFails PASS` (3 PASS, 0 FAIL, 0 SKIP via `go test ./cmd/gc -run 'TestDeliverSlingNudge' -count=1`). `go build ./...` and `go vet ./cmd/gc/...` passed.
- **Criterion 3c:** `ci_lane_run: n/a`. The diff changes only `cmd/gc/cmd_sling.go`, `cmd/gc/cmd_nudge_test.go`, and `release-gates/ga-406x21-managed-sling-wake-gate.md`; it changes no CI job, matrix, timeout, or required check.
- **Criterion 7:** One behavior, its regression tests, and this gate record: explicit managed wake from `gc sling --nudge` for a confirmed not-running managed target, skipped when the reconciler is unmanaged or the target observation fails. Diff: 3 files, 246 insertions, 0 deletions.
