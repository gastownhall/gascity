# Release Gate: ga-wm69vw

Bead: `ga-wm69vw` - Review: live-session SHOW PROCESSLIST probe + fail-closed (`ga-9h05hk` slice 2/5)

Branch: `builder/ga-9h05hk-1`

Base checked: `origin/main` at `2a1185fc6b1c364cb9beb70dfd64fb042ef9d920`

Head checked before gate commit: `e40471397f70044024db76aea0ea19b28f002bf6`

Project manifest note: `docs/PROJECT_MANIFEST.md` is not present in this repository or the city management repo. This gate applies the deployer prompt's six release criteria plus the repo guidance in `TESTING.md`.

## Gate Summary

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `ga-wm69vw` notes contain reviewer verdict `PASS - routing to gascity/deployer`. Review findings are informational only. |
| 2 | Acceptance criteria met | PASS | Source bead `ga-9h05hk` is closed. Pinned SQL, timeout value, skip reason, force-blocker kind, interface method, production probe, pure planner helper, fatal blocker helper, run-stage wiring, and all five test names are present. Documented deviations were reviewer-approved: timeout is a test-overridable `var`, two compile-only fake methods were added, and two `internal/api` lint fixes are included. |
| 3 | Tests pass | PASS | Focused cleanup tests, `internal/api`, vet, build, dashboard check, and the sharded fast baseline passed. See test evidence below. |
| 4 | No high-severity review findings open | PASS | Review notes list only `info` observations; no HIGH or request-changes findings are open. |
| 5 | Final branch is clean | PASS | `git status --short --branch` showed only `## builder/ga-9h05hk-1...fork/builder/ga-9h05hk-1` before adding this gate file. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree HEAD origin/main` exited 0 and produced tree `12171949ef15327b233f0d8b6114e1f2476d60cf`. Diff against `origin/main...HEAD` is the expected 10 files / 774 insertions / 4 deletions. |

## Acceptance Evidence

- `cleanupLiveSessionProbeQuery` exists in `cmd/gc/dolt_cleanup_drop.go` and matches the pinned SQL string exactly.
- `cleanupLiveSessionProbeTimeout` is `2 * time.Second`; reviewer accepted `var` instead of `const` so the timeout test can run quickly.
- `DropSkipReasonLiveSession = "live-session"` exists in `cmd/gc/dolt_cleanup_drop_planner.go`.
- `cleanupErrorKindLiveSessionProbeFailed = "live-session-probe-failed"` exists in `cmd/gc/cmd_dolt_cleanup.go`.
- `CleanupDoltClient` includes `ProbeLiveSessions(ctx context.Context) (map[string]int, error)`.
- `*sqlCleanupDoltClient.ProbeLiveSessions` issues the pinned query and returns `map[string]int`.
- `applyLiveSessionsToPlan` is pure and rewrites `ToDrop` into `Skipped[reason=live-session]` in input order.
- `hasFatalForceBlocker` gates the `--force` exit-1 path for live-session probe failure.
- `runDropStage` probes after `planDoltDrops` and before dry-run/drop branches.
- All five pinned tests exist and pass:
  - `TestProbeLiveSessions_HealthyServer`
  - `TestProbeLiveSessions_TimesOut`
  - `TestProbeLiveSessions_FailClosed`
  - `TestProbeLiveSessions_DryRunSurvivesFailure`
  - `TestProbeLiveSessions_RemovesFromToDrop`
- Out-of-scope surfaces remain absent: no `CleanupReport.LiveSessions`, no audit-log writes, no bash forwarder changes, no human-rendering special case, and no slice-5 regression test.

## Test Evidence

| Command | Result | Notes |
|---------|--------|-------|
| `go test ./cmd/gc -run 'TestProbeLiveSessions|TestRunDoltCleanup|TestPlanDoltDrops' -count=1` | PASS | `ok github.com/gastownhall/gascity/cmd/gc 1.986s` |
| `go test ./internal/api/... -count=1` | PASS | `internal/api` and `internal/api/genclient` passed. |
| `go vet ./...` | PASS | No output. |
| `go build ./...` | PASS | No output. |
| `make dashboard-check` | PASS | OpenAPI TS generation, Vite build, typecheck, and dashboard package tests passed; no generated-file drift. |
| `make test-fast-parallel` | PASS | All fast jobs passed: `unit-core`, `unit-cmd-gc-1-of-6` through `unit-cmd-gc-6-of-6`, and `fsys-darwin-compile`. |

Diagnostic note: a raw non-sharded `go test -timeout=240s ./cmd/gc/... ./internal/api/... -count=1` timed out in `TestCmdStopMarginExhaustion`. The isolated test passed immediately with `go test ./cmd/gc -run '^TestCmdStopMarginExhaustion$' -count=1 -v` (`PASS`, 1.59s). `TESTING.md` directs broad local sweeps to `make test-fast-parallel`, which passed.

## Branch Evidence

- `git diff --name-only origin/main...HEAD`:
  - `cmd/gc/cmd_dolt_cleanup.go`
  - `cmd/gc/dolt_cleanup_drop.go`
  - `cmd/gc/dolt_cleanup_drop_planner.go`
  - `cmd/gc/dolt_cleanup_drop_test.go`
  - `cmd/gc/dolt_cleanup_human_test.go`
  - `cmd/gc/dolt_cleanup_purge_test.go`
  - `engdocs/plans/dolt-cleanup-live-session-probe.md`
  - `engdocs/plans/dolt-port-resolve-helper.md`
  - `internal/api/client.go`
  - `internal/api/response_cache.go`

- Commits before this gate:
  - `0b4ab5b6f` docs(plans): decompose ga-u0lx9p + ga-lyv6d4 into builder beads (ga-rq2e5a, ga-9h05hk)
  - `d2e707aa9` fix(lint): inline reflect.Ptr -> reflect.Pointer (govet)
  - `e40471397` feat(dolt-cleanup): live-session SHOW PROCESSLIST probe + fail-closed (ga-nw4z6 slice 2/5)

## Result

PASS. The branch is ready for PR creation.
