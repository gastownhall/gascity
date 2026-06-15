# Release gate: emergency relay + SupervisorHTTPCheck

- Deploy bead: ga-4zsxco
- Duplicate deploy bead observed: ga-gcoud4
- Source review bead: ga-y0jt8v
- Pull request: https://github.com/gastownhall/gascity/pull/3402
- Reviewed branch: builder/ga-frmdxd.3
- Reviewed commit: 43284cb2bebd5cb24708ba4f7bd05b5e9d692d1a
- Gate result: PASS
- Gate date: 2026-06-11 PDT / 2026-06-12 UTC

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | ga-y0jt8v is closed with `Reviewer verdict: PASS`; reviewer notes identify branch `builder/ga-frmdxd.3` and commit `43284cb2b`. The review lists no blocking findings. |
| 2 | Acceptance criteria met | PASS | The branch adds `internal/emergency` spool writing and event mirroring, registers typed `EmergencySignaled` and `EmergencyAcked` payloads, wires `startEmergencyEventRelay`, and adds `SupervisorHTTPCheck` to `gc doctor`. Coverage includes emergency spool/dedupe tests, supervisor HTTP tests, `TestEveryKnownEventTypeHasRegisteredPayload`, and `TestOpenAPISpecInSync`. Generated OpenAPI and dashboard client artifacts are updated. |
| 3 | Tests pass | PASS | GitHub PR #3402 reports `mergeStateStatus=CLEAN` and all required CI checks green, including `CI / required`, `CI / preflight`, `CI / integration`, CodeQL, dashboard SPA, all `cmd/gc process` shards, integration package shards, and worker-core jobs. Local checks passed: `make dashboard-check`, `go vet ./...`, and focused `go test ./internal/emergency/... ./internal/doctor/... ./internal/api/... -run 'TestEveryKnownEventTypeHasRegisteredPayload|TestOpenAPISpecInSync|TestSupervisorHTTP|TestMarkNotify|TestWriteSpool|TestRecord'`. Local `make test-fast-parallel` was attempted three times but the host has a live `/tmp/.gc` city/Dolt server (`dolt` pid 1047713 holding `/tmp/.gc/runtime/packs/dolt/dolt.log`), causing no-city `cmd/gc` tests to resolve `/tmp/city.toml`; this is host contamination, not a branch-specific failure. |
| 4 | No high-severity review findings open | PASS | Reviewer notes list no blocking findings. The security review covers `MarkNotifyDedupe`, `WriteSpool`, and `SupervisorHTTPCheck`; architecture review confirms typed event contracts and CI enforcement. |
| 5 | Final branch is clean | PASS | Clean detached worktree at `43284cb2b` before gate commit; after the gate commit only this release-gate file is added and `git status --short --branch` is clean. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree --quiet origin/main HEAD` succeeds, and GitHub reports PR #3402 merge state `CLEAN`. |
| 7 | Single feature theme | PASS | The commit set is one reliability/observability feature: emergency spool/event relay plus the supervisor HTTP doctor probe. Generated OpenAPI/dashboard artifacts are direct consequences of the new typed events. |

## Local Check Details

- `make dashboard-check`: PASS from `/home/jaword/projects/gc-management/.gc/worktrees/gascity/deploy-ga-4zsxco-emergency-gate` and `/tmp/gascity-ga-4zsxco-gate`.
- `go vet ./...`: PASS from `/tmp/gascity-ga-4zsxco-gate`.
- Focused branch tests: PASS.
- `make test-fast-parallel`: local host contamination prevented a clean local run. Final clean-`TMPDIR` run had `unit-core` OK, with failures limited to `cmd/gc` tests that intentionally expect no discoverable city, while `/tmp/.gc` is live on this host. CI's equivalent sharded jobs for PR #3402 are green.

## PR Body Check

PR #3402 already exists for `builder/ga-frmdxd.3`; this gate updates the PR branch rather than opening a duplicate PR.
