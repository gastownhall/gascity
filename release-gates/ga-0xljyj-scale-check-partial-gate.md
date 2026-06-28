# Release Gate: ga-0xljyj scale_check partial create gate

## Summary

- Deploy bead: `ga-0xljyj.2` - Gate and PR the partial scale_check create-gate branch
- Work bead: `ga-0xljyj` - fix: gate pool session creates on partial scale_check reads
- Source branch: `builder/ga-0xljyj-scale-check-partial-gate`
- PR: https://github.com/gastownhall/gascity/pull/3686
- Reviewed code head: `108c4a3e28782aa4e5ac3ae30b4c80f697299cca`
- Base checked: `origin/main` at `32ca47acd639b80eee37f4623d0277018b674c06`
- Note: `docs/PROJECT_MANIFEST.md` is not present in this worktree, so this gate uses the deployer release criteria and the bead acceptance criteria.

## Gate Checklist

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead `ga-0xljyj.1` is closed with re-review PASS by `reviewer-gm-wisp-fgacy95` at `108c4a3e28782aa4e5ac3ae30b4c80f697299cca`. |
| 2 | Acceptance criteria met | PASS | Branch implements `poolScaleCheckPartialTemplates`, partial-read fresh-create refusal, exact sentinel text `pool session create skipped: demand read partial`, log suffix `(partial demand read, fresh create blocked)`, and retained-capacity narrowing with the `isPendingPoolCreate` fallback. Tests cover no-create on partial reads, alive-session retention, in-flight create retention, and stale-create rollback. |
| 3 | Tests pass | PASS | `make test-fast-parallel` passed all fast shards. `go vet ./...` passed. `go build ./cmd/gc` plus built `gc --help` smoke passed. |
| 4 | No high-severity review findings open | PASS | The only recorded reviewer finding was MEDIUM spec wording; it was resolved by `108c4a3e2`. No HIGH findings are recorded in the review bead. |
| 5 | Final branch is clean | PASS | Final verification after this gate commit: `git status --short --branch` is clean. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-base origin/main HEAD` equals `origin/main` (`32ca47acd639b80eee37f4623d0277018b674c06`), so the branch is directly based on current `origin/main` with no merge conflicts. |
| 7 | Single feature theme | PASS | Net PR diff is confined to `cmd/gc` desired-state planning, associated `cmd/gc` tests, and this release gate. Two stale gate artifacts from earlier deploy attempts were removed from the branch: `ga-f963pu-scale-check-partial-gate.md` and `ga-rowxra-scale-check-partial-gate.md`. |

## Deploy Bead Acceptance

| Acceptance criterion | Result | Evidence |
|---------------------|--------|----------|
| Start only after review PASS | PASS | `ga-0xljyj.1` recorded re-review PASS before deploy claim. |
| Prepare branch without unrelated changes | PASS | Branch was fast-forwarded to the latest remote tip, then stale gate artifacts were removed. Final net diff contains only the partial scale_check create-gate implementation, tests, and this gate file. |
| Satisfy ga-0xljyj scope and avoid ga-4qbgqf.2/.3 conflicts | PASS | `ga-4qbgqf.2` create-gate contract and `ga-4qbgqf.3` retention contract were checked against the final code. The accepted sentinel/log wording and retainable/preservable split match. |
| Run required checks | PASS | `make test-fast-parallel`, `go vet ./...`, and build smoke passed. |
| Open or update PR and record release result | PASS | Existing PR #3686 will be updated to reference this gate file and final head SHA. |
| Route merge only after gate PASS | PASS | Merge request will be mailed to mayor after branch push and bead close. Deployer will not merge. |

## Command Evidence

```text
make test-fast-parallel
All fast jobs passed

go vet ./...
PASS (no output)

go build ./cmd/gc && gc --help smoke
PASS
```
