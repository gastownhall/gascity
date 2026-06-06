# Release Gate: doctor backlog-depth Ready error coverage

Date: 2026-06-05
Deployer: gascity/deployer
Primary deploy bead: ga-acgsbx
PM deploy bead: ga-5gd6pa.2
Source review bead: ga-04k6nl
Source implementation bead: ga-j5n5xr

## Candidate

- Branch: `fix/ga-5gd6pa1-doctor-backlog-depth-ready-error`
- Reviewed commit: `74475ca78c6fc6d42521063c7a40a8893844e652`
- Existing PR: https://github.com/gastownhall/gascity/pull/3134
- Reviewer-visible diff:
  - `cmd/gc/doctor_backlog_depth.go`
  - `cmd/gc/doctor_backlog_depth_ready_error_test.go`
  - `cmd/gc/doctor_backlog_depth_test.go`

## Gate Results

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `ga-04k6nl` is closed with review verdict PASS. Reviewer notes cite commit `74475ca78c6fc6d42521063c7a40a8893844e652`, branch `fix/ga-5gd6pa1-doctor-backlog-depth-ready-error`, four passing doctor backlog-depth tests, `go vet ./cmd/gc/` clean, and no blockers. |
| 2 | Acceptance criteria met | PASS | `ga-j5n5xr` requested `TestBacklogDepthCheckReadyErrorIsGraceful` for `store.Ready()` failures. The branch adds that test and verifies `StatusWarning`, not `StatusError`, the Ready error context, and `CanFix() == false`. The implementation now builds the claimable set from `store.Ready()` so dep-blocked beads are excluded from the real backlog count. `ga-5gd6pa.2` isolation criteria are met: the diff is limited to three doctor backlog-depth files and excludes order-dispatch, config/schema, macOS icu4c test-script, prior release-gate, and internal planning-doc changes. |
| 3 | Tests pass | PASS | `go test ./cmd/gc -run 'TestBacklogDepth|TestClassifyBacklog' -count=1` passed. `make test` passed with `observable go test: PASS log=/tmp/gascity-test.jsonl.hz6WVm`. `go vet ./...` passed. |
| 4 | No high-severity review findings open | PASS | Reviewer notes on `ga-04k6nl` list no blockers or security findings; only one non-blocking nit about a defensive duplicate status check. |
| 5 | Final branch is clean | PASS | Evaluated in clean detached deploy worktree at `74475ca78c6fc6d42521063c7a40a8893844e652`; no uncommitted changes before gate file creation. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree` against `origin/main` produced no conflict markers or unmerged conflict records. |
| 7 | Single feature theme | PASS | Commit set touches only the doctor backlog-depth check and its tests. The change is one subsystem and one user-visible behavior: `gc doctor` backlog-depth now counts dep-ready work from the store and reports Ready lookup failures as advisory warnings. |

## Deploy Decision

PASS. Commit this gate checklist to the existing PR branch, push the branch, reuse PR #3134, close the deploy beads with the PR URL, and route a merge-request to mayor. Deployer does not merge.
