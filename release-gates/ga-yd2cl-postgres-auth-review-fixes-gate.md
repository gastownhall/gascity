# Release Gate: ga-yd2cl postgres-auth review fixes

Date: 2026-05-19T18:02:40Z
Bead: ga-yd2cl
Source bead: ga-b8buug
Branch evaluated: deploy/ga-yd2cl at 3bb6070a9
PR: https://github.com/gastownhall/gascity/pull/1796

Note: `docs/PROJECT_MANIFEST.md` is not present in this worktree, so this
gate uses the deployer role's release criteria.

## Gate Results

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Bead notes contain `### Verdict: pass` from gascity/reviewer. |
| 2 | Acceptance criteria met | PASS | See acceptance table below; all five scope items from the bead description were verified against code. |
| 3 | Tests pass | PASS | `make dashboard-check`, focused Go tests, `make test-fast-parallel`, and `go vet ./...` all passed. |
| 4 | No high-severity review findings open | PASS | Bead notes list no HIGH findings; prior medium DRY finding is marked moot and non-blocking. |
| 5 | Final branch is clean | PASS | `git status --short --branch` reported no uncommitted changes before writing this gate. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree HEAD origin/main` exited 0; GitHub reports PR #1796 mergeable. |

## Acceptance Verification

| Acceptance item | Result | Evidence |
|-----------------|--------|----------|
| Merge `origin/main` into PR branch | PASS | HEAD is `3bb6070a9 Merge origin/main into PR 1796`; PR #1796 head is `builder/ga-yih2-1`. |
| Preserve `gc doctor --json` wiring | PASS | `cmd/gc/cmd_doctor.go` keeps `--json`, `writeDoctorJSON`, `writeDoctorJSONError`, and the `RunCollect` JSON path. |
| Preserve `gc doctor --explain-postgres-auth` wiring | PASS | `cmd/gc/cmd_doctor.go` binds the flag, sets `doctor.CheckContext.ExplainPostgresAuth`, and registers postgres-auth only when a Postgres-backed scope exists. |
| Keep postgres-auth registered | PASS | `cmd/gc/cmd_doctor.go` calls `doctor.NewPostgresAuthCheck(cityPath, cfg)` under `doctorWorkspaceHasPostgresScope`. |
| Render extras only for streaming human output | PASS | `internal/doctor/doctor.go` routes `RunCollect` through `io.Discard`; `RenderExtras` runs only in the streaming path. |
| `PostgresAuthCheck.WarmupEligible()` is false | PASS | `internal/doctor/warmup_eligible.go` includes `func (c *PostgresAuthCheck) WarmupEligible() bool { return false }`. |

## Test Evidence

| Command | Result |
|---------|--------|
| `make dashboard-check` | PASS |
| `go test ./internal/doctor/... ./internal/events/... ./internal/pgauth/... ./internal/api/... ./cmd/gc/ -run "TestPostgresAuth|TestHumanSourceLabel|TestPostgresEventOmitsPassword|TestTypedEventEnvelopeUnionsCoverKnownEventTypes|TestOpenAPISpecInSync|TestDoDoctor|TestDoctor|TestJsonlArchiveDoctor" -count=1` | PASS |
| `make test-fast-parallel` | PASS |
| `go vet ./...` | PASS |

## PR State

PR #1796 is open at `quad341:builder/ga-yih2-1`, base `main`, and GitHub
reported `mergeable: MERGEABLE` during gate evaluation.
