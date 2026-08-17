# Release gate - PG-auth warmup mail producer (ga-uslskt.3)

**Verdict:** PASS

- Bead: `ga-uslskt.3`
- Source branch: `builder/ga-uslskt-postgres-auth-warmup-mail`
- Reviewed HEAD: `a054b971f0188bd67abd7bd1d8ec111d38446352`
- Base checked: `origin/main` at `9ddbea5c0b4b3cebf09fc36c0f88a8c52f9dd991`
- Manifest note: `docs/PROJECT_MANIFEST.md` is not present in the Gas City worktree; this gate applies the deployer prompt's seven release criteria.

## Criteria

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 6 | Branch diverges cleanly from main | PASS | Evaluated first. `git merge-tree --write-tree origin/main origin/builder/ga-uslskt-postgres-auth-warmup-mail` returned tree `cd5bcac437dd2b3cdc1d162e72d1b6c56809f169` with exit 0. Branch is 6 behind / 2 ahead of `origin/main`; no merge conflicts. |
| 1 | Review PASS present | PASS | Review bead `ga-uslskt.2` is closed with `Review verdict 2026-07-19 (gascity/reviewer): PASS`; review notes report no blocking findings. |
| 2 | Acceptance criteria met | PASS | `PostgresAuthCheck.WarmupEligible()` returns true; `WarmupMailSubject` is exactly `postgres-auth alert during city warm-up`; `SoleFailureMail` implements the PG-auth sole-failure body with deterministic severity/scope sorting and fixed footer. The already-landed warmup runner extension is present on this branch: `CustomWarmupMail`, `tryCustomSoleFailureMail`, generic fallback on mixed checks, defensive-copy isolation, and 4096-byte truncation. The five PG-auth-specific test additions are intentionally split to follow-up deploy bead `ga-xawilr`, which is blocked on this deploy per `ga-uslskt.3` instructions. |
| 3 | Tests pass | PASS | `go build ./...` passed. `go vet ./...` passed. `go test ./internal/doctor/... ./internal/warmup/...` passed. `make test-fast-parallel` passed all 8 fast jobs. |
| 4 | No high-severity review findings open | PASS | Reviewer notes identify both scrutinized items (package move and severity sort) as correct, with no blocking or high-severity findings open. |
| 5 | Final branch is clean | PASS | Scratch worktree at reviewed HEAD was clean before gate-file creation (`git status --short --branch` reported only detached HEAD). The gate file is the only deployer-authored delta and is committed as the release-gate commit. |
| 7 | Single feature theme | PASS | The commit set touches one subsystem/theme: PG-auth doctor check registration and warmup mail behavior. Diff paths are `cmd/gc/cmd_doctor.go`, `internal/doctor/checks/postgres_auth.go`, `internal/doctor/checks/postgres_auth_test.go`, `internal/doctor/checks/testenv_import_test.go`, and `internal/doctor/warmup_eligible.go`. |

## Validation

- `git fetch origin main builder/ga-uslskt-postgres-auth-warmup-mail` - PASS
- `git ls-remote origin refs/heads/builder/ga-uslskt-postgres-auth-warmup-mail refs/heads/main` - confirmed remote branch at `a054b971f0188bd67abd7bd1d8ec111d38446352` and main at `9ddbea5c0b4b3cebf09fc36c0f88a8c52f9dd991`
- `git merge-tree --write-tree origin/main origin/builder/ga-uslskt-postgres-auth-warmup-mail` - PASS, no conflicts
- `go build ./...` - PASS
- `go vet ./...` - PASS
- `go test ./internal/doctor/... ./internal/warmup/...` - PASS
- `make test-fast-parallel` - PASS, all fast jobs passed

## Review Notes

The PR should call out that this moves `PostgresAuthCheck` into `internal/doctor/checks` to avoid the `doctor -> warmup -> doctor` import cycle while keeping registration in `cmd/gc/cmd_doctor.go`. It should also call out that the dedicated PG-auth test-coverage follow-up is separate and blocked on this implementation landing.
