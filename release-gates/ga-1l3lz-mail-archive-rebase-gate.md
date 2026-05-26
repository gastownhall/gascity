# Release Gate: ga-1l3lz mail archive/delete eager removal

Generated: 2026-05-26T14:11:42Z

Bead: ga-1l3lz - Review: PR #2554 rebase
Feature branch: builder/ga-7a0x3.1-clean
Feature HEAD before this gate: 9d4ceb8315aadd798d10d174a8640c243ca8c91b
Base: origin/main

Note: docs/PROJECT_MANIFEST.md is not present in this checkout, so this gate uses the six deployer release criteria from the active prompt.

## Gate Summary

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Bead notes contain `Reviewer Verdict: PASS` from `gascity/reviewer` for branch `builder/ga-7a0x3.1-clean` at `9d4ceb831`. |
| 2 | Acceptance criteria met | PASS | `Archive`, `Delete`, `ArchiveMany`, and `DeleteMany` now remove message beads eagerly. Missing, closed, and already-deleted IDs are treated as idempotent already-archived/deleted results at provider, CLI, and API layers. |
| 3 | Tests pass | PASS | Focused mail/API/CLI tests passed; `make test-fast-parallel` passed all fast jobs; `go vet ./...` completed cleanly; `make dashboard-check` passed; acceptance mail slice passed; dashboard preview served the built app. |
| 4 | No high-severity review findings open | PASS | Reviewer notes list no HIGH findings. The only finding is LOW informational context about the prior gate covering the original feature commit before follow-up idempotency and test-alignment commits. |
| 5 | Final branch is clean | PASS | `git status --short --branch` was clean before adding this checklist; deployer will re-check after committing the checklist before push. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree origin/main HEAD` produced a clean tree object (`a7fac94ad8ac0f3091bffe7e9cb1b1b7841c1a56`) with no conflict records; `git diff --check origin/main...HEAD` completed cleanly. |

## Acceptance Evidence

- `internal/mail/beadmail.Provider.Archive` deletes open message beads instead of closing them, and returns `mail.ErrAlreadyArchived` for missing or already-closed messages.
- `ArchiveMany` and `DeleteMany` preserve per-ID reporting while delegating to the single-message eager-delete path.
- `cmd/gc mail archive` and `cmd/gc mail delete` keep idempotent user output for repeat operations and missing IDs.
- `POST /v0/mail/{id}/archive` and `DELETE /v0/mail/{id}` return typed OK responses for repeat calls after the message has already been removed.
- API control-plane docs were reviewed for typed-wire and object-model invariants before gate evaluation.

## Commands Run

```text
go test ./internal/mail/... -count=1
go test ./internal/api -run Mail -count=1
go test ./cmd/gc -run 'TestMailArchive|TestMailDelete|TestCLIDocsFreshness' -count=1
make test-fast-parallel
go vet ./...
make dashboard-check
go test -tags acceptance_a -timeout 10m ./test/acceptance -run 'TestMailLifecycle|TestMailErrorPaths|TestMailCommands' -count=1
npm run preview -- --host 127.0.0.1 --port 4174
curl -fsS http://127.0.0.1:4174/
git merge-tree origin/main HEAD
git diff --check origin/main...HEAD
git status --short --branch
```
