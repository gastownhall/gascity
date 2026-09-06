# Release gate: keep the non-live session census cache-served

- Deploy bead: `ga-px24i7`
- Build bead: `ga-tr8iti`
- Review bead: `ga-t05wsj`
- Reviewed source: `54d8fba222542fdd21655ad606de2e2d266ab5ff`
- Base checked: `origin/main@1130f27c902fb5b320b2bfe4d37f91cc3ec78ab2`
- Planned deploy branch: `deploy/ga-px24i7-gate`
- Deploy mode: remote; push target: `fork`
- Evaluated: 2026-09-04
- Verdict: **PASS with attributed and pre-authorized raw test failures**

`docs/PROJECT_MANIFEST.md` is absent from both the reviewed source and the
evaluated base. This record therefore applies the active seven deployer
criteria, the build bead's done-when criteria, `TESTING.md`, and
`engdocs/contributors/release-gate-criteria-conventions.md`.

## Gate checklist

The target pre-flight ran before criterion 6. The recorded source resolved to
the full commit above and is not associated with an existing pull request.
Criterion 6 then passed against the current fetched base, so the remaining
criteria were evaluated.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Closed review bead `ga-t05wsj` records `verdict: pass` on the exact reviewed source. It records no style, security, specification, or coverage finding. No review carryover is involved. |
| 2 | Acceptance criteria met | **PASS** | `listAllBeads` now supplies `Status: "open"` only for non-live queries that exclude closed sessions, allowing a partially primed `CachingStore` to serve the session census without changing live-query shape. The new deterministic test primes the cache, writes a session directly to the backing store, and proves the ordinary census remains on the primed snapshot. The change does not touch `hookRouteIdentitiesEqual`, `cacheServableForListQueryLocked`, PR #5991, or other empty-status callers. The full documented runner exercised all `cmd/gc` process shards; the changed package, build, and vet checks passed independently. |
| 3 | Tests pass | **PASS with attributed/waived raw failures** | The documented full-suite command scheduled and completed all 40 jobs: **36 PASS / 4 raw FAIL / 0 SKIP jobs**. Those jobs contain five top-level raw test failures, all non-diff-owned and adjudicated under criterion 3a. A fresh full-package JSON run reported **1,096 PASS / 0 FAIL / 0 SKIP test events** and proved the one diff-owned test passed by name. `test_cmd_scope: full-suite`; the limited dirty-schema waiver is identified below. |
| 3a | Pre-existing failures attributed | **PASS** | `TestSendReloadControlRequestInvalidConfig` is tracked by open condition tracker `ga-vkhfnj` with exact cross-PR evidence from PR #5610 / `ga-42hj7l`; `TestBdFlagManifestCurrent` is tracked by `ga-f0uceo` with a package-reachability proof; the two beads#4566 bootstrap failures are tracked by `ga-esyijp` and remain raw **FAIL-WAIVED** under the standing authorization on `ga-lpfjhc` / `ga-6bnc42`; `TestE2E_SuspendResume_City` is tracked by `ga-dqd7gf` with exact cross-PR evidence from PR #5885. Every open tracker predates this run, no failing test or condition is diff-owned, and no failing path overlaps the diff. Current sightings were appended and read back from the ledger. |
| 3b | Policy and static lanes | **PASS** | `make test-ci-policy`, dependency-surface checks, event-export isolation, the core boundary check, native DoltLite tests, affected lint, changed formatting, and docs synchronization all exited 0. Affected lint reported `0 issues`. `go build ./...`, `go vet ./...`, and `git diff --check` also passed. `policy_lane: required repository policy/static command — PASS`. |
| 3c | CI-config lane | **PASS — n/a** | `ci_lane_run: n/a (no CI job, matrix, timeout, workflow, or required-check change)`. |
| 4 | No high-severity review findings open | **PASS** | Unresolved HIGH findings: 0. The review-of-record reports no blocking correctness, security, style, specification, or coverage finding. |
| 5 | Final branch is clean | **PASS** | `git status --short` produced no output after the test and static runs and before this checklist was added. The checklist is the only deployer-authored file and will be committed separately on the isolated deploy branch. |
| 6 | Branch diverges cleanly from main | **PASS** | After fetching the base, `git merge-tree --write-tree origin/main 54d8fba222542fdd21655ad606de2e2d266ab5ff` exited 0, produced tree `be7a951be9c6e9a7fd113ef23a50b64ddb68692e`, and reported no conflict. No bounded self-rebase was needed. |
| 7 | Single feature theme | **PASS** | The reviewed RED/GREEN commits change two files in `internal/session` for one behavior: preserve partial-cache service for the non-live session census without altering live queries. No independent feature is bundled. |

## Full-suite test evidence

The environment was prepared before the run:

```text
DOCKER_HOST=unix:///run/user/1000/podman/podman.sock
TESTCONTAINERS_RYUK_DISABLED=true
LOCAL_TEST_LOG_DIR=/var/tmp/ga-px24i7-full.H8FmMr/jobs
LOCAL_TEST_JOBS=4 CMD_GC_PROCESS_TOTAL=6 GO_TEST_TIMEOUT=30m \
  make test-local-full-parallel
```

The rootless Podman socket was present and `podman info` reported rootless
operation with `crun`. No pinned testcontainers image reference exists in the
candidate's test surface. All 40 jobs reached a terminal state; raw logs are
retained in the directory above.

- `test_cmd_scope: full-suite`
- `test_counts: 36 PASS jobs, 4 attributed raw FAIL jobs, 0 SKIP jobs`
- `raw_test_failures: 5 attributed/waived FAIL, none diff-owned`
- `diff_tests_executed: 1 PASS, 0 FAIL, 0 SKIP`
- `skip_justification: n/a; no job-level or diff-owned test skip occurred`
- `waiver_ref: ga-lpfjhc / ga-6bnc42, limited to the two exact beads#4566 dirty-schema bootstrap failures`
- `ci_lane_run: n/a (no CI-config change)`

The fresh supplemental command ran the complete changed package, not a named
filter:

```text
go test -json -count=1 -timeout 15m ./internal/session/...
```

It produced 1,096 PASS, 0 FAIL, and 0 SKIP terminal test events. The diff-owned
`TestListAll_CachePartialDoesNotReadThroughToBackingStore` event was PASS.

## Raw failures and attribution

| Raw result | Test | Tracker and proof |
|---|---|---|
| **FAIL — ATTRIBUTED** | `TestSendReloadControlRequestInvalidConfig` | Open root-condition tracker `ga-vkhfnj`, created 2026-08-29. Clause 3(b) cross-PR proof: closed tracker `ga-42hj7l` records the same initial-reconcile timeout on PR #5610 candidate `e10dcfcb814e3950a29ae4600f1107d7db08dd04`, whose only paths were `TESTING.md` and `internal/testutil/providerledger`. This candidate changes only `internal/session/list_all.go` and its test; no failing-path overlap exists. |
| **FAIL — ATTRIBUTED** | `TestBdFlagManifestCurrent` and its 17 manifest subtests | Open tracker `ga-f0uceo`, created 2026-08-15. Clause 3(a) mechanism proof: the test compares the host-installed `bd` help surface to `internal/bdflags`; `go list -deps ./internal/bdflags` does not reach `internal/session`, and neither the executable nor the manifest is changed. No path overlap exists. |
| **FAIL — WAIVED** | `TestAdoptPRFormulaSoftFailsGeminiAfterTransientRetries` | Open root-condition tracker `ga-esyijp`, created 2026-08-29. Clause 3(a) mechanism proof: fixture `gc init` stopped in the beads#4566 migration guard on a dirty `issues` table before the formula assertion. The candidate cannot change Dolt schema migration or store bootstrap. The exact occurrence is recorded on `ga-lpfjhc` under the standing mayor authorization on `ga-6bnc42`. No path overlap exists. |
| **FAIL — ATTRIBUTED** | `TestE2E_SuspendResume_City` | Open tracker `ga-dqd7gf`, created 2026-09-02. Clause 3(b) cross-PR proof: its first occurrence records the same missing `citysus.report` timeout on PR #5885 candidate `a774fee25e76c75f746b70603e2d08e02882aacf`, whose diff was limited to `.trivyignore.yaml` and `scripts/container_tool_security_test.go`. No failing-path overlap exists. |
| **FAIL — WAIVED** | `TestCleanInstallTutorialPath` | Open root-condition tracker `ga-esyijp`. Clause 3(a) mechanism proof: fixture bootstrap stopped on dirty `dependencies` plus a missing `leases` table, the exact beads#4566 family, before tutorial assertions. The candidate cannot change schema migration or store bootstrap. The exact occurrence is recorded on `ga-lpfjhc` under the standing mayor authorization on `ga-6bnc42`. No path overlap exists. |

`failure_attribution`:

- `TestSendReloadControlRequestInvalidConfig -> ga-vkhfnj | cross-PR proof via PR #5610 / ga-42hj7l`
- `TestBdFlagManifestCurrent -> ga-f0uceo | package-reachability mechanism proof`
- `TestAdoptPRFormulaSoftFailsGeminiAfterTransientRetries -> ga-esyijp | dirty-schema mechanism; FAIL-WAIVED by ga-lpfjhc / ga-6bnc42`
- `TestE2E_SuspendResume_City -> ga-dqd7gf | cross-PR proof via PR #5885`
- `TestCleanInstallTutorialPath -> ga-esyijp | dirty-schema mechanism; FAIL-WAIVED by ga-lpfjhc / ga-6bnc42`

`inconclusive-guard: n/a — every attribution has a landed mechanism or
cross-PR proof.` The candidate adds one test inside an existing package target
and changes no resource-census baseline or test target.

## Policy and static evidence

```text
make test-ci-policy                                                     PASS
make check-gomod-replace check-native-dependency-surface                PASS
make check-eventexport-isolation check-core-boundary                    PASS
make test-native-doltlite-beads                                         PASS
LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main \
  make lint-affected fmt-check-changed                                  PASS (0 issues)
make check-docs                                                         PASS
go build ./...                                                          PASS
go vet ./...                                                            PASS
git diff --check origin/main...54d8fba222542fdd21655ad606de2e2d266ab5ff       PASS
git config --get core.hooksPath                                         .githooks
```

## Source, pre-flight, and ancestry evidence

- The recorded candidate passed the hex-only guard and resolved with
  `git rev-parse --verify --quiet` to the exact full reviewed source above.
- `gh api repos/gastownhall/gascity/commits/54d8fba222542fdd21655ad606de2e2d266ab5ff/pulls`
  returned an empty array, so no merged or closed-PR reconciliation applies.
- The source range is the TDD pair `e45bbf1ac6e6f4797372e40b4f6f4fcd28ec25be`
  (RED) and `54d8fba222542fdd21655ad606de2e2d266ab5ff` (GREEN), both citing
  implementation bead `ga-tr8iti`.
- `assert_deploy_ancestry_scope origin/main 54d8fba222542fdd21655ad606de2e2d266ab5ff ga-px24i7 ga-tr8iti ga-t05wsj`
  passed. No unrelated commit or `.claude/**` path rides in the deploy range.

## Disposition

Gate PASS. Create `deploy/ga-px24i7-gate` from the exact reviewed source,
commit this checklist, push only the isolated branch to `fork`, open a pull
request against `main`, publish `release-gate/deploy-clearance=success` on the
exact pull-request head, and route the merge request to mayor/mpr. The deployer
does not merge.
