# Release Gate: ga-gsljgf suspend/resume API events

Bead: ga-gsljgf
Source bead: ga-4qo2xw
Reviewed commits:
- c52113665 fix(api): emit city.suspended and city.resumed events via API path
- a0370f05c test(cmd/gc): cover city suspension event emission

Branch gated: release/ga-gsljgf-suspend-resume-events
Reviewed commit tip: a0370f05c

Note: the original feature branch `fix/ga-ttpt2-suspend-resume-events` has advanced to 5f592af1b after review. This gate uses a release branch cut at the reviewed commit tip so unreviewed branch-tip changes are not included.

## Gate Checklist

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead ga-4qo2xw is closed with `Review: PASS` and explicitly lists both reviewed commits. Earlier deploy bead ga-vscud9 covered c52113665 only and is superseded by this two-commit review. |
| 2 | Acceptance criteria met | PASS | `cmd/gc/api_state.go` emits `events.CitySuspended` after successful `SuspendCity()` mutation and `events.CityResumed` after successful `ResumeCity()` mutation. Both events use `Actor: "gc"` and existing `events.Event` envelope timestamps. `cmd/gc/api_state_test.go` verifies suspend/resume config mutation plus exactly one emitted event of the expected type. Event types are present in `events.KnownEventTypes` and registered with `events.NoPayload` in `internal/api/event_payloads.go`; this change does not add a reason payload. |
| 3 | Tests pass | PASS | `go test ./cmd/gc -run 'TestControllerStateCitySuspensionRecordsEvents\|TestControllerStateMutationsPokeController\|TestSuspendResume'` PASS. `go test ./internal/api -run TestEveryKnownEventTypeHasRegisteredPayload -count=1` PASS. `make test-fast-parallel` PASS. `go vet ./...` PASS. |
| 4 | No high-severity review findings open | PASS | Reviewer notes for ga-4qo2xw report no blockers and no security concerns. No unresolved HIGH findings are recorded in the deploy or review bead notes. |
| 5 | Final branch is clean | PASS | Gate branch was clean before writing this checklist; the only deployer-authored change is this gate file, committed as the branch tip before PR creation. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree $(git merge-base origin/main HEAD) origin/main HEAD` produced no conflicts for the reviewed commits. |
| 7 | Single feature theme | PASS | The commit set touches one subsystem and behavior: `cmd/gc` controller state suspend/resume event emission and its regression test. |

## Commands Run

```text
go test ./cmd/gc -run 'TestControllerStateCitySuspensionRecordsEvents|TestControllerStateMutationsPokeController|TestSuspendResume'
go test ./internal/api -run TestEveryKnownEventTypeHasRegisteredPayload -count=1
make test-fast-parallel
go vet ./...
```
