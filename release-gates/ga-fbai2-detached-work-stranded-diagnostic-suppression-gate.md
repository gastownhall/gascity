# Release Gate: detached work stranded diagnostic suppression

Bead: ga-fbai2
Branch: builder/ga-d457b
Reviewed head: 2b324e31a9c05c07b995da635aaee56be7791848
Base: origin/main @ 8fe54229572b539ad7e5e2d3fe236ab621b565b6

Note: `docs/PROJECT_MANIFEST.md` is not present in this worktree, so this gate
uses the release criteria from the deployer instructions.

## Commit Stack

| Commit | Subject |
|--------|---------|
| d7b434137 | fix(doctor): guard local-only dolt remotes |
| 7eb789d6d | fix(session): clear breaker on kill |
| 7483229bd | fix(sling): nudge bead-named pool sessions |
| 5c14613c1 | feat(gc): add detached tmux probe primitive |
| f31b167b6 | fix(gc): protect orphan release for detached work |
| 2b324e31a | fix(gc): suppress stranded diagnostics for live detached work |

## Gate Checklist

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `bd show ga-fbai2` contains reviewer verdict `PASS` for reviewed head `2b324e31a`; minor findings are explicitly non-blocking. |
| 2 | Acceptance criteria met | PASS | Branch implements the reviewed behavior: detached tmux probe primitive, detached orphan-release protection, stranded diagnostic suppression for live detached work, circuit-breaker clear on session kill, sling nudge resolution through bead session names, and local-only Dolt remote doctor guard/fix. Tests cover these surfaces in `cmd/gc` and `internal/doctor`. |
| 3 | Tests pass | PASS | `TMPDIR=/tmp make test-fast-parallel`; `TMPDIR=/tmp go vet ./...`. |
| 4 | No high-severity review findings open | PASS | Reviewer notes list three minor non-blocking findings and no HIGH findings. |
| 5 | Final branch is clean | PASS | `git status --short` was empty before writing this gate file; deployer rechecks after the gate commit before opening the PR. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` succeeded before and after the gate commit; no content conflicts with `origin/main`. |

## Changed Surface

- `cmd/gc`: detached session probing, stranded diagnostic filtering, session
  reset/circuit behavior, and sling nudge session-name resolution.
- `internal/doctor`: local-only Dolt remote check and explicit-fix guard.
- `internal/beads/contract`: file helper support used by the doctor check.
- `examples/gastown`: prompt/template cleanup for detached-work guidance.

## Test Output Summary

```text
TMPDIR=/tmp make test-fast-parallel
All fast jobs passed

TMPDIR=/tmp go vet ./...
PASS (no output)
```

## Diagnostic Notes

An initial monolithic `go test ./cmd/gc ./internal/doctor ./internal/beads/contract -count=1`
run was discarded as gate evidence because it hit local environment issues:
`/home/jaword/.local/bin/bd` was unavailable for a slow provider path, the
shared `/tmp` root was under pressure, and an unrelated controller socket test
timed out. The official sharded fast baseline was rerun with `/tmp` after
space recovered and passed.
