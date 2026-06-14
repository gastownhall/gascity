# Release Gate: compact concurrent DELETE downgrade

Bead: `ga-mb7y1f`
Source bead: `ga-czj02f`
Review bead: `ga-1rbe9r`
PR: https://github.com/gastownhall/gascity/pull/3262
Head under test: `1617ed0335f6a9018a65c9572513851e11ae283a`
Base: `origin/main` at `890b31c8c916538ad313d2616301fd2530aa675f`

Note: `docs/PROJECT_MANIFEST.md` is not present in this checkout. This gate
uses the deployer release criteria supplied in the agent instructions.

## Summary

PASS. The change is limited to the Dolt compact script and its compact-script
tests. It fixes the concurrent-writer DELETE false-positive quarantine by
separating row-decrease hash drift from same-count hash drift, and downgrades
only the proven writer-race row-decrease-only case to skip-and-retry.

## Gate Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `bd show ga-1rbe9r` records `REVIEWER VERDICT: PASS (pending arch-hold clearance)` for commit `1617ed033` on branch `builder/ga-czj02f`. |
| 2 | Acceptance criteria met | PASS | `examples/dolt/commands/compact/run.sh` adds `verify_counts_saw_decrease_hash_drift` and leaves same-count drift separate (`run.sh:794`, `run.sh:881`, `run.sh:888`). The row-decrease-only writer-race downgrade calls `defer_writer_race_after_flatten` while excluding same-count drift, table-list changes, and probe failures (`run.sh:1932`, `run.sh:1937`, `run.sh:1958`, `run.sh:1964`). Coverage added for both the positive writer-race defer and stable-HEAD quarantine negative control (`dog_exec_scripts_test.go:4068`, `dog_exec_scripts_test.go:4100`). |
| 3 | Tests pass | PASS | `go test ./examples/dolt/... -run TestCompactScript` passed: `ok github.com/gastownhall/gascity/examples/dolt 45.658s`. `make test-fast-parallel` passed: all fast jobs passed. `go vet ./...` passed. `gh pr checks 3262` reports required CI pass, including `CI / preflight`, `CI / integration`, `CI / required`, CodeQL, and integration shards. |
| 4 | No high-severity review findings open | PASS | Review bead `ga-1rbe9r` lists no unresolved HIGH findings and closes with PASS. `gh pr view 3262 --json reviews,comments` returned no GitHub review threads or comments. |
| 5 | Final branch is clean | PASS | Gate worktree was clean before this checklist was added: `git status --short --branch` returned only `## HEAD (no branch)`. Final clean state is rechecked after committing the gate file before push. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` exited 0 and produced tree `260260b638bc4e3ef59a33df7639174a094ca7d1`. GitHub reports `mergeStateStatus: CLEAN`. |
| 7 | Single feature theme | PASS | Commit set touches only `examples/dolt/commands/compact/run.sh` and `examples/dolt/dog_exec_scripts_test.go`. Both commits implement and test the same Dolt compact row-decrease writer-race behavior. |

## PR And Hold State

- PR head: `builder/ga-czj02f` at `1617ed0335f6a9018a65c9572513851e11ae283a`.
- PR state: open, not draft, merge state `CLEAN`.
- PR labels at gate time: `kind/bug`, `priority/p1`, `status/reviewing`.
- Arch-hold: the review bead records that an arch-hold existed during review.
  At deploy gate time, GitHub reports clean merge state and no hold label is
  present. Merge authority remains mayor/mpr; deployer does not merge.

## Local Commands

```text
go test ./examples/dolt/... -run TestCompactScript
make test-fast-parallel
go vet ./...
git merge-tree --write-tree origin/main HEAD
gh pr checks 3262
```
