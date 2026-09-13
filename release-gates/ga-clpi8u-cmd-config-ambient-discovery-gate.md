# Release Gate: cmd_config_test.go explicit city path

Outcome: **PASS**

- Deploy bead: `ga-clpi8u`
- Source review bead: `ga-wx2gub`
- Reviewed source commit: `1c339dea92911f37e8aaefed82176c69a03bd96d`
- Provenance branch: `builder/ga-klo4gz.1-pilot-cmd-config-test` (not a deploy target)
- Base: `origin/main` at `2c4c43b270d72365a0080530b0c3e3503f898e7d`
- Release criteria source: deployer gate instructions; `docs/PROJECT_MANIFEST.md` is not present in this checkout.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead `ga-wx2gub` is closed with close reason `pass` and an explicit `REVIEW VERDICT: PASS` for the reviewed commit. |
| 2 | Acceptance criteria met | PASS | Each of the three affected tests sets `GC_CITY_PATH` to its temporary city immediately after changing directory. This selects the explicit-environment resolution path before ambient upward discovery, preserving the tests when the later test-binary ambient-discovery guard lands. |
| 3 | Tests pass | PASS | `make test-fast-parallel` passed all nine jobs. `go build ./...`, `go vet ./...`, and the focused run of all three changed tests passed. |
| 4 | No high-severity review findings open | PASS | The review found no blockers, no production or security surface, and no coverage gap. Unresolved HIGH findings: 0. |
| 5 | Final branch is clean | PASS | `git status --short --branch` reported a clean detached checkout before this gate file was added. `gofmt` and `git diff --check` were clean. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-base --is-ancestor origin/main 1c339dea92911f37e8aaefed82176c69a03bd96d` passed, and `git merge-tree --write-tree` completed cleanly. No self-rebase was required. |
| 7 | Single feature theme | PASS | The one-file, three-line test-only commit has one theme: removing `cmd_config_test.go` tests from ambient city discovery. |

## Verification Commands

| Command | Result |
|---------|--------|
| `make test-fast-parallel` | PASS: all 9 fast jobs passed |
| `go build ./...` | PASS |
| `go vet ./...` | PASS |
| `gofmt -l cmd/gc/cmd_config_test.go` | PASS: no output |
| `git diff --check origin/main...HEAD` | PASS |
| `go test ./cmd/gc/... -run '^(TestDoConfigShowMissingRemoteImportSuggestsInstall\|TestConfigShowJSON\|TestConfigShowValidateJSONReturnsNonzeroForInvalidConfig)$' -count=1 -v` | PASS: all 3 tests |

## Diff Scope

```text
cmd/gc/cmd_config_test.go
```
