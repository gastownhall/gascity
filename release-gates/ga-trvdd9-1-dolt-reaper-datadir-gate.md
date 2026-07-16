# Release Gate: ga-trvdd9.1 dolt cleanup reaper datadir sweep

- Bead: `ga-trvdd9.1`
- Type: single-bead deploy
- Candidate branch: `builder/ga-478c0o-reaper-clean-deploy-v6`
- Candidate SHA before gate refresh: `c26261450f223cdb4addb446f3796ba7005b66b3`
- Base: `origin/main`
- Base SHA: `4fda5a28445f42d6e789fc7f5751645ac4fecd19`
- Evaluated: `2026-07-16T19:34:00Z`
- Manifest note: `docs/PROJECT_MANIFEST.md` is not present in this checkout; this gate uses the deployer release criteria and the local `TESTING.md` gates.

## Summary

PASS. The branch is current with `origin/main`, reviewer PASS is present, the
acceptance criteria are covered by code and tests, and the release-gate test
suite passed in the deployer worktree.

## Evidence

- `git rev-parse origin/main`: `4fda5a28445f42d6e789fc7f5751645ac4fecd19`
- `git rev-parse HEAD`: `c26261450f223cdb4addb446f3796ba7005b66b3`
- `git rev-parse origin/builder/ga-478c0o-reaper-clean-deploy-v6`: `c26261450f223cdb4addb446f3796ba7005b66b3`
- `git rev-list --left-right --count origin/main...HEAD`: `0 7`
- `git rev-list --left-right --count origin/builder/ga-478c0o-reaper-clean-deploy-v6...HEAD`: `0 0`
- `git merge-tree --write-tree origin/main HEAD`: `838607b9ede7b33065a98490ff58e3e67d4b72fe`
- `git config core.hooksPath`: `.githooks`
- `scripts/rebase-resolve-lib.sh`: absent; no self-rebase was needed because criterion 6 passed directly.

Candidate diff scope:

```text
M	TESTING.md
M	cmd/gc/cmd_dolt_cleanup.go
M	cmd/gc/cmd_dolt_cleanup_test.go
M	cmd/gc/dolt_cleanup_reaper.go
M	cmd/gc/dolt_cleanup_reaper_test.go
M	cmd/gc/dolt_leak_helper_test.go
M	cmd/gc/path_helpers_test.go
A	examples/gastown/dolt_orphan_sweep_integration_test.go
A	examples/gastown/main_test.go
A	internal/doltorphan/sweep.go
A	internal/doltorphan/sweep_test.go
A	internal/doltorphan/testenv_import_test.go
M	internal/testpolicy/resourcecensus/census.go
A	release-gates/ga-trvdd9-1-dolt-reaper-datadir-gate.md
M	test/dolttest/dolttest.go
M	test/dolttest/dolttest_test.go
M	test/test-resources.toml
```

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 6 | Branch diverges cleanly from main | PASS | Scoped fetches refreshed `origin/main` and the candidate branch. `rev-list` is `0 7`, remote branch matches local HEAD, and `merge-tree` completed conflict-free. |
| 1 | Review PASS present | PASS | Parent review bead `ga-trvdd9` is closed with `REVIEW VERDICT: PASS`; deploy bead carries `source:actual-reviewer`. |
| 2 | Acceptance criteria met | PASS | Reviewer verified the four mayor criteria: confirmed-orphan datadir removal gated on classification, symptom-based old `.dolt` store-dir sweep with lsof fail-closed behavior, SIGKILL leak-guard integration coverage, and no shell backstop removed. Deployer re-ran the relevant suites below. |
| 3 | Tests pass | PASS | `go build ./...`; `go vet ./...`; `go test ./internal/testpolicy/resourcecensus/... ./internal/doltorphan/... ./test/dolttest/...`; `go test -tags integration ./examples/gastown/... -run TestSweep_ReapsRealDoltDataDirAfterSIGKILL -count=1`; and `make test-fast-parallel` all passed. |
| 4 | No high-severity review findings open | PASS | Reviewer recorded no blocking correctness, security, or style findings. The only noted residual TOCTOU race is non-blocking and narrowed by age/lsof gates. |
| 5 | Final branch is clean | PASS | Worktree was clean before refreshing this gate file; this gate file is committed as the final branch tip and `git status` is clean after commit. |
| 7 | Single feature theme | PASS | All changes are one release theme: removing leaked Dolt data dirs and adding the test-only orphan store-dir sweep, with supporting tests and resource-census baseline updates. |

## Test Log

```text
go build ./...
PASS

go vet ./...
PASS

go test ./internal/testpolicy/resourcecensus/... ./internal/doltorphan/... ./test/dolttest/...
ok  	github.com/gastownhall/gascity/internal/testpolicy/resourcecensus	2.737s
ok  	github.com/gastownhall/gascity/internal/doltorphan	0.007s
ok  	github.com/gastownhall/gascity/test/dolttest	0.003s

go test -tags integration ./examples/gastown/... -run TestSweep_ReapsRealDoltDataDirAfterSIGKILL -count=1
ok  	github.com/gastownhall/gascity/examples/gastown	14.272s

make test-fast-parallel
[fsys-darwin-compile] ok
[unit-cmd-gc-1-of-6] ok
[unit-cmd-gc-2-of-6] ok
[unit-cmd-gc-3-of-6] ok
[unit-cmd-gc-4-of-6] ok
[unit-cmd-gc-5-of-6] ok
[unit-cmd-gc-6-of-6] ok
[unit-core] ok
All fast jobs passed
```
