# Release gate: orphan-sweep live-claim preservation (`ga-sqv77i`)

Gate result: **PASS**

- Evaluated: 2026-09-01 PDT
- Deploy mode: `remote`
- Base: `origin/main@3a75c71944bd1e551e486203dedc16438cd70272`
- Reviewed deploy source: `1450b1be6d9bbba8d61399b8fae30614ed037dfa`
- Source branch: `builder/ga-7p4aab` (provenance only)
- Deploy branch: `deploy/ga-sqv77i-gate`
- Existing-PR pre-flight: no pull request is associated with the reviewed deploy source

## Checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Reviewer PASS present | **PASS** | Closed review bead `ga-04pd5t` records `verdict: pass` and the corrected exact `deploy_commit: 1450b1be6d9bbba8d61399b8fae30614ed037dfa`. |
| 2 | Acceptance criteria met | **PASS** | The exact reviewed source preserves claims when session evidence is empty or unverifiable, recognizes a live pool seat through `gc.session_name`, and records a cause on every reset. All four added acceptance tests pass by name. |
| 3 | Tests pass | **PASS with attribution** | The documented `make test-local-full-parallel` union completed 35/40 jobs PASS and 5/40 FAIL with 0 observed test SKIP. No failure is candidate-owned: one is installed-`bd` manifest drift (`ga-f0uceo`), three are the beads#4566 dirty-schema fixture failure (`ga-esyijp`), and one is the whole-suite city-report timeout (`ga-vkhfnj`). All trackers predate this run. The candidate-specific tests and census assertion pass independently. |
| 3b | Policy/lint lane | **PASS** | `make test-ci-policy`, clean-cache merge-base-scoped `make lint-affected` (full reverse-dependent selection, 0 issues), `make fmt-check-changed`, `make vet`, `bash -n` on the script, and `git diff --check` all pass. |
| 3c | CI-config lane run | **PASS / n/a** | No workflow, matrix, timeout, required-check list, build-policy file, or CI configuration changed. |
| 4 | No high-severity review findings open | **PASS** | Review bead `ga-04pd5t` records no blocking style, security, specification, or coverage finding. |
| 5 | Final branch clean | **PASS** | The isolated deploy branch was clean before this gate record was written, and `git diff --check origin/main...1450b1be6d9bbba8d61399b8fae30614ed037dfa` passed. |
| 6 | Branch diverges cleanly from main | **PASS** | After a fresh fetch, `git merge-tree --write-tree --messages origin/main 1450b1be6d9bbba8d61399b8fae30614ed037dfa` returned exit 0 and tree `da47b1cc6e5f59177b82475d5c47ff33241aff93`; its sole message was `Auto-merging TESTING.md`. The candidate is 5 commits behind and 3 commits ahead of the checked base. |
| 7 | Single feature theme | **PASS** | The three TDD commits and five changed files are confined to orphan-sweep live-claim preservation, its regression coverage, and the required three-way resource-census synchronization. |

## Test evidence

- `test_cmd`: `LOCAL_TEST_JOBS=4 make test-local-full-parallel`
- `test_cmd_scope`: `full-suite` (the documented 40-job local union; fan-out reduced to four concurrent jobs)
- `test_counts`: 35 PASS jobs, 5 FAIL jobs, 0 observed test SKIP
- `full_log`: `/var/tmp/ga-sqv77i-gate-full-suite-r2.out`
- `shard_logs`: `/var/tmp/gc-local-tests.trjiSW`
- `diff_tests_executed`:
  - `TestOrphanSweepSkipsRigWhenSessionListSucceedsButReportsZeroSessions`: PASS
  - `TestOrphanSweepPreservesPoolSeatWithOnlySessionNameMetadataWhenLive`: PASS
  - `TestOrphanSweepTreatsUnresolvableDoubleDashAssigneeAsUnverifiable`: PASS
  - `TestOrphanSweepRecordsCauseNoteOnEveryReset`: PASS
  - `TestRepositoryLedgerMatchesCensusAndDocumentation`: PASS
- `failure_attribution`:
  - `TestBdFlagManifestCurrent`: `ga-f0uceo`; the installed `bd` exposes flags absent from the repository manifest. The tracker was created 2026-08-15, and the candidate changes neither `internal/bdflags` nor the installed binary.
  - `TestAdoptPRFormulaRetriesTransientReviewerStep`, `TestRetryManagedPooledWorkerRecoversClaimedAttemptAfterCrash`, and `TestGraphWorkflowSuccessPath`: `ga-esyijp`; each failed in fixture `gc init` before its scenario because beads#4566 rejected pre-existing dirty schema tables. The canonical tracker was created 2026-08-29. Candidate paths do not alter beads schema migration or integration fixture initialization.
  - `TestE2E_SuspendResume_City`: `ga-vkhfnj`; the resumed `citysus` session did not produce its report within the test budget under whole-suite load. The canonical tracker was created 2026-08-29 and consolidates earlier exact candidate/base reproductions. Candidate paths do not alter suspend/resume or session-report behavior.
- `skip_justification`: none; no test SKIP was observed
- `waiver_ref`: none; all red results satisfy the documented non-diff-owned failure attribution protocol
- `ci_lane_run`: n/a; no CI-config change

## Static and focused evidence

- `make test-ci-policy`: PASS
- `GOLANGCI_LINT_CACHE=/var/tmp/ga-sqv77i-golangci-cache-r2 LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=a4361e58228b82b668609c19159031baa0d6928c make lint-affected`: PASS, full reverse-dependent selection, 0 issues
- `make fmt-check-changed`: PASS; no changed existing Go files relative to current `origin/main`
- `make vet`: PASS (`go vet ./...`)
- `bash -n internal/bootstrap/packs/core/assets/scripts/orphan-sweep.sh`: PASS
- focused four-test orphan-sweep command: 4 PASS, 0 FAIL, 0 SKIP
- focused resource-census command: 1 PASS, 0 FAIL, 0 SKIP
- `git diff --check origin/main...HEAD`: PASS

The first lint attempt used the host's shared golangci-lint cache and emitted
stale diagnostics for already-removed sibling worktrees. A fresh on-disk
golangci-lint cache eliminated those invalid cache entries and completed the
same full reverse-dependent package selection with zero issues. The shared Go
build cache was not cleared or replaced.

## Disposition

Technical gate PASS. Publish `deploy/ga-sqv77i-gate`, open a pull request to
`main`, attach exact-head deploy clearance, and route the merge request to the
mayor/MPR. The deployer does not merge the pull request.
