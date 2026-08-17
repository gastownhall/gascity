# Release Gate: ga-q32h2b test deflakes

Outcome: PASS

- Deploy bead: ga-q32h2b
- Source review bead: ga-r4jmdq
- Branch under evaluation: builder/ga-q32h2b-test-deflakes-clean
- Local deploy branch: deploy/ga-q32h2b-test-deflakes-clean
- Base: origin/main @ 2d90faf433c5ad1b28edc51f2ba80aa066d9027a
- Reviewed source commit: ed544a1f7
- Clean branch commit: 16d3453bdc00cc5c5b0565d6044875955bc7abc2
- Release criteria source: deployer gate instructions. `docs/PROJECT_MANIFEST.md` was not present in this checkout.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Source review bead ga-r4jmdq is closed with close reason `pass` and notes contain `REVIEWER VERDICT: PASS`. Deploy bead ga-q32h2b notes record the clean rebuilt branch as ready for deploy gate. |
| 2 | Acceptance criteria met | PASS | C2 changes `TestCleanInstallTutorialPath` to use `tutorial-rig-alpha`, whose derived prefix is `tra`, avoiding the prior 2-character random-city prefix collision. C3 adds a 30s `WaitForCondition` before `TestSessionDefaultNamedSession` subtests so the asynchronously created default `mayor` session is visible before assertions run. Diff is limited to `test/acceptance/session_test.go` and `test/integration/tutorial_path_test.go`. |
| 3 | Tests pass | PASS | See verification commands below. Focused acceptance, focused integration, fast unit shards, vet, build, and diff whitespace checks all passed. |
| 4 | No high-severity review findings open | PASS | Review notes for ga-r4jmdq report PASS, test-only scope, no production code, no security concerns, and no unresolved HIGH findings. Deploy bead notes contain no HIGH/CRITICAL open finding. |
| 5 | Final branch is clean | PASS | `git status --porcelain=v1` was empty before writing this gate file. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-base --is-ancestor origin/main HEAD` passed before the gate commit; the clean branch is a direct descendant of `origin/main` with no merge conflict state. |
| 7 | Single feature theme | PASS | The commit set has one test-reliability theme: deflaking two existing tests identified by the same investigator pass. It touches only test files and no unrelated subsystem or user-facing behavior. |

## Verification Commands

| Command | Result |
|---------|--------|
| `go test -tags acceptance_a -run TestSessionDefaultNamedSession -count=1 -v ./test/acceptance` | PASS: `ok github.com/gastownhall/gascity/test/acceptance 14.504s` |
| `go test -tags integration -run TestCleanInstallTutorialPath -count=1 -v ./test/integration` | PASS: `ok github.com/gastownhall/gascity/test/integration 34.126s`; cleanup noted supervisor was already stopped. |
| `make test-fast-parallel` | PASS: all fast jobs passed. |
| `go vet ./...` | PASS |
| `go build ./cmd/gc/` | PASS |
| `git diff --check origin/main...HEAD` | PASS |

## Scope Check

`git diff --name-only origin/main...HEAD`:

```text
test/acceptance/session_test.go
test/integration/tutorial_path_test.go
```
