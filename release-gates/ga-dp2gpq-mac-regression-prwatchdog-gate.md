**Verdict:** **FAIL**

# Release gate: clean Mac Regression/prwatchdog split (ga-dp2gpq)

- Deploy mode: `remote`
- Base ref: `origin/main`
- Reviewed commit: `3a68249aeb087e93c5923b11294bb129f6f5f50e`
- Base tip evaluated: `fea5c10c810d025a26cb77f7267bf422105c62a5`
- Push remote: `fork`
- Pre-flight: PASS — no pull request carries the reviewed commit, and `git cherry -v origin/main <reviewed>` reports all four feature commits as unmerged.

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | SKIPPED | Fail-fast after criterion 6, per evaluation order. Reviewer bead `ga-fa0ux1` does contain an explicit PASS at the reviewed commit. |
| 2 | Acceptance criteria met | SKIPPED | Fail-fast after criterion 6. |
| 3 | Tests pass | SKIPPED | Fail-fast after criterion 6; no test command was run on a deployable head. |
| 3a | Pre-existing failures attributed | SKIPPED | No criterion-3 run occurred. |
| 3b | Policy/lint lane | SKIPPED | Fail-fast after criterion 6. |
| 3c | Changed CI lane run | SKIPPED | Fail-fast after criterion 6; no draft PR or workflow dispatch was opened. |
| 4 | No high-severity review findings open | SKIPPED | Fail-fast after criterion 6. |
| 5 | Final branch is clean | SKIPPED | No final deploy branch was cut. |
| 6 | Branch diverges cleanly from main | **FAIL** | `origin/main` was not an ancestor of the reviewed commit. The authorized `attempt_bounded_self_rebase pm/ga-ma70z5.1-mac-clean main` rebased locally but returned `rc=13` when its guarded `--force-with-lease` push could not be completed. The local post-attempt head is `0217ff28726b945f825425791e369a11eea55f50`, but it is not an approved deploy source because the bounded exception did not report success or emit `AFTER_SHA`. No retry or bypass was attempted. |
| 7 | Single feature theme | SKIPPED | Fail-fast after criterion 6. |

## Disposition

Technical gate failure. Route to the builder to produce and review a fresh commit based on current `origin/main`, then return it through review and deployment. The rejected bundled commit `6ef0f408f34012960682e35ff411fb315aa472e5` remains forbidden as a deploy source.
