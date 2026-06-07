# Release Gate: ga-nhwmeb

## Summary

- Bead: `ga-nhwmeb` — needs-deploy: fix timing test flakes in api_state_test.go
- Source bead: `ga-ca05s0`
- Reviewed commit: `388d432e89552f6dff2fd04e6335107fd3ae2a6e`
- Source branch: `builder/ga-0idf2l-fix-timing-test-flakes`
- Scope: test-only timeout bump in `cmd/gc/api_state_test.go`
- Gate result: PASS

`docs/PROJECT_MANIFEST.md` is not present in this worktree. This gate uses
the deployer release-gate criteria from the agent contract and the test
tier guidance in `TESTING.md`.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `bd show ga-ca05s0` contains `REVIEW VERDICT: PASS` from `gascity/reviewer` for commit `388d432e8`. |
| 2 | Acceptance criteria met | PASS | Diff changes exactly three tight `time.After(time.Second)` deadlines to `time.After(5 * time.Second)` in `cmd/gc/api_state_test.go`; no production files changed. |
| 3 | Tests pass | PASS | `go test ./cmd/gc -run 'TestControllerStateEstablishesBeadEventCursorBeforePrimingStores|TestControllerStateBeadEventWatcherRetriesSetupErrors' -count=1` passed. `make test-fast-parallel` passed all fast shards. `go vet ./...` passed. |
| 4 | No high-severity review findings open | PASS | Reviewer notes report "No findings. Clean pass." |
| 5 | Final branch is clean | PASS | Evaluation worktree was clean before writing this gate; final status is checked after the gate commit. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree $(git merge-base origin/main HEAD) origin/main HEAD` reported a clean merge for the single changed file. |
| 7 | Single feature theme | PASS | Commit set touches only `cmd/gc/api_state_test.go` and only increases flaky test deadlines in one controller-state test area. |

## Test Commands

```text
go test ./cmd/gc -run 'TestControllerStateEstablishesBeadEventCursorBeforePrimingStores|TestControllerStateBeadEventWatcherRetriesSetupErrors' -count=1
make test-fast-parallel
go vet ./...
```

## Acceptance Evidence

Changed deadlines:

- `TestControllerStateEstablishesBeadEventCursorBeforePrimingStores`: initial event cursor wait from 1s to 5s.
- `TestControllerStateEstablishesBeadEventCursorBeforePrimingStores`: controller return wait from 1s to 5s.
- `TestControllerStateBeadEventWatcherRetriesSetupErrors`: initial watch attempt wait from 1s to 5s.

Unchanged surface:

- No production code changed.
- No API schema or dashboard files changed.
- No security-sensitive path changed.
