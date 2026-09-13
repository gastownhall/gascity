# Release gate - PG-auth slices 1+2+3 (ga-6zqpw, ga-pnqg.1)

**Verdict:** PASS after local maintainer fixups; requires a fresh review pass
before merge.

- Primary review bead: `ga-6zqpw`
- Workflow review root: `ga-st3pu8g`
- PR: `gastownhall/gascity#1792`
- Branch: `quad341:builder/ga-wvka-1`
- Rebase base: `origin/main` `3d2636b7`
- Stack: 10 commits ahead of `origin/main`

## Current stack

| # | Subject |
|---|---------|
| 1 | `feat(pgauth): add Postgres credential resolver mirroring internal/doltauth` |
| 2 | `feat(contract): add Postgres MetadataState fields with parse validation` |
| 3 | `fix(contract): correct misspell 'behaviour' -> 'behavior'` |
| 4 | `feat(bd_env): wire pgauth into gc bd subprocess env` |
| 5 | `docs(bd_env): drop historical rename comment from godoc` |
| 6 | `fix(lint): resolve four golangci findings on PG-auth slice 2/3 stack` |
| 7 | `chore: release gate PASS for ga-6zqpw + ga-pnqg.1 (PG-auth slices 1+2+3)` |
| 8 | `fix(pg-auth): complete backend env projection` |
| 9 | `fix(pg-auth): surface remaining projection errors` |
| 10 | `fix(pg-auth): keep order env postgres-only` |

## Attempt-5 maintainer fixups

The attempt-5 review blocked on stale base and backend-projection regressions.
This local fixup:

- rebases the PR stack onto `origin/main` `fbc06f8d8`, preserving current-main
  behavior from the API/hook/session/order work that landed after the previous
  base;
- projects city-level Postgres metadata for inherited rig scopes instead of
  falling back to Dolt;
- treats inherited city Postgres scopes as external PG endpoints in transport
  errors, so managed Dolt recovery is not attempted;
- restores Dolt env-override fallback when the managed city runtime is
  unavailable, while keeping `.invalid` ambient sentinel hosts scrubbed;
- keeps `TestGcBdRejectsGCBeadsFileOverride` isolated from inherited Beads
  environment variables;
- deprecates silent env-wrapper call sites in favor of the `*WithError`
  variants where Postgres projection can fail; and
- removes the unreachable order-dispatch output log from the env-build error
  path.

## Operator note

PG-auth slices 1+2+3 deliberately make canonical `metadata.json` validation
strict. E1-E5 parse failures now surface as command errors or skipped
controller iterations instead of silently building a partial bd environment.
Operators should fix rejected metadata before expecting pool, session, order,
or controller probe work for that scope to continue.

## Attempt-8 maintainer fixups

The attempt-8 review found that `origin/main` had advanced again and that two
backend-exclusivity gaps remained. This local fixup:

- rebases the PR stack onto `origin/main` `3d2636b7`, removing the stale-base
  test-deletion artifacts from the current review diff;
- treats inherited-city Postgres rigs as Postgres-backed during lifecycle init,
  so `initAndHookDir` skips provider Dolt init and does not pin inherited rigs
  with local Dolt metadata; and
- clears inherited Postgres projection before applying explicit legacy rig Dolt
  host/port config, keeping child bd/order subprocess env backend-exclusive.

## Validation

- `git diff --check` - PASS
- `go test ./internal/pgauth ./internal/beads/contract ./internal/doctor` - PASS
- Focused cmd/gc regression suite for PG projection, inherited city PG,
  env-override fallback, order env, and Beads override rejection - PASS
- `GC_FAST_UNIT=1 GO_TEST_COUNT=1 GO_TEST_TIMEOUT=10m ./scripts/test-go-test-shard ./cmd/gc <shard> 6` for shards 1-6 - PASS
- `timeout 300s go test ./cmd/gc ./internal/pgauth ./internal/beads/contract`
  - TIMED OUT with no diagnostics; replaced by the passing package-specific
  and sharded cmd/gc validation above.

## Review status

The stale-base blocker and attempt-8 backend-exclusivity findings have been
addressed locally with targeted regression tests. Because attempt 8 blocked
before these fixes, the workflow verdict remains `iterate` so the next review
pass can verify the amended stack.
