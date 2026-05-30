# Release Gate: ga-pxbjxw BD_VERSION v1.0.5

Status: FAIL
Date: 2026-05-30

## Scope

- Deploy bead: ga-pxbjxw
- Source review bead: ga-wl0hix
- Branch: builder/ga-l2souo-6-e2e
- Reviewed commit: a0dabf3062b9c9eba687a62409fa26fd0919a365
- Base checked: origin/main f272db921
- Manifest note: docs/PROJECT_MANIFEST.md was not present in this checkout, so this gate uses the deployer role release criteria.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `bd show ga-wl0hix` contains `VERDICT: PASS`; reviewer mail `gm-wisp-qsdv1t` says commit `a0dabf306` passed reviewer gate. |
| 2 | Acceptance criteria met | PASS | Commit `a0dabf306` bumps all checked workflow `BD_VERSION` definitions to `v1.0.5`, adds four `v1.0.5` SHA-256 pins in `.github/scripts/install-bd-archive.sh`, and removes the bd-specific CVE-2026-34986 and CVE-2026-41602 suppressions from `.trivyignore.yaml`. `git show a0dabf306:.github/scripts/install-bd-archive.sh \| bash -n` and `git diff --check origin/main..a0dabf306 --` both passed. |
| 3 | Tests pass | FAIL | Deployer did not run the final test command because the branch fails the clean-branch and single-theme gates below. Reviewer evidence reports prior `make test` and `go vet ./...` on the submitted branch, but those are not sufficient for release from this invalid final branch. |
| 4 | No high-severity review findings open | PASS | Reviewer notes list spec, security, and style as PASS with no blocking or high-severity findings. |
| 5 | Final branch is clean | FAIL | `git status --short --branch` in the branch worktree reports an uncommitted deletion: `D schemas/convoy/target/result.schema.json`. |
| 6 | Branch diverges cleanly from main | FAIL | `git merge-tree $(git merge-base a0dabf306 origin/main) a0dabf306 origin/main` reports conflicts, including `.trivyignore.yaml`. The branch is not directly releasable against current `origin/main`. |
| 7 | Single feature theme | FAIL | `git log origin/main..a0dabf306` contains 18 commits spanning native beads store work, API/dashboard generated files, docs, Go dependency changes, release-gate artifacts, and the BD_VERSION bump. The reviewed BD bump itself is focused, but the release branch commit set is not a single feature theme. |

## Required Follow-Up

Prepare a clean release branch from current `origin/main` containing only the BD_VERSION v1.0.5 change, with `.trivyignore.yaml` reconciled against the current mainline security suppressions. Rerun builder verification and send it back for release evaluation.
