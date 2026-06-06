# Release gate - session-bead prune reaper step (ga-6zig)

Date: 2026-05-16

## Scope

- Source bead: `ga-6zig` - Add session-bead prune step to reaper.sh (gm DB, 30d, bd prune)
- Review bead: `ga-rblaak` - Review: session-bead prune reaper step
- Branch under gate: `builder/ga-6zig-session-prune`
- Builder commit: `99e10caa1d193c9b6710d15e03d76ba2205fddf1`
- Change surface:
  - `examples/gastown/packs/maintenance/assets/scripts/reaper.sh`
  - `examples/gastown/maintenance_scripts_test.go`
  - `docs/reference/cli.md`

Note: `docs/PROJECT_MANIFEST.md` is not present on this branch. This gate uses
the deployer role's release criteria plus the repository testing guidance in
`TESTING.md`.

## Gate Checklist

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `ga-rblaak` notes include `REVIEWER VERDICT: PASS` for builder commit `99e10caa1`. Single-pass review is sufficient while the gemini second-pass is disabled. |
| 2 | Acceptance criteria met | PASS | The reaper script defines `GC_REAPER_SESSION_PURGE_AGE` defaulting to `720h`, calls `bd prune --pattern gm-* --older-than ... --json` from the city root with `BEADS_DIR` scoped to the city bead store, omits `--force` in dry-run mode, records an anomaly above 1000 pruned sessions, degrades to `sessions-pruned:0` when `bd` is missing, and includes `sessions-pruned:N` in the summary. Tests cover normal prune, dry-run preview, anomaly escalation, missing `bd`, and no-Dolt-database execution. |
| 3 | Tests pass | PASS | `bash -n examples/gastown/packs/maintenance/assets/scripts/reaper.sh`; `go test ./examples/gastown -run 'TestReaperSessionPruneRunsWhenNoDoltDatabases|TestReaperSessionPrune|TestReaperPrunesClosedSessionBeadsWithBdPrune'`; `go test ./examples/gastown/...`; `make test-fast-parallel`; `go vet ./...`; manual dry run `GC_REAPER_DRY_RUN=1 GC_CITY_PATH=/home/jaword/projects/gc-management examples/gastown/packs/maintenance/assets/scripts/reaper.sh` exited 0 and printed `sessions-pruned:0`. |
| 4 | No high-severity review findings open | PASS | `ga-rblaak` review notes list security/spec/coverage checks and only two non-blocking minor observations. No unresolved HIGH findings are present. |
| 5 | Final branch is clean | PASS | After the gate commit, `git status --porcelain=v1 --branch` reported a clean tree on `deployer/ga-6zig-session-prune...origin/builder/ga-6zig-session-prune [ahead 1]`. |
| 6 | Branch diverges cleanly from main | PASS | After the gate commit, `git merge-tree --write-tree origin/main HEAD` exited 0 with no conflict output. |

## Acceptance Evidence

| Requirement | Evidence |
|-------------|----------|
| `go test ./examples/gastown/...` passes | PASS: package sweep completed in 24.253s. |
| `go vet ./...` clean | PASS: `go vet ./...` exited 0. |
| Manual dry-run succeeds and reports `sessions-pruned:0` | PASS: dry run exited 0 and printed `reaper - stale_wisps:0, closed_wisps:0, purged:0, sessions-pruned:0, closed:0, skipped_non_city_issues:0 (dry run)`. |
| Summary output includes `sessions-pruned:N` | PASS: tests assert `sessions-pruned:7`, `sessions-pruned:3`, `sessions-pruned:1500`, and `sessions-pruned:0`; manual dry run also reported `sessions-pruned:0`. |

## Test Output Summary

```text
bash -n examples/gastown/packs/maintenance/assets/scripts/reaper.sh
PASS

go test ./examples/gastown -run 'TestReaperSessionPruneRunsWhenNoDoltDatabases|TestReaperSessionPrune|TestReaperPrunesClosedSessionBeadsWithBdPrune'
ok  	github.com/gastownhall/gascity/examples/gastown	0.239s

go test ./examples/gastown/...
ok  	github.com/gastownhall/gascity/examples/gastown	24.253s
?   	github.com/gastownhall/gascity/examples/gastown/packs/gastown	[no test files]
?   	github.com/gastownhall/gascity/examples/gastown/packs/maintenance	[no test files]

make test-fast-parallel
All fast jobs passed

go vet ./...
PASS

GC_REAPER_DRY_RUN=1 GC_CITY_PATH=/home/jaword/projects/gc-management examples/gastown/packs/maintenance/assets/scripts/reaper.sh
reaper: reaper - stale_wisps:0, closed_wisps:0, purged:0, sessions-pruned:0, closed:0, skipped_non_city_issues:0 (dry run)
```
