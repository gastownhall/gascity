# Release gate: nudge dialog-blocked delivery guard

- Deploy bead: `ga-xo0m1j`
- Review bead: `ga-1j7nyl`
- Reviewed commit: `a8c041aaac93780473df98b1ec1cd2ac75e7304c`
- Base: `origin/main` at `89400c6e710b0e95b5b6ee891447470d1434770d`
- Deploy mode: `remote`
- Push remote: `fork`
- Result: **FAIL**

The target commit was not already associated with a pull request, so the
post-merge reconciliation path did not apply. Gate evaluation then stopped at
criterion 6, as required by the fail-fast ordering.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | SKIPPED | Not evaluated after criterion 6 failed. The reviewer record remains available on `ga-1j7nyl`. |
| 2 | Acceptance criteria met | SKIPPED | Not evaluated after criterion 6 failed. |
| 3 | Tests pass | SKIPPED | The full suite was intentionally not run because criterion 6 failed first. `test_cmd_scope: not-run`; `diff_tests_executed: not-run`; `waiver_ref: none`. |
| 3b | Policy/lint lane | SKIPPED | Not evaluated after criterion 6 failed. |
| 3c | CI-config lane | SKIPPED | Not evaluated after criterion 6 failed. |
| 4 | No unresolved HIGH review findings | SKIPPED | Not evaluated after criterion 6 failed. |
| 5 | Final branch clean | SKIPPED | Not scored after criterion 6 failed. The branch was nevertheless restored clean after the bounded attempt. |
| 6 | Branch diverges cleanly from main | **FAIL** | `git merge-tree --write-tree origin/main a8c041aaac93780473df98b1ec1cd2ac75e7304c` exited 1. Current `main` and the reviewed commit independently regenerated dashboard/API artifacts, causing rename/rename conflicts across hashed dashboard assets plus content conflicts in `internal/api/dashboardspa/dist/index.html`, generated TypeScript clients, and related generated surfaces. The authorized `attempt_bounded_self_rebase builder/ga-1yqxh7.1 main` returned rc 12, refused to classify the conflict set as provably trivial, and restored HEAD unchanged at the reviewed SHA with a clean worktree. |
| 7 | Single feature theme | SKIPPED | Not evaluated after criterion 6 failed. |

## Disposition

No deploy branch was cut, no branch was pushed, and no pull request was opened.
The implementation must return to the builder for a fresh rebase and generated
artifact refresh against current `main`, followed by review and a new deploy
handoff.
