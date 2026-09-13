# Release gate — PG-auth slice 4 (ga-j65i3c / ga-yih2)

**Verdict:** PASS

- Bead: `ga-j65i3c` (review of `ga-yih2`, closed)
- Branch: `quad341:builder/ga-yih2-1`
- HEAD: `c7fb142e` (slice 4 stacked on slices 1+2+3)
- Slice-4 commits (3): `65f1eb43` main, `557093cf` lint, `c7fb142e` review-fixup

## Stack dependency

This PR is **stacked on PR #1792** (PG-auth slices 1+2+3). The branch
contains all 9 PG-auth commits. While #1792 is open, this PR will show
9 commits.

## Criteria

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 1 | Reviewer PASS verdict in bead notes | PASS | `gascity/reviewer` PASS at HEAD `c7fb142e` (per gm-2uktfc); all 3 prior findings (fd leak, test-smell, unused param) addressed in `c7fb142e`. |
| 2 | Acceptance criteria met | PASS | Doctor `postgres-auth` check + `pg.credential_resolved` event payload registered + 10/10 `TestPostgresAuth` cases incl. 5 status branches, multi-scope aggregation, `CanFix==false`, RenderExtras, humanSourceLabel mappings. |
| 3 | Tests pass on final branch | PASS | `go test ./internal/doctor -run TestPostgresAuth -count=1` — PASS; `go test ./internal/events -count=1` — PASS; `go test ./internal/pgauth -count=1` — PASS. |
| 4 | No high-severity review findings open | PASS | All findings addressed in fixup commit; reviewer "all 3 prior findings addressed; all gates green". |
| 5 | Working tree clean | PASS | `git status` clean before gate-file commit. |
| 6 | Branch diverges cleanly from main | PASS | Test merge into `origin/main` 5f1a686d succeeded with no conflicts. |

## Validation (deployer re-run on `deploy/ga-j65i3c` at HEAD `c7fb142e`)

- `go build ./...` — clean
- `go vet ./...` — clean
- `golangci-lint run` — 0 issues (full repo)
- Targeted suites:
  - `go test ./internal/doctor -run TestPostgresAuth -count=1` — PASS (9ms)
  - `go test ./internal/events -count=1` — PASS (1.19s)
  - `go test ./internal/pgauth -count=1` — PASS (114ms)

## Pre-existing doctor flakes — not introduced by this PR

`go test ./internal/doctor -count=1` reports 3 unrelated failures on
`origin/main` 5f1a686d itself:

- `TestBDSplitStoreCheck_FileProviderUsesNeutralRecoveryGuidance`
- `TestBeadsStoreCheck_GCBeadsExecOverrideExternalCityUnavailableFailsBeforePing`
- `TestBeadsStoreCheck_GCBeadsFileOverrideSkipsBdPreflight`

These match the documented pre-existing failures in `ga-mvvitw`'s
review notes and are environmental (dolt-server-unreachable in this
local sandbox). The slice-4 branch in fact reports fewer overall
doctor failures.

## Push target

Pushing to fork (`quad341/gascity`); PR cross-repo. Stacked on PR #1792.
