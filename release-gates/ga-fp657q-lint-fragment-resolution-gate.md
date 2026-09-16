# Release gate: lint fragment resolution through city rig includes

- Bead: `ga-fp657q`
- Reviewed commit: `a891c7b877f18cdebc112fb233d2a8671f348d93`
- Base: `origin/main@a95c730a5c62dce2f32171b32df857aa3e5b57f6`
- Deploy mode: `remote`
- Evaluation date: 2026-09-10
- Result: **PASS**

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-6ppoot` is closed with `verdict: pass` at the exact reviewed commit. No review carryover was used. |
| 2 | Acceptance criteria met | **PASS** | The implementation makes lint-time fragment lookup include `City.PackDirsForRig("")`, matching the runtime renderer's city-composed pack search. `TestLintResolvesOwnPackFragmentCleanly` preserves same-pack behavior and `TestLintResolvesFragmentComposedViaCityRigIncludes` exercises the reported city `rigs[].includes` shape; both passed twice in the full sweep. |
| 3 | Tests pass | **PASS** | The documented full-suite command ran once on the exact candidate. It recorded 45,597 top-level PASS events, 10 top-level FAIL events, and 199 top-level SKIP events across 40 jobs (32 jobs green, 8 jobs red). Every FAIL is either attributed under criterion 3a or covered by the exact mayor waiver below. All four executions of the two diff-owned tests passed; no diff-owned test failed or skipped. |
| 3a | Pre-existing failures attributed | **PASS** | Every raw failure has a specific tracker and non-causation evidence. The only same-package cases are covered by `waiver_ref=mayor-2026-09-10-ga-fp657q-c3`, which is pinned to this exact diff and these exact two tests. All tracker records and sighting comments were opened and verified. Details follow below. |
| 3b | Policy/lint lane | **PASS** | `make test-ci-policy` exited 0: runner policy 5/5, CI-suite coverage 15/15, `scripts/cipolicy`, `scripts/prwatchdog/...`, and the scoped static-policy tests all passed. |
| 3c | CI-config diff lane | **PASS** | `ci_lane_run: n/a (no CI-config change)`. The diff changes only `cmd/gc/cmd_lint.go` and `cmd/gc/cmd_lint_test.go`. |
| 4 | No unresolved HIGH review findings | **PASS** | Reviewer recorded no style, security, or specification findings; unresolved HIGH count is 0. |
| 5 | Final branch clean | **PASS** | `git status --porcelain` produced no output before the gate file was written. `git diff --check origin/main...HEAD` also produced no output. |
| 6 | Branch diverges cleanly from main | **PASS** | After `git fetch origin main`, `git merge-tree --write-tree origin/main HEAD` exited 0 and produced tree `0da043cd82420c5bf293166ba0053d3958ba4b2e`. Merge base: `09bae7ad1706aced67a72775d2b1d11549002cd0`. No self-rebase was needed. |
| 7 | Single feature theme | **PASS** | Two TDD commits touch only the lint implementation and its regression tests (`cmd/gc/cmd_lint.go`, `cmd/gc/cmd_lint_test.go`): one cohesive lint/runtime fragment-resolution parity fix. |

## Criterion 3 evidence

`test_cmd`: `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true GO_TEST_TIMEOUT=30m LOCAL_TEST_JOBS=4 GOFLAGS=-v make test-local-full-parallel`

`test_cmd_scope`: `full-suite`

`test_counts`: `PASS=45597 FAIL=10 SKIP=199` (top-level test events)

`job_counts`: `PASS=32 FAIL=8` (40 full jobs)

`diff_tests_executed`:

- `TestLintResolvesOwnPackFragmentCleanly`: PASS in `cmd-gc-process-3-of-6` and `integration-packages-cmd-gc-4-of-6`.
- `TestLintResolvesFragmentComposedViaCityRigIncludes`: PASS in `cmd-gc-process-4-of-6` and `integration-packages-cmd-gc-5-of-6`.

`skip_justification`: The suite's 199 explicit skips are preconditioned platform, provider/live-environment, helper-process, and optional integration cases. Neither diff-owned test skipped; both ran and passed twice. Raw output is retained under `/var/tmp/gc-local-tests.JoGf84`.

`waiver_ref`: `mayor-2026-09-10-ga-fp657q-c3`

`failure_attribution`:

- `TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix` -> `ga-ukteq1` | tracked beads#4566 dirty-table schema-migration race under shard contention. Raw FAIL is **WAIVED BY MAYOR** under the waiver above; the changed lint functions cannot reach this test's init path.
- `TestSessionReconcilerTraceGH1654WorkRequestedStartCandidates` -> `ga-hgjlhi` | tracked host-load async-start timing race. Raw FAIL is **WAIVED BY MAYOR** under the waiver above; only `named_session_post-kill` failed and the changed lint functions cannot reach session reconciliation.
- `TestGetKeyBinding_CapturesDefaultBinding` and `TestGetKeyBinding_CapturesDefaultBindingWithArgs` -> `ga-k3fxvj` | clause 3(a), mechanism: `internal/runtime/tmux` cannot import or execute `cmd/gc` lint code; host tmux returned empty default bindings. No path overlap.
- `TestAdoptPRFormulaCompileAndRun`, `TestPersonalWorkFormulaCompileAndRun`, and `TestRetryManagedPooledWorkerRecoversClaimedAttemptAfterCrash` -> `ga-esyijp` | clause 3(a), mechanism: all failed during `gc init` on the tracked beads#4566 dirty-table migration condition, before any lint command; no failing-test path overlap.
- `TestBdFlagManifestCurrent` -> `ga-f0uceo` | clause 3(a), mechanism: the installed `bd` binary exposes flags missing from `internal/bdflags`; this lint-only diff cannot alter either surface. No path overlap.
- `TestCatalogMatchesProductionWiringAndDocumentation` (two executions) -> `ga-cojd80` | clause 3(a), mechanism: expired `runtime.Provider` waiver dates in `internal/testutil/providerledger`; the lint-only diff cannot change that catalog or ledger. No path overlap.
- `TestCleanInstallTutorialPath` -> `ga-z16i80` | clause 3(a), mechanism: the failure occurs in `gc rig add`, while the changed production functions are reachable only through `gc lint`. No path overlap. This tracker was created during the discovering run under the livelock exception because the mechanism proof had landed and clauses 1 and 4 were clear.

All existing tracker records predate this run. Each occurrence was appended to its tracker and read back from the ledger. The newly created tutorial tracker is non-routed and carries only `gate-tracker`.

## Source and scope evidence

```text
6e7dd02c77 test(lint): red — gc lint misses fragments composed via city.toml rig includes
a891c7b877 feat: green — gc lint resolves fragments composed via city rig includes
```

The deploy ancestry guard accepted `ga-fp657q`, review bead `ga-6ppoot`, and build bead `ga-as6dhb`; it found no `.claude/**` paths or unrelated commit theme.
