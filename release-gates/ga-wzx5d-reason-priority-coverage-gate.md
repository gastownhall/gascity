# Release Gate: REASON priority coverage

- Deploy bead: `ga-wzx5d`
- Source bead: `ga-4arrr.1.3`
- Branch: `builder/ga-4arrr-1-2`
- Remote branch: `origin/builder/ga-4arrr-1-2`
- Reviewed commit: `ed0660200 test(gc): cover session reason priority matrix`
- Base checked: `origin/main`
- Stack note: this branch builds on open PR #2538 (`builder/ga-k0n20-1-3`) for the reset-pending REASON display. PR #2538 is clean with CI green at gate time.
- Manifest note: `docs/PROJECT_MANIFEST.md` is not present in this checkout, so this gate uses the deployer role criteria plus the source bead acceptance checklist.

## Gate Criteria

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | `bd show ga-wzx5d` notes include `Review verdict: PASS` from `gascity/reviewer` at `ed0660200`. |
| 2 | Acceptance criteria met | PASS | The source bead requested coverage for reset-pending, circuit-open, `sleep_reason`, wake/config fallback, and reset-pending-over-circuit-open conflict. `cmd/gc/cmd_session_test.go` now has `TestSessionReason_PriorityMatrix` plus focused tests for reset-pending, circuit-open, and sleep-reason priority. |
| 3 | Tests pass | PASS | `go test ./cmd/gc -run 'TestSessionReason' -count=1` passed; `go vet ./...` passed; `make test-fast-parallel` completed with `All fast jobs passed`. |
| 4 | No high-severity review findings open | PASS | Reviewer findings list is empty. The only note is a non-blocking observation about a defensive impossible-state test value. |
| 5 | Final branch is clean | PASS | `git status --short --branch` was clean before gate creation; deployer will recheck after committing this gate file before push. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-base --is-ancestor origin/main HEAD` exited 0; `git merge-tree` reported `merged`; `git diff --check origin/main...HEAD` exited 0. |

## Acceptance Evidence

| Source criterion | Result | Evidence |
|---|---|---|
| Cover reset-pending priority. | PASS | `TestSessionReason_ResetPendingLiveRuntimeOverridesOtherReasons` and `TestSessionReason_PriorityMatrix/reset-pending beats circuit-open and sleep_reason` assert `restart_requested=true` on a live runtime displays `reset-pending` ahead of lower-priority reasons. |
| Cover circuit-open priority. | PASS | `TestSessionReason_CircuitOpenMetadataVisible` and `TestSessionReason_PriorityMatrix/circuit-open beats sleep_reason` assert `session_circuit_state=CIRCUIT_OPEN` displays `circuit-open`. |
| Cover `sleep_reason` fallback before wake/config reasons. | PASS | `TestSessionReason_SleepReasonOverridesWakeReason` and `TestSessionReason_PriorityMatrix/sleep_reason beats wake config` assert lifecycle display reasons win before wake reasons. |
| Cover wake/config fallback and no-config fallback. | PASS | `TestSessionReason_PriorityMatrix/wake config remains fallback` and `TestSessionReason_PriorityMatrix/no config and no blocking reason falls back to dash` cover both tail cases. |
| Avoid hardcoded production roles. | PASS | The production code adds reason helpers only. Tests use `config.Agent{Name: "worker"}` as local fixture data, not production role-conditioned logic. |
| Affected package tests pass. | PASS | `go test ./cmd/gc -run 'TestSessionReason' -count=1` passed in `0.404s`. |

## Validation

| Command | Result |
|---|---|
| `go test ./cmd/gc -run 'TestSessionReason' -count=1` | PASS |
| `go vet ./...` | PASS |
| `make test-fast-parallel` | PASS (`fsys-darwin-compile`, `unit-core`, and `unit-cmd-gc` shards 1-6 passed; `All fast jobs passed`) |
| `git diff --check origin/main...HEAD` | PASS |

## Changed Files

- `cmd/gc/cmd_session.go`
- `cmd/gc/cmd_session_test.go`
- `release-gates/ga-oeqam-reset-pending-session-reason-display-gate.md` (stack prerequisite from PR #2538)
