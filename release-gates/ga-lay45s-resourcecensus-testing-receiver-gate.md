# Release Gate: exclude testing receiver environment/CWD helpers from resource census

- Deploy bead: `ga-lay45s`
- Source bead: `ga-njovi7`
- Reviewed commit: `54c6a78785ac44c7e5bc102f6e2c99bb7e7d8333`
- Deploy branch: `deploy/ga-lay45s-gate`
- Current base: `origin/main@02831408893c86d49274611b37257cdbcc115870`
- Merge base: `660b79166a4ab11dfedaa2b2ceac990a15874e2a`
- Evaluated: 2026-07-25
- Gate source: deployer prompt release-gate table. `docs/PROJECT_MANIFEST.md` is not present in this checkout.

## Summary

PASS. The resource census no longer charges `testing.T`/`testing.TB` receiver calls to `Setenv` and `Chdir` as ambient environment/CWD debt because those helpers restore state automatically. Direct `os.Setenv`, `os.Unsetenv`, `os.Clearenv`, and `os.Chdir` calls remain counted. The implementation, regression coverage, four exact-equality baseline rows, and generated ledger documentation form one resource-census policy change.

## Criteria

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 6 | Branch diverges cleanly from main | PASS | Checked first after fetching current `origin/main`. `git merge-tree --write-tree origin/main 54c6a78785ac44c7e5bc102f6e2c99bb7e7d8333` exited 0 and produced tree `8a82b52bf92350f4de1889bb311e0b783826b1d4`; `git diff --check origin/main...54c6a78785ac44c7e5bc102f6e2c99bb7e7d8333` produced no output. No self-rebase was needed. |
| 1 | Review PASS present | PASS | The closed source bead `ga-njovi7` records an independent exact-SHA review ending in `Verdict: PASS`; the deploy bead reproduces that PASS evidence and identifies the reviewed commit. |
| 2 | Acceptance criteria met | PASS | `matchedResourcesForCall` keeps direct `os.*` charging, excludes testing receiver `Setenv`/`Chdir`, and preserves fail-closed receiver-binding errors. `TestScanExcludesTestingReceiverSetenvChdirFromEnvironmentAndCWD` proves differential 1-not-2 counts for both resources. All four `cmd/gc+untagged` Debt/SmallDebt environment/CWD rows are lowered consistently in `bootstrapPolicy` and `test/test-resources.toml`; `TESTING.md` is regenerated and synchronized. No new resource/scope or key-specific exception was added. |
| 3 | Tests pass | PASS | On the exact reviewed SHA, the focused regression and ledger-sync tests passed; the full `internal/testpolicy/resourcecensus/...` package passed; `go build ./...` and `go vet ./...` passed; `make test-fast-parallel` passed all 8 jobs on its first authoritative run. |
| 4 | No high-severity review findings open | PASS | Source/deploy review notes contain no HIGH, request-changes, or security finding. The open-work query for `ga-lay45s`/`ga-njovi7` returned only the sling helper bead `ga-537678`. |
| 5 | Final branch is clean | PASS | Before adding this gate file, `git status --short --branch` returned only `## HEAD (no branch)`. This gate file is the sole deploy-gate commit payload. |
| 7 | Single feature theme | PASS | The six-file commit set is one resource-census policy change: classifier implementation and tests, exact baseline data, and the generated ledger projection. |

## Acceptance Evidence

- Testing receiver `Setenv`/`Chdir` excluded while direct `os.*` calls remain counted: PASS.
- Fail-closed errors for unresolved receiver identifiers remain propagated: PASS.
- Differential regression test covers both Environment and CWD: PASS.
- Four `ScopeCmdGCUntagged` Debt/SmallDebt environment/CWD rows re-measured and synchronized: PASS.
- Generated `TESTING.md` ledger matches source policy and TOML exactly: PASS.
- Remaining migrations may add `t.Setenv`/`t.Chdir` calls without consuming the ambient resource ratchet: PASS by receiver-based classification and the differential regression.

## Commands

```bash
git fetch origin main builder/ga-njovi7-testenv-exclude
git ls-remote origin refs/heads/builder/ga-njovi7-testenv-exclude
git merge-tree --write-tree origin/main 54c6a78785ac44c7e5bc102f6e2c99bb7e7d8333
git diff --check origin/main...54c6a78785ac44c7e5bc102f6e2c99bb7e7d8333
gofmt -l internal/testpolicy/resourcecensus/census.go internal/testpolicy/resourcecensus/census_test.go internal/testpolicy/resourcecensus/hermetic.go internal/testpolicy/resourcecensus/hermetic_test.go
go test -count=1 ./internal/testpolicy/resourcecensus/... -run 'TestScanExcludesTestingReceiverSetenvChdirFromEnvironmentAndCWD|TestRepositoryLedgerMatchesCensusAndDocumentation' -v
go test -count=1 ./internal/testpolicy/resourcecensus/...
go build ./...
go vet ./...
make test-fast-parallel
bd list --status open --limit 0
git status --short --branch
git config --get core.hooksPath
```
