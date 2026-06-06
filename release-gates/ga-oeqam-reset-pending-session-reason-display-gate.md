# Release Gate: reset-pending session reason display

Bead: ga-oeqam
Branch: builder/ga-k0n20-1-3
Commit: c365b7c5b516917e751f26ac0efb2b8fd639725a
Base: origin/main @ 8fe54229572b539ad7e5e2d3fe236ab621b565b6

Note: `docs/PROJECT_MANIFEST.md` is not present in this worktree, so this gate
uses the release criteria from the deployer instructions.

## Gate Checklist

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `bd show ga-oeqam` contains `Verdict: PASS - no findings` for commit `c365b7c5b`. |
| 2 | Acceptance criteria met | PASS | The change makes `gc session list` show `reset-pending` when bead metadata has `restart_requested=true` and the runtime provider reports the session live; it falls back to the existing sleep reason when the runtime is not live. Covered by `TestSessionReason_ResetPendingLiveRuntimeOverridesOtherReasons` and `TestSessionReason_ResetPendingNotLiveFallsBack`. |
| 3 | Tests pass | PASS | `go test ./cmd/gc -run 'TestSessionReason_ResetPending' -count=1`; `make test-fast-parallel`; `go vet ./...`. |
| 4 | No high-severity review findings open | PASS | Reviewer notes list no findings and no HIGH findings. |
| 5 | Final branch is clean | PASS | `git status --short` was empty before writing this gate file; deployer rechecks after the gate commit before opening the PR. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-base --is-ancestor origin/main HEAD` succeeded before the gate commit; feature branch is one commit over `origin/main`. |

## Changed Surface

- `cmd/gc/cmd_session.go`: adds the `reset-pending` reason before normal wake
  reason calculation when a live session has a pending restart.
- `cmd/gc/cmd_session_test.go`: adds two focused tests covering live and
  not-live restart-pending behavior, including metadata immutability checks.

## Test Output Summary

```text
go test ./cmd/gc -run 'TestSessionReason_ResetPending' -count=1
ok  	github.com/gastownhall/gascity/cmd/gc	0.457s

make test-fast-parallel
All fast jobs passed

go vet ./...
PASS (no output)
```
