# Release gate: ga-6gi0gn named session pool conflict check

Status: PASS

Evaluated: 2026-05-30

Bead: ga-6gi0gn
Source review bead: ga-xj36nq
Branch: builder/ga-ihrikr.1-named-session-doctor
Reviewed head: 8f1e169ed6445ff9b014015dfaa856c39d13da81

Release criteria source: deployer role prompt. `docs/PROJECT_MANIFEST.md`
was not present in this worktree.

## Gate checklist

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `bd show ga-xj36nq` records reviewer PASS for `builder/ga-ihrikr.1-named-session-doctor` at `8f1e169ed6445ff9b014015dfaa856c39d13da81`. |
| 2 | Acceptance criteria met | PASS | `internal/doctor/checks_named_session.go` adds `NamedAlwaysMinConflictCheck`, warns only for non-suspended agents with `mode="always"` named sessions and `min_active_sessions > 0`, returns advisory severity, and cannot auto-fix. Tests cover no-session, on-demand, zero-minimum, suspended-agent, min=1, min=2, and two-agent cases. |
| 3 | Tests pass | PASS | `go test ./internal/doctor -run TestNamedAlwaysMinConflictCheck -count=1`; `make test-fast-parallel`; `go vet ./...`. |
| 4 | No high-severity review findings open | PASS | Review notes list one INFO finding only; no HIGH findings are open. |
| 5 | Final branch is clean | PASS | `git status --short --branch` was clean before writing this gate file. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` returned tree `0643fc6b184660629f2593db557742fdd128a04e` with no conflicts. |
| 7 | Single feature theme | PASS | The commit set touches one subsystem (`internal/doctor`) for one behavior: detecting named-session/pool-minimum duplicate-session configuration. |

## Test evidence

```text
go test ./internal/doctor -run TestNamedAlwaysMinConflictCheck -count=1
ok  	github.com/gastownhall/gascity/internal/doctor	0.003s

make test-fast-parallel
All fast jobs passed

go vet ./...
PASS
```
