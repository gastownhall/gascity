# Release gate: `ga-yxgivi` — Dolt orphan sweeper live-flock safety

- **Result:** PASS
- **Evaluated:** 2026-08-26 (America/Los_Angeles)
- **Reviewed commit:** `a2d50d633971c31ddaea7bf619f7e0c3c4be20c5`
- **Source branch (provenance only):** `builder/ga-63rfxj`
- **Base:** `origin/main@d7fe11583675375132ff25adc2ebb1ee252a9d84`
- **Deploy mode:** remote
- **Test environment:** rootless Podman API reachable at `unix:///run/user/1000/podman/podman.sock`; `TESTCONTAINERS_RYUK_DISABLED=true`; Dolt `2.2.3`; `lsof` at `/usr/bin/lsof`

## Criteria

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Review bead `ga-dkh58q` is closed with `verdict: pass`; its recorded deploy commit resolves exactly to the reviewed commit above. |
| 2 | Acceptance criteria met | PASS | The documented full-suite run exercised and passed all six diff-owned `internal/doltorphan` tests. It also passed `TestSweep_ReapsRealDoltDataDirAfterSIGKILL` under the same 40-job load. Together these cover the second `lsof` confirmation scan, fail-closed scan errors, held real NBS flock protection, and removal when a stale lock file is unheld. |
| 3 | Tests pass | PASS with attributed pre-existing failures | `test_cmd: GOFLAGS=-v GO_TEST_TIMEOUT=30m LOCAL_TEST_LOG_DIR=/var/tmp/ga-yxgivi-gate-r5.Y8duAY/jobs make test-local-full-parallel`; `test_cmd_scope: full-suite`. The 40-job union completed 33 PASS / 7 FAIL. Top-level test results were 44,661 PASS / 8 FAIL / 189 SKIP; all verbose result lines were 78,125 PASS / 26 FAIL / 275 SKIP. The seven unique failures satisfy all four pre-existing-failure attribution clauses below. `waiver_ref: none applicable`. |
| 3a | Pre-existing failures may be attributed | PASS | Every failure is outside the diff, has an exact tracker opened and verified in this session that predates the run, has a decisive non-diff cause, and has no path overlap with `go.mod` or `internal/doltorphan/**`. Full mappings are below. |
| 3b | Policy/lint lane | PASS with attributed generated-file finding | `make test-ci-policy`, `make check-gomod-replace`, `make check-native-dependency-surface`, `make check-eventexport-isolation`, `make check-core-boundary`, `make fmt-check`, `go vet ./...`, `make check-docs`, `go build ./...`, and the diff check passed. `make lint` found only three diagnostics in ignored generated `internal/api/dashboardspa/web/node_modules/flatted/.../flatted.go`; exact predating tracker `ga-di310j` covers that traversal. The candidate does not touch the dashboard or generated dependency tree. |
| 4 | No high-severity review findings open | PASS | The reviewer recorded no blocker, high, or major finding. The residual time-of-check/time-of-use window was explicitly classified as minor and non-blocking. |
| 5 | Final branch is clean | PASS | Before this gate record was written, `git status --short` was empty. Formatting, vet, docs, build, and `git diff --check origin/main...a2d50d633971c31ddaea7bf619f7e0c3c4be20c5` passed. |
| 6 | Branch diverges cleanly from main | PASS | No PR carries the reviewed commit. `git merge-tree --write-tree origin/main a2d50d633971c31ddaea7bf619f7e0c3c4be20c5` succeeded with tree `d243ef5fff51af4481bbb26d5a999d0142db2d06`; no self-rebase was needed. |
| 7 | Single feature theme | PASS | The four-commit range, including sibling work `ga-vbyn8v`, changes only Dolt orphan-sweeper safety: `go.mod`, `internal/doltorphan/sweep.go`, and `internal/doltorphan/sweep_test.go`. |

## Diff-owned tests

Each diff-owned test reported PASS in the required full-suite output; none skipped:

- `TestSweep_ConfirmsUnheldWithSecondLsofScanBeforeRemoving`
- `TestSweep_SkipsConfirmScanWhenFirstScanAlreadyHeld`
- `TestSweep_RemovesWhenBothScansAgreeUnheld`
- `TestSweep_ConfirmScanErrorFailsClosed`
- `TestSweep_RespectsRealDoltLockEvenWhenLsofMissesIt`
- `TestSweep_RemovesWhenDoltLockFileExistsButIsUnheld`

`diff_tests_executed: 6 PASS / 0 FAIL / 0 SKIP`.

The remaining skips are pre-existing platform, build-tag, live-infrastructure, helper-process, or opt-in coverage skips. Examples in this run include Darwin-only path-alias checks on Linux, tests requiring an enclosing tmux session or live external service, unsupported-OS cases, golden regeneration, and helper subprocess entry points. None is diff-owned and none exercises the changed orphan-sweeper path.

## Failure attribution

- `TestProviderLiveClaudeKindPath` -> `ga-fh1flg` | clause 3(a), mechanism: the live herdr launch was rejected because fleet tmux pane `w1:p1` was busy. The changed `internal/doltorphan` package is unreachable from this herdr test; tracker created 2026-08-18.
- `TestCatalogMatchesProductionWiringAndDocumentation` -> `ga-9vz14c` | clause 3(a), mechanism: runtime-provider waivers owned by the dead `ga-80po0c.3` reference expired on 2026-08-26. The candidate changes neither provider wiring nor the provider ledger; tracker created before this run on 2026-08-26. The same deterministic failure appeared in two jobs.
- `TestBdFlagManifestCurrent` -> `ga-f0uceo` | clause 3(a), mechanism: the installed `bd` binary exposes flags absent from the checked manifest. The candidate changes neither `bd` nor `internal/bdflags`; tracker created 2026-08-15.
- `TestConditionChecksRunConcurrentlyWithinTheirCap/cap-admits-the-wave` -> `ga-e7cxrg` | clause 3(d), base-condition reproduction: the predating tracker records the identical `3 order(s) fired, want 4` timing failure in an isolated repeated run without this diff. The candidate changes no order-gate code or test path; tracker created 2026-08-19.
- `TestGetKeyBinding_CapturesDefaultBinding` and `TestGetKeyBinding_CapturesDefaultBindingWithArgs` -> `ga-afqddr` | clause 3(a), mechanism: host tmux 3.7b returns an empty default keytable. The candidate changes no tmux path; tracker created 2026-08-15.
- `TestCleanInstallTutorialPath` -> `ga-io7xwr` | clause 3(a), mechanism: external `bd config get issue_prefix` stdout was polluted by circuit-breaker cleanup diagnostics before the correct `tra` value. The candidate cannot produce those `bd` log lines and changes no tutorial or circuit-breaker path; tracker created 2026-08-20.
- `make lint` generated `flatted.go` findings -> `ga-di310j` | clause 3(a), mechanism: repository-wide lint traverses an ignored generated `node_modules` Go source. The candidate has no dashboard/generated-tree path overlap; tracker created before this run on 2026-08-26. Independent `go vet ./...` passed.

For every mapping, clause 1 passes because the failing test/source is not added or modified by this diff; clause 2 passes because the cited tracker predates this run and was opened to verify it covers the exact signature; clause 3 is named above; clause 4 passes because the candidate paths are limited to `go.mod` and `internal/doltorphan/**`.

## Logs

- Full-suite wrapper: `/var/tmp/ga-yxgivi-gate-r5.Y8duAY/full-suite.log`
- Full-suite job logs: `/var/tmp/ga-yxgivi-gate-r5.Y8duAY/jobs/`
- Policy/static summary: `/var/tmp/ga-yxgivi-gate-r5.Y8duAY/policy-summary.tsv`
- Individual policy/static logs: `/var/tmp/ga-yxgivi-gate-r5.Y8duAY/*.log`

## Disposition

All release criteria pass. Cut the isolated deploy branch from the exact reviewed commit, commit this gate record, open the PR, publish deploy clearance on that exact PR head, and route the merge request to the merge authority. The deployer does not merge.
