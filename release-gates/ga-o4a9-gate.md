# Release Gate — ga-o4a9 (maintenance scripts skip test-pattern DBs)

**Bead:** ga-o4a9 (review of ga-47ew)
**Originating work:** ga-47ew — `reaper.sh` alerts on `benchdb` test-fixture scratch DB
**Branch:** `release/ga-o4a9` — cherry-pick of `2e653fdc` onto `origin/main`
**Evaluator:** gascity/deployer on 2026-04-24
**Verdict:** **PASS**

## Deploy strategy note

Single-bead deploy. The builder's source branch (`gc-builder-1-01561d4fb9ea`)
is 40+ commits ahead of `origin/main` carrying unrelated in-flight work, so
the gate uses the rollup-ship cherry-pick recipe to land just `2e653fdc` on
a fresh `release/ga-o4a9` cut from `origin/main`. No `EXCLUDES` needed — the
commit only touches `examples/gastown/maintenance_scripts_test.go` and the
two shell scripts.

## Gate criteria

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 1 | Review PASS present | PASS | ga-o4a9 notes: `Review verdict: PASS` from `gascity/reviewer-1` on builder commit `2e653fdc`. Rubric covered gates, style, security, spec compliance, coverage; "Findings: None". Mail `gm-wisp-pdnd` (subject "ready for release gate") confirms handoff. Single-pass sufficient while gemini second-pass is disabled. |
| 2 | Acceptance criteria met | PASS | Both `reaper.sh` and `jsonl-export.sh` extended with the canonical mol-dog-stale-db exclusion patterns: `benchdb` (exact), `testdb_*`, `beads_t*`, `beads_pt*`, `beads_vr*`, `doctest_*`, `doctortest_*`. Grep style `-vi` matches the existing exclusion line (BRE alternation). New `TestMaintenanceDoltScriptsSkipTestPatternDatabases` parameterizes the dolt stub via `DOLT_DBS` (default `beads` preserves prior fixtures); seeds 7 excluded-pattern names + 2 production names; asserts dolt args log never references excluded DBs and always references included DBs across both `reaper` and `jsonl_export` subtests. |
| 3 | Tests pass | PASS | `go vet ./...` clean. `go build ./...` clean. `go test ./examples/gastown/...` green (12.762s). Targeted `TestMaintenanceDoltScriptsSkipTestPatternDatabases` passes. Full `go test ./...` shows one pre-existing failure in `internal/runtime/k8s` (`TestControllerScriptDeployFailsWhenBootstrapFails` — bootstrap GC_DOLT_HOST/GC_DOLT_PORT message check); confirmed unrelated to this change by reproducing on `origin/main` code. The change touches only shell scripts under `packs/maintenance/assets/scripts/` and the maintenance test file — no path of code reachable from the failing k8s test. |
| 4 | No high-severity review findings open | PASS | Zero HIGH findings. Reviewer notes "Findings: None". |
| 5 | Final branch is clean | PASS | `git status` on tracked tree clean after the cherry-pick. Only `.gitkeep` untracked (pre-existing scaffold marker, unrelated). |
| 6 | Branch diverges cleanly from main | PASS | 1 commit ahead of `origin/main` after cherry-pick (plus the gate commit once added). Cherry-pick of `2e653fdc` applied with no conflicts. |

## Cherry-pick log

| Source SHA | Branch SHA | Summary |
|------------|------------|---------|
| 2e653fdc | 2ff4633a | fix(maintenance): skip test-pattern DBs in reaper + jsonl-export (ga-47ew) |

No `EXCLUDES`. The commit was authored on a builder branch where
`issues.jsonl` had already been sync'd by an earlier commit, so the
ga-47ew code commit itself does not include `issues.jsonl` and applies
cleanly to `origin/main`.

## Acceptance criteria — ga-47ew done-when

- [x] `reaper.sh` exclusion regex extended with `benchdb`, `testdb_*`, `beads_t*`, `beads_pt*`, `beads_vr*`, `doctest_*`, `doctortest_*` patterns (line `grep -vi 'mol-dog-stale-db patterns'`).
- [x] `jsonl-export.sh` carries the identical exclusion regex with the same comment citing `mol-dog-stale-db`.
- [x] No other maintenance script under `packs/maintenance/assets/scripts/` uses a `SHOW DATABASES` → exclusion-grep pipeline (verified by reviewer; both files cover the surface).
- [x] `TestMaintenanceDoltScriptsSkipTestPatternDatabases` added to `examples/gastown/maintenance_scripts_test.go` covering both `reaper` and `jsonl_export` subtests; default-`beads` `DOLT_DBS` preserves existing test behavior.
- [x] Hardcoded patterns (not env var) — matches existing exclusion style; avoids premature flexibility per the builder plan.

## Test evidence

```
$ go vet ./...
(clean)

$ go build ./...
(clean)

$ go test -run TestMaintenanceDoltScriptsSkipTestPatternDatabases ./examples/gastown/...
ok   github.com/gastownhall/gascity/examples/gastown   0.113s

$ go test ./examples/gastown/...
ok   github.com/gastownhall/gascity/examples/gastown   12.762s
?    github.com/gastownhall/gascity/examples/gastown/packs/gastown      [no test files]
?    github.com/gastownhall/gascity/examples/gastown/packs/maintenance  [no test files]

$ go test ./...
(all green except pre-existing FAIL in internal/runtime/k8s
 TestControllerScriptDeployFailsWhenBootstrapFails — reproduced on
 origin/main; unrelated to this shell-script-only change)
```

## Pre-existing failure (not a deploy blocker)

`internal/runtime/k8s.TestControllerScriptDeployFailsWhenBootstrapFails`
fails on `origin/main` with the same assertion error
(`deploy output did not report bootstrap failure: controller bootstrap
requires both GC_DOLT_HOST and GC_DOLT_PORT when either is set`). This
is a controller-script bootstrap-error-message regression unrelated to
the maintenance-script exclusion work. Worth a separate bead if not
already tracked.
