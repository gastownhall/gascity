# Release Gate: bd.slow telemetry on bd-wrapper

Bead: `ga-x4m5zv`
Source bead: `ga-2k9m`
Branch: `builder/ga-2k9m-1`
Commit under review: `ab76170ef`
Gate run: 2026-05-15 22:11 America/Los_Angeles

## Summary

PASS. The reviewed branch adds `bd.slow` telemetry for long-running `bd`
subprocesses, sanitizes sensitive `bd` arguments, and includes focused tests for
slow-call emission, fast-call suppression, and redaction.

`docs/PROJECT_MANIFEST.md` is not present in this worktree, so this gate uses the
release criteria supplied to the deployer role and the repository testing rules in
`TESTING.md`.

## Gate Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `bd show ga-x4m5zv` notes contain `Review Verdict: PASS` from `gascity/reviewer`. |
| 2 | Acceptance criteria met | PASS | Each acceptance criterion is covered by code and tests listed below. |
| 3 | Tests pass | PASS | `make test` completed with `observable go test: PASS`; `go vet ./...` completed cleanly. |
| 4 | No high-severity review findings open | PASS | Review notes list only INFO findings; no HIGH, CRITICAL, FAIL, or request-changes findings are present. |
| 5 | Final branch is clean | PASS | `git status --short --branch` was clean before the gate file was added; final clean status is rechecked after committing this gate. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree $(git merge-base HEAD origin/main) HEAD origin/main` reported no conflicts. `git diff --check origin/main...HEAD` reported no whitespace errors. |

## Acceptance Criteria Evidence

| Acceptance criterion | Result | Evidence |
|---------------------|--------|----------|
| Slow `bd list` emits `bd.slow` at the threshold | PASS | `TestExecCommandRunnerEmitsBDSlowForLongBDCommand` installs a fake `bd` that sleeps past a lowered threshold and asserts a `bd.slow` log record. |
| Fast `bd list` emits no `bd.slow` event | PASS | `TestExecCommandRunnerStopsBDSlowTimerForFastBDCommand` runs a fast fake `bd`, waits past the lowered threshold, and asserts zero `bd.slow` records. |
| Secret args are redacted | PASS | `TestSanitizeBDArgsRedactsSecretFlags`, `TestRecordBDCallSanitizesArgs`, and `TestRecordBDSlowEmitsSanitizedWarnEvent` cover `--flag value` and `--flag=value` redaction without mutating caller args. |
| Fast path adds only timer schedule and stop | PASS | Implementation wires `time.AfterFunc` only for `name == "bd"` and defers `slowTimer.Stop()`; no synchronous telemetry work is added to the successful fast path. |

## Changed Files Reviewed

- `internal/beads/bdstore.go`
- `internal/beads/bdstore_exec_internal_test.go`
- `internal/telemetry/recorder.go`
- `internal/telemetry/recorder_test.go`

## Commands Run

```text
gc hook gascity/deployer
bd show ga-x4m5zv
bd show ga-2k9m
git diff --stat origin/main...HEAD
git diff --check origin/main...HEAD
git merge-tree $(git merge-base HEAD origin/main) HEAD origin/main
make test
go vet ./...
gh pr list --head quad341:builder/ga-2k9m-1 --state all --json number,url,state,title,headRefName,baseRefName
```

## Notes

- No existing PR was found for `quad341:builder/ga-2k9m-1`.
- `origin/main` is not an ancestor of the feature branch, but the branch merges
  cleanly with current `origin/main`; no deployer-side rebase was performed.
