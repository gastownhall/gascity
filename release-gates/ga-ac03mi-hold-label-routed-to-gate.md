# Release Gate: hold-label-routed-to doctor check

Bead: ga-ac03mi
Branch: builder/ga-fm2vgd.1-hold-label-routing-doctor-check
Head: 0e8641d5908d6746269df5906ecd7b1afe1d9967
Base: origin/main b7d312eb5ae026d87ed655908de9d090e7a4f07a
Date: 2026-07-18

Note: docs/PROJECT_MANIFEST.md is not present on origin/main or this branch.
This gate uses the deployer release criteria supplied by the Gas City role
prompt and TESTING.md for test selection.

## Summary

PASS. The branch is current with origin/main, has a closed reviewer PASS, is
limited to the cmd/gc doctor-check subsystem, and passed the focused acceptance
tests plus the repo fast baseline.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 6 | Branch diverges cleanly from main | PASS | `git rev-list --left-right --count origin/main...origin/builder/ga-fm2vgd.1-hold-label-routing-doctor-check` returned `0 5`; merge-base equals origin/main (`b7d312eb5ae026d87ed655908de9d090e7a4f07a`). |
| 1 | Review PASS present | PASS | Review bead ga-fm2vgd.3 is closed with close reason `PASS: hold-label-routed-to now scans all non-closed beads...`. |
| 2 | Acceptance criteria met | PASS | `cmd/gc/doctor_hold_label_routed_to.go` registers `hold-label-routed-to`, scans city and rig stores, leaves `Status` unset with `AllowScan: true`, excludes `hold:external`, and fixes drift through `SetMetadata`. `cmd/gc/doctor_hold_label_routed_to_test.go` covers missing and mismatched `gc.routed_to`, city and rig stores, `hold:external`, arbitrary hold values, idempotent fix behavior, and the `in_progress` regression case. No production branch special-cases a role name; role-like strings appear only as generic test fixture values. |
| 3 | Tests pass | PASS | `go test ./cmd/gc/ -run 'TestHoldLabelRoutedTo|TestBuildDoctorChecks_NameSetUnchanged' -v` passed 5/5. `go build ./...` passed. `go vet ./...` passed. `make test-fast-parallel` passed all 8 fast jobs (`fsys-darwin-compile`, `unit-core`, and `unit-cmd-gc-1-of-6` through `unit-cmd-gc-6-of-6`). |
| 4 | No high-severity review findings open | PASS | ga-fm2vgd.3 contains no unresolved HIGH finding and was closed PASS after the earlier request-changes bead ga-fm2vgd.2 was fixed. |
| 5 | Final branch is clean | PASS | Scratch worktree was clean before writing this gate file; final cleanliness is verified after committing this checklist. |
| 7 | Single feature theme | PASS | Commit set touches one subsystem: `cmd/gc` doctor check registration, implementation, tests, and doctor-check golden names. |

## Changed Files

- `cmd/gc/cmd_doctor.go`
- `cmd/gc/doctor_hold_label_routed_to.go`
- `cmd/gc/doctor_hold_label_routed_to_test.go`
- `cmd/gc/testdata/doctor_check_names.golden`
- `release-gates/ga-ac03mi-hold-label-routed-to-gate.md`

## Commands Run

```text
git rev-list --left-right --count origin/main...origin/builder/ga-fm2vgd.1-hold-label-routing-doctor-check
git merge-base origin/main origin/builder/ga-fm2vgd.1-hold-label-routing-doctor-check
go test ./cmd/gc/ -run 'TestHoldLabelRoutedTo|TestBuildDoctorChecks_NameSetUnchanged' -v
go build ./...
go vet ./...
make test-fast-parallel
```
