# Release Gate: workspace-trust dialog selection

Bead: `ga-g3ynzz`
Source bead: `ga-rycvqb`
Review bead: `ga-7hlgl0`
Reviewed commit: `1639750d7fc3385be6ccac9a6cf082d477de8e1f`
Base: `origin/main@17333ea115945bae32baac4e12cba832a5b30af0`
Deploy mode: `remote` (push remote: `fork`)

## Gate checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Review bead `ga-7hlgl0` is closed with verdict `pass` at the resolved reviewed commit. The review records no blocker or major findings. |
| 2 | Acceptance criteria met | PASS | `workspaceTrustConfirmKeys` derives cursor movement for Claude, Gemini, and pi layouts, preserves unconditional confirmation for Codex's non-list prompt, and refuses to send keys for unrecognized layouts. Both polling and stream paths use the derived keys. Committed selection cases cover the trust row pre-selected (Enter alone), the cursor on `No, exit` (Down then Enter), the cursor below the trust row (Up then Enter), an unrecognized layout (no keys), a dialog preceded by pane scrollback, and a box-bordered rendering. |
| 3 | Tests pass | PASS | The documented full local sweep executed 40 unit, process, formula, bead-store, runtime/tmux, and REST jobs. Raw result: 48,304 PASS / 4 FAIL / 208 SKIP. All four failures are pre-existing, tracked, non-diff-owned conditions with mechanism proof and no path overlap; attribution is detailed below. Every diff-owned test passed in both the unit and integration-tagged runtime sweeps. |
| 4 | No high-severity review findings open | PASS | The reviewer recorded zero blocker/major findings and one informational Gemini hardening observation that is no worse than the pre-existing behavior. Unresolved HIGH findings: 0. |
| 5 | Final branch is clean | PASS | The reviewed tree was clean before gate generation; the isolated deploy branch was rechecked clean after committing this checklist. Ignored dashboard `node_modules/` and the advisory `.worktree-stale` marker are not candidate changes. |
| 6 | Branch diverges cleanly from main | PASS | Pre-flight found no PR carrying the reviewed commit. `git merge-tree $(git merge-base origin/main 1639750d7f...) origin/main 1639750d7f...` produced no conflict markers. No self-rebase was needed. |
| 7 | Single feature theme | PASS | The two TDD commits touch only `internal/runtime` workspace-trust dialog selection and its tests. `assert_deploy_ancestry_scope origin/main 1639750d7f... ga-g3ynzz ga-rycvqb` passed. |

## Acceptance evidence

- The real Claude pane capture with `No, exit` selected yields `Down`, then `Enter`; a pre-selected trust row yields only `Enter`.
- Missing cursor or trust-row information returns `ok=false`, and the polling handler sends no keys.
- The stream path calls `matchKeysFor` and leaves the snapshot unmatched when the safe key sequence cannot be derived.
- The existing workspace-trust matcher and unrelated dialog handlers are unchanged.
- Changed paths are limited to `internal/runtime/dialog.go`, `internal/runtime/dialog_test.go`, and `internal/runtime/dialog_trust_selection_test.go`.

## Test evidence

- `test_cmd: GOFLAGS=-v make test-local-full-parallel`
- `test_cmd_scope: full-suite`
- `test_counts: 48,304 PASS / 4 FAIL / 208 SKIP`
- `skip_justification: expected pre-existing phase, platform, live-provider, persistence-opt-in, registry, and helper-process skips in the repository's full local runner; none is diff-owned.`
- `diff_tests_executed:`
  - `TestWorkspaceTrustDialogDoesNotConfirmNoExit`: PASS in `unit-core` and `integration-packages-core-3-of-4`
  - `TestWorkspaceTrustConfirmKeysTrustPreSelected`: PASS in both sweeps
  - `TestWorkspaceTrustConfirmKeysNoExitSelected`: PASS in both sweeps
  - `TestWorkspaceTrustConfirmKeysUnrecognizedLayoutSendsNothing`: PASS in both sweeps
  - `TestAcceptStartupDialogsWithTimeoutRefreshesBudgetOnProgress`: PASS in both sweeps
  - `TestWorkspaceTrustConfirmKeysIgnoresScrollbackCursor`: PASS in `go test ./internal/runtime/ -run Trust -count=1`
  - `TestWorkspaceTrustConfirmKeysBorderedLayout`: PASS in `go test ./internal/runtime/ -run Trust -count=1`
  - `TestWorkspaceTrustConfirmKeysUpwardMovement`: PASS in `go test ./internal/runtime/ -run Trust -count=1`
- `waiver_ref: none`
- `ci_lane_run: n/a (no CI configuration change in this diff)`
- Raw logs: `/var/tmp/gascity-gate-ga-g3ynzz.Kqwxsj`

### Failure attribution

- `TestBdFlagManifestCurrent -> ga-f0uceo | clause 3(a): mechanism proven.` The candidate cannot alter `internal/bdflags` or the installed `bd` binary; `go list -deps -test ./internal/bdflags` does not include `internal/runtime`. No failing-test path overlaps the diff.
- `TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix -> ga-esyijp | clause 3(a): mechanism proven.` The fixture's installed `bd` process rejected a dirty-table schema migration under `gastownhall/beads#4566` before any agent or workspace-trust dialog started. No failing-test path overlaps the diff.
- `TestGraphWorkflowFailureRunsCleanup -> ga-esyijp | clause 3(a): mechanism proven.` The fixture failed during the same tracked `bd` schema initialization, before workflow or dialog behavior. No failing-test path overlaps the diff.
- `TestE2E_SuspendResume_City -> ga-dqd7gf | clause 3(a): mechanism proven.` The test uses `e2eReportScript`, not an interactive coding-agent trust prompt, and timed out on the already-tracked `citysus` report under whole-suite load. No failing-test path overlaps the diff.

Each tracker predated this run, was opened before citation, and now contains this run's verified sighting. No inconclusive attribution path was used.

## Policy and static lanes

- `policy_lane: make test-ci-policy` — PASS (Python runner policy, CI suite coverage, `scripts/cipolicy`, `scripts/prwatchdog`, and static-scope contracts).
- `LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main make lint-affected` — raw FAIL, attributed to `ga-u8z8j6`. Conservative stale-head fallback selected full lint, whose only findings were two `govet` and one `revive` diagnostic in ignored, untracked `internal/api/dashboardspa/web/node_modules/flatted/**`; the candidate has no path overlap. Tracker predates this run and contains the new sighting.
- `make check-gomod-replace` — PASS.
- `make check-native-dependency-surface` — PASS.
- `make check-eventexport-isolation` — PASS.
- `make check-core-boundary` — PASS.
- `make test-native-doltlite-beads` — PASS.
- `LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main make fmt-check-changed` — PASS.
- `make vet` — PASS.
- `make check-docs` — PASS.

## Environment integrity

- Rootless Podman socket active at `/run/user/1000/podman/podman.sock` with `TESTCONTAINERS_RYUK_DISABLED=true` prepared before the full sweep.
- No `dolt-tests-via-podman` cairn entry exists for this rig, and the documented full local command has no testcontainers-backed image requirement.
