# Release Gate: resource-census TESTING.md ledger generator

Bead: ga-yg3x8u  
Source review bead: ga-ffmf9m  
Build bead: ga-cwfzvz  
Reviewed commit: 57ff991178fd2a0a788591cb5e86651ee476af28  
Gate date: 2026-07-28

## Summary

PASS. The resource-census package now exposes a checked-markdown block
replacement helper and gives
`TestRepositoryLedgerMatchesCensusAndDocumentation` an `-update` mode. The
failure message names the exact regeneration command, and regeneration
replaces only the marked ledger block while preserving the rest of
`TESTING.md` byte-for-byte.

`docs/PROJECT_MANIFEST.md` is not present on the reviewed commit or current
`origin/main`; this gate uses the deployer role release criteria and the
canonical repository guidance in `TESTING.md`.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead ga-ffmf9m is closed with close reason `pass`; its notes record `verdict: pass`, no style or security findings, and independent acceptance verification at reviewed commit 57ff991178fd2a0a788591cb5e86651ee476af28. |
| 2 | Acceptance criteria met | PASS | All four ga-cwfzvz acceptance criteria were checked against the reviewed code and reproduced locally. The focused acceptance suite passed 5 tests with 0 failures and 0 skips. A deliberate one-line corruption of the checked `TESTING.md` block failed with the exact documented `-update` command; running that command passed and restored the original Git blob exactly. `TestGeneratedLedgerBlockRoundTrips` proves generated content round-trips while surrounding content remains unchanged. |
| 3 | Tests pass | PASS | Documented CI-equivalent command `make test-fast-parallel`: 10 PASS, 0 FAIL, 0 SKIP jobs. Focused acceptance command: 5 PASS, 0 FAIL, 0 SKIP tests. `go vet ./...` passed. No skip justification is required because both recorded runs had zero skips. |
| 4 | No high-severity review findings open | PASS | Reviewer notes report no style findings, no security findings, no blockers, and no uncovered criteria. Unresolved HIGH findings: 0. |
| 5 | Final branch is clean | PASS | The reviewed commit was checked out detached with an empty `git status --short`; the deliberate acceptance-test corruption was repaired by the generator back to the exact original blob before the full suite. The gate artifact is the only deployer-added file and will be committed on the isolated deploy branch. |
| 6 | Branch diverges cleanly from main | PASS | Evaluated first as required. `git merge-tree --write-tree origin/main 57ff991178fd2a0a788591cb5e86651ee476af28` exited 0 against `origin/main@1c8573165a5e8d52146ca7cdbf4b9d9b4429b731` and produced tree `d0c51d4410a1ac020236d2912cba0d803179fbca`; no self-rebase was needed. |
| 7 | Single feature theme | PASS | The commit changes only `internal/testpolicy/resourcecensus/census.go` and its adjacent test file. Both changes implement and prove one behavior: deterministic regeneration of the checked TESTING.md resource ledger. |

## Acceptance Evidence

1. A single command is documented by the test flag comment and surfaced in
   the stale-ledger diagnostic:
   `go test ./internal/testpolicy/resourcecensus -run TestRepositoryLedgerMatchesCensusAndDocumentation -update`.
2. After changing the checked ledger's subprocess count from 541 to 999, the
   non-update test failed and printed that exact command.
3. Running the command alone made the test pass and restored `TESTING.md` from
   modified content to its original blob
   `c5aad29fedf6c1880c42c14457638acb010c6fbe`, with no hand edit.
4. `TestGeneratedLedgerBlockRoundTrips` passed and verifies both generated
   block equality and preservation of surrounding documentation.

## Test Evidence

| Command | Counts | Result |
|---------|--------|--------|
| `go test -count=1 -v ./internal/testpolicy/resourcecensus/... -run 'TestReplaceMarkdownBlockRoundTrips\|TestGeneratedLedgerBlockRoundTrips\|TestReplaceMarkdownBlockRequiresOneOrderedMarkerPair\|TestRepositoryLedgerMatchesCensusAndDocumentation\|TestCheckedMarkdownBlock'` | 5 PASS, 0 FAIL, 0 SKIP tests | PASS |
| Deliberate stale-ledger run: `go test -count=1 ./internal/testpolicy/resourcecensus -run TestRepositoryLedgerMatchesCensusAndDocumentation` | 0 PASS, 1 expected FAIL, 0 SKIP tests | Expected RED; diagnostic named the regeneration command |
| Repair run: `go test ./internal/testpolicy/resourcecensus -run TestRepositoryLedgerMatchesCensusAndDocumentation -update` | 1 PASS, 0 FAIL, 0 SKIP tests | PASS; original and regenerated Git blob IDs matched |
| `make test-fast-parallel` | 10 PASS, 0 FAIL, 0 SKIP jobs | PASS |
| `go vet ./...` | Not a test-counting command | PASS |

## Final Gate Result

PASS. The reviewed commit is suitable for an isolated deploy branch, pull
request, and merge-authority handoff.
