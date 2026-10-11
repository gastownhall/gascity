# Release Gate: rotation conformance per-read timeout

Status: PASS

Deploy bead: `ga-u7f149`
Source bead: `ga-mllb6t`
Review bead: `ga-7y3hku`
Reviewed commit: `e26a24c1868d057d615cb5533fbed3dc97e10e9a`
Planned deploy branch: `deploy/ga-u7f149-gate`
Base evaluated: `origin/main` at `7a5bdeeee5c240663964916cea4c8f72dd91c1f4`

`docs/PROJECT_MANIFEST.md` is not present in this checkout, so this gate uses
the deployer role's release criteria and the repository testing policy in
`TESTING.md`.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 6 | Branch diverges cleanly from main | PASS | Evaluated first. The reviewed branch's remote tip exactly matched the recorded SHA. `git merge-tree --write-tree origin/main e26a24c1868d057d615cb5533fbed3dc97e10e9a` exited 0 and produced tree `b552224ff6f5d068933d28bc0e395da972430397`. |
| 1 | Review PASS present | PASS | Review bead `ga-7y3hku` is closed with verdict PASS for `builder/ga-c1r8af` at the reviewed commit. Its style, security, and specification checks all report no blocking findings. |
| 2 | Acceptance criteria met | PASS | `RunRotationTests` now uses `context.WithCancel` for watcher lifetime and routes every blocking read through `nextWithin(testutil.GoroutineRaceTimeout)`. No `context.WithTimeout`, direct loop-level `w.Next`, or `10*time.Second` literal remains in the function. The file is gofmt-clean, the focused conformance test passed 20 repetitions, the independent exec consumer passed, and a freshly built stress binary completed 600/600 rotation-invariant runs without failure. |
| 3 | Tests pass | PASS | `go test ./internal/events/... -run TestFileRecorderConformance -count=20`; `go test ./internal/events/exec/... -count=1`; 24 workers × 25 runs of `TestFileRecorderConformance/RotationPreservesInvariants` from a freshly built binary (600 runs, 0 failures); `make test-fast-parallel` (all 9 jobs passed); and `go vet ./...` all passed. |
| 4 | No high-severity review findings open | PASS | Review notes report no style or security findings and no blocking issue; unresolved HIGH findings: 0. |
| 5 | Final branch is clean | PASS | Before creating this checklist, `git status --porcelain=v1` returned no entries at the exact reviewed commit. The checklist is committed separately as the deploy-branch tip. |
| 7 | Single feature theme | PASS | The reviewed commit changes only `internal/events/eventstest/conformance.go`, within one test-harness subsystem, to replace a shared rotation deadline with per-read deadlines. |

## Acceptance Evidence

- The watcher remains explicitly bounded by the existing deferred `cancel` and
  `Close` calls, while rotation I/O no longer consumes the read deadline.
- Both the pre-rotation drain and post-rotation read loop call the same local
  `nextWithin` helper with the repository's centralized goroutine-race timeout.
- The helper uses a buffered result channel, matching the existing per-read
  timeout idiom in this conformance package without introducing a new
  production abstraction.
- Production event recording and watcher code are unchanged.

## Commands

```text
git ls-remote origin refs/heads/builder/ga-c1r8af
git merge-tree --write-tree origin/main e26a24c1868d057d615cb5533fbed3dc97e10e9a
gofmt -l internal/events/eventstest/conformance.go
git diff --check e26a24c1868d057d615cb5533fbed3dc97e10e9a^
go test ./internal/events/... -run TestFileRecorderConformance -count=20
go test ./internal/events/exec/... -count=1
go test -c ./internal/events
make test-fast-parallel
go vet ./...
```
