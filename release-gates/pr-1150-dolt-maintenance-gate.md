# Release gate: PR #1150 Dolt maintenance loop

Evaluated: 2026-05-28T15:58:44Z

## Scope

- Deploy bead: `ga-8oyd3f` - Review: PR #1150 maintenance wiring fix
- Source bead: `ga-x1nrsn` - Review: PR #1150 rebase Dolt maintenance
- PR: https://github.com/gastownhall/gascity/pull/1150
- Branch: `feat/adr-0002-dolt-maintenance`
- PR head remote: `fork` (`quad341:gascity`)
- Evaluated commit: `d182e067b`
- Current `origin/main`: `3203b502f`
- Merge base with `origin/main`: `e6c12d21a`

The `docs/PROJECT_MANIFEST.md` file referenced by the deployer prompt is not
present in this checkout, so this gate uses the six release criteria from the
active deployer instructions.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `ga-8oyd3f` notes contain `REVIEW VERDICT: PASS` for PR #1150 at `feat/adr-0002-dolt-maintenance @ d182e067b`. |
| 2 | Acceptance criteria met | PASS | `executeCycleLocked` now runs snapshot before Dolt GC, preserves `MaintenanceError` stages and snapshot path, and `TriggerNow` coverage proves stage order and lease behavior. API/CLI maintenance routes, generated schema/client surfaces, config, events, alert mail, and runbook updates are present. |
| 3 | Tests pass | PASS | Focused maintenance/API/CLI/config/genclient tests, fast baseline, vet, dashboard check, dashboard smoke, and whitespace check all passed on the evaluated head. |
| 4 | No high-severity review findings open | PASS | The prior HIGH review finding on placeholder `executeCycleLocked` is fixed in `d182e067b`; `ga-8oyd3f` review notes list no blocking or HIGH findings after the fix. |
| 5 | Final branch is clean | PASS | `git status --short` was empty before adding this release-gate file; this file is the only deployer change and is committed with the gate. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree HEAD origin/main` exited 0 and produced tree `17c9f7ce5bb3ac9da6d35bd0c79e631e1b005461`; no merge conflict was reported against current `origin/main`. |

## Acceptance Evidence

- `executeCycleLocked(ctx)` calls `runSnapshot(ctx)` before `runDoltGC(ctx)`.
- Snapshot path is retained in success and GC-error maintenance runs.
- `MaintenanceError.Stage` is preserved for backup, GC, and smoke-test failures.
- Manual trigger behavior remains non-blocking on lease contention and records
  in-flight state.
- API mutation route requires the anti-CSRF `X-GC-Request` header.
- CLI exit codes remain documented and test-covered.

## Validation

- `go test ./internal/supervisor -count=1` - PASS
- `go test ./internal/api -run Maintenance -count=1` - PASS
- `go test ./cmd/gc -run Maintenance -count=1` - PASS
- `go test ./internal/config ./internal/api/genclient -count=1` - PASS
- `make test-fast-parallel` - PASS
- `go vet ./...` - PASS
- `make dashboard-check` - PASS after rerunning serially
- `make dashboard-smoke` - PASS after rerunning serially
- `git diff --check origin/main...HEAD` - PASS

## Push Target

PR #1150 is cross-repo from `quad341:feat/adr-0002-dolt-maintenance`.
Dry-run push to `fork` was up to date before the gate commit. Dry-run push to
`origin` for the same branch name was rejected as non-fast-forward, so this
gate must be pushed to `fork` to update the open PR head.
