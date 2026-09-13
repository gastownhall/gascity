# Release Gate: ga-17xlnc - process-table scanner contract

Deploy bead: ga-17xlnc
Source review bead: ga-qiyedo
Source build bead: ga-qmbisj.1
Branch: builder/ga-qmbisj-1-process-scanner
PR: https://github.com/gastownhall/gascity/pull/2837
Reviewed commit: 4321a48462bf630906dcbb2d31fa22c3548a2eb9
Final branch head: daa33edad188346e361c3349ba09b5f16fe3a414
Gate evaluated: 2026-05-31

Note: `docs/PROJECT_MANIFEST.md` is not present in this checkout. This gate
uses the release criteria from the deployer prompt loaded by `gc prime`, with
test scope aligned to `TESTING.md`.

## Summary

This change adds the runtime process-table scanner contract without changing
the core `runtime.Provider` interface. Providers can opt into
`runtime.ProcessTableScanner`, callers can reason about discovered live
runtimes through `runtime.LiveRuntime`, and `runtime.Fake` now implements the
optional scanner seam for tests.

The reviewed implementation commit was followed by a validator-owned,
test-only commit that adds focused coverage for the fake scanner decision
branches. No production code changed after the reviewed commit.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `bd show ga-qiyedo` records `Review Verdict: PASS` from `gascity/reviewer` for commit `4321a48462bf630906dcbb2d31fa22c3548a2eb9` on PR #2837. Final head adds only `internal/runtime/fake_test.go` coverage in `daa33edad188346e361c3349ba09b5f16fe3a414`, closing validator follow-up `ga-0hof7r`. |
| 2 | Acceptance criteria met | PASS | `runtime.LiveRuntime` and optional `runtime.ProcessTableScanner` are added in `internal/runtime/runtime.go`; `runtime.Provider` is unchanged; `runtime.Fake` initializes `OrphanedRuntimes` in `NewFake` and `NewFailFake`; fake scanner methods use `GC_SESSION_ID` from tracked session env; `TerminateRuntime` records by session ID and removes orphan entries; compile-time conformance covers the fake scanner seam; focused tests now exercise the fake scanner branches. |
| 3 | Tests pass | PASS | `go test ./internal/runtime -count=1` passed; `make test-fast-parallel` completed with `All fast jobs passed`; `go vet ./...` exited 0; `git diff --check origin/main...HEAD` exited 0. GitHub PR checks for #2837 are green at final head. |
| 4 | No high-severity review findings open | PASS | Review notes for `ga-qiyedo` state `No findings`; the only follow-up was coverage bead `ga-0hof7r`, now closed by the test-only commit. No HIGH findings are recorded in the deploy bead or review notes. |
| 5 | Final branch is clean | PASS | Before writing this gate, `git status --short --branch` showed a clean `builder/ga-qmbisj-1-process-scanner` branch aligned with `origin/builder/ga-qmbisj-1-process-scanner`; cleanliness is rechecked after committing this gate before push. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree HEAD origin/main` exited 0 at final branch head before the gate commit. |
| 7 | Single feature theme | PASS | The commit set touches one runtime testing seam: `internal/runtime/runtime.go`, `internal/runtime/fake.go`, and focused tests in `internal/runtime/fake_test.go`. It does not bundle unrelated subsystems or user-facing features. |

## Acceptance Evidence

- `runtime.LiveRuntime` carries the observed runtime fields needed by future
  process-table scanners: session ID, epoch, PID, provider name, and tracked
  state.
- `runtime.ProcessTableScanner` is an optional interface, preserving the
  existing `runtime.Provider` contract for providers that cannot scan the
  process table.
- `runtime.Fake` records scanner calls and exposes tracked sessions from
  `Config.Env["GC_SESSION_ID"]` plus explicit orphan runtimes from
  `OrphanedRuntimes`.
- `runtime.Fake.TerminateRuntime` records the session ID, removes orphaned
  entries, returns nil for missing entries, and returns contextual errors from
  the broken fake.
- `internal/runtime/fake_test.go` covers the fake scanner branch behavior added
  by the implementation.

## Commands

```text
gh auth status
git pull --ff-only origin builder/ga-qmbisj-1-process-scanner
git fetch origin main
git status --short --branch
git diff --check origin/main...HEAD
git merge-tree --write-tree HEAD origin/main
git diff --stat origin/main...HEAD
go test ./internal/runtime -count=1
make test-fast-parallel
go vet ./...
```
