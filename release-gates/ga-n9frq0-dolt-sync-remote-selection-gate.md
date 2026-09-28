# Release gate: deterministic Dolt sync remote selection

- Deploy bead: `ga-n9frq0`
- Review bead: `ga-qapoc6` (`verdict: pass`)
- Reviewed source: `abf9c263a78e00bf5a4dbe7e63dc82ac7eaf6d8d`
- Base checked: `origin/main@5d486251b8a448d31e41c6e826af473a69f6d1ea`
- Deploy mode: `remote` (`gastownhall/gascity`, push through the configured fork if origin is not writable)
- Evaluated: 2026-09-04

## Verdict

**PASS.** The reviewed change is ready for an isolated deploy branch and pull request. The full-suite run retained six raw failures; all six are attributed below to four pre-existing, non-diff-owned conditions under the release-gate attribution protocol.

## Criteria

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | `ga-qapoc6` is closed with `verdict: pass` and pins the exact reviewed commit above. The commit resolves locally to a commit object and equals the evaluated HEAD. |
| 2 | Acceptance criteria met | PASS | `select_remote` sorts candidates, honors `GC_DOLT_REMOTE_<DB>`, prefers a local `file://` remote, and returns `AMBIGUOUS` rather than selecting a non-local remote without an override. Both SQL and CLI paths use the shared selection policy. The new order-independence, ambiguous-remote, local-preference, override, and sole-non-local tests pass. |
| 3 | Tests pass | PASS with attributed failures | `make test-local-full-parallel` ran the documented full 40-job suite with the required rootless Podman environment: 34 jobs PASS, 6 jobs FAIL, 0 jobs SKIP. `examples/bd/dolt` passed in the full suite (`ok`, 80.820s). Every failure is non-diff-owned and attributed below. A verbose supplement, `go test -count=1 -v ./examples/bd/dolt/...`, recorded 305 PASS, 0 FAIL, 0 SKIP and maps all 47 top-level tests in the modified test files to PASS. |
| 3a | Pre-existing failures attributed | PASS | Trackers predate this run, cover the observed root conditions, and were opened before use. The candidate touches only `examples/bd/dolt/**`; none of the failing test paths overlaps the diff. Per-condition evidence is below. |
| 3b | Policy/lint lane | PASS | `make test-ci-policy`: PASS. `make lint-affected LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=734f18f45915399a900561c3f964e3af96cace0b`: PASS, 0 issues, including its standalone-vet closure. `make fmt-check-changed` with the same diff base: PASS. `make vet`: PASS. An earlier over-expanded invocation using the newer `origin/main` as a direct ref selected full scope because the candidate lacks a main-only gate file and found only tracked third-party `node_modules/flatted` diagnostics; that occurrence is attributed to `ga-bvixfw` and is not the candidate-diff lane. |
| 3c | CI-config lane | PASS | `ci_lane_run: n/a (no CI configuration, job, matrix, timeout, or required-check change in this diff)`. |
| 4 | No high-severity review findings open | PASS | `ga-qapoc6` records no blocking style or security findings and closes with PASS; unresolved HIGH count is 0. |
| 5 | Final branch clean | PASS | `git status --porcelain=v1` produced no output after all test and policy runs. |
| 6 | Branch diverges cleanly from main | PASS | After a final `git fetch origin main`, `git merge-tree --write-tree origin/main abf9c263a78e00bf5a4dbe7e63dc82ac7eaf6d8d` exited 0 and produced tree `cd08aec0dc1401a6ce68cdf31632ed206abd0c68`. No PR currently carries the reviewed SHA. |
| 7 | Single feature theme | PASS | The six-commit range changes only three files under `examples/bd/dolt` and implements/tests one behavior: safe deterministic remote selection for `gc dolt sync`. `assert_deploy_ancestry_scope` passed for `ga-n9frq0`, `ga-h9n6hq`, `ga-2w96wd`, and `ga-fqi7kq`. |

## Test evidence integrity

- `test_cmd: make test-local-full-parallel`
- `test_cmd_scope: full-suite`
- `test_counts: 34 PASS jobs, 6 FAIL jobs, 0 SKIP jobs`
- `diff_tests_executed: 47/47 top-level tests in examples/bd/dolt/sync_ffclassify_test.go and examples/bd/dolt/sync_test.go PASS`
- `skip_justification: none (0 skips)`
- `waiver_ref: none`
- `ci_lane_run: n/a (no CI-config change)`
- Full-suite log: `/var/tmp/ga-n9frq0-full-suite.log`
- Per-job logs: `/var/tmp/gc-local-tests.bdQ2DM/`
- Verbose Dolt log: `/var/tmp/ga-n9frq0-dolt-verbose.log`

### Failure attribution

- `TestBdFlagManifestCurrent -> ga-f0uceo | clause 3(a), mechanism`: the installed `bd` 1.1.0 build exposes newer flags than `internal/bdflags`; an `examples/bd/dolt` sync script/test diff cannot alter either the installed CLI or the flag manifest. Clause 1 passes (test not modified), clause 2 passes (tracker created 2026-08-15 and opened), and clause 4 passes (no path overlap).
- `TestPersonalWorkFormulaCompileAndRun`, `TestAdoptPRFormulaSoftFailsGeminiAfterTransientRetries`, and `TestHumaBinary_SessionMessageAsync -> ga-esyijp | clause 3(a), mechanism`: all fail during fixture `bd init`, before formula/API assertions, with the tracked beads#4566 dirty issues/comments schema-migration refusal. The candidate cannot alter schema migration or fixture bootstrap. Clauses 1, 2, and 4 pass.
- `test/integration TestMain pinned-bd installation -> ga-qwkwiv | clause 3(a), mechanism`: DNS for `proxy.golang.org` failed through the host resolver before the REST shard could run. The candidate cannot alter host DNS or the pre-test pinned-binary installer. The open tracker predates this run and records the same full-gate DNS-loss condition on an unrelated candidate; there is no path overlap.
- `TestE2E_SuspendResume_City -> ga-dqd7gf | clause 3(b), cross-candidate`: the exact missing `citysus.report` full-suite timeout was already recorded on an unrelated candidate. The test and session-report path do not overlap or import the changed example package. Clauses 1, 2, and 4 pass.
- Initial over-expanded full-lint diagnostics in third-party `internal/api/dashboardspa/web/node_modules/flatted/** -> ga-bvixfw | clause 3(a), mechanism`: the candidate does not modify or generate that untracked dependency tree. The correctly scoped affected-diff lane passed with 0 issues.

Each tracker received this run's bead, reviewed SHA, shard, log path, and symptom, and the appended comment was read back after writing.

### Diff-owned test map

The verbose supplement recorded PASS for every top-level test in the two modified test files:

`TestSyncAheadOnlyFastForwardPushes`, `TestSyncBehindRefusesAndDoesNotPush`, `TestSyncCLIFallbackIgnoresNestedRepoStateHead`, `TestSyncCLIFallbackPushesOriginMain`, `TestSyncCLIFallbackReadsRepoStateForActiveBranch`, `TestSyncCLIForcePushReportsExitCode`, `TestSyncCLIPushReportsExitCode`, `TestSyncDivergedRefusesAndDoesNotPush`, `TestSyncDryRunShowsResolvedActiveBranch`, `TestSyncEmptyRemoteFirstPushPushes`, `TestSyncFetchTimeoutSkipsNeverPushes`, `TestSyncFirstPushWhenRemoteRefAbsentPushes`, `TestSyncForceStillPushesWhenDiverged`, `TestSyncForceUsesRefspecEnvOverrideWithLiveSQL`, `TestSyncForceUsesResolvedActiveBranchWithLiveSQL`, `TestSyncForceUsesSetUpstreamWithLiveSQL`, `TestSyncMultiRemoteAmbiguousNonLocalSkipsAndNeverPushes`, `TestSyncMultiRemotePrefersLocalOverGitHttpsRemote`, `TestSyncPushesActiveBranchWhenSet`, `TestSyncRefspecEnvOverride`, `TestSyncRefspecEnvOverrideHyphenInDBName`, `TestSyncRefspecInvalidOverrideFails`, `TestSyncRefspecOptionShapedOverrideFails`, `TestSyncRejectsEmptyPushTimeout`, `TestSyncRejectsInvalidFetchTimeout`, `TestSyncRejectsLeadingZeroPushTimeout`, `TestSyncRejectsNonNumericPushTimeout`, `TestSyncRejectsTripleZeroPushTimeout`, `TestSyncRejectsZeroPushTimeout`, `TestSyncRemoteEnvOverridePinsNonLocalRemote`, `TestSyncRemoteSelectionIsOrderIndependent`, `TestSyncReportsLiveSQLRemoteLookupFailure`, `TestSyncSQLPushEmptyStderrNoBlankLines`, `TestSyncSQLPushReplayDoesNotLeakPassword`, `TestSyncSQLPushReplaysStderr`, `TestSyncSQLPushReplaysStderrFinalLineWithoutTrailingNewline`, `TestSyncSQLPushReportsExitCode`, `TestSyncSQLPushTempFileFailureDegradesPerDb`, `TestSyncSQLPushTimeoutHonorsConfiguredCeiling`, `TestSyncSQLPushTimeoutReplaysNoMechanismMarker`, `TestSyncSQLPushTimeoutReportsTimeout`, `TestSyncSkipsDatabasesWithNoSyncMarker`, `TestSyncSoleNonLocalRemoteSkipsAndNeverPushes`, `TestSyncSummaryNamesFailedDatabaseAmongHealthyOnes`, `TestSyncUpToDateSkipsPush`, `TestSyncUsesLiveSQLWhenManagedServerReachable`, and `TestSyncWarnsWhenActiveBranchFallbacksToMain`.

## Acceptance evidence

- The final diff is 317 insertions and 49 deletions across `commands/sync/run.sh`, `sync_ffclassify_test.go`, and `sync_test.go`.
- Candidate rows are sorted before selection, so database row order cannot change the winner.
- An explicit configured remote name must match a real candidate and overrides scheme preference.
- Without an override, local `file://` candidates are preferred; all-non-local sets, including a sole non-local remote, are skipped as ambiguous rather than pushed.
- SQL and CLI modes both call the same `select_remote` policy.
- The existing multi-database summary fixture now uses a local remote and still passes under the stricter selection policy.

