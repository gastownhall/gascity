# Release Gate: credential-provider test hang budgets

Deploy bead: `ga-cb7cqw`  
Review bead: `ga-3olsjg`  
Build bead: `ga-42mt5x.3`  
Reviewed source: `c13b1b5fba1fc1eb8e4cecfbfe4c27f548a0bbbc`  
Base: `origin/main@5d3a8378474c4f2bcbc46323efbc40b31ea783ab`
Gate date: 2026-08-21

`docs/PROJECT_MANIFEST.md` is not present in this worktree. This record uses
the seven release criteria in the deployer contract and the repository's
documented test requirements in
`engdocs/contributors/release-gate-criteria-conventions.md` and `TESTING.md`.

## Gate Results

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead `ga-3olsjg` is closed with verdict PASS for the exact reviewed source. |
| 2 | Acceptance criteria met | PASS | The package defines one documented 60-second hang detector derived from `testutil.GoroutineRaceTimeout`; all 12 direct floor-as-deadline sites are converted, while scenario inputs and negative assertions remain unchanged. All converted waits return immediately when their condition resolves and use the budget only on a fatal wedge path. |
| 3 | Tests pass | PASS | The race-enabled credential-provider suite reports 79 PASS, 0 FAIL, 0 SKIP by default and 87 PASS, 0 FAIL, 0 SKIP with integration tags. Every runnable test in the modified Linux files passes by name; Windows-tagged tests cross-build and vet clean. The documented 40-job union reports 34 PASS jobs, 6 FAIL jobs, 0 job-level SKIP. Its six raw failures are tracked, outside this test-only package, logged on their trackers, and preserved below as FAIL — WAIVED under standing dispositions. Policy, build, full vet, targeted lint, lint-new, formatting, and diff checks pass. |
| 4 | No high-severity review findings open | PASS | The reviewer independently verified all converted sites are pure hang detectors, found no production-code or security surface, and reported no correctness or style defect. Unresolved HIGH findings: 0. |
| 5 | Final branch is clean | PASS | `git status --porcelain` was empty before adding this gate record; `git diff --check origin/main...HEAD` passes. |
| 6 | Branch diverges cleanly from main | PASS | After a final refresh, `git merge-tree --write-tree origin/main HEAD` succeeded against `origin/main@5d3a8378474c4f2bcbc46323efbc40b31ea783ab` and produced `37679d73346c104a8f9d239ca94d5e3573edd6ac`. The new main commit changes only beads endpoint/path-durability code and tests, outside this candidate's credential-provider test package. `assert_deploy_ancestry_scope` passed for the deploy, review, and build bead IDs. No self-rebase was required. |
| 7 | Single feature theme | PASS | One commit changes four `_test.go` files in `internal/credentialprovider` to apply one package-level hang-budget convention. No production file changes. |

## Acceptance Evidence

- `hangBudget` is `6 * testutil.GoroutineRaceTimeout` and is documented as a
  hang detector rather than a latency assertion.
- Cache coordination waits, Unix process cleanup waits, and Windows process
  cleanup waits now share that ceiling.
- Every converted timeout branch only reports a wedge; the real behavioral
  assertions are on the successful branch after the wait returns.
- `TestCredentialProviderWholeResponseTimeout` still receives its actual
  production timeout from `helperTimeout`; its observed run completes in about
  10 seconds, well before the test-side hang ceiling.
- The explicit 15-second output-drain assertion and the deadline-less Windows
  release wait are unchanged.
- A fresh search finds no remaining direct
  `testutil.ExecRaceTimeout`/`testutil.GoroutineRaceTimeout` deadlines in the
  package outside the single derivation in `hangbudget_test.go`.

## Test Evidence

```text
test_cmd: go test -race -json -count=1 -timeout 5m ./internal/credentialprovider/...
test_counts: 79 PASS, 0 FAIL, 0 SKIP
focused_log: /var/tmp/ga-cb7cqw-credentialprovider-race.json

test_cmd: go test -race -tags=integration -json -count=1 -timeout 5m ./internal/credentialprovider/...
test_counts: 87 PASS, 0 FAIL, 0 SKIP
focused_integration_log: /var/tmp/ga-cb7cqw-credentialprovider-integration-race.json
diff_tests_executed: 24 runnable Linux tests in cache_test.go and credentialprovider_process_unix_test.go = PASS by name; 0 FAIL, 0 SKIP
platform_tests: TestCredentialProviderWindowsJobKillsDescendants, TestCredentialProviderWindowsJobCloseKillsDescendantsAfterParentExit, and TestCredentialProviderWindowsProcessHelper are Windows-tagged and not runnable on this Linux host; GOOS=windows integration build and vet = PASS
waiver_ref: none for diff-owned tests

test_cmd: DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true LOCAL_TEST_JOBS=4 CMD_GC_PROCESS_TOTAL=6 GO_TEST_TIMEOUT=30m make test-local-full-parallel
test_counts: 34 PASS jobs, 6 FAIL jobs, 0 job-level SKIP (40 total)
full_log: /var/tmp/ga-cb7cqw-full.out
full_logs: /var/tmp/gc-local-tests.BTiZpt
environment: rootless Podman socket verified; repository-pinned dolthub/dolt-sql-server:2.1.7 present

policy_lane: make test-ci-policy — PASS
build_lane: go build ./... — PASS
static_lane: go vet ./... — PASS
windows_lane: GOOS=windows go build/go vet -tags=integration ./internal/credentialprovider/... — PASS
lint_lane: golangci-lint ./internal/credentialprovider/... — PASS, 0 issues
lint_new_lane: make lint-new from merge-base 599afe65be39c327a70a2986948a79b0993d8c45 — PASS, 0 new issues
format_lane: make fmt-check-changed — PASS
diff_lane: git diff --check origin/main...HEAD — PASS
```

The shared golangci-lint cache emitted one warning about a deleted `/var/tmp`
worktree while `lint-new` ran; the lane still reported zero candidate issues.
The direct credential-provider lint independently reported `0 issues`.

### Raw failures and standing disposition

The failures below remain failures in the raw output. None is diff-owned, none
is in `internal/credentialprovider`, and the candidate is test-only. Each
occurrence was written to and read back from its tracker before this gate was
signed. The standing authorization recorded on `ga-cqq3hs` permits proceeding
only when every failure has a specific tracker, the diff cannot reach the
failure mechanism, the occurrence is logged, and the gate preserves the raw
result as FAIL — WAIVED. Those conditions are satisfied here.

- FAIL — WAIVED: `TestProviderLiveClaudeKindPath` hit the tracked
  `agent_pane_busy` / startup-delivery timeout signature. Tracker: `ga-fh1flg`;
  standing disposition: `mayor-2026-08-20-herdr-pane-standing`.
- FAIL — WAIVED: `TestBdFlagManifestCurrent` reported the tracked installed-`bd`
  flag-manifest skew. Tracker: `ga-f0uceo`.
- FAIL — WAIVED: `TestGetKeyBinding_CapturesDefaultBinding` and
  `TestGetKeyBinding_CapturesDefaultBindingWithArgs` observed the tracked empty
  host-tmux default key table. Tracker: `ga-afqddr`.
- FAIL — WAIVED: `TestDoltConfigWiringExternalHost` exceeded its hard bd-init
  timeout after initialization had succeeded. Tracker: `ga-gajll3`; reviewed
  fix `09c63aa404929089238c7499a762001d8892f999` remains absent from main and is
  awaiting deployment on `ga-l4xwgh`.
- FAIL — WAIVED: `TestCleanInstallTutorialPath` failed during fixture store
  initialization with the exact `gastownhall/beads#4566` pending dirty
  `dependencies`-table migration signature. Trackers: `ga-lpfjhc` and
  `ga-6bnc42`.

```text
failure_attribution: TestProviderLiveClaudeKindPath -> ga-fh1flg + mayor-2026-08-20-herdr-pane-standing + separate-subsystem no-mechanism proof
failure_attribution: TestBdFlagManifestCurrent -> ga-f0uceo + separate-subsystem no-mechanism proof
failure_attribution: TestGetKeyBinding_CapturesDefaultBinding{,WithArgs} -> ga-afqddr + separate-subsystem no-mechanism proof
failure_attribution: TestDoltConfigWiringExternalHost -> ga-gajll3 + ga-l4xwgh pending fix + separate-package no-mechanism proof
failure_attribution: TestCleanInstallTutorialPath -> ga-lpfjhc + ga-6bnc42 + exact beads#4566 signature + separate-package no-mechanism proof
waiver_ref: ga-cqq3hs standing tracked-failure authorization; ga-6bnc42 beads#4566 authorization; mayor-2026-08-20-herdr-pane-standing
```

## Pre-push Hook Evidence

The first guarded push of gate commit
`e8df0e81895bbfc9b8b9682dc9ae3f27ce3f6ee1` ran the repository's 10-job fast
matrix. Nine jobs passed. `unit-core` failed only on
`TestProviderLiveClaudeKindPath`, with the exact `agent_pane_busy` /
startup-delivery timeout signature already tracked on `ga-fh1flg` and covered
by `mayor-2026-08-20-herdr-pane-standing`. The specific-head occurrence was
logged and read back before bypassing the hook. The candidate changes only
`internal/credentialprovider` test files and cannot affect the herdr provider or
pane allocation.

Under the standing tracked-failure authorization on `ga-cqq3hs`, this
specific-head failure permits `git push --no-verify`: it has an exact tracker,
is outside and unreachable from the candidate, is preserved here as FAIL —
WAIVED, and was recorded before bypass. The bypass authorizes only the push;
merge authority and remote CI remain unchanged.

```text
pre_push_cmd: LOCAL_TEST_JOBS=2 CMD_GC_PROCESS_TOTAL=6 ./scripts/test-local-parallel fast
pre_push_counts: 9 PASS jobs, 1 FAIL job, 0 SKIP jobs (10 total); 1 top-level test failure
pre_push_logs: /var/tmp/gc-local-tests.lkz0HX
failure_attribution: TestProviderLiveClaudeKindPath -> ga-fh1flg + mayor-2026-08-20-herdr-pane-standing + separate-subsystem no-mechanism proof
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
git merge-tree --write-tree origin/main c13b1b5fba1fc1eb8e4cecfbfe4c27f548a0bbbc
assert_deploy_ancestry_scope origin/main c13b1b5fba1fc1eb8e4cecfbfe4c27f548a0bbbc ga-cb7cqw ga-3olsjg ga-42mt5x.3
git diff --check origin/main...HEAD
go test -race -json -count=1 -timeout 5m ./internal/credentialprovider/...
go test -race -tags=integration -json -count=1 -timeout 5m ./internal/credentialprovider/...
DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true LOCAL_TEST_JOBS=4 CMD_GC_PROCESS_TOTAL=6 GO_TEST_TIMEOUT=30m make test-local-full-parallel
make test-ci-policy
go build ./...
go vet ./...
GOOS=windows go build -tags=integration ./internal/credentialprovider/...
GOOS=windows go vet -tags=integration ./internal/credentialprovider/...
golangci-lint run --allow-parallel-runners ./internal/credentialprovider/...
make lint-new
make fmt-check-changed
```
