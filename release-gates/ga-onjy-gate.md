# Release Gate — ga-onjy (clear inherited GC_BEADS env in test config writers)

**Bead:** ga-onjy (review of ga-y64o)
**Originating work:** ga-y64o — tests inherit `GC_BEADS=bd` from agent env, leaking orphan `dolt sql-server` processes
**Branch:** `release/ga-onjy` — cherry-pick of `e16ccf1f` onto `origin/main`
**Evaluator:** gascity/deployer on 2026-04-24
**Verdict:** **PASS**

## Deploy strategy note

Single-bead deploy. The builder's source branch (`gc-builder-1-01561d4fb9ea`)
carries unrelated in-flight work ahead of `origin/main`, so the gate uses the
rollup-ship cherry-pick recipe to land just `e16ccf1f` on a fresh
`release/ga-onjy` cut from `origin/main`. No `EXCLUDES` needed — the commit
only touches three test files in `cmd/gc/`.

## Gate criteria

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 1 | Review PASS present | PASS | ga-onjy notes: `Reviewer verdict: PASS` from `gascity/reviewer-1` on builder commit `e16ccf1f`. Rubric covered style, security (OWASP), spec compliance, coverage; "Findings: None". Mail `gm-wisp-syrr` (subject "ready for release gate") confirms handoff. Single-pass sufficient while gemini second-pass is disabled. |
| 2 | Acceptance criteria met | PASS | `clearInheritedBeadsEnv(t)` helper added to `cmd/gc/path_helpers_test.go:22-45` — name, comment, 11-key env list, and loop body match the investigator spec byte-for-byte. All five required call sites updated: `writeCityRuntimeConfig`, `writeCityRuntimeConfigNamed`, `writeCityRuntimeConfigWithIncludes` in `cmd/gc/city_runtime_test.go`; `writeCityTOML`, `writeControllerNamedSessionCityTOML` in `cmd/gc/controller_test.go` (verified by `grep -rn 'clearInheritedBeadsEnv(t)' cmd/gc` → 5 hits). No new tests added — existing `TestCityRuntimeReload\|TestControllerReloads` regress-test the leak via pgrep delta. "Do not touch" list (`cr.shutdown`, `configuredBeadsProviderValue`, `gc-beads-bd.sh`) honored. |
| 3 | Tests pass | PASS | `go vet ./...` clean. Targeted `go test ./cmd/gc/ -run 'TestCityRuntimeReload\|TestControllerReloads' -count=1` passes (0.309s) with `pgrep -af 'dolt sql-server.*--config /tmp/'` delta = 0 (19 → 19). Broader `cmd/gc/` suite shows 4 pre-existing failures unrelated to this change: `TestOpenStoreAtForCityExecProjectsConfiguredTargets`, `TestOpenStoreAtForCityExecBeadsBdProjectsScopedExternalDoltEnv`, `TestOpenStoreAtForCityExecUsesUniversalStoreTargetEnv`, `TestControllerQueryRuntimeEnvReturnsNilForNonBD`. Reproduced byte-for-byte on `origin/main` baseline (deployer re-ran with `git checkout origin/main -- cmd/gc/`); same four FAILs with identical messages. None of the 4 touch the env-clearing surface in this change. |
| 4 | No high-severity review findings open | PASS | Zero HIGH findings. Reviewer notes "Findings: None". |
| 5 | Final branch is clean | PASS | `git status` on tracked tree clean after the cherry-pick. Only `.gitkeep` untracked (pre-existing scaffold marker, unrelated). |
| 6 | Branch diverges cleanly from main | PASS | 1 commit ahead of `origin/main` after cherry-pick (plus this gate commit once added). Cherry-pick of `e16ccf1f` applied with auto-merge of `cmd/gc/city_runtime_test.go`, no conflicts. |

## Cherry-pick log

| Source SHA | Branch SHA | Summary |
|------------|------------|---------|
| e16ccf1f | 97dee9e7 | test(cmd/gc): clear inherited GC_BEADS/dolt env in city.toml writers (ga-y64o) |

No `EXCLUDES`. The commit touches only three test files; `issues.jsonl` is not in the diff.

## Acceptance criteria — ga-y64o done-when

- [x] `clearInheritedBeadsEnv` helper exists in a `_test.go` file in `cmd/gc/` (`cmd/gc/path_helpers_test.go:22-45`).
- [x] All five test-config helpers call `clearInheritedBeadsEnv(t)` (verified via grep, 5 call sites).
- [x] From an agent session with `GC_BEADS=bd` set, `go test ./cmd/gc/ -run "TestCityRuntimeReload|TestControllerReloads" -count=1` passes AND `pgrep -af 'dolt sql-server.*--config /tmp/'` count does not grow during the run (deployer measured 19 → 19).
- [x] No new tests added unless they reproduce the leak — diff is +29/-0 across 3 files, no new `Test*` functions.
- [x] `go vet ./...` clean. `go test ./cmd/gc/...` passes for everything not pre-existing-broken.

## Test evidence

```
$ go vet ./...
(clean)

$ pgrep -af 'dolt sql-server.*--config /tmp/' | wc -l
19

$ go test ./cmd/gc/ -run 'TestCityRuntimeReload|TestControllerReloads' -count=1 -timeout 120s
ok   github.com/gastownhall/gascity/cmd/gc   0.309s

$ pgrep -af 'dolt sql-server.*--config /tmp/' | wc -l
19

$ go test ./cmd/gc/ -run 'TestCityRuntime|TestController|TestOpenStoreAt|TestPathHelpers' -count=1 -timeout 300s
--- FAIL: TestOpenStoreAtForCityExecProjectsConfiguredTargets         (pre-existing on origin/main)
--- FAIL: TestOpenStoreAtForCityExecBeadsBdProjectsScopedExternalDoltEnv (pre-existing on origin/main)
--- FAIL: TestOpenStoreAtForCityExecUsesUniversalStoreTargetEnv       (pre-existing on origin/main)
--- FAIL: TestControllerQueryRuntimeEnvReturnsNilForNonBD             (pre-existing on origin/main)
FAIL   github.com/gastownhall/gascity/cmd/gc   110.166s

$ git checkout origin/main -- cmd/gc/   # baseline check
$ go test ./cmd/gc/ -run '<the 4 above>' -count=1 -timeout 120s
--- FAIL: ... (same 4 fail identically — confirmed pre-existing)
```

## Pre-existing failures (not deploy blockers)

The 4 failures listed above reproduce on `origin/main` baseline with byte-for-byte
identical assertion messages. They concern store-target env file resolution
(`store_target_exec_test.go`) and a controller-runtime-env probe
(`work_query_probe_test.go:172`) — none touch `GC_BEADS` env clearing or the
test-config writers modified by ga-y64o. Worth separate beads if not already
tracked.
