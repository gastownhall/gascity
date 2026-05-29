# Release Gate: detached work stranded diagnostic suppression

Bead: ga-fbai2
Post-rebase review bead: ga-eqhao
PR: https://github.com/gastownhall/gascity/pull/2539
Branch: builder/ga-d457b
Reviewed rebased head: f9455e7e738e5c35ab299994f4ac9d1877bb6a7d
Base: origin/main @ 3268ee094b2d7b5f7f753029a929ac5f88691c1e

Note: `docs/PROJECT_MANIFEST.md` is not present in this worktree, so this gate
uses the release criteria from the deployer instructions.

## Commit Stack

| Commit | Subject |
|--------|---------|
| 95ea97d6b | fix(doctor): guard local-only dolt remotes |
| 5e0acc1d3 | feat(gc): add detached tmux probe primitive |
| 23f8e8051 | fix(gc): protect orphan release for detached work |
| 40f63157e | fix(gc): suppress stranded diagnostics for live detached work |
| f9455e7e7 | chore: release gate PASS for ga-fbai2 |

The rebase intentionally dropped duplicate upstream session-breaker and sling
nudge patches that were present in the earlier reviewed stack.

## Gate Checklist

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `bd show ga-fbai2` contains reviewer verdict `PASS` for the original feature stack. `bd show ga-eqhao` contains `REVIEWER VERDICT: PASS` for the rebased PR head `f9455e7e7`; low/info findings are explicitly non-blocking. |
| 2 | Acceptance criteria met | PASS | The rebased branch preserves the reviewed behavior: detached tmux probe primitive, detached orphan-release protection, stranded diagnostic suppression for live detached work, and local-only Dolt remote doctor guard/fix. Duplicate session-breaker and sling-nudge commits were dropped because those changes are already in `origin/main`. Focused tests remain in `cmd/gc` and `internal/doctor`. |
| 3 | Tests pass | PASS | From isolated worktree `/home/jaword/.gotmp/gascity-pr2539-gate.uDdSKk`: `TMPDIR=/home/jaword/.gotmp/gc-pr2539-gate LOCAL_TEST_JOBS=2 CMD_GC_PROCESS_TOTAL=6 make test-fast-parallel` passed; `TMPDIR=/home/jaword/.gotmp/gc-pr2539-gate go vet ./...` passed. GitHub PR checks for PR #2539 are also green. |
| 4 | No high-severity review findings open | PASS | Rebase review notes list two LOW findings and one INFO note; no HIGH or BLOCKING findings. Original feature review listed only minor non-blocking findings. |
| 5 | Final branch is clean | PASS | `git status --short` was empty before writing this refreshed gate file; deployer rechecks after the gate commit before pushing. |
| 6 | Branch diverges cleanly from main | PASS | `gh pr view 2539` reports merge state `CLEAN`; `git merge-tree origin/main f9455e7e738e5c35ab299994f4ac9d1877bb6a7d` succeeded with tree `770490ce176ad0c9815da1406dfd11e7c40f5c1c`. |

## Changed Surface

- `cmd/gc`: detached session probing, stranded diagnostic filtering, and
  detached orphan-release protection.
- `internal/doctor`: local-only Dolt remote check and explicit-fix guard.
- `internal/beads/contract`: file helper support used by the doctor check.
- `AGENTS.md`: tmux safety guidance aligned with the detached-work behavior.

## Test Output Summary

```text
TMPDIR=/home/jaword/.gotmp/gc-pr2539-gate LOCAL_TEST_JOBS=2 CMD_GC_PROCESS_TOTAL=6 make test-fast-parallel
All fast jobs passed

TMPDIR=/home/jaword/.gotmp/gc-pr2539-gate go vet ./...
PASS (no output)
```

## Diagnostic Notes

An initial `make test-fast-parallel` from the deployer worktree failed in
`TestRigAnywhere_ResolveRigToContext` because the test discovered the parent
`/home/jaword/projects/gc-management` city and parsed its local
`packs/maintainer-pr-review/pack.toml`, which still uses the deprecated
`[formulas].dir` field. The same branch passed from the isolated worktree above,
so the parent-city failure is treated as environmental contamination rather than
release-gate evidence for PR #2539.
