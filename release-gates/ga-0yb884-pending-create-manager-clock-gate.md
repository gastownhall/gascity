# Release Gate: pending-create timestamps use the manager clock

- Deploy bead: `ga-0yb884`
- Review bead: `ga-g8n6ot`
- Reviewed source commit: `d19b9e51eb51aad0b924766804dbd7cc6677bae9`
- Base checked: `origin/main` at `b677c58ac3628d70636fa7ad58286cc7d8074df8`

`docs/PROJECT_MANIFEST.md` is not present in this checkout. This checklist
applies the release criteria supplied in the deployer instructions and the
repository's documented test targets.

## Checklist

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `ga-g8n6ot` is closed with reason `pass`; its notes record `REVIEWER VERDICT: PASS` for the exact reviewed source commit. |
| 2 | Acceptance criteria met | PASS | `createBeadOnly` now stamps `pending_create_started_at` using the manager clock in UTC; the existing nil-safe production fallback remains the real clock. The session chaos harness supplies its fake clock, the rollback tests no longer rewrite timestamp metadata manually, and the lease-expiry test releases at fake-clock tick 12, beyond the 10-minute floor. |
| 3 | Tests pass | PASS | `LOCAL_TEST_JOBS=2 make test-local-full-parallel` completed 40 runner jobs: **40 PASS, 0 FAIL, 0 SKIP**. The controlled local toolchain used the repository-compatible `bd` 1.1.0, Dolt 2.1.7, and tmux 3.4 while retaining the real city home. Four named clock/rollback tests passed with **4 PASS, 0 FAIL, 0 SKIP**. `go build ./...` and `go vet ./...` both exited 0. |
| 4 | No high-severity review findings open | PASS | The reviewer reported no blockers and no OWASP concerns; unresolved HIGH finding count is 0. |
| 5 | Final branch is clean | PASS | Before writing this gate, `git status --porcelain=v1` produced no output and `git diff --check` exited 0. The gate file is the only deployer-added change and will be committed before push. |
| 6 | Branch diverges cleanly from main | PASS | Evaluated first and rechecked after the test run. `git merge-tree --write-tree origin/main d19b9e51eb51aad0b924766804dbd7cc6677bae9` exited 0 against the current base and produced tree `d353717df2396a2c379bf6994edfa73e42b93957`; no self-rebase was needed. |
| 7 | Single feature theme | PASS | The two reviewed commits touch four files in `internal/session` and `cmd/gc` for one behavior: sourcing pending-create timestamps and their rollback tests from the manager clock. |

## Test Evidence

```text
LOCAL_TEST_JOBS=2 make test-local-full-parallel
40 PASS, 0 FAIL, 0 SKIP

go test ./internal/session/... -run TestCreateSessionBeadOnlyStampsPendingCreateStartedAtFromManagerClock -json
1 named test PASS, 0 FAIL, 0 SKIP

go test ./cmd/gc/... -run 'TestDesiredPendingCreateRollsBackWhenStartKeepsFailing|TestDesiredQuarantinedPendingCreateRollsBackAfterLeaseExpiry|TestDesiredCreatingPendingCreateReleasesClaim' -json
3 named tests PASS, 0 FAIL, 0 SKIP

go build ./...
PASS

go vet ./...
PASS
```

The full runner's zero-skip result needs no skip exception. Earlier diagnostic
runs exposed host-tool mismatches and local Dolt bootstrap contention; they
were not counted as release evidence. The final audited run used pinned,
repository-compatible tools and two-way local concurrency and completed
without failures or skips.

## Scope Evidence

```text
cmd/gc/session_lifecycle_chaos_test.go
cmd/gc/session_pending_create_rollback_desired_test.go
internal/session/manager.go
internal/session/manager_test.go

4 files changed, 54 insertions(+), 27 deletions(-)
```
