# Release Gate: REASON priority coverage

- Adopted PR: `gastownhall/gascity#2544`
- Workflow root: `ga-mw3g4`
- Source bead: `ga-chg2`
- Branch worktree: `/data/projects/gascity/worktrees/ga-chg2`
- Reviewed commit: this follow-up maintainer fix commit
- Base checked: `refs/adopt-pr/ga-chg2/latest-base`
- Review attempt: `2`
- Final merge surface: follow-up branch from current `main`; original PR #2544 is already merged.

## Gate Criteria

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review findings addressed | PASS | Attempt 1 raised one major architectural consistency finding: CLI-only `circuit-open` REASON derivation could diverge from API/dashboard session projection. The fix moves the circuit metadata key, durable state values, and `circuit-open` display reason into `internal/session` and routes `gc session list` through `session.LifecycleDisplayReasonWithLiveness`. |
| 2 | Acceptance criteria met | PASS | The priority stack is now reset-pending, circuit-open, lifecycle sleep/hold/quarantine reasons, then wake/config fallback. `cmd/gc/cmd_session_test.go` covers the CLI matrix, `internal/session/lifecycle_projection_test.go` covers the shared projection, and `internal/api/handler_sessions_test.go` covers the HTTP session list projection. |
| 3 | Tests pass | PASS | Focused verification passed for the session projection fix on the follow-up branch based on current `main`. |
| 4 | No high-severity review findings open | PASS | The major CLI/API projection drift finding is fixed. The stale release-gate evidence finding is fixed by this refreshed artifact. |
| 5 | Final branch state is explicit | PASS | The original contributor commits are already merged by PR #2544; this follow-up branch carries only the maintainer fixup needed after adoption review. |
| 6 | Diff hygiene checked | PASS | `git diff --check` is part of the pre-commit validation set for the final maintainer fixup. |

## Acceptance Evidence

| Source criterion | Result | Evidence |
|---|---|---|
| Cover reset-pending priority. | PASS | `TestSessionReason_ResetPendingLiveRuntimeOverridesOtherReasons`, `TestSessionReason_PriorityMatrix/reset-pending beats circuit open and sleep`, and `TestLifecycleDisplayReasonWithLivenessShowsResetPending` assert `restart_requested=true` on a live runtime displays `reset-pending` ahead of lower-priority reasons. |
| Cover circuit-open priority. | PASS | `TestSessionReason_CircuitOpenMetadataVisible`, `TestSessionReason_PriorityMatrix/circuit-open beats sleep reason`, `TestLifecycleDisplayReasonUsesOnlyActiveLifecycleReasons/circuit open wins`, `TestLifecycleDisplayReasonWithLivenessShowsCircuitOpenBeforeLifecycleReason`, and `TestHandleSessionListShowsCircuitOpenReason` assert `session_circuit_state=CIRCUIT_OPEN` displays `circuit-open` through the shared session/API projection path. |
| Cover `sleep_reason` fallback before wake/config reasons. | PASS | `TestSessionReason_SleepReasonOverridesWakeReason`, `TestSessionReason_PriorityMatrix/sleep reason beats wake config`, and `TestHandleSessionListIncludesReason` assert lifecycle display reasons win before wake reasons. |
| Cover wake/config fallback and no-config fallback. | PASS | `TestSessionReason_PriorityMatrix/wake config falls through after blocking states` and `TestSessionReason_PriorityMatrix/no config fallback remains empty reason` cover both tail cases. |
| Avoid hardcoded production roles. | PASS | Production code adds generic session projection constants and reason derivation only. Test fixture names such as `worker` are local data, not production role-conditioned logic. |
| Preserve API metadata redaction. | PASS | The API still redacts `session_circuit_state`; only the typed `reason` field exposes `circuit-open`. |

## Validation

| Command | Result |
|---|---|
| `go test ./internal/session -run 'TestLifecycleDisplayReason' -count=1` | PASS |
| `go test ./internal/api -run 'TestHandleSessionListShows(CircuitOpenReason\|ResetPendingForLiveRuntime\|IncludesReason)' -count=1` | PASS |
| `go test ./cmd/gc -run TestSessionReason -count=1` | PASS |
| `git diff --check refs/adopt-pr/ga-chg2/latest-base..HEAD` | PASS |

## Changed Files

- `cmd/gc/cmd_session.go`
- `cmd/gc/session_circuit_breaker.go`
- `internal/api/handler_sessions_test.go`
- `internal/session/lifecycle_projection.go`
- `internal/session/lifecycle_projection_test.go`
- `release-gates/ga-wzx5d-reason-priority-coverage-gate.md`
