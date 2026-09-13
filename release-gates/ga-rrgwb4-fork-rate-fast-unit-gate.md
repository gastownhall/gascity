# Release Gate: ga-rrgwb4 - forkRateCheck GC_FAST_UNIT sleep skip

Generated: 2026-06-16T13:20:48Z

## Candidate

- Deploy bead: `ga-rrgwb4`
- Source bead: `ga-iujcgp`
- Source branch: `builder/ga-iujcgp`
- Base: `origin/main` at `0ed81eb88def517fb6b39f1d7d6930e1dbee9ce6`
- Reviewed head: `7c49d5d11fd8bfb4d8c8b1eb382353bd4db65318`
- Diff scope:
  - `cmd/gc/doctor_fork_rate.go`
  - `cmd/gc/doctor_fork_rate_test.go`

`docs/PROJECT_MANIFEST.md` is not present in this checkout, so this gate uses
the deployer prompt's seven release-gate criteria.

## Gate Result

PASS

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `ga-iujcgp` notes contain `REVIEWER PASS (gascity/reviewer, 2026-06-16)` with verdict `PASS`; deploy bead `ga-rrgwb4` records reviewed + passed status. |
| 2 | Acceptance criteria met | PASS | `newForkRateCheck()` keeps the check registered and injects a no-op sleep only when `GC_FAST_UNIT=1`; default sleep is retained without that env var. Added tests cover both paths. |
| 3 | Tests pass | PASS | `GC_FAST_UNIT=1 go test ./cmd/gc -run 'TestPhase0Doctor\|TestForkRate\|TestDoDoctor' -count=1` passed in 8.812s; focused fork-rate tests passed in 0.615s; `go build ./cmd/gc` passed; `make test-fast-parallel` passed all fast jobs; `go vet ./...` passed. |
| 4 | No high-severity review findings open | PASS | Reviewer notes list no HIGH findings; the only reviewer observation is a non-blocking wall-clock threshold comment. |
| 5 | Final branch is clean | PASS | Clean gate worktree started with `git status --short --branch` showing only `## HEAD (no branch)` before writing this gate artifact. Final clean status is checked after the gate commit. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-base --is-ancestor origin/main HEAD` exited 0; `git merge-tree --write-tree origin/main HEAD` exited 0 and produced tree `32ed594ac596d86556c8f1ef4adaa1c7894968c5`. |
| 7 | Single feature theme | PASS | Commit set touches only the `cmd/gc` doctor fork-rate check and its tests; this is one unit-cover runtime-budget fix with no unrelated CI, Makefile, or Dolt-skip changes. |

## Acceptance Criteria Evidence

- Under `GC_FAST_UNIT=1`, the fork-rate doctor check does not pay the 1s
  sample wait: `TestForkRateCheck_FastUnitSleepIsNoop` passed.
- With `GC_FAST_UNIT` unset, the default 1s sample behavior is unchanged:
  `TestForkRateCheck_DefaultSleepRetained` passed.
- `forkRateCheck` remains registered and still emits a result; the doctor smoke
  command covering `TestPhase0Doctor`, `TestForkRate`, and `TestDoDoctor`
  passed under `GC_FAST_UNIT=1`.
- Out-of-scope surfaces were not changed: no Makefile timeout changes, no
  `.github/workflows/ci.yml` changes, and no Dolt lifecycle skip changes.

## Commands Run

```text
git fetch origin main
git diff --name-status origin/main...HEAD
git merge-base --is-ancestor origin/main HEAD
git merge-tree --write-tree origin/main HEAD
GC_FAST_UNIT=1 go test ./cmd/gc -run 'TestPhase0Doctor|TestForkRate|TestDoDoctor' -count=1
go test ./cmd/gc -run 'TestForkRateCheck_(FastUnitSleepIsNoop|DefaultSleepRetained|HighRateWarns|LowRateOK|NonLinuxSkips|ReadErrorSkips)|TestParseProcessesCounter' -count=1
go build ./cmd/gc
go vet ./...
make test-fast-parallel
gofmt -l cmd/gc/doctor_fork_rate.go cmd/gc/doctor_fork_rate_test.go
git diff --check origin/main...HEAD
```

