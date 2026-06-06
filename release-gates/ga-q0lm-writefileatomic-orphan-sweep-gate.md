# Release Gate: WriteFileAtomic orphan sweep

- Deploy bead: ga-e1nma6
- Source bead: ga-q0lm
- Branch: builder/ga-q0lm-1
- Implementation commit: 91ef2bf61
- Base checked: origin/main at e7ca02712
- Gate date: 2026-05-15 PDT
- Manifest note: docs/PROJECT_MANIFEST.md is not present in this repo snapshot; this gate uses the deployer release criteria.

## Checklist

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `bd show ga-e1nma6` contains `Review Verdict: PASS` for commit 91ef2bf61285. |
| 2 | Acceptance criteria met | PASS | `internal/fsys/atomic.go` runs `sweepDeadAtomicOrphans` only after a successful rename. The sweep is bounded to same-directory siblings named `<basename>.tmp.<pid>.<suffix>`, parses only valid positive PIDs, preserves live PIDs with `pidutil.Alive`, and ignores cleanup errors. `internal/fsys/atomic_test.go` covers dead-PID removal, live-PID preservation, unrelated basename preservation, and unparseable peer preservation. |
| 3 | Tests pass | PASS | `go test -short -run TestWriteFileAtomic ./internal/fsys/...` passed. `go vet ./...` passed. `make test` passed with `observable go test: PASS log=/tmp/gascity-test.jsonl`. |
| 4 | No high-severity review findings open | PASS | Review notes report Style PASS, Security PASS, Spec Compliance PASS, Test Coverage PASS, and no blocking or HIGH findings. |
| 5 | Final branch is clean | PASS | After committing this gate, `git status --short --branch` showed only the branch line with the release-gate commit ahead of `fork/builder/ga-q0lm-1`; there were no unstaged, staged, or untracked worktree changes. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` exited 0 on the final branch with no conflict diagnostics. |

## Acceptance Details

The change addresses temp-file accumulation after a process dies between
`WriteFileAtomic` temp-file creation and rename. Cleanup is deliberately
best-effort and happens after the successful write path, so cleanup failures
cannot fail a write that already completed.

The matching rule is narrow: only sibling files for the same target basename
and the current temp suffix scheme are eligible. The implementation preserves
live writer temp files by checking the encoded PID before removal, and tests
exercise the preservation cases explicitly.

## Test Commands

```text
go test -short -run TestWriteFileAtomic ./internal/fsys/...
go vet ./...
make test
```
