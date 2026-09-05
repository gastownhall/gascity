# Release gate: startup-health episodes in `gc doctor`

- Bead: `ga-pbakj9`
- Review bead: `ga-y4n4ir`
- Reviewed commit: `773bbf2e38a9ccf0dd73fb4bca51ee632916de2a`
- Base: `origin/main@1f19d26c849b5b4c43c897e9e5651f93d8989a6b`
- Deploy mode: `remote`; push remote resolved to `origin`
- Result: **FAIL** — criterion 6 fails; criteria 1–5 and 7 were skipped by the required fail-fast order.

## Pre-flight

- The recorded value was resolved with `git rev-parse --verify --quiet 773bbf2e38a9ccf0dd73fb4bca51ee632916de2a^{commit}` and returned the same full commit SHA.
- GitHub reports no pull request carrying the reviewed commit, so the already-merged and closed-without-merging reconciliation paths do not apply.
- The source is the internally-authored `builder/ga-o04bfr.1.4` branch. No contributor PR or contributor interaction is involved.
- Scope inspection found one coupled startup-health feature: persistence and reconciler behavior feed the `gc doctor` observation surface. The doctor portion cannot operate without the startup-health store it reads, so the commit set is not a bundle of independently shippable themes.

## Checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | SKIPPED | Not evaluated after criterion 6 failed. The reviewer record remains on `ga-y4n4ir`; this checklist makes no release claim from it. |
| 2 | Acceptance criteria met | SKIPPED | Not evaluated after criterion 6 failed. |
| 3 | Tests pass | SKIPPED | The full-suite command was not run because criterion 6 failed first. No test PASS is claimed. |
| 4 | No high-severity review findings open | SKIPPED | Not evaluated after criterion 6 failed. |
| 5 | Final branch is clean | SKIPPED | Not scored after criterion 6 failed. The bounded helper nevertheless left the worktree clean. |
| 6 | Branch diverges cleanly from main | **FAIL** | Against `origin/main@1f19d26c849b5b4c43c897e9e5651f93d8989a6b`, the candidate is 12 commits ahead and 2 behind, with merge-base `8de6b46df7971efc8b43efe2fab568d123478a38`. `git merge-tree --write-tree` reports add/add conflicts in `internal/session/startup_health.go` and `internal/session/startup_health_test.go`. The mandated bounded self-rebase ran on the exact internal source branch with `PUSH_REMOTE=origin`, returned `rc=12`, aborted cleanly, restored `HEAD` to the reviewed SHA, and left no working-tree changes. |
| 7 | Single feature theme | SKIPPED | The preliminary scope check found one coupled theme, but the formal criterion was not scored after criterion 6 failed. |

## Disposition

This is a technical gate failure. No criterion-3 test run, deploy branch, push, pull request, deploy-clearance status, or merge-request is permitted. Route `ga-pbakj9` back to the builder to reconcile the two startup-health add/add conflicts against current `origin/main`, rerun the required evidence, and return the new commit through review.
