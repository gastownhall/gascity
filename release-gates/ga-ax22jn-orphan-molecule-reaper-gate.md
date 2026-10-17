# Release gate: orphan molecule reaper (`ga-ax22jn`)

- Deploy source: `64d3ee1f6226bbddfb73ee660a7ad96aa37d4cf0`
- Base evaluated: `origin/main@3c1de2a1e0d0d4a1a654933dd29e7ebe8e1525a9`
- Isolated branch: `deploy/ga-ax22jn-gate`
- Gate result: **PASS**

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-a47dba` records round-2 `VERDICT: pass` on the exact deploy source. The reviewer independently rebuilt and vetted the commit, checked the live reap result, and named all eight diff-owned tests PASS. |
| 2 | Acceptance criteria met | **PASS** | The hidden `gc molecule reap-orphans` path implements the required non-terminal-root, all-targets-terminal, never-entered-descendant predicate. `TestWispAutoclosePreservesParkedMoleculeSubtree` passed in both relevant suite profiles; `TestReapOrphans_TouchedOpenStepIsNotReaped` passed twice; the zero-progress, dry-run, missing-target, open-target, and three `crn-vbsr0` cases all passed. The real gascity run closed 50 molecule roots / 307 beads and a live audit still shows `ga-b6y3er` closed with the reaper's distinct close reason. Residual cairn/beads/MCDClient execution remains recorded in `ga-3n8erp` after the store-identity blocker documented on `ga-e5lyfu`; the reviewer explicitly accepted that durable disposition. Idempotence is pinned by the zero-progress test's empty second run. |
| 3 | Tests pass | **PASS** | The documented full-scope local CI equivalent completed. All eight diff-owned tests ran and passed twice. Nine raw failures were attributed under criterion 3a to five open, pre-existing root-condition trackers with independent mechanism/cross-run evidence; no diff-owned test failed or skipped. Required non-fast `TestTutorial01` passed. Full details follow below. |
| 4 | No high-severity review findings open | **PASS** | Unresolved HIGH findings: 0. The review records one non-blocking TOCTOU hardening advisory in `internal/molecule/cleanup.go`; it is not a high-severity finding and does not alter the accepted manual-command safety boundary. |
| 5 | Final branch is clean | **PASS** | The worktree was clean at the reviewed source before this checklist was added. This checklist is committed as the sole deploy-only delta below. |
| 6 | Branch diverges cleanly from main | **PASS** | `git merge-tree --write-tree origin/main 64d3ee1f6226bbddfb73ee660a7ad96aa37d4cf0` exited 0 and produced tree `37d63f822fc39b3eea5f28458377bc22a33e1e11`; no bounded self-rebase was needed. `assert_deploy_ancestry_scope origin/main 64d3ee1f... ga-ax22jn ga-a47dba ga-g5xpbl` also passed. |
| 7 | Single feature theme | **PASS** | The three source commits and six changed files form one `cmd/gc` molecule-orphan maintenance feature: the hidden command, its registration/census entries, and its regression tests. |

## Criterion 3 evidence

`test_cmd_scope: full-suite`

`test_cmd: DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true GO_TEST_TIMEOUT=30m LOCAL_TEST_JOBS=4 GOFLAGS=-v make test-local-full-parallel`

`test_counts: 45301 PASS, 9 FAIL, 191 SKIP` (top-level Go test events across 40 jobs; 8 jobs were red only because of the attributed failures below)

`diff_tests_executed:`

- `TestCrnVbsr0_A_TargetCloseWithOpenStepsOrphans` — PASS twice
- `TestCrnVbsr0_B_LastStepCloseSelfHealsRoot` — PASS twice
- `TestCrnVbsr0_C_StepWithoutParentNeverReaps` — PASS twice
- `TestReapOrphans_ZeroProgressOrphanIsClosed` — PASS twice
- `TestReapOrphans_DryRunClosesNothing` — PASS twice
- `TestReapOrphans_TouchedOpenStepIsNotReaped` — PASS twice
- `TestReapOrphans_NoTargetBeadIsNotReaped` — PASS twice
- `TestReapOrphans_OpenTargetIsNotReaped` — PASS twice

`skip_justification: 191 existing platform, external-capability, helper-process, and profile-selection skips; none is diff-owned. The full command's process profile exercised the required cmd/gc surface, including TestTutorial01, and both process/integration package profiles executed every diff-owned test.`

`failure_attribution:`

- `TestBdFlagManifestCurrent` -> `ga-f0uceo`: installed `bd` exposes flags beyond the repository manifest. The tracker predates this run and contains independent base reproductions; the candidate has no `internal/bdflags` or installed-binary path overlap.
- `TestCatalogMatchesProductionWiringAndDocumentation` (two runs) -> `ga-cojd80`: deterministic expired `runtime.Provider` waiver dates. The candidate changes no provider-ledger/catalog/waiver file.
- `TestGetKeyBinding_CapturesDefaultBinding` and `TestGetKeyBinding_CapturesDefaultBindingWithArgs` -> `ga-k3fxvj`: host tmux default-binding lookup returned empty. The tracker predates this run and records independent base reproduction; no tmux path overlaps the diff.
- `TestAdoptPRFormulaRetriesTransientReviewerStep` and `TestAdoptPRFormulaSoftFailsGeminiAfterTransientRetries` -> `ga-esyijp`: exact beads#4566 dirty-schema migration refusal during fixture `gc init`, before formula behavior. The tracker predates this run and contains repeated cross-candidate sightings and the upstream root cause; the candidate changes no schema/bootstrap path or integration fixture.
- `TestSQLiteWriterFenceSIGKILLAtReservationBoundaries/hot_rollback_journal/reservation-open-journal` -> `ga-vkhfnj`: child-protocol timeout at its fixed 10-second boundary under the full 40-job sweep. The pre-existing host-contention tracker already contains the SQLite writer-fence and child-protocol timeout families; `internal/storebinding` is outside and unreachable from this diff.
- `TestCmdStopForceEscalatesInProgressControllerStop` -> `ga-vkhfnj`: fixed 10-second stop timeout under the same full-sweep contention. Mechanism proof landed: this candidate only registers a dormant, manually invoked molecule command; the test calls `cmdStop` directly, never invokes the new reaper path, and no failing test file overlaps the diff.

`inconclusive-guard: not taken — each attribution has a landed mechanism, cross-candidate, or base-reproduction proof. The candidate does add test load, but no attribution relies on an inconclusive base pass.`

`waiver_ref: none — all raw failures are non-diff-owned criterion-3a attributions, not self-granted waivers.`

`policy_lane: make test-ci-policy — PASS (5 workflow-runner policy tests, 15 suite-coverage tests, scripts/cipolicy, and four static-scope checks all passed).`

`ci_lane_run: n/a (no CI configuration change in this diff).`

Raw per-job logs: `/var/tmp/gc-local-tests.hkR5F1`.
