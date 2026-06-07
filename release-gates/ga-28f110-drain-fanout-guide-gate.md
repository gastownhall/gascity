# Release Gate: ga-28f110 drain fan-out guide

Date: 2026-06-06
Deployer: gascity/deployer
PR: https://github.com/gastownhall/gascity/pull/3179
Branch: builder/ga-paqwas.1-drain-fanout-guide
Reviewed commit: 6a2ac7ee21828842d699b13d999c0b94b43f0917
Base checked: origin/main at 9e3c2ce51106337d1c1b0ee977d06faea179d563

Note: `docs/PROJECT_MANIFEST.md` is not present in this checkout, so no
project-specific Release Criteria section was available. This gate uses the
deployer release criteria and `TESTING.md`.

## Gate Summary

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Source review bead `ga-2b690p` is closed with `REVIEWER VERDICT: PASS`; deploy bead `ga-28f110` records reviewer PASS for PR #3179. |
| 2 | Acceptance criteria met | PASS | `engdocs/drain-fanout.md` exists; it includes quick reference, selection guidance, a full drain field table, minimal `graph.v2` TOML example, `gc.output_json` grandfathering guidance, FAQ, and related references. Field references were checked against `internal/formula/types.go`. |
| 3 | Tests pass | PASS | `go test ./test/docsync/...` passed; `make test` passed; `go vet ./...` passed. |
| 4 | No high-severity review findings open | PASS | Reviewer notes contain PASS and no blocker or HIGH findings. |
| 5 | Final branch is clean | PASS | Clean detached gate worktree before writing this checklist; final status rechecked after committing the gate file. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` succeeded; GitHub reports PR #3179 `mergeStateStatus: CLEAN`. |
| 7 | Single feature theme | PASS | Commit set touches one subsystem/theme: documentation for formula drain fan-out (`engdocs/drain-fanout.md`). |

## Changed Surface

| Path | Status | Notes |
|------|--------|-------|
| `engdocs/drain-fanout.md` | Added | Canonical guide for formula authors choosing `drain` for new `graph.v2` fan-out while leaving `gc.output_json` as legacy/grandfathered behavior. |

## Commands Run

```bash
git diff --name-status origin/main...HEAD
git merge-tree --write-tree origin/main HEAD
go test ./test/docsync/...
make test
go vet ./...
```

## Decision

PASS. The branch is ready for merge-authority review.
