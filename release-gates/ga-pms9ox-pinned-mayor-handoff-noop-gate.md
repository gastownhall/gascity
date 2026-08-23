# Release Gate: pinned mayor handoff no-op

- Bead: ga-pms9ox
- Source fix bead: ga-fedwpf
- Review bead: ga-sj15c9
- Deploy source SHA: `75b7395905b7d172d9e80ed4403d09e29235b0dd`
- Deploy branch: `deploy/ga-pms9ox-gate`
- Base checked: `origin/main` at `2861a4fd5ef8717781fb665ea486edf75e7d03ef`
- Manifest note: `docs/PROJECT_MANIFEST.md` is not present in this checkout; gate criteria are the deployer prompt criteria.

## Gate Results

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 6 | Branch diverges cleanly from main | PASS | Evaluated first. `git merge-tree --write-tree origin/main 75b7395905b7d172d9e80ed4403d09e29235b0dd` exited 0 and produced tree `1e5c880d77664c4cbe4349bb2ab249935e162534`; `git diff --check origin/main...HEAD` exited 0. |
| 1 | Review PASS present | PASS | `bd show ga-sj15c9` contains `Review verdict: PASS` for commit `75b739590`, with no blocking findings. |
| 2 | Acceptance criteria met | PASS | Code inspection confirmed pinned always-mode sessions now require `persistRestart()` before promising a restart in both `gc handoff` and `gc runtime request-restart`; failures return 1 and suppress false restart messaging. `waitForControllerRestart` now returns success only when the restart flag is clear and `sp.IsRunning(sn)` is false. Regression and unit coverage exists in `cmd/gc/repro_ga_rble9w_test.go`, `cmd/gc/cmd_handoff_test.go`, `cmd/gc/cmd_runtime_drain_test.go`, and `cmd/gc/session_reconciler_restart_request_test.go`. |
| 3 | Tests pass | PASS | `go test ./cmd/gc -run 'TestDoHandoff_PinnedAlwaysSessionRequiresPersistRestart|TestDoRuntimeRequestRestart_PinnedRequiresPersistRestart|TestDoHandoff_PinnedAlwaysSessionPersistsResetAndReconcilerStopsSession|TestRepro_ga_rble9w_PinnedMayorHandoffNeverCyclesAndFalselyReportsSuccess|TestWaitForControllerRestartHandoffFlagClearedButSessionStillRunning'` passed. `make test-fast-parallel` passed all 8 fast jobs. `go vet ./...` passed. `go build ./...` passed. |
| 4 | No high-severity review findings open | PASS | Review notes for ga-sj15c9 record PASS. The only follow-up, ga-e17tpz, is explicitly non-blocking and pre-existing: `gc handoff --target` direct-kill behavior is outside this fix's self-handoff/request-restart scope. |
| 5 | Final branch is clean | PASS | Isolated worktree `/var/tmp/codex-gascity-ga-pms9ox-gate-3554070` was clean before this gate file; after the gate commit, `git status --short --branch` is expected to show only `## deploy/ga-pms9ox-gate`. |
| 7 | Single feature theme | PASS | Single commit touches only `cmd/gc` handoff/runtime restart behavior and adjacent tests: `cmd_handoff.go`, `cmd_runtime_drain.go`, and related `cmd/gc` tests. No independent subsystem or unrelated user-facing behavior is bundled. |

## Acceptance Detail

- Pinned self-handoff no longer claims success when the persistent restart marker is unavailable.
- `gc runtime request-restart` follows the same pinned-session requirement.
- A cleared `GC_RESTART_REQUESTED` flag is no longer treated as proof of restart while the session is still running.
- Non-pinned restart behavior remains best-effort for the persisted bead marker and is covered by the existing passing command tests.
