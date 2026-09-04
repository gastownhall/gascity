# Release gate: Dolt sync failure summary

- Deploy bead: `ga-g514cl`
- Build bead: `ga-0uzqsf`
- Review bead: `ga-01v8g7`
- Reviewed source: `2e35396a6d82d9f9a5e0101e41420ff79c48c719`
- Base: `origin/main@145cc2be9b2be3b16aedfd64fe884820a27c6d3e`
- Deploy mode: `remote`
- Overall result: **PASS**
- Additional manifest criteria: none; `docs/PROJECT_MANIFEST.md` is absent at the reviewed source.

## Gate checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | The deploy bead contains a fresh `gascity/reviewer` verdict of PASS for the exact rewritten commit `2e35396a6d82d9f9a5e0101e41420ff79c48c719`. The reviewer verified that the rewrite changed commit messages only, preserved both tree objects, fixed the accepted bead citations, and re-ran build, vet, and package tests. |
| 2 | Acceptance criteria met | **PASS** | D1 is already present: `03549ad968` is an ancestor of the reviewed source, so failed Order Events retain the bounded, redacted output tail. D2 is supplied here: the sync command records each failed database and emits `sync: N/M database(s) failed: name (reason)` as its last stderr line. The full-scope verbose run records `TestSyncCLIPushReportsExitCode` PASS and `TestSyncSummaryNamesFailedDatabaseAmongHealthyOnes` PASS. |
| 3 | Tests pass | **PASS** | `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true LOCAL_TEST_JOBS=4 GOFLAGS=-v make test-local-full-parallel`; `test_cmd_scope: full-suite`. Rootless Podman 5.8.4 was reachable and `docker.io/dolthub/dolt-sql-server:2.2.0` was cached before the run. Result: 31/40 jobs PASS, 9/40 jobs raw FAIL, 0 skipped jobs; 45,540 top-level test executions PASS, 10 raw FAIL, 187 SKIP. Every raw failure is attributed below to a predating tracker. `diff_tests_executed: TestSyncCLIPushReportsExitCode PASS; TestSyncSummaryNamesFailedDatabaseAmongHealthyOnes PASS`. |
| 3a | Pre-existing failures may be attributed | **PASS** | All failing test files are outside the two changed paths. The failing test sources contain no reference to `commands/sync/run.sh`; the fixture failures occur in `bd init` before any sync order runs. Exact trackers, mechanism proofs, and raw dispositions are recorded below and appended to the trackers. |
| 3b | Policy/lint lane | **PASS** | PASS: `go build ./...`, `go vet ./...`, `make test-ci-policy`, `make check-gomod-replace`, `make check-eventexport-isolation`, `make check-core-boundary`, `make check-docs`, `make test-native-doltlite-beads`, and changed-scope `make fmt-check-changed`. Raw `make lint-affected` FAIL is attributed to `ga-u8z8j6`: an isolated-cache rerun widened to the full repository because a newer workflow is absent from this reviewed head, then found only three vendored `node_modules/flatted` findings. Raw `make check-native-dependency-surface` FAIL at 270,285,336 bytes is attributed to `ga-5flk3r`; current `origin/main` independently fails the same 270,000,000-byte ceiling at 270,267,744 bytes. |
| 4 | No unresolved high-severity review findings | **PASS** | Fresh reviewer verdict says “No blockers found.” Unresolved HIGH findings: 0. |
| 5 | Final branch clean | **PASS** | `git status --short` was empty at the reviewed source before this gate artifact was added. |
| 6 | Branch diverges cleanly from main | **PASS** | `git merge-tree --write-tree origin/main 2e35396a6d82d9f9a5e0101e41420ff79c48c719` exited 0 and produced tree `8321ea2c29a1a103b55b5560f71fe47ef685d7cc`. No self-rebase was needed. Preflight found no PR carrying the reviewed commit. |
| 7 | Single feature theme | **PASS** | Three commits modify only `examples/bd/dolt/commands/sync/run.sh` and `examples/bd/dolt/sync_test.go`: one operator-facing failure-summary behavior and its tests/refactor. |

## Full-suite failure attribution

All four attribution clauses pass for each entry: the test is not diff-owned; the cited tracker predates this run and covers the exact signature; the failing path does not execute the changed sync command; and neither changed path overlaps the failing test package.

| Raw failing test(s) | Tracker | Proof and disposition |
|---|---|---|
| `TestProviderLiveClaudeKindPath` | `ga-fh1flg` | Clause 3(a), exact `agent_pane_busy` / startup-delivery signature in `internal/runtime/herdr`; `waiver_ref: mayor-2026-08-20-herdr-pane-standing`. |
| `TestBdFlagManifestCurrent` | `ga-f0uceo` | Clause 3(a), installed `bd` exposes flags absent from the source manifest; candidate cannot change the installed binary or `internal/bdflags`. |
| `TestGetKeyBinding_CapturesDefaultBinding`, `TestGetKeyBinding_CapturesDefaultBindingWithArgs` | `ga-afqddr` | Clause 3(a), host tmux 3.7b returns the tracked empty filtered default-binding result. |
| `TestPersonalWorkFormulaCompileAndRun`, `TestAdoptPRFormulaSoftFailsGeminiAfterTransientRetries`, `TestAdoptPRFormulaRetriesTransientReviewerStep`, `TestGCLiveContract_BeadsAndEvents` | `ga-lpfjhc` | Exact `gastownhall/beads#4566` dirty-table schema-migration signatures during fixture `bd init`; raw FAIL preserved as FAIL-WAIVED under standing authorization `ga-6bnc42`. |
| `TestCleanInstallTutorialPath` | `ga-z08s5l` | Clause 3(a), tracked Dolt TCP read timeout followed by database-existence `invalid connection` during fixture bootstrap. The same shard passed in the immediately preceding full-scope run. |
| `TestE2E_SuspendResume_City` | `ga-yc0e3a` | Clause 3(a), tracked timeout waiting for `citysus.report`; the sync command is not executed by suspend/resume. |

The immediately preceding non-verbose run used the same full scope and recorded 35/40 jobs PASS and five raw failing jobs. It additionally observed the tracked `TestSessionReconcilerTraceGH1654WorkRequestedStartCandidates` async-start timeout (`ga-hgjlhi`); all six `cmd/gc` process shards passed in the authoritative verbose rerun.

## Skip justification

The 187 top-level skips are pre-existing platform, live-infrastructure, opt-in, helper-process, or environment gates spread across unrelated packages. Neither diff-owned test skipped; both executed and passed by name. No skip waiver is needed for the candidate.

## Ancestry and scope

`assert_deploy_ancestry_scope origin/main 2e35396a6d82d9f9a5e0101e41420ff79c48c719 ga-g514cl ga-0uzqsf ga-01v8g7` exited 0. The rewritten commits now cite accepted bead `ga-0uzqsf`, and the deploy range introduces no `.claude/**` path.
