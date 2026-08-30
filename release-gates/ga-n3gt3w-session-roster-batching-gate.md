# Release gate: Session roster batching (`ga-n3gt3w`)

Gate result: **FAIL**

- Evaluated: 2026-08-22
- Deploy mode: `remote`
- Base: `origin/main@16f2f3c8466a0f240f10ddaaf38e86d22e54f222`
- Reviewed source: `builder/ga-n3gt3w-lint-fix@9764176843688de1d9db78559ccdb7ebcc3650fb`
- Push remote selected by the required dry-run check: `fork`
- Existing-PR pre-flight: no pull request is associated with the reviewed source commit

## Checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Reviewer PASS present | SKIPPED | Fail-fast after criterion 6. The bead retains the independent reviewer PASS from the earlier reviewed implementation. |
| 2 | Acceptance criteria met | SKIPPED | Fail-fast after criterion 6; acceptance was not re-evaluated against a stale branch. |
| 3 | Tests pass | SKIPPED | The required evaluation order forbids spending a current CI-equivalent run after an unresolved criterion-6 failure. No current test counts, `diff_tests_executed`, waiver, or failure attribution are asserted by this gate attempt. |
| 3b | Policy/lint lane | SKIPPED | Fail-fast after criterion 6. |
| 4 | No high-severity review findings open | SKIPPED | Fail-fast after criterion 6. |
| 5 | Final branch is clean | SKIPPED | Fail-fast after criterion 6. The helper nevertheless verified a clean worktree before attempting the rebase and restored it cleanly afterward. |
| 6 | Branch diverges cleanly from main | **FAIL** | `git merge-tree --write-tree origin/main 9764176843688de1d9db78559ccdb7ebcc3650fb` reported content conflicts in `internal/api/huma_handlers_agents.go`, `scripts/runtime-tmux-tests.manifest`, and `scripts/runtime_tmux_manifest_test.go`. The mandated `attempt_bounded_self_rebase builder/ga-n3gt3w-lint-fix main` with `PUSH_REMOTE=fork` returned `12`, classifying the conflicts as non-trivial and restoring HEAD to the reviewed source SHA. |
| 7 | Single feature theme | SKIPPED | Fail-fast after criterion 6. |

## Disposition

Technical gate failure. Route `ga-n3gt3w` to the builder to rebase the existing `builder/ga-n3gt3w-lint-fix` feature branch onto current `origin/main` and resolve the real content conflicts. The mayor's prior non-attribution ruling covers only the already-documented baseline test failures; it does not waive branch freshness or any new post-rebase failure. Nothing was pushed and no pull request was opened.
