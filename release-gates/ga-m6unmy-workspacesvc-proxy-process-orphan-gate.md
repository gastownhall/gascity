# Release Gate: workspacesvc proxy-process orphan prevention

- Deploy bead: `ga-m6unmy`
- Source branch (provenance only): `builder/ga-m6unmy-gate-rebase`
- Evaluated source commit: `9d13719c848abe62d13381af32600ab45c3764ac`
- Base checked: `origin/main` at `31ee5bd4e9ee3ca6d9411d06972666a712803071`
- Isolated deploy branch: `deploy/ga-m6unmy-gate`
- Overall result: **PASS**

`docs/PROJECT_MANIFEST.md` is not present in this checkout. This checklist
applies the release criteria supplied in the deployer instructions and the
test boundaries documented in `TESTING.md`.

## Checklist

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | **PASS** | The reviewer recorded `verdict: pass` after independently reviewing the production change, Linux/non-Linux split, hard-exit proof, TestMain leak detector, security, and style at the original green commit. The rebased candidate adds the required mirrored resource-census update; mayor then independently checked its proportionality against the branch diff, cleared `hold:mayor`, and explicitly ruled `PROCEED TO DEPLOY. Do not route back to reviewer.` |
| 2 | Acceptance criteria met | **PASS** | Linux proxy children receive kernel-enforced `Pdeathsig: SIGKILL`; non-Linux builds retain the previous `Setpgid` behavior; a real re-exec harness proves the child dies after a direct `os.Exit` with no Go cleanup; package `TestMain` fails on surviving direct children; no Family B files or `gc dolt-cleanup` behavior changed. The resource-census increase is exactly proportional to this branch's two new subprocess and two new fixed-sleep call sites and is mirrored in `census.go`, `test-resources.toml`, and `TESTING.md`. |
| 3 | Tests pass | **PASS** | `go build ./...`: PASS. `go vet ./...`: PASS. Documented CI-equivalent `make test-fast-parallel`: 10 PASS jobs, 0 FAIL jobs, 0 SKIP jobs. A JSON-counted full affected-package run reported 59 PASS tests, 0 FAIL, 8 SKIP. Two skips are re-exec helper entry points that intentionally run only with their harness environment; six orphan-reaper tests require direct init-parenting and safely skip because this host has a child subreaper. Focused hard-exit, survivor-detector, and live resource-ledger tests: 3 PASS, 0 FAIL, 0 SKIP. `GOOS=darwin go test -c ./internal/workspacesvc`: PASS. |
| 4 | No high-severity review findings open | **PASS** | Reviewer reported no blocker, major, security, or style findings. Mayor's follow-up proportionality audit found the census delta exact and required for CI. Unresolved HIGH findings: 0. |
| 5 | Final branch is clean | **PASS** | The detached candidate and newly cut isolated deploy branch were clean before adding this gate checklist. No generated files or test artifacts are present in the branch. |
| 6 | Branch diverges cleanly from main | **PASS** | `git rev-list --left-right --count origin/main...9d13719c...` reported `0 3`: the candidate contains current main and is three feature commits ahead. `git merge-tree --write-tree origin/main 9d13719c...` returned 0 with no conflicts. |
| 7 | Single feature theme | **PASS** | All changes implement one feature theme: preventing and detecting orphaned `proxy_process` test children. The three resource-ledger files are the mandatory census mirror for the new tests, not an independent feature. |

## Acceptance Evidence

- `TestProxyProcessSurvivesHardParentExit` passed against the production
  `Manager.Reload` path and a direct `os.Exit` harness.
- `TestLivingTestChildrenDetectsSurvivor` passed for both live-child detection
  and post-reap disappearance.
- `TestRepositoryLedgerMatchesCensusAndDocumentation` passed against the live
  repository AST.
- The Darwin test-binary compile passed, proving the `!linux` process-attribute
  implementation remains buildable.

## Test Commands

```text
go build ./...
go vet ./...
go test ./internal/workspacesvc/... -count=1 \
  -run 'TestProxyProcessSurvivesHardParentExit|TestLivingTestChildrenDetectsSurvivor' -v
go test ./internal/testpolicy/resourcecensus/... -count=1 \
  -run TestRepositoryLedgerMatchesCensusAndDocumentation -v
GOOS=darwin go test -c -o <temporary-output> ./internal/workspacesvc
make test-fast-parallel
go test -json -count=1 ./internal/workspacesvc/...
```
