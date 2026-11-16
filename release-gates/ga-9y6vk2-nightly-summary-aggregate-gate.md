# Release gate: Nightly D5 aggregate summary (`ga-9y6vk2`)

- Deploy bead: `ga-9y6vk2`
- Source/build bead: `ga-clmemp.3` (implementation branch provenance: `builder/ga-282rbv`)
- Review bead: `ga-22e36g` — PASS
- Reviewed source: `218e69a46b5bce9db1528e519c7af3ab42279335`
- Base: `origin/main@d807c8e0dd6d071a79e821be5da73097dbf4ff96`
- Merge base: `e589fdea3330ca33b7f6b53a1bc90699893ab4f3`
- Deploy mode: `remote`
- Evaluated: 2026-09-17
- Pre-flight: the reviewed source has no associated pull request, so it has not already merged

**Verdict:** **FAIL**

The mayor granted `waiver_ref mayor-2026-09-17-ga-9y6vk2-c3` for criterion
3c only. The gate failed earlier at criterion 6, which the waiver does not
cover. No tests, push, pull request, or deploy-clearance status were attempted
after that fail-fast result.

## Gate checklist

| # | Criterion | Verdict | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **SKIPPED** | Fail-fast after criterion 6. The recorded review remains `ga-22e36g` at exact SHA `218e69a46b5bce9db1528e519c7af3ab42279335`. |
| 2 | Acceptance criteria met | **SKIPPED** | Fail-fast after criterion 6. |
| 3 | Tests pass | **SKIPPED** | Fail-fast after criterion 6; the criterion-3c waiver was not reached and cannot waive a criterion-6 failure. |
| 4 | No high-severity review findings open | **SKIPPED** | Fail-fast after criterion 6. |
| 5 | Final branch is clean | **SKIPPED** | Fail-fast after criterion 6. The bounded self-rebase restored the original reviewed SHA and left the worktree clean before this evidence file was written. |
| 6 | Branch diverges cleanly from main | **FAIL** | Evaluated first against `origin/main@d807c8e0dd6d071a79e821be5da73097dbf4ff96`. `git merge-tree --write-tree origin/main 218e69a46b5bce9db1528e519c7af3ab42279335` reported content conflicts in `TESTING.md`, `internal/testpolicy/resourcecensus/census.go`, and `test/test-resources.toml`. The mandated `attempt_bounded_self_rebase builder/ga-282rbv main` returned `rc=12`, identifying non-trivial conflicts; it aborted and restored `HEAD` to the reviewed SHA with a clean tree. |
| 7 | Single feature theme | **SKIPPED** | Fail-fast after criterion 6. |

## Disposition

No PR may be opened from this candidate. Return `ga-9y6vk2` to the builder to
rebase the Nightly aggregate work onto current main, resolve the three policy
and resource-census conflicts, rerun the affected checks, and obtain a fresh
review for the resulting exact SHA. The existing criterion-3c waiver is bound
to candidate `218e69a46b5bce9db1528e519c7af3ab42279335` and must not be carried to a
different candidate without a new mayor grant.
