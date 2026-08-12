# Release Gate: ga-trvdd9.1 dolt cleanup reaper datadir sweep

- Bead: `ga-trvdd9.1`
- Type: single-bead deploy
- Candidate branch: `builder/ga-478c0o-reaper-clean-deploy-v6`
- Candidate SHA before gate refresh: `d5f5aee4447654cdc751832e7adeab13b6272ef8`
- Base: `origin/main`
- Base SHA: `aa0c20af554ae75cd6a53cce5434f201e2e39c9f`
- Evaluated: `2026-07-19T02:30:00Z`
- Manifest note: `docs/PROJECT_MANIFEST.md` is not present in this checkout; this gate uses the deployer release criteria and the local `TESTING.md` gates.
- Follow-up rebase note: this is the second rebase of this PR (bead `ga-u4vkyp`, filed by the hourly pr-audit order after PR #4351 drifted back into conflict following the prior `ga-7amgxo` rebase). `origin/main` advanced from `ed3d0626f5` to `aa0c20af55` between the two rebases; the only re-conflicting file set was again the resource-census ratchet triple.

## Summary

PASS. The branch is current with `origin/main`, reviewer PASS is present, the
acceptance criteria are covered by code and tests, and the release-gate test
suite passed in the deployer worktree.

## Evidence

- `git rev-parse origin/main`: `aa0c20af554ae75cd6a53cce5434f201e2e39c9f`
- `git rev-parse HEAD`: `d5f5aee4447654cdc751832e7adeab13b6272ef8`
- `git rev-parse origin/builder/ga-478c0o-reaper-clean-deploy-v6` (pre-push): `b9a37c8f08d346878cbf336034dfff295829a52d`
- `git rev-list --left-right --count origin/main...HEAD`: `0 9`
- `git rev-list --left-right --count origin/builder/ga-478c0o-reaper-clean-deploy-v6...HEAD`: `8 71` (origin's PR branch predates this self-rebase onto newer `main`; all feature commits are carried forward with identical content under new SHAs, plus the additional `main` commits pulled in by the rebase)
- `git merge-tree --write-tree origin/main HEAD`: `d9379eb609aaff9d1316deaaaa62db772c224bec` (clean, no conflict markers)
- `git config core.hooksPath`: `.githooks`
- `scripts/rebase-resolve-lib.sh`: absent; this refresh required a real self-rebase onto `origin/main` (PR had drifted to `mergeStateStatus: DIRTY` / `mergeable: CONFLICTING` per `gh pr view` again, after the prior rebase's target `origin/main` advanced further). Conflicts were again limited to the resource-census ratchet triple (`internal/testpolicy/resourcecensus/census.go`, `test/test-resources.toml`, `TESTING.md`). Per the same protocol as the prior rebase, the correct baseline was confirmed by running `TestRepositoryLedgerMatchesCensusAndDocumentation` rather than guessing: an initial take-HEAD resolution (533 calls / 158 files) was falsified by the test (`resource ledger drift: ... calls=534 (baseline 533), files=159 (baseline 158)`), so the baseline was bumped to the test-reported `534` calls / `159` files, propagated identically to all three files, and re-verified green. The `take-HEAD` interim resolution also produced an empty replay for the original ledger-bump commit, which git auto-skipped during `rebase --continue`; the real 534/159 bump was carried by a fresh commit (`d5f5aee44`, "test(resourcecensus): rebase ledger bump onto origin/main") so the branch still carries 9 commits above `origin/main`, matching the original PR's commit count.

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
| 6 | Branch diverges cleanly from main | PASS | Scoped fetches refreshed `origin/main` and the candidate branch. `rev-list` is `0 9`, `merge-tree --write-tree` against `origin/main` completed conflict-free at `d9379eb609aaff9d1316deaaaa62db772c224bec`. |
| 1 | Review PASS present | PASS | Parent review bead `ga-trvdd9` is closed with `REVIEW VERDICT: PASS`; deploy bead carries `source:actual-reviewer`. |
| 2 | Acceptance criteria met | PASS | Reviewer verified the four mayor criteria: confirmed-orphan datadir removal gated on classification, symptom-based old `.dolt` store-dir sweep with lsof fail-closed behavior, SIGKILL leak-guard integration coverage, and no shell backstop removed. Deployer re-ran the relevant suites below. |
| 3 | Tests pass | PASS | `go build ./...`; `go vet ./...`; `go test ./internal/testpolicy/resourcecensus/... ./internal/doltorphan/... ./test/dolttest/...`; `go test -tags integration ./examples/gastown/... -run TestSweep_ReapsRealDoltDataDirAfterSIGKILL -count=1`; and `HOME=/home/jaword make test-fast-parallel` all passed. Note: this rebase's `make test-fast-parallel` run hit one failure in shard `unit-cmd-gc-4-of-6`: `TestProductMetricsLifecycleFailuresPreserveOutputAndOTel/JSONL_failure/factory_error`, a known live-event-store race unrelated to this diff (the test and the code it exercises are both untouched by `git diff origin/main...HEAD`). Two isolated re-runs of that test alone (`go test -count=1 ./cmd/gc/... -run TestProductMetricsLifecycleFailuresPreserveOutputAndOTel`) both passed cleanly, confirming the flake rather than a regression. |
| 4 | No high-severity review findings open | PASS | Reviewer recorded no blocking correctness, security, or style findings. The only noted residual TOCTOU race is non-blocking and narrowed by age/lsof gates. |
| 5 | Final branch is clean | PASS | Worktree was clean before refreshing this gate file; this gate file is committed as the final branch tip and `git status` is clean after commit. |
| 7 | Single feature theme | PASS | All changes are one release theme: removing leaked Dolt data dirs and adding the test-only orphan store-dir sweep, with supporting tests and resource-census baseline updates. |

## Test Log

```text
go build ./...
PASS

go vet ./...
PASS

go test -count=1 ./internal/testpolicy/resourcecensus/... -run TestRepositoryLedgerMatchesCensusAndDocumentation -v
--- PASS: TestRepositoryLedgerMatchesCensusAndDocumentation (1.49s)
ok  	github.com/gastownhall/gascity/internal/testpolicy/resourcecensus	1.522s

go test -count=1 ./internal/doltorphan/... ./test/dolttest/... -v
ok  	github.com/gastownhall/gascity/internal/doltorphan	0.003s
ok  	github.com/gastownhall/gascity/test/dolttest	0.001s

go test -tags integration ./examples/gastown/... -run TestSweep_ReapsRealDoltDataDirAfterSIGKILL -count=1 -v
--- PASS: TestSweep_ReapsRealDoltDataDirAfterSIGKILL (9.71s)
ok  	github.com/gastownhall/gascity/examples/gastown	15.079s

HOME=/home/jaword make test-fast-parallel
[fsys-darwin-compile] ok
[unit-core] ok
[unit-cmd-gc-3-of-6] ok
[unit-cmd-gc-6-of-6] ok
[unit-cmd-gc-2-of-6] ok
[unit-cmd-gc-5-of-6] ok
[unit-cmd-gc-1-of-6] ok
[unit-cmd-gc-4-of-6] failed with exit 1 (TestProductMetricsLifecycleFailuresPreserveOutputAndOTel/JSONL_failure/factory_error)

go test -count=1 ./cmd/gc/... -run TestProductMetricsLifecycleFailuresPreserveOutputAndOTel -v   (isolated re-run #1)
PASS
ok  	github.com/gastownhall/gascity/cmd/gc	20.851s

go test -count=1 ./cmd/gc/... -run TestProductMetricsLifecycleFailuresPreserveOutputAndOTel -v   (isolated re-run #2)
PASS
ok  	github.com/gastownhall/gascity/cmd/gc	23.089s
```

Note: `HOME=/home/jaword make test-fast-parallel` hit one failure in shard
`unit-cmd-gc-4-of-6`: `TestProductMetricsLifecycleFailuresPreserveOutputAndOTel/JSONL_failure/factory_error`,
a comparison of command-result output that races against a live event store
in that test's fixture setup. Neither the failing test nor the code it
exercises is touched by this branch's diff (`git diff origin/main...HEAD`
empty for both). Two isolated re-runs of the test alone, back to back, both
passed cleanly — confirming a pre-existing flake, not a regression. The prior
rebase's gate refresh independently hit and cleared the same class of
`$HOME`-sensitive-environment issue (a different test,
`TestProductMetricsServiceChildEnvSupervisorStart`, guarded by
`platformSupervisorHomeOverrideError` in `cmd/gc/cmd_supervisor_lifecycle.go`);
this run already used the real user `HOME` throughout, so that specific
guard did not trip this time.
