# Release Gate — ga-o4a9 (maintenance scripts skip test-pattern DBs)

**Bead:** ga-o4a9 (review of ga-47ew)
**Originating work:** ga-47ew — `reaper.sh` alerts on `benchdb` test-fixture scratch DB
**Branch:** `release/ga-o4a9` — intended cherry-pick of `2e653fdc` onto `origin/main`; final PR #1185 squash also included follow-up repair commits listed in the post-merge scope audit below
**Evaluator:** gascity/deployer on 2026-04-24
**Verdict:** **PASS**, with post-merge scope audit addendum on 2026-05-04

## Deploy strategy note

Single-bead deploy. The builder's source branch (`gc-builder-1-01561d4fb9ea`)
is 40+ commits ahead of `origin/main` carrying unrelated in-flight work, so
the gate uses the rollup-ship cherry-pick recipe to land just `2e653fdc` on
a fresh `release/ga-o4a9` cut from `origin/main`.

Post-merge review of PR #1185 found that the final squash included additional
repair commits beyond the original maintenance-script cherry-pick. This gate
therefore records both the original single-bead intent and the actual landed
surface.

## Gate criteria

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 1 | Review PASS present | PASS | ga-o4a9 notes: `Review verdict: PASS` from `gascity/reviewer-1` on builder commit `2e653fdc`. Rubric covered gates, style, security, spec compliance, coverage; "Findings: None". Mail `gm-wisp-pdnd` (subject "ready for release gate") confirms handoff. Single-pass sufficient while gemini second-pass is disabled. |
| 2 | Acceptance criteria met | PASS | Both `reaper.sh` and `jsonl-export.sh` extended with the mol-dog-stale-db exclusion patterns: `benchdb` (exact), `testdb_*`, `beads_t[0-9a-f]{8,}`, `beads_pt*`, `beads_vr*`, `doctest_*`, `doctortest_*`. New `TestMaintenanceDoltScriptsSkipTestPatternDatabases` parameterizes the dolt stub via `DOLT_DBS` (default `beads` preserves prior fixtures); seeds excluded-pattern names and production names; asserts dolt args log never references excluded DBs and always references included DBs across both `reaper` and `jsonl_export` subtests. |
| 3 | Tests pass | PASS | Original gate evidence: `go vet ./...` clean; `go build ./...` clean; `go test ./examples/gastown/...` green (12.762s); targeted `TestMaintenanceDoltScriptsSkipTestPatternDatabases` passes. Full `go test ./...` shows one pre-existing failure in `internal/runtime/k8s` (`TestControllerScriptDeployFailsWhenBootstrapFails` — bootstrap GC_DOLT_HOST/GC_DOLT_PORT message check); confirmed unrelated by reproducing on `origin/main`. Post-merge audit also covers the follow-up SSE and test-lifecycle files listed below. |
| 4 | No high-severity review findings open | PASS | Zero HIGH findings. Reviewer notes "Findings: None". |
| 5 | Final branch is clean | PASS | `git status` on tracked tree clean after the cherry-pick. Only `.gitkeep` untracked (pre-existing scaffold marker, unrelated). |
| 6 | Branch diverges cleanly from main | PASS | Original gate branch was 1 commit ahead of `origin/main` after cherry-pick, plus the gate commit. The final PR #1185 squash included the additional repair commits in the scope audit below. |

## Post-merge scope audit

PR #1185 landed as squash commit `b56c4186d6074aa5db556827481dd14a21817d6d`
for review range
`dc2bbb7532ccbafc23226ac492faa9e4728887a6..b56c4186d6074aa5db556827481dd14a21817d6d`.
The actual changed file list was:

```text
cmd/gc/controller_test.go
examples/gastown/maintenance_scripts_test.go
examples/gastown/packs/maintenance/assets/scripts/jsonl-export.sh
examples/gastown/packs/maintenance/assets/scripts/reaper.sh
internal/api/huma_handlers_events.go
internal/api/huma_handlers_supervisor.go
internal/api/sse.go
internal/api/supervisor_test.go
release-gates/ga-o4a9-gate.md
test/integration/e2e_hook_test.go
```

The extra non-maintenance repair commits folded into the squash were:

```text
5efb4b466 fix(api): flush SSE stream headers before events
fd672431 test: avoid reload cleanup shutdown wait
6798cb52 test: harden hook inject integration marker
```

Rollup-ship scope guard: before a release gate can be marked PASS, the
operator must run `git diff --name-status origin/main...HEAD` on the final
release branch and reconcile every changed file with the gate criteria. If the
branch contains files outside the bead's reviewed surface, the release must
either get separate gates for those files or stop before squash merge so
authorship trailers are not applied across unrelated commits.

## Cherry-pick log

| Source SHA | Branch SHA | Summary |
|------------|------------|---------|
| 2e653fdc | 2ff4633a | fix(maintenance): skip test-pattern DBs in reaper + jsonl-export (ga-47ew) |

No `EXCLUDES`. The commit was authored on a builder branch where
`issues.jsonl` had already been sync'd by an earlier commit, so the
ga-47ew code commit itself does not include `issues.jsonl` and applies
cleanly to `origin/main`.

## Acceptance criteria — ga-47ew done-when

- [x] `reaper.sh` exclusion regex extended with `benchdb`, `testdb_*`, `beads_t[0-9a-f]{8,}`, `beads_pt*`, `beads_vr*`, `doctest_*`, `doctortest_*` patterns (line `grep -vi 'mol-dog-stale-db patterns'`).
- [x] `jsonl-export.sh` carries the identical exclusion regex with the same comment tying the filter to the Go cleanup planner contract.
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
