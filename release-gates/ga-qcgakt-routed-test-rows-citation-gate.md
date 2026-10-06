# Release Gate: fix stale docs/plans citation in check-routed-test-rows.sh

Bead: ga-qcgakt
Source bead: ga-h7ppr8
Implementation bead: ga-f74ph9.3
Branch under review (provenance only): builder/ga-f74ph9.3
Reviewed commit: ea26fc3d7
Deploy branch: deploy/ga-qcgakt-gate
Gate SHA: 9e0983a61 (cherry-pick of ea26fc3d7 onto origin/main@7a739e29b)
Gate date: 2026-07-26

Note: docs/PROJECT_MANIFEST.md is not present in this worktree. This gate uses
the deployer release criteria and the repo testing guidance in TESTING.md.

## Background

The first deploy attempt on reviewed SHA ea26fc3d7 (local gate tip cf3da432c)
failed the mandatory pre-push `make test-fast-parallel` run on an unrelated
pre-existing flake: `TestCmdStopWallClockTimeoutBoundsDirectStop` exceeded its
1s bound under sharded load. That flake's fix (the "evidence-based 5s
remediation", commit 25eb009e8) was already on `origin/main` at gate time but
not yet in the reviewed branch's base. Per the routed gate-FAIL instruction,
this gate re-cuts the same one-line fix on a fresh `deploy/ga-qcgakt-gate`
branch built directly from current `origin/main`, so the resulting SHA
contains the flake fix.

## Gate Results

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | ga-h7ppr8 review verdict PASS on ea26fc3d7; deploy bead ga-qcgakt created by gascity/reviewer with that reviewed commit. |
| 2 | Acceptance criteria met | PASS | `scripts/check-routed-test-rows.sh:116` no longer cites the nonexistent `docs/plans/ga-h6w-read-path-api-routing.md`; the hint now points to the six-row matrix definition in this script's own header comment (bead ga-h6w), matching the reviewed content of ea26fc3d7. |
| 3 | Tests pass | PASS | `go build ./...`, `go vet ./...`, `go test ./cmd/gc -run TestRoutedRowsManifestFullyCovered -count=1`, and `make check-routed-test-rows` all green on 9e0983a61. Full `make test-fast-parallel`: 9/9 fast jobs passed (see Commands log). |
| 4 | No high-severity review findings open | PASS | Single-line static-string message change, no interpolation, no new attack surface; ga-h7ppr8 review recorded no open findings. |
| 5 | Final branch is clean | PASS | `git status --short` empty before this gate file was added; this file is committed as the branch tip. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` succeeded, produced tree 3f25cb2223a9652847c903849eeb13d8a1ecec08; `git diff --check origin/main...HEAD` reported no conflict markers or whitespace errors. |
| 7 | Single feature theme | PASS | The commit touches exactly one file, `scripts/check-routed-test-rows.sh` (1 insertion, 1 deletion) — the stale-citation fix only. |

## Acceptance Checks

- PASS: `check-routed-test-rows.sh`'s manifest-violation hint no longer
  references a deleted docs/plans path.
- PASS: The six-row matrix rule itself is unchanged — this is a message-text
  fix only, not a behavior change to the check.
- PASS: `deploy/ga-qcgakt-gate` is built from current `origin/main`
  (7a739e29b), so the previously-blocking `TestCmdStopWallClockTimeoutBoundsDirectStop`
  flake fix (25eb009e8) is included in this gate SHA.
- PASS: `builder/ga-f74ph9.3` (provenance branch) was not pushed to or
  otherwise touched by this deploy.

## Commands

```text
git diff --stat origin/main HEAD
go build ./...
gofmt -l scripts/check-routed-test-rows.sh
go vet ./...
go test ./cmd/gc -run TestRoutedRowsManifestFullyCovered -count=1
make check-routed-test-rows
LOCAL_TEST_JOBS=16 CMD_GC_PROCESS_TOTAL=6 ./scripts/test-local-parallel fast
git diff --check origin/main...HEAD
git merge-tree --write-tree origin/main HEAD
```

All commands above were run on gate SHA 9e0983a61; the full fast-parallel
suite result: 9/9 jobs passed (`unit-core`, `fsys-darwin-compile`,
`push-gate-lock-selftest`, `unit-cmd-gc-1-of-6` through `unit-cmd-gc-6-of-6`),
`EXIT:0`.
