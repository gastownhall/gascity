# Release Gate: push-ownership-guard multi-level bead ID

Status: PASS

Source bead: ga-nnjcuc.2
Deploy bead: ga-jzrl1m
Branch: deploy/ga-jzrl1m-gate
Reviewed commit: 7af0671436ba10f2e0f275e3ef627393806651ef
Reviewed commits: d877d93e5643f829dcd584b2de0f7cfba661e223, 7af0671436ba10f2e0f275e3ef627393806651ef

`docs/PROJECT_MANIFEST.md` is not present in this worktree, so this gate uses
the deployer role's release criteria table plus the repo testing policy in
`TESTING.md`.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `bd show ga-nnjcuc.2` contains `REVIEWER VERDICT: PASS` for branch `builder/ga-nnjcuc.2` at commit `7af0671436ba10f2e0f275e3ef627393806651ef`. |
| 2 | Acceptance criteria met | PASS | Candidate confirms the fix was not already on `origin/main`: `origin/main:scripts/push-ownership-guard.sh` still uses `ga-[0-9a-z]{6}(\.[0-9]+)?`, while this branch uses `ga-[0-9a-z]{6}(\.[0-9]+)*`. The branch changes only `scripts/push-ownership-guard.sh` and `scripts/test-push-ownership-guard.sh`, adds a regression for `ga-o3ko1j.4.3`, and excludes the unrelated dead-assignee fallback theme. |
| 3 | Tests pass | PASS | `bash scripts/test-push-ownership-guard.sh` passed 20/20; `shellcheck scripts/push-ownership-guard.sh scripts/test-push-ownership-guard.sh` passed; `go build ./...` passed; `go vet ./...` passed; after adding this gate file, `go test ./test/docsync/...` passed. A pre-push dry-run from this live city worktree failed `cmd/gc` shard 5 at `TestErrorReturningSessionProviderFactoriesPreserveSuccessBehavior/default` with ambient native-store schema mismatch; the same focused test passed in clean temporary worktrees for both `origin/main` and this deploy branch, so it is environment-specific to the live city worktree, not introduced by this shell-only diff. |
| 4 | No high-severity review findings open | PASS | Reviewer notes report no security findings and no HIGH or CRITICAL findings. |
| 5 | Final branch is clean | PASS | Before adding this gate file, `git status --short --branch` reported `## deploy/ga-jzrl1m-gate` with no file changes. The final branch is clean after committing this gate file. |
| 6 | Branch diverges cleanly from main | PASS | Evaluated first: `git merge-tree --write-tree origin/main 7af0671436ba10f2e0f275e3ef627393806651ef` exited 0 and produced merged tree `949a286481ced522c123ee393cefacfac47ce674`. |
| 7 | Single feature theme | PASS | `git diff --name-status origin/main..7af0671436ba10f2e0f275e3ef627393806651ef` lists only `scripts/push-ownership-guard.sh` and `scripts/test-push-ownership-guard.sh`; both commits are the regression test and fix for full multi-level dotted bead ID resolution. |

## Acceptance Evidence

- `_pog_resolve_bead_id` now extracts repeated dotted suffixes from branch names
  with `ga-[0-9a-z]{6}(\.[0-9]+)*`, so `builder/ga-o3ko1j.4.3-*` resolves to
  `ga-o3ko1j.4.3` instead of truncating to `ga-o3ko1j.4`.
- `scripts/test-push-ownership-guard.sh` adds
  `test_bead_id_branch_resolves_multi_level_subbead_id`, which forces the
  branch resolver to expose the full grandchild bead ID in the branch-vs-fallback
  warning and passes under the fixed regex.
- The branch is a two-commit extraction from `origin/main`: red test
  `d877d93e5643f829dcd584b2de0f7cfba661e223` followed by green fix
  `7af0671436ba10f2e0f275e3ef627393806651ef`.
