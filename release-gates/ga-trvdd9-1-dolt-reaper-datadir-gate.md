# Release Gate: ga-trvdd9.1 dolt cleanup reaper datadir sweep

- Bead: `ga-trvdd9.1`
- Type: single-bead deploy
- Candidate branch: `origin/builder/ga-478c0o-reaper-clean-deploy-v6`
- Candidate SHA before gate commit: `6ad4d626fe6812eb9a8f2f372ed85cb124174d20`
- Base: `origin/main`
- Base SHA: `58a3f764924341a21e3566eb01611572dd8953bf`
- Evaluated: `2026-07-15T23:34:00Z`

## Summary

PASS. The branch is current with `origin/main`, reviewer PASS is present, the
acceptance criteria are covered by code and tests, and the release-gate test
suite passed in the deployer worktree.

## Evidence

- `git rev-parse origin/main`: `58a3f764924341a21e3566eb01611572dd8953bf`
- `git rev-parse origin/builder/ga-478c0o-reaper-clean-deploy-v6`: `6ad4d626fe6812eb9a8f2f372ed85cb124174d20`
- `git merge-base origin/main origin/builder/ga-478c0o-reaper-clean-deploy-v6`: `58a3f764924341a21e3566eb01611572dd8953bf`
- `git rev-list --left-right --count origin/main...origin/builder/ga-478c0o-reaper-clean-deploy-v6`: `0 6`
- `git merge-tree --write-tree origin/main origin/builder/ga-478c0o-reaper-clean-deploy-v6`: `000ce28be29c982aecc3d1c973b572b839e2b4a4`
- `git config core.hooksPath`: `.githooks`

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
M	test/dolttest/dolttest.go
M	test/dolttest/dolttest_test.go
M	test/test-resources.toml
```

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 6 | Branch diverges cleanly from main | PASS | Candidate is based on current `origin/main` (`rev-list` = `0 6`, merge-base equals base SHA); `merge-tree` completed conflict-free. |
| 1 | Review PASS present | PASS | `ga-trvdd9` is the review bead and is closed with `REVIEW VERDICT: PASS`; deploy bead carries `source:actual-reviewer`. |
| 2 | Acceptance criteria met | PASS | Reviewer verified the four mayor criteria: confirmed-orphan datadir removal gated on classification, symptom-based old `.dolt` store-dir sweep with lsof fail-closed behavior, SIGKILL leak-guard integration coverage, and no shell backstop removed. Deployer re-ran the relevant suites below. |
| 3 | Tests pass | PASS | `go vet ./...`; `go test ./internal/testpolicy/resourcecensus/... ./internal/doltorphan/... ./test/dolttest/...`; `go test -tags integration ./examples/gastown/... -run TestSweep_ReapsRealDoltDataDirAfterSIGKILL -count=1`; `make test-fast-parallel` all passed. |
| 4 | No high-severity review findings open | PASS | Reviewer recorded no blocking correctness, security, or style findings. The only noted residual TOCTOU race is non-blocking and narrowed by age/lsof gates. |
| 5 | Final branch is clean | PASS | Worktree was clean before writing this gate file; gate file is committed as the final branch tip and `git status` is clean after commit. |
| 7 | Single feature theme | PASS | All changes are one release theme: removing leaked Dolt data dirs and adding the test-only orphan store-dir sweep, with supporting tests and generated resource-census baselines. |

## Test Log

```text
go vet ./...
PASS

go test ./internal/testpolicy/resourcecensus/... ./internal/doltorphan/... ./test/dolttest/...
ok  	github.com/gastownhall/gascity/internal/testpolicy/resourcecensus	2.398s
ok  	github.com/gastownhall/gascity/internal/doltorphan	0.012s
ok  	github.com/gastownhall/gascity/test/dolttest	0.008s

go test -tags integration ./examples/gastown/... -run TestSweep_ReapsRealDoltDataDirAfterSIGKILL -count=1
ok  	github.com/gastownhall/gascity/examples/gastown	13.655s

make test-fast-parallel
[fsys-darwin-compile] ok
[unit-core] ok
[unit-cmd-gc-1-of-6] ok
[unit-cmd-gc-2-of-6] ok
[unit-cmd-gc-3-of-6] ok
[unit-cmd-gc-4-of-6] ok
[unit-cmd-gc-5-of-6] ok
[unit-cmd-gc-6-of-6] ok
All fast jobs passed
```
