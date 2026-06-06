# Release Gate: ga-ycw6.1 - selfhost UX acceptance scaffold

**Deploy bead:** ga-il4xof
**Originating bead:** ga-ycw6.1
**Branch:** `builder/ga-ycw6-1` (fork: `quad341/gascity`)
**PR:** https://github.com/gastownhall/gascity/pull/2260
**Commit:** 1e5f29a1956b343e2fc4dffd63fbe67d93c2f0ce
**Evaluated:** 2026-05-17T00:59:22Z
**Verdict:** PASS

Note: `docs/PROJECT_MANIFEST.md` is not present in this worktree. This gate
uses the deployer role's six release criteria plus the originating bead's
verification gates.

## Criteria

| # | Criterion | Status | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | ga-il4xof notes contain `Review verdict: PASS` from `gascity/reviewer` for commit `1e5f29a1956b343e2fc4dffd63fbe67d93c2f0ce`. |
| 2 | Acceptance criteria met | PASS | `test/acceptance/integration_selfhost_ux_test.go` adds the `acceptance_a` selfhost UX scaffold, active `gc stop` bypass and op-init fast-path checks, architecture-approved skips for the two prerequisite-dependent tests, and placeholder tests for the remaining follow-up beads. `test/acceptance/helpers/city.go` adds `WriteV1AgentBlock`, `WriteV2AgentDir`, and `StartExpectingFatal` with both `FATAL:` and `gc-fatal:` prefix support. |
| 3 | Tests pass | PASS | `go test -tags=acceptance_a -run TestSelfhostUX ./test/acceptance/...` PASS; `make test-fast-parallel` PASS; `go vet ./...` PASS; `go build ./...` PASS; `git diff --check origin/main...HEAD` PASS. GitHub PR checks are also green on PR #2260. |
| 4 | No high-severity review findings open | PASS | Reviewer notes list "Findings: none blocking" and contain no unresolved HIGH findings. |
| 5 | Final branch is clean | PASS | Branch was clean before adding this gate; final cleanliness is verified after the gate commit. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-base --is-ancestor origin/main HEAD` returned 0 before the gate commit, and GitHub reports PR #2260 merge state `CLEAN`. |

## Changed Files

| Path | Gate note |
|------|-----------|
| `test/acceptance/integration_selfhost_ux_test.go` | New selfhost UX umbrella acceptance scaffold with two active tests, two guarded tests, and discoverable placeholders. |
| `test/acceptance/helpers/city.go` | Adds City DSL helpers for v1/v2 agent fixtures and fatal-start assertions. |
| `docs/reference/cli.md` | Generated CLI reference sync from the pre-commit hook. |

## Local Commands

```text
go test -tags=acceptance_a -run TestSelfhostUX ./test/acceptance/...
make test-fast-parallel
go vet ./...
go build ./...
git diff --check origin/main...HEAD
```
