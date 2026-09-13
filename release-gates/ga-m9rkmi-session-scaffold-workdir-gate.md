# Release Gate: session scaffold staging work_dir resolution

Bead: `ga-m9rkmi`

Source review beads: `ga-yuij34`, `ga-ji89ce`

Candidate branch: `origin/builder/ga-ajw1no.2-session-scaffold-workdir`

Deploy gate branch: `deploy/ga-m9rkmi-session-scaffold-workdir-gate`

Candidate SHA: `74fdd38a7eb8c1bc8b2dd411c5c695507b965f90`

Candidate cut point: `origin/main` at `5becf8854dc357392bef81e8da6eea9486a49999`

Current `origin/main` during deploy gate: `5becf8854dc357392bef81e8da6eea9486a49999`

`docs/PROJECT_MANIFEST.md` is not present in this checkout; this gate uses the deployer prompt criteria and the repo testing guidance in `TESTING.md`.

## Release Unit

Included commits:

| Commit | Summary |
| --- | --- |
| `5456d8ecf` | Reproduce session scaffold workdir leakage. |
| `74fdd38a7` | Resolve task `work_dir` against the city root, not the reconciler process cwd. |

Scoped file diff from the candidate cut point:

```text
M cmd/gc/assigned_work_scope_test.go
M cmd/gc/session_lifecycle_parallel.go
M cmd/gc/session_lifecycle_parallel_test.go
M cmd/gc/session_reconciler.go
A cmd/gc/session_scaffold_staging_test.go
M internal/runtime/tmux/staging_test.go
```

The branch is based directly on current `origin/main`. Merge conflict check passed with `git merge-tree $(git merge-base origin/main HEAD) origin/main HEAD`; no conflict diagnostics were emitted.

## Acceptance Evidence

The candidate addresses the reviewed failure mode:

- `resolveTaskWorkDir` and `newAssignedTaskWorkDirResolver` now resolve relative bead `work_dir` metadata against `cityPath` before statting it.
- Already absolute `work_dir` values pass through unchanged via `filepath.IsAbs`.
- `buildPreparedStartWithWorkDirResolver` and `resolvePreparedTaskWorkDir` receive `cityPath` so the prepared start path and reconciler snapshot path agree.
- Rendered `PreStart` commands are retargeted when a task-level workdir override replaces the original template workdir, keeping scaffold materialization under the final session workdir.
- The new regression proves a shared builder cwd does not receive a bead-slug scaffold directory, while the resolved city worktree receives `.claude`, `.codex`, and `.gc` scaffold files.
- The companion cleanup review `ga-ji89ce` verified no separate deployable artifact: it confirmed live stray scaffold cleanup and the same code branch reviewed under `ga-yuij34`.

## Criteria

| # | Criterion | Result | Evidence |
| --- | --- | --- | --- |
| 1 | Review PASS present | PASS | `ga-yuij34` is closed with `REVIEWER VERDICT: PASS` for commits `5456d8ecf` and `74fdd38a7`. `ga-ji89ce` is closed with `REVIEWER VERDICT: PASS` for the companion cleanup verification. |
| 2 | Acceptance criteria met | PASS | Code inspection confirmed city-root-relative resolution for task `work_dir`, absolute-path preservation, retargeting of rendered `PreStart` commands, and generic session/work_dir handling with no role-specific branch. New tests cover the shared-cwd leak and tmux scaffold staging guard. |
| 3 | Tests pass | PASS | `go test ./cmd/gc ./internal/runtime/tmux -run 'TestPrepareStartCandidateStagesScaffoldInResolvedTaskWorkDirWhenCWDIsSharedWorktree|TestStageStartFilesKeepsScaffoldOutOfSpawnerCWD|TestStartCandidate|TestResolveTaskWorkDir|TestSessionStart' -count=1`; `go build ./...`; `go vet ./...`; `go test ./internal/api -run TestHandleExtMsgInboundDefaultRouteMatchesMixedCaseProvider -count=1 -v`; `go test ./internal/api -run TestHandleExtMsgInboundDefaultRouteMatchesMixedCaseProvider -count=3 -v`; `go test ./internal/api -count=1`; final `make test-fast-parallel` retry passed all 8 fast jobs. Initial `make test-fast-parallel` attempt failed only in unrelated `internal/api` with `TempDir RemoveAll cleanup: directory not empty`; the failing test and full package passed immediately on rerun before the clean full fast-target retry. |
| 4 | No high-severity review findings open | PASS | Review notes contain PASS verdicts and no unresolved HIGH findings. The reviewer recorded two optional non-blocking observations for future audit only: a similar relative-path pattern in retry dispatch and a harmless `cityPath=""` rebuild path. |
| 5 | Final branch is clean | PASS | `git status --short --branch` was clean before adding this gate artifact; the gate artifact is the only deployer change and is committed on the deploy gate branch. |
| 6 | Branch diverges cleanly from main | PASS | `origin/main` and `HEAD` share merge base `5becf8854dc357392bef81e8da6eea9486a49999`; `git merge-tree` against current `origin/main` emitted a clean merged tree with no conflicts. |
| 7 | Single feature theme | PASS | The commit set touches one subsystem and one behavior: session startup workdir resolution and scaffold staging for assigned task worktrees. All changed files are in `cmd/gc` session lifecycle/reconciler tests or `internal/runtime/tmux` staging tests. |

## Gate Verdict

PASS.
