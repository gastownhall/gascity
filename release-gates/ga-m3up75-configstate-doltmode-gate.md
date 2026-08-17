# Release Gate: ConfigState.DoltMode canonical config write

- Bead: ga-m3up75.3
- Branch: deploy/ga-m3up75-configstate-doltmode
- Tested implementation head: 288d54dfbe2ac8fef6ca48bc3fa285ebe67e5571
- Base: 4f1dc179df0de97a28cab7393249a321e459d942
- Scope: ConfigState.DoltMode and EnsureCanonicalConfig support for writing `dolt.mode`

## Gate Summary

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | ga-m3up75.2 records `VERDICT: PASS` for head `288d54dfbe2ac8fef6ca48bc3fa285ebe67e5571`. |
| 2 | Acceptance criteria met | PASS | Diff is limited to `internal/beads/contract/files.go` and `internal/beads/contract/files_test.go`; it preserves the clean candidate from ga-m3up75.1 and the reviewer-confirmed behavior from ga-m3up75.2. |
| 3 | Tests pass | PASS | `go test ./internal/beads/contract` passed; `go build ./cmd/gc` passed; `make test-fast-parallel` passed all fast jobs; `go vet ./...` passed. |
| 4 | No high-severity review findings open | PASS | ga-m3up75.2 lists no blockers and no HIGH findings; reviewer recorded "Blockers: none". |
| 5 | Final branch is clean | PASS | Branch was clean before the gate artifact was added; gate artifact is intended to be committed as the final branch tip. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` succeeded and returned tree `aa31a2da567aea1e9eac3fab5e58cccb5c69c21f`. |
| 7 | Single feature theme | PASS | The commit set touches one subsystem, `internal/beads/contract`, and one behavior: persisting configured Dolt server mode to canonical beads config. |

## Diff Scope

```text
M	internal/beads/contract/files.go
M	internal/beads/contract/files_test.go
```

```text
internal/beads/contract/files.go      | 14 +++++-
internal/beads/contract/files_test.go | 92 +++++++++++++++++++++++++++++++++++
2 files changed, 105 insertions(+), 1 deletion(-)
```

## Acceptance Evidence

- `ConfigState` now carries `DoltMode`.
- `EnsureCanonicalConfig` writes `dolt.mode` when `ConfigState.DoltMode` is non-empty.
- Existing `dolt.mode` is preserved when `ConfigState.DoltMode` is empty.
- `crossBackendKeysToScrub("dolt")` does not include the YAML config key `dolt.mode`; it remains separate from the metadata key `dolt_mode`.
- The candidate excludes the unrelated `cmd/gc` pool/session desired-state changes and old release-gate artifacts from the prior failed gate.

## Commands Run

```text
git diff --name-status origin/main...HEAD
git merge-tree --write-tree origin/main HEAD
go test ./internal/beads/contract
go build ./cmd/gc
make test-fast-parallel
go vet ./...
```

## Result

PASS. Open a PR for `deploy/ga-m3up75-configstate-doltmode` and route the merge request to mayor/mpr. Do not merge from the deployer seat.
