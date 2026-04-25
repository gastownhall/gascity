# Release gate — dolt 1.86.2 version floor (ga-iwec + ga-kmb4)

**Verdict:** PASS

Branch: `release/ga-iwec-dolt-1862-floor` (cut fresh off `origin/main` @ `73f52d59`)
Commits (rebased SHAs on this branch):
- `2a498497` — feat(dolt): require dolt >= 1.86.2 in pack guards (ga-iwec)
- `ed7ba1ea` — test(dolt/doctor): cover dolt 1.86.2 version-floor + missing prereqs (ga-kmb4)

Source SHAs on `tracking/ga-iwec-rebased` (fork): `31dc90d4`, `bd9050b5`. Cherry-picked clean (zero conflicts).

Diff vs `origin/main`: +313 lines across 4 files (run.sh, doctor_test.go, mol-dog-backup.toml, pack.toml).

## Review beads bundled in this PR

| Review bead | Reviews            | Verdict | Reviewer            |
|-------------|--------------------|---------|---------------------|
| ga-zguq     | ga-iwec (run.sh + mol-dog-backup.toml + pack.toml) | PASS | gascity/reviewer-1 |
| ga-245m     | ga-iwec (same surface, second review pass)         | PASS | gascity/reviewer-1 |
| ga-57v7     | ga-kmb4 (examples/dolt/doctor_test.go)             | PASS | gascity/reviewer-1 |

## Criteria

| # | Criterion                                  | Verdict | Evidence                                                                                       |
|---|--------------------------------------------|---------|------------------------------------------------------------------------------------------------|
| 1 | Review PASS present                        | PASS    | All three review beads carry an explicit reviewer-1 PASS verdict (see notes on each bead).     |
| 2 | Acceptance criteria met                    | PASS    | run.sh rejects dolt <1.86.2 with upstream-commit explainer; mol-dog-backup.toml has `preflight` step before `sync` (`sync.needs = ["preflight"]`); pack.toml notes 1.86.2 floor; doctor_test.go covers all 9 documented branches. |
| 3 | Tests pass                                 | PASS    | `go test -count=1 ./examples/dolt/... ./internal/formula/...` → ok 11.0s + 0.04s. Full `./...` has 4 failures in `internal/runtime/k8s` (`TestControllerScriptDeploy*`) — verified pre-existing on `origin/main` @ `73f52d59`, unrelated to dolt floor (errors mention `GC_DOLT_HOST`/`GC_DOLT_PORT` controller bootstrap, not pack guards). |
| 4 | No high-severity review findings open      | PASS    | All three review beads list "Findings: None blocking." Informational notes about empty-version fall-through were addressed by the rebase (`unrecognized dolt version output` exit-1 path now lives upstream in run.sh on main; ga-kmb4 sandbox tracks it). |
| 5 | Final branch is clean                      | PASS    | `git status` clean (only this gate file untracked at write time).                              |
| 6 | Branch diverges cleanly from main          | PASS    | `git cherry-pick 31dc90d4 bd9050b5` applied cleanly with zero conflicts onto fresh branch off `origin/main`. The previous run.sh conflict from `c5128407` is resolved by builder's rebase commit `31dc90d4`. |

## Build / vet

- `go build ./...` → clean
- `go vet ./...` → clean

## Pre-existing failures (not introduced)

`internal/runtime/k8s` controller-script tests fail on baseline:
- `TestControllerScriptDeployUsesResolvedConfigPrefixesForBootstrap`
- `TestControllerScriptDeployBootstrapsAfterStartSignalAndLogProbe`
- `TestControllerScriptDeployBootstrapsWhenLogsNeverMatch`
- `TestControllerScriptDeployFailsWhenBootstrapFails`

Cause is unrelated controller bootstrap env-var validation (`GC_DOLT_HOST`/`GC_DOLT_PORT`). Not in scope for this PR.

## Push target

`fork` (quad341/gascity) — `origin` (gastownhall/gascity) is read-only from this rig (verified via `git push --dry-run origin HEAD` → 403).
PR cross-repo: `--head quad341:release/ga-iwec-dolt-1862-floor --base main`.
