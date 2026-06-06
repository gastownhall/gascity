# Release Gate: ga-uqjge PR #1796 rebase

Date: 2026-05-24T02:32:49Z
Bead: ga-uqjge
Source bead: ga-yih2
Builder bead: ga-7xlnl
Branch evaluated: builder/ga-yih2-1 at f1f6c3acd
PR: https://github.com/gastownhall/gascity/pull/1796

Note: `docs/PROJECT_MANIFEST.md` is not present in this worktree, so this
gate uses the deployer role's release criteria.

## Gate Results

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Bead notes contain `Review verdict: PASS` from gascity/reviewer for HEAD `f1f6c3acd` on 2026-05-24. |
| 2 | Acceptance criteria met | PASS | See acceptance table below; PR #1796 behavior remains intact after the rebase/adaptation commits. |
| 3 | Tests pass | PASS | `make test-fast-parallel`, `go vet ./...`, and `make dashboard-check` passed locally; GitHub reports all non-skipped PR checks passing. |
| 4 | No high-severity review findings open | PASS | Reviewer notes list two LOW findings only; both are non-blocking. |
| 5 | Final branch is clean | PASS | `git status --short --branch` reported no uncommitted changes before writing this gate; this gate is the sole deployer artifact. |
| 6 | Branch diverges cleanly from main | PASS | `git rev-list --left-right --count origin/main...HEAD` reported `0 8`; `git diff --check origin/main...HEAD` passed; local merge-tree check produced no conflicts; GitHub reports `mergeable: MERGEABLE`, `mergeStateStatus: CLEAN`. |

## Acceptance Verification

| Acceptance item | Result | Evidence |
|-----------------|--------|----------|
| Rebased PR head is current and review-approved | PASS | PR #1796 head is `quad341:builder/ga-yih2-1` at `f1f6c3acd`; bead notes record reviewer PASS for that HEAD. |
| Rebase adaptation commits are present | PASS | Branch includes `bb0b15c` (current-main doctor/auth adaptation), `71b8e97` (dedup `pg.credential_resolved`), and `f1f6c3a` (doctor v2 test signature adaptation). |
| `gc doctor --explain-postgres-auth` remains opt-in human output | PASS | `cmd/gc/cmd_doctor.go` binds the flag; `internal/doctor/types.go` exposes the opt-in renderer; `internal/doctor/doctor.go` calls `RenderExtras` only on the streaming path. |
| `pg.credential_resolved` remains typed and password-free | PASS | `internal/pgauth/events.go` defines `PostgresCredentialResolvedPayload`; `internal/api/event_payloads.go` registers the payload; generated OpenAPI and dashboard types include `pg.credential_resolved`; `TestPostgresEventOmitsPassword` covers payload, envelope, and redaction surfaces. |
| Postgres auth doctor check stays out of startup warmup | PASS | `internal/doctor/warmup_eligible.go` includes `func (c *PostgresAuthCheck) WarmupEligible() bool { return false }`. |
| Dashboard/API schema surfaces are regenerated and valid | PASS | `make dashboard-check` ran OpenAPI TS generation, dashboard build, typecheck, and `go test ./cmd/gc/dashboard/...` successfully. |

## Test Evidence

| Command | Result |
|---------|--------|
| `make test-fast-parallel` | PASS (`unit-core`, `fsys-darwin-compile`, and `unit-cmd-gc` shards 1-6 passed). |
| `go vet ./...` | PASS |
| `make dashboard-check` | PASS |
| `git diff --check origin/main...HEAD` | PASS |
| `gh pr checks 1796` | PASS for all non-skipped checks, including CI preflight, integration shards, CodeQL, dashboard SPA, and required summary checks. |

## PR State

PR #1796 is already open against `main` from `quad341:builder/ga-yih2-1`.
Because this is a rebase/re-review gate for an existing PR, the deployer action
is to add this gate artifact to the PR branch and update the PR body, not to
open a duplicate pull request.
