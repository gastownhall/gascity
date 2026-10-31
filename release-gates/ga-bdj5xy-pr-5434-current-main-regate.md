**Verdict:** **PASS**

# Release Gate Revalidation: ga-bdj5xy / PR #5434

Recovery bead: `ga-bdj5xy`
Original deploy bead: `ga-0ckn7x`
Review bead: `ga-09qq0u`
Reviewed content commit: `341069eee3aa90b32afe2ff015600d7f0090acce`
Previous PR head: `68a717f4d765f5a6678014e145f2b660633ffbd4`
Evaluated source head: `c049685a5162deeb4b7918c2352ebf0c8e21d375`
Deploy branch: `deploy/ga-0ckn7x-gate-r2-20260820`
Base: `origin/main@f383a24397ee701dd44abe84e1dca180486ac2bb`
Gate evaluated: 2026-09-17

This record supersedes the 2026-09-16 revalidation for the current PR head.
The original 2026-08-20 gate and its narrow mayor waiver remain as historical
evidence only. This revalidation does not use that waiver.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead `ga-09qq0u` records PASS at `341069eee3aa90b32afe2ff015600d7f0090acce`. `git diff --exit-code 341069eee3aa90b32afe2ff015600d7f0090acce c049685a5162deeb4b7918c2352ebf0c8e21d375 -- cmd/gc/cmd_reload_test.go` returned 0, so the reviewed implementation is byte-identical after rebase. |
| 2 | Acceptance criteria met | PASS | Both duplicated five-second initial-reconcile polling loops use the existing `awaitCond`/`hangBudget` path. The live resource census re-derived the synchronized fixed-sleep ledgers as 480 calls / 172 files for all source and 317 calls / 120 files for both untagged rows. `TestRepositoryLedgerMatchesCensusAndDocumentation` passed on the final source tree. |
| 3 | Tests pass | PASS | The required isolated 40-job full union completed 37 green jobs with 90,810 PASS / 4 attributed FAIL / 317 SKIP results, counting top-level tests and subtests. Both diff-owned reload tests passed in their process shards. The four raw failures are independently tracked, cross-PR-proven conditions outside this diff; details are below. No test was rerun. `waiver_ref: none`. |
| 4 | No high-severity review findings open | PASS | The reviewer reported no unresolved HIGH finding. The four raw failures are non-diff-owned host/shared-fixture conditions with no path overlap. |
| 5 | Final branch is clean | PASS | The rebase conflict was limited to the synchronized fixed-sleep ledger triple and was resolved from a live census rather than copied counts. Build, vet, affected lint/format, boundary checks, topology checks, hook ownership, release-ignore, and resource-census validation all passed. |
| 6 | Branch diverges cleanly from main | PASS | After fetching, `origin/main@f383a24397ee701dd44abe84e1dca180486ac2bb` is an ancestor of evaluated source `c049685a5162deeb4b7918c2352ebf0c8e21d375`. The implementation replayed onto current main; only the three generated fixed-sleep ledger files conflicted, and the remaining gate-history commits replayed cleanly. |
| 7 | Single feature theme | PASS | The source delta replaces two reload-test polling loops and synchronizes the three generated fixed-sleep ledgers. The release-gate records document that same change and its revalidations. |

## Test evidence

`test_cmd_scope: full-suite`

The authoritative final-source-tree command was:

```text
DOCKER_HOST=unix:///run/user/1000/podman/podman.sock
TESTCONTAINERS_RYUK_DISABLED=true
GOFLAGS=-v
GO_TEST_TIMEOUT=30m
LOCAL_TEST_JOBS=4
CMD_GC_PROCESS_TOTAL=6
LOCAL_TEST_LOG_DIR=/var/tmp/ga-bdj5xy-full-logs
$GC_CITY_ROOT/packs/actual/all/scripts/isolated-test-run.sh -- bash -c 'make test-local-full-parallel'
```

The coordinator log is `/var/tmp/ga-bdj5xy-full.log`; per-job logs are under
`/var/tmp/ga-bdj5xy-full-logs`.

- 40 jobs ran: 37 green, 3 red.
- 90,810 tests/subtests passed, 4 failed, and 317 skipped.
- `TestSendReloadControlRequestNoChange` passed in `cmd-gc-process-3-of-6`
  in 0.51 seconds.
- `TestSendReloadControlRequestInvalidConfig` passed in
  `cmd-gc-process-5-of-6` in 0.54 seconds.
- The duplicate unit-lane selections of those two process-backed tests skipped
  with the explicit `starts real Dolt lifecycle` guard after each test had
  already executed and passed in its designated process lane.
- Remaining skips are the suite's existing platform, helper-process,
  opt-in-live, privilege, and lane-partition guards; this diff adds none.
- `ci_lane_run: n/a (no CI-config change in this diff)`.

### Attributed raw failures

All tracker records predate this run, were opened before attribution, and now
carry peek-verified sightings from this exact candidate. The candidate changes
only two test bodies, the synchronized resource-census values, and gate records;
it adds no test target or load and lowers the fixed-sleep census by two calls.

- `failure_attribution: TestActivityLive -> ga-fua7nj | clause 3(b) CROSS-PR`.
  The exact revision-2 condition, where both reads capture the same freshly
  seeded idle timestamp, has occurred on unrelated candidates. The failing
  file is under `internal/runtime/herdr`; there is no diff-path overlap and it
  cannot execute the changed reload test bodies.
- `failure_attribution: TestProviderLiveClaudeKindPath -> ga-iepsvr | clause
  3(b) CROSS-PR`. External Herdr returned `agent_pane_busy` for shared pane
  `w1:p1`, the exact condition repeatedly recorded on unrelated candidates.
  There is no `internal/runtime/herdr` or pane-state delta in this branch.
- `failure_attribution: TestRetryManagedPooledWorkerRecoversClaimedAttemptAfterCrash
  -> ga-lejnse | clause 3(b) CROSS-PR`. The fixture stopped during `gc init`
  when external bd observed a fresh database mid-migration at v3 and refused
  63 pending shared-server migrations to v66. Exact-test occurrences on
  unrelated candidates predate this run; root fix `ga-e2z1zb` is not on main.
- `failure_attribution: TestGCLiveContract_BeadsAndEvents -> ga-lejnse | clause
  3(b) CROSS-PR`. Rig fixture initialization stopped at the same external-bd
  boundary, at v40 with 26 pending migrations to v66. Exact-test occurrences
  on unrelated candidates predate this run; the scenario never reached reload
  behavior.

`inconclusive-guard: not used; clause 3(b) landed for every failure. Added test
load=no (fixed-sleep census decreased; no new test target).`

## Static and policy evidence

The following passed on the evaluated source tree:

- `make test-ci-policy`
- `make build`
- `go vet ./...`
- `GOLANGCI_LINT_CACHE=/var/tmp/ga-bdj5xy-golangci-cache-candidate LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=f383a24397ee701dd44abe84e1dca180486ac2bb make lint-affected fmt-check-changed`
- `make check-core-boundary check-native-dependency-surface check-residency-boundary check-gomod-replace check-eventexport-isolation`
- `make check-routed-test-rows check-split-topology-rows`
- `make check-release-dist-ignore`
- `make check-hooks`
- `go test -count=1 ./internal/testpolicy/resourcecensus -run '^TestRepositoryLedgerMatchesCensusAndDocumentation$' -v`
- `git diff --check origin/main...HEAD`

An initial affected-lint invocation used the fleet-wide golangci cache and
reported paths from a deleted, unrelated worktree. A detached current-main run
with an isolated lint cache passed with zero issues, and the candidate run with
its own isolated lint cache also passed with zero issues. The contaminated-cache
output is not gate evidence.

## Disposition

Commit this record, force-update the existing PR branch with lease, verify PR
#5434 at the new exact head, and publish
`release-gate/deploy-clearance=success` on that head. Merge authority remains
with the operator, mayor, or mpr; the deployer does not merge.
