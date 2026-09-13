# Release Gate: ga-mpi3a

Bead: ga-mpi3a - Review: clear session circuit breaker on explicit kill
Source bead: ga-k0n20.1.2 - Builder: clear circuit breaker after explicit reset or kill
Feature branch: builder/ga-k0n20-1-2
Evaluated by: gascity/deployer
Date: 2026-05-24

## Gate Result

PASS

## Scope Note

`builder/ga-k0n20-1-2` is stacked on the reviewed prerequisite
`ga-k0n20.1.1` commit `c849db89e`, then adds the `ga-k0n20.1.2`
circuit-breaker clear commit `5381615e`. The branch is evaluated as the PR
surface because `ga-k0n20.1.2` depends on the restart-request kill block being
ordered before the autonomous reconciler gates.

`docs/PROJECT_MANIFEST.md` is not present in this worktree; this gate uses the
release criteria supplied by the deployer role prompt.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `ga-mpi3a` notes contain `REVIEWER VERDICT: PASS` for `5381615e`. The carried prerequisite review bead `ga-brf9n` contains `REVIEW VERDICT: PASS` for `c849db89e`. |
| 2 | Acceptance criteria met | PASS | The prerequisite commit moves the `restart_requested` kill block after drain-ack handling and before rate-limit, stability, and churn gates. The source commit clears named-session circuit breaker state after explicit `gc session kill` and restart-requested reconciler kills, reusing the reset circuit-breaker path. Tests cover the preemption gates and circuit-open-to-wakeable recovery paths. |
| 3 | Tests pass | PASS | Focused `cmd/gc` regression tests passed. `LOCAL_TEST_JOBS=8 make test-fast-parallel` passed all fast shards. `go vet ./...` passed. `git diff --check origin/main...HEAD` passed. |
| 4 | No high-severity review findings open | PASS | Reviewer notes list no HIGH findings. The only noted item for `ga-mpi3a` is MINOR / no action required around loading the session bead before kill. |
| 5 | Final branch is clean | PASS | `git status --short --branch` was clean before this gate file was added. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` completed without conflicts (`5768f9f812527fc0c025902ea41f95bd7782e09d`). |

## Commits Evaluated

| Bead | Review | Commit |
|------|--------|--------|
| ga-k0n20.1.1 | PASS via ga-brf9n | `c849db89e` - `fix(gc): preempt autonomous gates for reset requests` |
| ga-k0n20.1.2 | PASS via ga-mpi3a | `5381615e` - `fix(gc): clear session circuit breaker on explicit kill` |

## Acceptance Evidence

### ga-k0n20.1.1

- `cmd/gc/session_reconciler.go` now handles restart-requested sessions after
  drain-ack handling and before rate-limit, stability, and churn gates.
- Runtime liveness uses `running || alive`, so provider-visible zombie sessions
  still get stopped before the next wake.
- Existing drain-ack behavior remains ahead of the restart-request block.
- Tests present for restart requests preempting rate-limit, stability, and
  churn gates.

### ga-k0n20.1.2

- `gc session kill` loads the session bead, resolves named-session identity,
  kills through the worker boundary, then clears circuit-breaker state for that
  identity.
- The shared explicit-kill helper uses the managed controller circuit-breaker
  path when available and falls back to the local default breaker otherwise.
- The reconciler clears circuit-breaker state after a successful
  restart-requested kill and before recording the restart handoff metadata.
- Tests present for `gc session kill` clearing circuit-breaker state and for a
  restart-requested kill making an open-circuit named session wakeable on the
  next reconciler pass.

## Test Evidence

Commands run:

```text
go test ./cmd/gc -run 'Test(CmdSession(Kill|Reset)_ClearsCircuitBreaker|ReconcileSessionBeads_RestartRequest|Reconciler_Circuit(OpenBlocksSpawn|ClosedAllowsSpawn))' -count=1
git diff --check origin/main...HEAD
LOCAL_TEST_JOBS=8 make test-fast-parallel
go vet ./...
git merge-tree --write-tree origin/main HEAD
```

Results:

```text
focused go test: PASS
git diff --check: PASS
make test-fast-parallel: PASS
go vet ./...: PASS
merge-tree with origin/main: PASS
```
