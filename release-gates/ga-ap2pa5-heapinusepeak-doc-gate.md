# Release Gate: HeapInusePeakTarget Comment Alignment

Bead: ga-ap2pa5
Source review bead: ga-fj8xxj
PR: https://github.com/gastownhall/gascity/pull/2995
Branch: builder/ga-vcky1k-heapinusepeak-doc-drift
Commit under review: f24cb4ca9bcaecba836f57b078f12f193dbe9dcc

Note: `docs/PROJECT_MANIFEST.md` is not present in this checkout, so this
checklist applies the deployer release-gate criteria from the role
configuration.

## Summary

This is a docs-only coordstore scorecard change. It updates the comments for
`HeapInusePeakTarget` and `HeapInuseDeltaTarget` so they match the current
delta-based memory gate: `HeapInuseDeltaTarget` drives `MemPass`, while
`HeapInusePeakTarget` is retained as informational output.

## Gate Results

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Source review bead ga-fj8xxj is closed with `REVIEW VERDICT: PASS`; deploy bead ga-ap2pa5 notes reviewer PASS and no blockers. |
| 2 | Acceptance criteria met | PASS | `git show f24cb4ca9 -- internal/benchmarks/coordstore/scorecard.go` shows only comment changes to the two memory target constants. The comments now match the existing score logic: `MemPass` gates on heap in-use delta, with peak retained for visibility. |
| 3 | Tests pass | PASS | `go test ./internal/benchmarks/coordstore/ -run TestScore` passed; `go vet ./internal/benchmarks/coordstore/...` passed; `go vet ./...` passed; `make test-fast-parallel` passed all 8 fast jobs; `git diff --check origin/main...HEAD` clean. PR #2995 CI checks were green at source commit f24cb4ca9 before adding this gate-only markdown commit. |
| 4 | No high-severity review findings open | PASS | Review notes list style/security/spec/coverage PASS and `BLOCKERS: none`; PR #2995 has no review comments and no unresolved comments; CodeQL checks are successful. |
| 5 | Final branch is clean | PASS | Clean auxiliary worktree used for gate evaluation and release-gate commit. Final cleanliness is verified after committing this checklist. |
| 6 | Branch diverges cleanly from main | PASS | After `git fetch origin main`, `git merge-tree --write-tree HEAD origin/main` produced merge tree `3e6e0d57937a475a12798caf7582c4dae679452d` with no conflict diagnostics. GitHub reports PR #2995 `MERGEABLE`. |
| 7 | Single feature theme | PASS | The branch's commit set is one docs-only change in `internal/benchmarks/coordstore/scorecard.go`, confined to coordstore benchmark scorecard memory-threshold comments. |

## Command Evidence

```text
$ go test ./internal/benchmarks/coordstore/ -run TestScore
ok  	github.com/gastownhall/gascity/internal/benchmarks/coordstore	0.005s

$ go vet ./internal/benchmarks/coordstore/...
<no output>

$ go vet ./...
<no output>

$ make test-fast-parallel
[fsys-darwin-compile] ok
[unit-core] ok
[unit-cmd-gc-1-of-6] ok
[unit-cmd-gc-2-of-6] ok
[unit-cmd-gc-3-of-6] ok
[unit-cmd-gc-4-of-6] ok
[unit-cmd-gc-5-of-6] ok
[unit-cmd-gc-6-of-6] ok
All fast jobs passed

$ git diff --check origin/main...HEAD
<no output>
```
