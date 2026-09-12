# Release Gate: Confirm Dolt Leak Reaping Before Cleanup

- Deploy bead: `ga-vb82ss`
- Build bead: `ga-62mu45`
- Review bead: `ga-lry99i`
- Reviewed commit: `a4e2fff37c016145398dce3153ca06762f128f38`
- Base: `origin/main@e5ed16b566302e5cd9bdbb7d75ff47bd52a8cbfc`
- Deploy mode: remote
- Deploy branch: `deploy/ga-vb82ss-gate`
- Verdict: **FAIL — WAIVED BY MAYOR**

The reviewed commit is not present on GitHub, so the associated-PR preflight
returned HTTP 422 and found no PR to reconcile. Criterion 6 was evaluated
first. The repository does not currently contain `docs/PROJECT_MANIFEST.md`;
this record applies the release criteria in the deployer contract and the test
evidence rules in
`engdocs/contributors/release-gate-criteria-conventions.md`.

The original technical gate remains a failure. The Mayor's 2026-08-18 ruling,
recorded verbatim in `ga-vb82ss`, explicitly lifts the hold and directs that
this reviewed fix be deployed. The ruling also broadens the standing
authorization for fully tracked, non-diff-reachable failures. That waiver
permits release without rewriting the retained failure as green.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | `ga-lry99i` records round 2 `verdict: pass` on the reviewed commit. |
| 2 | Acceptance criteria met | PASS | The reaper sends the existing signals, probes process-table liveness, and waits through `backoff.Retry` with a shared five-second deadline. Timeout errors name the surviving PID, while the caller's preceding leak report retains PID and argv. The two focused reaper tests and the existing Dolt regression test passed 5/5 each; build, vet, and the resource-census ratchet also passed. |
| 3 | Tests pass | **FAIL — WAIVED** | The canonical `make test-fast-parallel` run selected 9,178 `cmd/gc` tests and recorded 9,177 PASS, 1 FAIL, 0 SKIP; the non-`cmd/gc` unit job passed. `TestCityRuntimeForceShutdownTearsDownAfterLateAsyncSweep` failed with `force shutdown missed the late async-started runtime`. The required process lane was not run after this decisive required-lane failure. The Mayor explicitly waived this failed criterion for `ga-vb82ss`; the result remains recorded below. |
| 4 | No high-severity review findings open | PASS | The round-2 review records no security, style, or uncovered acceptance findings; unresolved HIGH findings: 0. |
| 5 | Final branch is clean | PASS | `git status --porcelain=v1` was empty at the reviewed commit before this gate artifact was written. |
| 6 | Branch diverges cleanly from main | PASS | Evaluated first after `git fetch origin main`. `git merge-tree --write-tree origin/main a4e2fff37c016145398dce3153ca06762f128f38` exited 0 and produced tree `81d3568cdeff08ad5086827bb6c44add2e552936`; `git diff --check` was clean. No self-rebase was needed. |
| 7 | Single feature theme | PASS | The three-commit range changes only `cmd/gc/path_helpers_test.go`, all for synchronous, bounded Dolt leak reaping and its regression coverage. |

## Criterion 3 evidence

The rootless Podman socket was present at
`unix:///run/user/1000/podman/podman.sock`; Podman reported rootless mode. The
repository has no cached `dolt-tests-via-podman` cairn entry and this test path
does not pin a testcontainers image.

- Focused command: `GC_FAST_UNIT=0 go test -json -count=5 ./cmd/gc -run '<three named tests>'`
  - `TestReapDoltLeakPIDsWithKillerAndWaiter_WaitsForConfirmedExit`: 5 PASS, 0 FAIL, 0 SKIP
  - `TestReapDoltLeakPIDsWithKillerAndWaiter_TimesOutWithClearPIDError`: 5 PASS, 0 FAIL, 0 SKIP
  - `TestBdRigWorktreeStoreConsistentAcrossRawBdGcBdAndProviderStore`: 5 PASS, 0 FAIL, 0 SKIP
- Resource policy: `go test ./internal/testpolicy/resourcecensus/... -run '^TestRepositoryLedgerMatchesCensusAndDocumentation$'` — 1 PASS, 0 FAIL, 0 SKIP.
- Static/policy lane: `make test-ci-policy check-gomod-replace check-native-dependency-surface check-eventexport-isolation check-core-boundary test-native-doltlite-beads lint-affected fmt-check-changed check-docs` — PASS.
- `go build ./...` — PASS.
- `go vet ./...` — PASS.
- Fast lane: `TMPDIR=/var/tmp make test-fast-parallel` — 9,177 PASS, 1 FAIL, 0 SKIP among the 9,178 selected `cmd/gc` tests; all other fast jobs passed.

An earlier fast invocation was invalid test-harness evidence: an overly long,
gate-specific `TMPDIR` made Unix socket paths exceed the platform limit and
produced explicit `bind: invalid argument` and 112-character-path failures. It
was rerun with the repository's canonical short `/var/tmp` environment. The
canonical rerun above retained the one product-test failure.

`diff_tests_executed`:

- `TestReapDoltLeakPIDsWithKillerAndWaiter_WaitsForConfirmedExit`: PASS (5/5)
- `TestReapDoltLeakPIDsWithKillerAndWaiter_TimesOutWithClearPIDError`: PASS (5/5)
- `TestBdRigWorktreeStoreConsistentAcrossRawBdGcBdAndProviderStore`: PASS (5/5)

`waiver_ref`: Mayor's 2026-08-18 ruling recorded verbatim in `ga-vb82ss`,
sections 2 ("ga-vb82ss: hold:mayor LIFTED") and 3 (broadened standing
authorization); deployer routing notification `gm-wisp-t9lhiv`.

`failure_attribution`:

- `TestCityRuntimeForceShutdownTearsDownAfterLateAsyncSweep` -> `ga-550z2h`.
  The test file is not diff-owned, but attribution is not permitted: the same
  test passed at `origin/main@e5ed16b566302e5cd9bdbb7d75ff47bd52a8cbfc`,
  so pre-existing failure was not reproduced (clause 3), and the failing test
  and the diff both belong to `cmd/gc` (clause 4 package/path overlap).

## Waiver disposition

Criterion 3 remains **FAIL**. Its sole failing test is specifically tracked by
`ga-550z2h`, and the `ga-vb82ss` occurrence is logged there. The reviewed diff
changes Dolt leak-PID reaping and its regression coverage in
`cmd/gc/path_helpers_test.go`; it has no plausible mechanism to reach the
city-runtime async-start/force-shutdown ordering race in
`cmd/gc/city_runtime_server_lifecycle_test.go`. Although both files share the
large `cmd/gc` package, they are different files and subsystems, and the
failure is neither in a file nor a subsystem this diff touches.

The Mayor's bead-specific ruling explicitly directs that this fix be deployed
and supersedes the earlier no-push disposition. Push this isolated deploy
branch, open its pull request, and route merge authority to mayor/mpr. No rig
agent merges the pull request.
