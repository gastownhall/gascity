# Release Gate: Eager-delete archived mail wisps (`ga-eivr0`)

Date: 2026-05-24
Branch: `builder/ga-7a0x3.1-clean`
Source: `fork/builder/ga-7a0x3.1-clean`
Feature commit: `4523128f4 fix(mail): eager-delete archived beads`

`docs/PROJECT_MANIFEST.md` is not present in this worktree, so this gate uses
the release criteria from the deployer instructions.

## Scope

The final branch contains one feature commit above `origin/main`. The diff is
limited to mail archive/delete behavior, mail tests/bench coverage, and CLI
reference text:

- `cmd/gc/cmd_mail.go`
- `cmd/gc/cmd_mail_test.go`
- `docs/reference/cli.md`
- `internal/mail/beadmail/beadmail.go`
- `internal/mail/beadmail/beadmail_bench_test.go`
- `internal/mail/beadmail/beadmail_test.go`
- `internal/mail/mail.go`

## Gate Criteria

| # | Criterion | Status | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead notes contain `FINAL: PASS - ready for deploy` for clean branch `fork/builder/ga-7a0x3.1-clean` at `4523128f4`. |
| 2 | Acceptance criteria met | PASS | `Archive`, `ArchiveMany`, `Delete`, and `DeleteMany` now remove archived/deleted mail beads from the store while preserving idempotent already-archived behavior for closed, missing, and already-deleted IDs. CLI reference text was updated to describe removal from the store. Targeted mail and CLI docs tests pass. |
| 3 | Tests pass | PASS | `go test ./internal/mail/... -count=1` PASS. `go test ./cmd/gc -run 'TestMailArchiveMissingBeadAlreadyArchived|TestMailArchive|TestMailDelete|TestCLIDocsFreshness' -count=1` PASS. `make test` PASS from detached clean worktree `/home/jaword/tmp/gascity-ga-eivr0-test-1779615688` with `TMPDIR=/tmp/gct-eivr0`. `go vet ./...` PASS. Earlier `make test` attempts failed only due parent-city config contamination and an overly long test `TMPDIR` socket path; the clean short-`TMPDIR` run passed. |
| 4 | No high-severity review findings open | PASS | Reviewer listed only LOW informational findings and concluded final PASS; no unresolved HIGH findings are present in bead notes. |
| 5 | Final branch is clean | PASS | `git status --short --branch` was clean before adding this gate file. The release-gate commit is the only deployer change. |
| 6 | Branch diverges cleanly from main | PASS | `git diff --check origin/main...HEAD` PASS. `git merge-tree --write-tree HEAD origin/main` exited 0, indicating no merge conflicts with current `origin/main`. |

## Acceptance Evidence

- `TestArchiveReadAfterDeleteReturnsNotFound` verifies archived mail is removed
  from the store.
- `TestArchiveManyDeletesImmediately` verifies batch archive removes each bead.
- `TestArchiveManyDoesNotUseCloseAll` verifies the old close-in-place batch path
  is no longer used.
- `TestMailArchiveMissingBeadAlreadyArchived` verifies CLI idempotence for
  already-removed IDs.
- `TestCLIDocsFreshness` verifies the generated CLI reference text is current.

## Result

PASS. This branch is ready for PR creation.
