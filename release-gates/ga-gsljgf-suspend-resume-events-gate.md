# Release Gate: suspend/resume API events

Deploy beads:
- ga-gsljgf
- ga-mar94u

Source review beads:
- ga-4qo2xw
- ga-q6l40u

Reviewed source commits:
- c52113665 fix(api): emit city.suspended and city.resumed events via API path
- a0370f05c test(cmd/gc): cover city suspension event emission
- 5f592af1b test(cmd/gc): assert city suspension event actor

Release branch commits:
- c52113665 fix(api): emit city.suspended and city.resumed events via API path
- a0370f05c test(cmd/gc): cover city suspension event emission
- 616954f26 test(cmd/gc): assert city suspension event actor

Branch gated: release/ga-gsljgf-suspend-resume-events
Reviewed feature tip: 5f592af1b

Note: the original feature branch `fix/ga-ttpt2-suspend-resume-events` advanced after the first deploy bead. The follow-up actor assertion was reviewed under ga-q6l40u and is included here as a cherry-pick on the existing release branch.

## Gate Checklist

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead ga-4qo2xw is closed with `Review: PASS` for c52113665 and a0370f05c. Review bead ga-q6l40u is closed with `REVIEW VERDICT: pass` for 5f592af1b. Earlier deploy bead ga-vscud9 covered c52113665 only and is superseded by this PR. |
| 2 | Acceptance criteria met | PASS | `cmd/gc/api_state.go` emits `events.CitySuspended` after successful `SuspendCity()` mutation and `events.CityResumed` after successful `ResumeCity()` mutation. Both events use `Actor: "gc"` and existing `events.Event` envelope timestamps. `cmd/gc/api_state_test.go` verifies suspend/resume config mutation, exactly one emitted event of the expected type, and the `gc` actor for both operations. Event types are present in `events.KnownEventTypes` and registered with `events.NoPayload` in `internal/api/event_payloads.go`; this change does not add a reason payload. |
| 3 | Tests pass | PASS | `go test ./cmd/gc -run 'TestControllerStateCitySuspensionRecordsEvents\|TestControllerStateMutationsPokeController\|TestSuspendResume'` PASS. `go test ./internal/api -run TestEveryKnownEventTypeHasRegisteredPayload -count=1` PASS. `make test-fast-parallel` PASS. `go vet ./...` PASS. |
| 4 | No high-severity review findings open | PASS | Reviewer notes for ga-4qo2xw and ga-q6l40u report no blockers, no findings, and no security concerns. No unresolved HIGH findings are recorded in the deploy or review bead notes. |
| 5 | Final branch is clean | PASS | Gate branch was clean before updating this checklist; the only deployer-authored changes are gate file commits, committed before PR update. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree $(git merge-base origin/main HEAD) origin/main HEAD` produced no conflicts for the reviewed commits. |
| 7 | Single feature theme | PASS | The commit set touches one subsystem and behavior: `cmd/gc` controller state suspend/resume event emission and its regression test. |

## Commands Run

```text
go test ./cmd/gc -run 'TestControllerStateCitySuspensionRecordsEvents|TestControllerStateMutationsPokeController|TestSuspendResume'
go test ./internal/api -run TestEveryKnownEventTypeHasRegisteredPayload -count=1
make test-fast-parallel
go vet ./...
```
