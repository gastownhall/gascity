# Release Gate: ga-12t89g

Status: PASS

Source bead: ga-12t89g
Deploy bead: ga-c5plul
Branch: builder/ga-12t89g-1
Commit: a0f7c00075fe93736c9bfb9b44f6d2129c81ffe3

`docs/PROJECT_MANIFEST.md` is not present in this worktree, so this gate uses
the deployer role's release criteria table plus the repo testing policy in
`TESTING.md`.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `bd show ga-c5plul` contains `REVIEW VERDICT: PASS` for branch `builder/ga-12t89g-1`. |
| 2 | Acceptance criteria met | PASS | The branch implements the named-always heal exception, stamps `runtime-missing`, preserves existing ordinary/pool reset behavior, adds `runtime-missing` lifecycle/freeable handling, and covers the requested cmd/gc and internal/session cases. |
| 3 | Tests pass | PASS | `go test ./cmd/gc -run 'TestHealStatePatch_NamedAlwaysAwakeFlapsToAsleepWithoutReasonOnAliveFalse|TestHealState_ClearsStaleResumeMetadata|TestHealStatePatchProjectsRuntimeLiveness|TestIsPoolSessionSlotFreeable_Matrix' -count=3` passed; `go test ./internal/session -run 'TestProjectLifecycleRuntimeProjection|TestProjectLifecycle|TestLifecycle' -count=3` passed; `make test-fast-parallel` ended with `All fast jobs passed`; `go vet ./...` passed. |
| 4 | No high-severity review findings open | PASS | Review notes list one MEDIUM, one LOW, and one INFO finding; no HIGH or CRITICAL findings are present. |
| 5 | Final branch is clean | PASS | `git status --short --branch` reports `## builder/ga-12t89g-1...fork/builder/ga-12t89g-1` with no file changes. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-base --is-ancestor origin/main HEAD` passed and `git merge-tree $(git merge-base HEAD origin/main) HEAD origin/main` reported no conflicts. |

## Acceptance Evidence

- `cmd/gc/session_reconcile.go` sets `sleep_reason=runtime-missing` when a
  dead-runtime heal path transitions to asleep with empty sleep reason and
  reset-worthy stale continuation metadata.
- `cmd/gc/session_reconcile.go` skips clearing `session_key` and
  `started_config_hash` when the bead is a configured named session in
  `mode=always`.
- `internal/session/lifecycle_projection.go` excludes `runtime-missing` from
  continuation reset decisions, so the next projection tick does not undo the
  preservation.
- `cmd/gc/session_state_helpers.go` treats asleep + `runtime-missing` as a
  freeable pool slot while preserving deny-by-default behavior for missing or
  unknown reasons.
- Regression coverage is present in `cmd/gc/session_reconcile_test.go`,
  `cmd/gc/session_state_helpers_test.go`, and
  `internal/session/lifecycle_projection_test.go`.
