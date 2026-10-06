# Release Gate: Ephemeral work-query dependency gating

- Deploy bead: `ga-7ya515`
- Build bead: `ga-9q0k4o`
- Reviewed candidate: `3e7f437a81bc3fe5348b8af0e1ea0ad2e93d00f9`
- Source branch: `builder/ga-9q0k4o-ephemeral-workquery-blocked`
- Base checked: `origin/main@7c817e0640fae801631043005f1d54b17ce3e97c`
- Deploy mode: remote (`origin` and `fork` are GitHub remotes)
- Verdict: **FAIL**

The repository does not contain `docs/PROJECT_MANIFEST.md` at this revision.
This checklist applies the seven criteria in the active deployer protocol and
the release-evidence requirements in `TESTING.md` and
`engdocs/contributors/release-gate-criteria-conventions.md`.

## Pre-flight

The already-merged pre-flight found no pull request carrying the reviewed SHA.
The GitHub commit-to-PR lookup returned `422 No commit found for SHA`, so the
candidate is not present in the base repository's pull-request graph. No pull
request was inspected or mutated.

## Release criteria

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present for the deployed commit | **SKIPPED** | Fail-fast after criterion 6. The bead does contain the prior reviewer PASS for the exact candidate, but no new deploy decision relies on it because the branch is stale. |
| 2 | Acceptance criteria met | **SKIPPED** | Fail-fast after criterion 6; acceptance was not re-evaluated against a branch that cannot merge into current main. |
| 3 | Tests pass | **SKIPPED** | The deployer protocol requires criterion 6 before the expensive test lanes. No tests were run on this retry. `test_counts`: 0 PASS, 0 FAIL, 0 SKIP because the lane was not started. `diff_tests_executed`: not run (criterion-6 fail-fast). `waiver_ref`: none. `policy_lane`: not run (criterion-6 fail-fast). |
| 4 | No high-severity review findings open | **SKIPPED** | Fail-fast after criterion 6. |
| 5 | Final branch is clean | **SKIPPED** | Fail-fast after criterion 6. The working tree was clean before this gate artifact was added, and the bounded self-rebase restored the original candidate unchanged. |
| 6 | Branch diverges cleanly from main | **FAIL** | `git merge-tree --write-tree origin/main 3e7f437a81bc3fe5348b8af0e1ea0ad2e93d00f9` reported content conflicts. The required `attempt_bounded_self_rebase builder/ga-9q0k4o-ephemeral-workquery-blocked main` returned rc 12 (real/non-trivial conflict), aborted, and left HEAD at the reviewed SHA. |
| 7 | Single feature theme | **SKIPPED** | Fail-fast after criterion 6. |

## Criterion 6 evidence

The candidate diverged from merge base
`a4e4cc2bfac251b65116d536addbb4a7be9d95cd`; current `origin/main` is 94
commits ahead of that base. The merge-tree check reported conflicts in:

- `TESTING.md`
- six federated work-query golden fixtures under
  `internal/config/testdata/workquery/`
- `internal/config/workquery.go`
- `internal/testpolicy/resourcecensus/census.go`
- `test/test-resources.toml`

The canonical bounded self-rebase was sourced from
`packs/actual/deployer/scripts/rebase-resolve-lib.sh`. It returned rc 12 and
restored the branch exactly:

```text
before=3e7f437a81bc3fe5348b8af0e1ea0ad2e93d00f9
rc=12
after=3e7f437a81bc3fe5348b8af0e1ea0ad2e93d00f9
```

## Gate verdict

**FAIL — criterion 6.** The reviewed candidate needs a builder-owned rebase
onto current main and a fresh exact-SHA review before release evaluation can
continue. No feature branch was pushed, no pull request was opened, and no
deploy-clearance status was created.
