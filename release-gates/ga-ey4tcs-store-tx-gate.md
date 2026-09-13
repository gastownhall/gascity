# Release Gate: ga-ey4tcs Store.Tx session lifecycle writes

Gate evaluated: 2026-07-14T04:05:22Z

Bead: `ga-ey4tcs` - needs-deploy: Wrap closeBead/rollbackPendingCreate/reopen in Store.Tx

Source review bead: `ga-23a84u` - Review verdict: PASS

Branch: `builder/ga-ey4tcs-store-tx-rebuild-v3`

Base: `origin/main` at `5e2fa2872a6dc0de996397f6face30893a4c14da`

Head before gate commit: `afdf84fbe0e65c1542816bc55dee23c6b451d220`

Manifest note: `docs/PROJECT_MANIFEST.md` is not present in this repository at this head, so the gate uses the deployer prompt criteria and the local `TESTING.md` guidance.

## Criteria

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 1 | Review PASS present | PASS | `bd show ga-23a84u` contains `REVIEW VERDICT: PASS`, closes with reason `pass`, and states "No blocking findings." |
| 2 | Acceptance criteria met | PASS | The single commit wraps the requested session lifecycle write groups in `Store.Tx`: `closeBead`, `closeFailedCreateBead`, `rollbackPendingCreate`, and `reopenClosedConfiguredNamedSessionBead`. The diff keeps cascade cleanup outside the transaction, adds the explicit already-closed rollback guard, and pins the behavior with transaction/failure/idempotence tests. |
| 3 | Tests pass | PASS | `gofmt -l` on the 4 touched files produced no output. `go build ./...` passed. `go vet ./...` passed. Targeted `go test -count=1 ./cmd/gc -run ...` passed. `make test-fast-parallel` passed all 8 fast jobs. |
| 4 | No high-severity review findings open | PASS | Review bead `ga-23a84u` reports no blocking findings and no high-severity open findings in notes. |
| 5 | Final branch is clean | PASS | `git status --short --branch` showed `## builder/ga-ey4tcs-store-tx-rebuild-v3...origin/builder/ga-ey4tcs-store-tx-rebuild-v3` with no dirty paths before writing this gate file. |
| 6 | Branch diverges cleanly from main | PASS | After `git fetch origin main`, `git merge-base --is-ancestor origin/main HEAD` exited 0, `git rev-list --left-right --count origin/main...HEAD` returned `0 1`, and `git merge-tree` reported clean. |
| 7 | Single feature theme | PASS | The branch is one commit ahead of main and touches only `cmd/gc/session_beads.go`, `cmd/gc/session_beads_test.go`, `cmd/gc/session_lifecycle_parallel.go`, and `cmd/gc/session_lifecycle_parallel_test.go` for one session lifecycle atomic-write theme. |

## Test Evidence

```text
gofmt -l cmd/gc/session_beads.go cmd/gc/session_beads_test.go cmd/gc/session_lifecycle_parallel.go cmd/gc/session_lifecycle_parallel_test.go
# no output

go build ./...
# exit 0

go vet ./...
# exit 0

go test -count=1 ./cmd/gc -run 'Test(ReopenClosedConfiguredNamedSessionBead(ClearsPendingCreateStartedAtWhenActive|ClearsStaleStartMarkersWhenRecreating|UsesSingleTransactionForStatusAndMetadata|FailsWhenMetadataBatchFails)|CloseBead(ClearsPendingCreateClaimEvenWhenCloseFails|UsesSingleTransactionForMetadataAndClose|ReleasesWorkAssignedBySessionName|ClearsSessionAffinityOnRelease|ReleasesWorkAssignedByBeadID|ReleasesWorkAssignedByNamedIdentity|LeavesUnrelatedWorkAlone|ReleasesWorkAssignedByAlias|DoesNotDuplicateOwnershipGuard|IsNoopOnAlreadyClosedBead|CascadesExtmsgState)|CloseFailedCreateBead(UsesSingleTransactionForMetadataAndClose|CascadesExtmsgState)|RollbackPendingCreate(UsesSingleTransactionForAllWrites|IsNoopOnAlreadyClosedBead)|AsyncStartSessionStillCurrent_RollbackPendingCreateStillWorksWhenNotActive|CommitStartResult_(RollbackPendingErrorClearsInFlightLeaseWhenCloseFails|AtomicBatchFailureLeavesClaimIntact|AtomicBatchLandsStateAndClaimClearTogether))$'
ok  	github.com/gastownhall/gascity/cmd/gc	0.437s

make test-fast-parallel
[fsys-darwin-compile] ok
[unit-cmd-gc-1-of-6] ok
[unit-cmd-gc-2-of-6] ok
[unit-cmd-gc-3-of-6] ok
[unit-cmd-gc-4-of-6] ok
[unit-cmd-gc-5-of-6] ok
[unit-cmd-gc-6-of-6] ok
[unit-core] ok
All fast jobs passed
```

## Branch Evidence

```text
git diff --name-only origin/main..HEAD
cmd/gc/session_beads.go
cmd/gc/session_beads_test.go
cmd/gc/session_lifecycle_parallel.go
cmd/gc/session_lifecycle_parallel_test.go

git log --oneline --left-right --cherry-pick origin/main...HEAD
> afdf84fbe fix(session): wrap closeBead/rollbackPendingCreate/reopen in Store.Tx
```
