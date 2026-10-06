# Release Gate: named-session alias canonical detection

Deploy bead: `ga-rcroz7`  
Review bead: `ga-d3whdg`  
Build bead: `ga-89kxkd`  
Reviewed source: `15285684898d752028a52b36c81a364a2d63898a`  
Base: `origin/main@187e53828754894096fc295cea4baca909fe9a96`  
Gate date: 2026-08-21

`docs/PROJECT_MANIFEST.md` is not present in this worktree. This record uses
the seven release criteria in the deployer contract and the repository's
documented test requirements in
`engdocs/contributors/release-gate-criteria-conventions.md` and `TESTING.md`.

## Gate Results

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead `ga-d3whdg` is closed with verdict PASS for the exact reviewed source. |
| 2 | Acceptance criteria met | PASS | A unique, live, eligible alias candidate with the correct named-session template/spec is promoted to canonical in both Bead and Info lookup shapes; multiple matching live candidates still fall through to the existing conflict path. The five focused regression and collision-safety tests pass by name. Direction A stamping hardening remains intentionally out of scope. |
| 3 | Tests pass | PASS | The race-enabled `internal/session` suite reports 1,078 PASS, 0 FAIL, 0 SKIP; all five diff-owned tests pass by name. The documented 40-job local CI union reports 34 PASS jobs, 6 FAIL jobs, 0 job-level SKIP. Its six raw failures are each tracked, outside the changed subsystem, logged on their trackers, and preserved below as FAIL — WAIVED under the mayor's standing tracked-failure authorization. `make test-ci-policy`, `go build ./...`, `go vet ./...`, targeted lint, lint-new, formatting, and diff checks pass. |
| 4 | No high-severity review findings open | PASS | The reviewer found no security, correctness, dependency, or style issue. The alias pass retains the pre-existing liveness/eligibility gates, adds template/spec corroboration and uniqueness, and does not weaken true-collision detection. Unresolved HIGH findings: 0. |
| 5 | Final branch is clean | PASS | `git status --porcelain` was empty before adding this gate record; `git diff --check origin/main...HEAD` passes. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main 15285684898d752028a52b36c81a364a2d63898a` succeeded against the recorded base and produced `c29c3baa0582a6aa30484d1141e306f7ce01eb71`. `assert_deploy_ancestry_scope` passed for the deploy, review, and build bead IDs. No self-rebase was required. |
| 7 | Single feature theme | PASS | Two commits change only `internal/session/named_config.go` and its test file to fix one named-session alias self-conflict behavior. |

## Acceptance Evidence

- `FindCanonicalNamedSessionBead` and `FindCanonicalNamedSessionInfo` each add
  the same alias-based canonical pass.
- The pass requires a repairable/live bead, named-session continuity
  eligibility, a matching named-session spec, and exactly one qualifying alias
  candidate.
- Two-candidate Bead and Info tests prove a genuine collision is not promoted
  to an arbitrary canonical winner.
- `LookupConfiguredNamedSession` resolves the sole legitimate alias owner after
  session start instead of reporting that session as conflicting with itself.
- No API shape, configuration, persistence schema, dependency, or unrelated
  runtime provider changes.

## Test Evidence

```text
test_cmd: go test -race -json -count=1 -timeout 15m ./internal/session/...
test_counts: 1,078 PASS, 0 FAIL, 0 SKIP
focused_log: /var/tmp/ga-rcroz7-session-race.json
diff_tests_executed:
  TestFindCanonicalNamedSessionBead_AliasSoleLiveCandidateIsCanonical=PASS
  TestFindCanonicalNamedSessionBead_AliasMatchNotPromotedWithSecondLiveCandidate=PASS
  TestFindCanonicalNamedSessionInfo_AliasSoleLiveCandidateIsCanonical=PASS
  TestFindCanonicalNamedSessionInfo_AliasMatchNotPromotedWithSecondLiveCandidate=PASS
  TestLookupConfiguredNamedSession_AliasOnlyLiveBeadResolvesCanonical=PASS
waiver_ref: none for diff-owned tests

test_cmd: DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true LOCAL_TEST_JOBS=4 CMD_GC_PROCESS_TOTAL=6 GO_TEST_TIMEOUT=30m make test-local-full-parallel
test_counts: 34 PASS jobs, 6 FAIL jobs, 0 job-level SKIP (40 total)
full_log: /var/tmp/ga-rcroz7-full.out
full_logs: /var/tmp/gc-local-tests.OIsZkW

base_control: exact origin/main@187e53828754894096fc295cea4baca909fe9a96 under the same 40-job topology
base_counts: 34 PASS jobs, 6 FAIL jobs, 0 job-level SKIP
base_full_log: /var/tmp/ga-hgtxtq-base.AlrVxt/base-full.out
base_logs: /var/tmp/gc-local-tests.5SMCnj

policy_lane: make test-ci-policy — PASS
build_lane: go build ./... — PASS
static_lane: go vet ./... — PASS
lint_lane: golangci-lint ./internal/session/... — PASS, 0 issues
lint_new_lane: make lint-new from merge-base 599afe65be39c327a70a2986948a79b0993d8c45 — PASS, 0 new issues
format_lane: make fmt-check-changed — PASS
diff_lane: git diff --check origin/main...HEAD — PASS
```

`make lint-affected` selected every reverse dependent of `internal/session` and
reported 179 pre-existing repository diagnostics, including stale shared-cache
paths under deleted `/var/tmp` worktrees. The candidate-specific `lint-new`
lane and direct `internal/session` lint both report zero issues; the failed
repository-wide reverse-dependent scan is retained in
`/var/tmp/ga-rcroz7-lint-mergebase.out` and is not represented as green.

### Raw failures and standing disposition

The failures below remain failures in the raw output. None is diff-owned, and
none is in the changed `internal/session` subsystem. Each occurrence was
written to and read back from its tracker before this gate was signed. The
standing authorization recorded on `ga-cqq3hs` allows a builder or deployer to
proceed only when every failure has a specific tracker, the diff cannot reach
the failure mechanism, the occurrence is logged, and the gate preserves the
raw result as FAIL — WAIVED. Those conditions are satisfied here.

- FAIL — WAIVED: `TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix`
  failed after a Dolt socket I/O timeout and invalid connection during parallel
  `cmd/gc` fixture bootstrap. Tracker: `ga-p6p2rt`. The named-session lookup
  diff cannot affect Dolt connection creation, bootstrap, or circuit-breaker
  behavior.
- FAIL — WAIVED: `TestBdFlagManifestCurrent` reported the tracked installed-`bd`
  manifest skew. Tracker: `ga-f0uceo`. The exact failure reproduced on the
  recorded base, and the diff cannot alter `internal/bdflags` or the host CLI.
- FAIL — WAIVED: `TestGetKeyBinding_CapturesDefaultBinding` and
  `TestGetKeyBinding_CapturesDefaultBindingWithArgs` observed the tracked empty
  host-tmux default key table. Tracker: `ga-afqddr`. Both reproduced on the
  recorded base; the diff cannot reach `internal/runtime/tmux`.
- FAIL — WAIVED: `TestProviderLiveClaudeKindPath` hit the tracked
  `agent_pane_busy` signature. Tracker: `ga-fh1flg`; standing disposition:
  `mayor-2026-08-20-herdr-pane-standing`. It also occurred in the exact-base
  control, and the diff cannot reach the herdr provider or pane allocation.
- FAIL — WAIVED: `TestHumaBinary_CityCreateAsync` failed during fixture store
  initialization with the exact `gastownhall/beads#4566` pending dirty
  `issues`-table schema-migration signature. Trackers: `ga-lpfjhc` and
  `ga-6bnc42`. The occurrence was logged with deploy/build IDs, and the diff has
  no plausible schema-migration or store-bootstrap mechanism.

```text
failure_attribution: TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix -> ga-p6p2rt + separate-subsystem no-mechanism proof
failure_attribution: TestBdFlagManifestCurrent -> ga-f0uceo + exact-base reproduction + separate-subsystem no-mechanism proof
failure_attribution: TestGetKeyBinding_CapturesDefaultBinding{,WithArgs} -> ga-afqddr + exact-base reproduction + separate-subsystem no-mechanism proof
failure_attribution: TestProviderLiveClaudeKindPath -> ga-fh1flg + mayor-2026-08-20-herdr-pane-standing + exact-base occurrence + separate-subsystem no-mechanism proof
failure_attribution: TestHumaBinary_CityCreateAsync -> ga-lpfjhc + ga-6bnc42 + exact beads#4566 signature + separate-subsystem no-mechanism proof
waiver_ref: ga-cqq3hs standing tracked-failure authorization; ga-6bnc42 beads#4566 authorization; mayor-2026-08-20-herdr-pane-standing
```

## Pre-push Hook Evidence

The first guarded push of gate commit
`a28611688cadbb697fa8b14baf691f3f94f72497` ran the repository's 10-job fast
matrix. Nine jobs passed; `unit-core` failed on two tracked tests outside the
candidate subsystem. The push itself was rejected and nothing reached the
remote on that attempt.

- FAIL — WAIVED: `TestProviderLiveClaudeKindPath` repeated the exact
  `agent_pane_busy` / startup-delivery timeout signature already tracked on
  `ga-fh1flg` and covered by `mayor-2026-08-20-herdr-pane-standing`. The
  specific pre-push occurrence was logged and read back.
- FAIL — WAIVED: `TestDoStartSession_TreatsDeadlineAfterPostReadyAsSuccessWhenSessionAlive`
  returned `context deadline exceeded` instead of success under the fast
  matrix. This first recorded post-ready recurrence is tracked by
  `ga-vve1ws`; its exact failure, log, and candidate head were verified in the
  ledger. `go list -deps ./internal/runtime/tmux` confirms the failing package
  does not depend on `internal/session`, so the named-session lookup diff
  cannot affect `doStartSession`, `fakeStartOps`, context timing, or tmux
  startup behavior.

Under the standing tracked-failure authorization on `ga-cqq3hs`, these two
specific-head failures permit `git push --no-verify`: both have exact trackers,
neither is diff-owned or reachable from the candidate, both are preserved as
FAIL — WAIVED, and both occurrences were recorded before bypassing the hook.
The bypass authorizes only the push; merge authority and all remote CI checks
remain unchanged.

```text
pre_push_cmd: LOCAL_TEST_JOBS=5 CMD_GC_PROCESS_TOTAL=6 ./scripts/test-local-parallel fast
pre_push_counts: 9 PASS jobs, 1 FAIL job, 0 SKIP jobs (10 total); 2 top-level test failures
pre_push_logs: /var/tmp/gc-local-tests.tD3CWw
failure_attribution: TestProviderLiveClaudeKindPath -> ga-fh1flg + mayor-2026-08-20-herdr-pane-standing + separate-subsystem no-mechanism proof
failure_attribution: TestDoStartSession_TreatsDeadlineAfterPostReadyAsSuccessWhenSessionAlive -> ga-vve1ws + dependency-graph no-mechanism proof
push_disposition: authorized --no-verify under ga-cqq3hs standing tracked-failure rule
```

## Pre-flight

GitHub's commit-to-PR lookup returned no PR for the reviewed source after the
final `origin/main` refresh. The target has not already merged or been
superseded through a PR, so normal isolated-branch deployment applies. The
builder branch is provenance only.

## Commands

```text
git fetch origin
git merge-tree --write-tree origin/main 15285684898d752028a52b36c81a364a2d63898a
assert_deploy_ancestry_scope origin/main 15285684898d752028a52b36c81a364a2d63898a ga-rcroz7 ga-89kxkd ga-d3whdg
git diff --check origin/main...HEAD
go test -race -json -count=1 -timeout 15m ./internal/session/...
DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true LOCAL_TEST_JOBS=4 CMD_GC_PROCESS_TOTAL=6 GO_TEST_TIMEOUT=30m make test-local-full-parallel
make test-ci-policy
go build ./...
go vet ./...
golangci-lint run --allow-parallel-runners ./internal/session/...
make lint-new
make fmt-check-changed
```
