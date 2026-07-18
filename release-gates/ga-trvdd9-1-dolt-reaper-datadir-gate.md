# Release Gate: ga-trvdd9.1 dolt cleanup reaper datadir sweep

- Bead: `ga-trvdd9.1`
- Type: single-bead deploy
- Candidate branch: `builder/ga-478c0o-reaper-clean-deploy-v6`
- Candidate SHA before gate refresh: `0cbba9b6d982a4cad254ec889a7261dde17a61df`
- Base: `origin/main`
- Base SHA: `ed3d0626f505cbf1eb169488556b9b45184167d6`
- Evaluated: `2026-07-18T02:10:00Z`
- Manifest note: `docs/PROJECT_MANIFEST.md` is not present in this checkout; this gate uses the deployer release criteria and the local `TESTING.md` gates.

## Summary

PASS. The branch is current with `origin/main`, reviewer PASS is present, the
acceptance criteria are covered by code and tests, and the release-gate test
suite passed in the deployer worktree.

## Evidence

- `git rev-parse origin/main`: `ed3d0626f505cbf1eb169488556b9b45184167d6`
- `git rev-parse HEAD`: `0cbba9b6d982a4cad254ec889a7261dde17a61df`
- `git rev-parse origin/builder/ga-478c0o-reaper-clean-deploy-v6` (pre-push): `b9a37c8f08d346878cbf336034dfff295829a52d`
- `git rev-list --left-right --count origin/main...HEAD`: `0 8`
- `git rev-list --left-right --count origin/builder/ga-478c0o-reaper-clean-deploy-v6...HEAD`: `8 35` (origin's PR branch predates today's self-rebase onto newer `main`; all 8 feature commits are carried forward with identical content under new SHAs, plus the additional `main` commits pulled in by the rebase)
- `git merge-tree --write-tree origin/main HEAD`: `e0475e4a083e6da216210b8340ea3c9ee1f017bb` (clean, no conflict markers)
- `git config core.hooksPath`: `.githooks`
- `scripts/rebase-resolve-lib.sh`: absent; this refresh required a real self-rebase onto `origin/main` (PR had drifted to `mergeStateStatus: DIRTY` / `mergeable: CONFLICTING` per `gh pr view`). Conflicts were limited to the resource-census ratchet triple (`internal/testpolicy/resourcecensus/census.go`, `test/test-resources.toml`, `TESTING.md`); the correct baseline (`531` calls / `157` files) was confirmed by running `TestRepositoryLedgerMatchesCensusAndDocumentation` rather than guessing between the two conflicting sides, then propagated identically to all three files and re-verified green before continuing the rebase.

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
| 6 | Branch diverges cleanly from main | PASS | Scoped fetches refreshed `origin/main` and the candidate branch. `rev-list` is `0 8`, `merge-tree --write-tree` against `origin/main` completed conflict-free at `e0475e4a083e6da216210b8340ea3c9ee1f017bb`. |
| 1 | Review PASS present | PASS | Parent review bead `ga-trvdd9` is closed with `REVIEW VERDICT: PASS`; deploy bead carries `source:actual-reviewer`. |
| 2 | Acceptance criteria met | PASS | Reviewer verified the four mayor criteria: confirmed-orphan datadir removal gated on classification, symptom-based old `.dolt` store-dir sweep with lsof fail-closed behavior, SIGKILL leak-guard integration coverage, and no shell backstop removed. Deployer re-ran the relevant suites below. |
| 3 | Tests pass | PASS | `go build ./...`; `go vet ./...`; `go test ./internal/testpolicy/resourcecensus/... ./internal/doltorphan/... ./test/dolttest/...`; `go test -tags integration ./examples/gastown/... -run TestSweep_ReapsRealDoltDataDirAfterSIGKILL -count=1`; and `make test-fast-parallel` all passed. Note: a default-`$HOME` run of `make test-fast-parallel` in this session first hit a single failure, `TestProductMetricsServiceChildEnvSupervisorStart`, root-caused to the session's `$HOME` differing from the real OS user home and tripping the intentional `platformSupervisorHomeOverrideError` guard in `cmd/gc/cmd_supervisor_lifecycle.go` (added by #4268, confirmed untouched by this diff via empty `git diff origin/main...HEAD` on both the guard and test files). Re-running with `HOME` set to the real user home passed cleanly across all 8 shards; this matches the `HOME` the push itself runs under, so it is the correct evidence for this gate rather than a bypass. |
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
ok  	github.com/gastownhall/gascity/internal/testpolicy/resourcecensus	(cached)
ok  	github.com/gastownhall/gascity/internal/doltorphan	(cached)
ok  	github.com/gastownhall/gascity/test/dolttest	(cached)

go test -tags integration ./examples/gastown/... -run TestSweep_ReapsRealDoltDataDirAfterSIGKILL -count=1 -v
--- PASS: TestSweep_ReapsRealDoltDataDirAfterSIGKILL (8.95s)
ok  	github.com/gastownhall/gascity/examples/gastown	9.084s

HOME=/home/jaword make test-fast-parallel
[fsys-darwin-compile] ok
[unit-core] ok
[unit-cmd-gc-3-of-6] ok
[unit-cmd-gc-1-of-6] ok
[unit-cmd-gc-4-of-6] ok
[unit-cmd-gc-5-of-6] ok
[unit-cmd-gc-6-of-6] ok
[unit-cmd-gc-2-of-6] ok
All fast jobs passed
```

Note: an earlier `make test-fast-parallel` run in this session, under the
session's default `$HOME` (`/home/jaword/jim-claude`, which differs from the
real OS user home `/home/jaword`), hit one failure in shard
`unit-cmd-gc-2-of-6`: `TestProductMetricsServiceChildEnvSupervisorStart`.
That test exercises `platformSupervisorHomeOverrideError`
(`cmd/gc/cmd_supervisor_lifecycle.go`), an intentional guard added by #4268
that refuses `gc supervisor start` when `os.UserHomeDir()` disagrees with the
passwd-registered home for the running UID — working as designed, not a
regression. Both the guard and the test file are byte-identical to
`origin/main` (`git diff origin/main...HEAD` empty for both paths), so this
is confirmed unrelated to this branch's diff. Re-running with `HOME` set to
the real user home (the log captured above) passed cleanly across all 8
shards, and is the `HOME` the eventual `git push` also runs under.
