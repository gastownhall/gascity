# Release gate: ga-tavdtc — stop-fallback test timeout

**Verdict:** **FAIL — WAIVED BY MAYOR**

**Evaluated:** 2026-08-18 (America/Los_Angeles)

**Deploy bead:** `ga-tavdtc`

**Build bead:** `ga-j8z56i`

**Review bead:** `ga-isjlsh`

**Reviewed commit:** `bb9d53162f772ca91805eb22771747525808c61f`

**Base:** `origin/main@a565081fb87c13de8366594ad40ddfd731469539`

**Deploy mode:** remote (`origin` and `fork` are GitHub remotes)

**Deploy branch:** `deploy/ga-tavdtc-gate`

The original technical gate remains a failure. Criterion 3 failed because the
candidate's `TestGCLiveContract_BeadsAndEvents` failure did not reproduce on
the exact base, so the release-gate attribution policy did not permit it to be
treated as pre-existing. The Mayor's bead-specific 2026-08-18 ruling, recorded
verbatim in `ga-tavdtc`, explicitly authorizes this deploy to proceed without
rewriting that result as green.

`docs/PROJECT_MANIFEST.md` is absent from both this checkout and
`origin/main`; this record evaluates the active seven release criteria from
the deployer contract and the sharded test commands documented in
`TESTING.md`.

## Gate criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead `ga-isjlsh` records `REVIEW VERDICT: PASS` for the exact reviewed commit. |
| 2 | Acceptance criteria met | PASS | The one-file diff changes only three test call sites from a live `time.Second` cap to `0`. Production `cmdStop` documents `0` as the normal config-derived/default path, and no assertions or production behavior changed. All three changed tests pass directly and in the six process shards. |
| 3 | Tests pass | **FAIL — WAIVED** | The diff-owned tests and every required `cmd/gc` process/integration lane pass. The 40-job full sweep has four failures, but only the two tmux failures meet all four attribution clauses. The Mayor explicitly waived this failed criterion for `ga-tavdtc`; the original counts and failures remain recorded below. |
| 4 | No high-severity review findings open | PASS | Reviewer recorded no blocking or HIGH findings. Unresolved HIGH count: 0. |
| 5 | Final branch is clean | PASS | `git status --short` was empty at the reviewed commit immediately before this gate file was written. |
| 6 | Branch diverges cleanly from main | PASS | The reviewed SHA is one commit directly atop current `origin/main`; `git merge-tree --write-tree --messages origin/main <reviewed-sha>` exited 0 and produced tree `3e0b74cde7efe43ff466a4968de0f82e2d3928e5`. No self-rebase was needed. |
| 7 | Single feature theme | PASS | The single commit and single changed file address one test-infrastructure behavior: stop-fallback tests using the production-default timeout path under shard load. |

## Pre-flight

The recorded commit resolves to the full commit SHA above. It is exactly one
commit ahead of current `origin/main`, with merge base
`a565081fb87c13de8366594ad40ddfd731469539`. The GitHub commit-to-PR query
returned no associated pull requests, so the target has not already merged
and is not a closed or superseded PR.

## Criterion 3 evidence

The gate used CI-pinned Dolt `2.1.7` and official `bd 1.1.0`, with temporary
files on `/var/tmp`. The rootless Podman socket and testcontainers environment
were configured before testing. The shared Go build cache was not cleaned or
relocated.

### Diff-owned tests

Command:

```text
go test -json -count=1 -timeout 15m ./cmd/gc -run '^(TestCmdStopSupervisorManagedInvalidCityTomlWaitsForControllerStop|TestCmdStopJSONReportsUnregisteredTrueForSupervisorManagedCity|TestCmdStopJSONReportsUnregisteredTrueWhenSupervisorNotRunning)$'
```

Counts: **3 PASS, 0 FAIL, 0 SKIP**.

`diff_tests_executed`:

- `TestCmdStopSupervisorManagedInvalidCityTomlWaitsForControllerStop` — PASS
- `TestCmdStopJSONReportsUnregisteredTrueForSupervisorManagedCity` — PASS
- `TestCmdStopJSONReportsUnregisteredTrueWhenSupervisorNotRunning` — PASS

`waiver_ref`: Mayor's 2026-08-18 bead-specific ruling recorded verbatim in
`ga-tavdtc` under "TWO WAIVERS AND A STANDING AUTHORIZATION".

`skip_justification`: none required; zero changed tests skipped.

### Documented full sweep

Command: `LOCAL_TEST_JOBS=4 make test-local-full-parallel`, with the prepared
container and pinned-tool environment.

Log directory: `/var/tmp/gc-local-tests.by23un`.

Job counts: **36 PASS, 4 FAIL, 0 SKIP** out of 40 jobs.

All six `cmd/gc` process shards passed, including shard 2 where the original
one-second timeout failure was observed. All six `cmd/gc` integration package
shards, all four integration-core shards, both PR REST-smoke shards,
formula-recovery, product-metrics, and unit-core also passed.

### Failure attribution

These two failures satisfy all four attribution clauses: neither is
diff-owned, tracker `ga-afqddr` covers both, the exact tests failed on exact
base `a565081f` in the immediately preceding base sweep at
`/var/tmp/gc-local-tests.0azm2T`, and `internal/runtime/tmux` has no path
overlap with this diff.

- `TestGetKeyBinding_CapturesDefaultBinding` → `ga-afqddr`; exact base result:
  FAIL, expected `next-window`, received an empty binding.
- `TestGetKeyBinding_CapturesDefaultBindingWithArgs` → `ga-afqddr`; exact base
  result: FAIL, expected `choose-tree`, received an empty binding.

These failures do **not** satisfy the required base-proof clause:

- `TestGCLiveContract_BeadsAndEvents` → `ga-lpfjhc`. Candidate result: FAIL
  during fixture-city initialization with the tracked beads#4566 dirty-table
  migration signature. Exact base result from a direct run: **PASS** in
  76.58 seconds. Clauses 1, 2, and 4 pass; clause 3 fails.
- `TestCleanInstallTutorialPath` → `ga-lpfjhc` for the candidate's dirty-table
  migration signature. The exact base test also failed, but for a different,
  independently tracked circuit-breaker-output contamination (`ga-rsktma`),
  not the candidate's beads#4566 failure. That does not prove the candidate
  failure pre-existing, so clause 3 is not satisfied for `ga-lpfjhc`.

The direct base command was:

```text
go test -count=1 -v -tags integration -timeout 30m ./test/integration -run '^(TestCleanInstallTutorialPath|TestGCLiveContract_BeadsAndEvents)$'
```

### Policy and static lanes

- `policy_lane: make test-ci-policy` — PASS
- `GOLANGCI_LINT_CACHE=/var/tmp/gc-gate-ga-tavdtc-lint-cache LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main make lint-affected` — PASS, 0 issues
- `make fmt-check-changed` — PASS (`no changed existing Go files`)
- `go build ./cmd/gc/...` — PASS
- `go vet ./cmd/gc/...` — PASS
- `git diff --check origin/main...HEAD` — PASS

## Waiver disposition

The technical gate result is still **FAIL**. The Mayor's 2026-08-18 ruling
explicitly names `ga-tavdtc` as allowed to proceed because the two integration
failures match the `ga-lpfjhc` / gastownhall/beads#4566
dirty-table-during-schema-migration signature and this three-call-site
stop-test timeout diff has no plausible mechanism to touch schema migration or
store bootstrap. The corroborating occurrence is logged on `ga-lpfjhc` with
deploy bead `ga-tavdtc`, build bead `ga-j8z56i`, and the failing test names.

The bead-specific waiver supersedes the earlier no-push disposition and
authorizes pushing this isolated deploy branch, opening its pull request, and
routing merge authority to mayor/mpr. No rig agent merges the pull request.
