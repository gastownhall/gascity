# Release Gate: pool wake-known-identity vs named-session ambiguity

- Bead: ga-cjhqf0
- Feature branch: builder/ga-p0u752-pool-desired-state-identity-fix
- Evaluated implementation head: 20bce6d3d30910f97e5b29edf4cd43356dbce1b2
- Current base: origin/main @ 4189caf4fcac0d7b199a143346c2c1b704674994
- Merge base: d9225156f9b30fa4fe7fee28c30699ee2cc8cc3d
- Reviewer bead: ga-sd5w70
- Implementation bead: ga-p0u752

Note: `docs/PROJECT_MANIFEST.md` is absent in this Gas City checkout. This gate uses the deployer prompt's seven release criteria and the repository's `TESTING.md` guidance.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `ga-sd5w70` is closed with `REVIEWER VERDICT: PASS` and no blocking findings. |
| 2 | Acceptance criteria met | PASS | Diff contains the structural named-session skip in `cmd/gc/pool_desired_state.go`, removes the old expanded-identity guard in `cmd/gc/build_desired_state.go`, and adds/reuses the required tests. |
| 3 | Tests pass | PASS | `go build ./cmd/gc/...`, `go vet ./...`, focused `cmd/gc` tests, and `make test-fast-parallel` passed on the final branch. First fast run failed in unrelated supervisor-city tests under a longer TMPDIR; the exact failures passed on both branch and `origin/main` with short TMPDIR, then the full fast baseline passed with short TMPDIR. |
| 4 | No high-severity review findings open | PASS | Review notes contain no unresolved HIGH findings. Prior Medium/Low findings from the design chain are resolved or documented as non-blocking. |
| 5 | Final branch is clean | PASS | `git status --short --branch` was clean before this gate file was added; this file is the only gate commit payload. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree HEAD origin/main` exited 0 and produced tree `acdc91b0de0eed899a1ed8d33e13bfe6aca23d22`. The new main commit since the branch base touches only `.github/workflows/ci.yml`, `Makefile`, and `scripts/check-core-boundary.sh`. |
| 7 | Single feature theme | PASS | Commit set is one `cmd/gc` reconciler/session-demand fix: `cmd/gc/build_desired_state.go`, `cmd/gc/build_desired_state_ga80pen8_test.go`, `cmd/gc/pool_desired_state.go`, `cmd/gc/pool_desired_state_test.go`. |

## Acceptance Evidence

Implementation bead `ga-p0u752` required:

- Add a suspension-aware configured-named-session identity check before `wake-known-identity` pool demand: PASS.
- Delete the named-work-ready `SupportsExpandedSessionIdentities` suppression guard: PASS.
- Keep `TestBuildDesiredState_MultiSlotPoolNamedSession_OneRoutedBeadProvisionsTwoWorkers` green with the guard deleted: PASS.
- Add a reaped named-session bare-identity recovery test proving named-tier recovery without a competing pool worker: PASS.
- Cover the suspended-agent helper behavior: PASS via `TestIsConfiguredNamedSessionIdentity`.
- Keep `TestSharedTemplateAssignee_Tier1CrashRecoveryCrossAdopts` green: PASS.

## Test Log

- `gofmt -l cmd/gc/build_desired_state.go cmd/gc/build_desired_state_ga80pen8_test.go cmd/gc/pool_desired_state.go cmd/gc/pool_desired_state_test.go`: PASS, no output.
- `git diff --check origin/main...HEAD`: PASS.
- `git config core.hooksPath`: `.githooks`.
- `TMPDIR=/var/tmp/gascity-test-ga-cjhqf0 go build ./cmd/gc/...`: PASS.
- `TMPDIR=/var/tmp/gascity-test-ga-cjhqf0 go vet ./...`: PASS.
- `TMPDIR=/var/tmp/gascity-test-ga-cjhqf0 go test ./cmd/gc/ -run 'ControlReady|WorkflowServeControlReadyQuery|RunWorkflowServe|ComputePoolDesiredStates_WakeKnownIdentitySkipsConfiguredNamedSession|IsConfiguredNamedSessionIdentity|BuildDesiredState_NamedSessionBareIdentityReaped_RecoversViaNamedTier|BuildDesiredState_MultiSlotPoolNamedSession_OneRoutedBeadProvisionsTwoWorkers|SharedTemplateAssignee_Tier1CrashRecoveryCrossAdopts' -count=1 -v`: PASS.
- `TMPDIR=/var/tmp/gascity-test-ga-cjhqf0 make test-fast-parallel`: initial FAIL in supervisor-city tests unrelated to the diff.
- Exact initial failures rerun on branch with short TMPDIR: PASS.
- Exact initial failures rerun on `origin/main` with short TMPDIR: PASS.
- `TMPDIR=$(mktemp -d /var/tmp/gf.XXXXXX) make test-fast-parallel`: PASS, all 8 fast jobs passed.

## Scope Evidence

`git log --oneline --no-merges origin/main..20bce6d3d30910f97e5b29edf4cd43356dbce1b2` contains one implementation commit:

```text
20bce6d3d fix(reconciler): resolve pool wake-known-identity vs named-session ambiguity (ga-p0u752)
```

`git diff --stat origin/main...20bce6d3d30910f97e5b29edf4cd43356dbce1b2`:

```text
cmd/gc/build_desired_state.go               | 26 +++++-----
cmd/gc/build_desired_state_ga80pen8_test.go | 76 +++++++++++++++++++++++++++++
cmd/gc/pool_desired_state.go                | 29 +++++++++++
cmd/gc/pool_desired_state_test.go           | 75 ++++++++++++++++++++++++++++
4 files changed, 192 insertions(+), 14 deletions(-)
```
