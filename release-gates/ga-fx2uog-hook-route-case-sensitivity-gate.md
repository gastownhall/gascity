# Release Gate: preserve case-sensitive hook route identities

Date: 2026-09-04  
Deployer: `gascity/deployer`  
Status: PASS  
Deploy bead: `ga-fx2uog`  
Build bead: `ga-0yppjl`  
Review bead: `ga-o6th87`  
Reviewed and gated commit: `51d73596966496bebe995d653f9cc724ac3e6282`  
Base checked: `origin/main@407114321e1a8e7ad36ddd2660cc115bd2ca96b3`

`docs/PROJECT_MANIFEST.md` is not present in this checkout, so this gate uses
the deployer release criteria and the repository testing policy in
`TESTING.md`.

## Gate checklist

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead `ga-o6th87` is closed `pass` and records `verdict: pass` for the exact resolved commit `51d73596966496bebe995d653f9cc724ac3e6282`. |
| 2 | Acceptance criteria met | PASS | `hookRouteIdentitiesEqual` now compares the unsanitized identities with `==`, while preserving slash/dot session-name decoding. Its comment explains why case folding would cross agent boundaries and directs any future normalization to the write seam. The two obsolete case-insensitive assertions are removed, the case-differing regression expects `false`, and the diff is confined to the two specified files. `go build ./cmd/gc/` and `go vet ./...` pass. |
| 3 | Tests pass | PASS with attributed failures | The documented full-scope command completed all 40 jobs: 38 job PASS, 2 raw job FAIL attributed below, 0 job SKIP. The preserved output contains 377 package `ok` results and 2 package failures. All 13 top-level tests in the modified test file were present in green `cmd/gc` process shards. `test_cmd_scope: full-suite`; `waiver_ref: none`. |
| 4 | No high-severity review findings open | PASS | The review records no style, security, specification, or HIGH-severity finding; unresolved HIGH count is 0. |
| 5 | Final branch is clean | PASS | `git status --porcelain` was empty at the gated reviewed commit before this checklist was written. The isolated deploy branch is checked again after the gate commit. |
| 6 | Branch diverges cleanly from main | PASS | After fetching current `origin/main`, `git merge-tree --write-tree origin/main 51d73596966496bebe995d653f9cc724ac3e6282` exited 0 and produced tree `fac0e9746726c222ab091fa3e3a1bd559bfe0a1f`. No self-rebase was needed. |
| 7 | Single feature theme | PASS | Both reviewed commits and both changed files implement one `cmd/gc` behavior: keeping hook route/identity matching case-sensitive so case-distinct configured agents cannot see or claim each other's work. |

## Acceptance evidence

- The comparator uses exact equality after
  `agent.UnsanitizeQualifiedNameFromSession` on both operands; no
  `strings.EqualFold` remains in this seam.
- Canonical and dash/dot-encoded spellings remain equivalent, while
  `gascity/Builder` and `gascity/builder` are locked as distinct.
- The case-sensitivity rationale cites the case-sensitive config identity and
  preserves the legacy bound-template anti-collapse behavior.
- `git diff --name-status 2f2cea0dc2dfdbb6949422b4ae551c8175f11584...51d73596966496bebe995d653f9cc724ac3e6282`
  reports only:
  - `cmd/gc/cmd_hook_claim.go`
  - `cmd/gc/cmd_hook_visibility_test.go`
- `git diff --check` is clean.
- `assert_deploy_ancestry_scope origin/main 51d73596966496bebe995d653f9cc724ac3e6282 ga-fx2uog ga-0yppjl`
  exited 0; the two-commit ancestry is scoped to the confirmed build bead and
  introduces no `.claude/**` paths.

## Test evidence

```text
test_cmd: DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true LOCAL_TEST_LOG_DIR=/var/tmp/ga-fx2uog-full.F46wAS GO_TEST_TIMEOUT=30m make test-local-full-parallel
test_cmd_scope: full-suite
test_job_counts: 38 PASS, 2 raw FAIL, 0 SKIP (40 total)
test_package_counts: 377 ok, 2 raw FAIL, 42 no-test-files, 11 no-tests-to-run
policy_lane: make test-ci-policy — PASS
build_lane: go build ./cmd/gc/ — PASS
vet_lane: go vet ./... — PASS
ci_lane_run: n/a (no CI configuration change)
waiver_ref: none
full_log_dir: /var/tmp/ga-fx2uog-full.F46wAS
```

The full-suite runner's supported output granularity is job and package
results, not verbose per-test terminal events. It emitted zero skipped jobs;
no claim of zero suite-wide Go-test skips is made. Candidate-owned tests were
resolved by name from the shard inventories and green package results.

`diff_tests_executed`: every top-level test in the modified
`cmd_hook_visibility_test.go` file was present in one of the six green
process-backed `cmd/gc` shards:

- `TestHookRouteIdentitiesEqual` — PASS
- `TestHookClaimMatchesRouteToleratesSessionNameEncoding` — PASS
- `TestHookRouteIdentitiesEqualDotAxisRegression` — PASS
- `TestHookCandidateVisible` — PASS
- `TestHookCandidateVisibleWorkflowRunTargetFallback` — PASS
- `TestFilterForeignHookCandidatesFailsOpen` — PASS
- `TestFilterForeignHookCandidatesDropsForeignKeepsOwnAndUnrouted` — PASS
- `TestDoHookVisibilityIgnoredWhenEmpty` — PASS
- `TestDoHookDropsForeignAssigneeUnderVisibility` — PASS
- `TestDoHookKeepsUnroutedUnassignedWorkUnderVisibility` — PASS
- `TestDoHookGa1xaqgoRegression` — PASS
- `TestFilterForeignHookCandidatesPoolBaseRouteViaRoutedToIdentity` — PASS
- `TestFilterForeignHookCandidatesLegacyWorkflowControlAliasMatched` — PASS

`skip_justification`: no job or candidate-owned test skipped. The repository
contains no testcontainers import or pinned container image for this suite,
and the `dolt-tests-via-podman` cairn entry is absent; the rootless Podman
environment variables were nevertheless exported before the run.

## Failure attribution

Both failures satisfy all four clauses of the non-diff-owned failure protocol:
their test files are not modified by the candidate, their open trackers
predate this run and were opened during evaluation, a structural mechanism
proof lands for each, and neither failing path overlaps the two candidate
paths. This run's sightings were appended to the existing trackers.

- `TestBdFlagManifestCurrent` -> `ga-f0uceo` | clause 3(a), mechanism. The test
  compares `internal/bdflags`' checked manifest with the separately installed
  `bd --help` surface. A change confined to `cmd/gc`'s hook comparator and its
  table test cannot alter either input. The raw signature is the tracker's
  installed-`bd` flag-manifest drift.
- `TestE2E_SuspendResume_City` -> `ga-dqd7gf` | clause 3(a), mechanism. The
  fixture launches `e2eReportScript` directly and invokes only `gc suspend`,
  `gc session kill`, and `gc resume`; it never invokes the `gc hook`
  claim/display path that can call `hookRouteIdentitiesEqual`. Its 93.96-second
  missing-`citysus.report` signature matches the tracker, which also records
  exact-base reproductions.

`inconclusive-guard: n/a — a clause-3 mechanism proof landed for both raw failures.`

## Decision

PASS. The reviewed comparator tightening is one scoped feature, merges cleanly
with current main, satisfies its acceptance criteria, and is independently
reverified by the complete local test matrix with only tracked, unrelated raw
failures.
