# Release gate: dependency-aware ephemeral work-query probes

- Deploy bead: `ga-hur55i`
- Build lineage: `ga-9q0k4o` / `ga-7ya515`
- Review bead: `ga-0hhty0` (rebase-integrity re-review; original full review `ga-s15umt`)
- Reviewed commit: `82d5da40f33afcf1477ebbe66322567e20aeccab`
- Base: `origin/main@cf36f74a64b94faac79e9c07c06863768d6e715e`
- Deploy mode: remote
- Gate verdict: **PASS**

| # | Criterion | Result | Evidence |
| --- | --- | --- | --- |
| 1 | Review PASS present | **PASS** | `ga-0hhty0` is closed with a PASS rebase-integrity verdict at the exact reviewed SHA; `ga-s15umt` records the original full `REVIEW VERDICT: PASS`. |
| 2 | Acceptance criteria met | **PASS** | Ephemeral ready and assigned-in-progress probes split zero-dependency fast paths from real `bd show --json` enrichment for dependency-bearing candidates; all three new regression tests and the complete generated-golden suite pass. Hold transparency remains disabled for probe-only paths and enabled for work-serving paths. |
| 3 | Tests pass | **PASS** | Diff-owned tests passed by name with 0 skips. The documented full union completed 33/40 jobs PASS, 7/40 jobs FAIL, 0 tests SKIP. Every failure is independently attributed below; the beads#4566 failures remain **FAIL — WAIVED** under the standing mayor authorization. No candidate-owned failure remains. |
| 4 | No unresolved HIGH findings | **PASS** | Both reviews record no security or HIGH-severity finding. New jq fragments continue to pass through `shellquote.Quote`. |
| 5 | Final branch clean | **PASS** | `git status --short` was empty after focused, package, policy, vet, coverage, and full-union runs. |
| 6 | Branch diverges cleanly from main | **PASS** | `git merge-tree --write-tree origin/main 82d5da40f33afcf1477ebbe66322567e20aeccab` returned rc 0 against the current main and tree `b8f295eff990351458832a81f22c350e9c0a5c34`; merge base `75b12a0461254034effb319db9b1509258a899f6`. |
| 7 | Single feature theme | **PASS** | Source, regression tests, generated goldens, resource-ledger adjustment, and prior gate record all belong to one behavior: dependency-blocked ephemeral molecule steps must not be served by work-query probes. |

## Test evidence

- `go test -v -count=1 -run '^(TestEphemeralReadyProbeWithholdsBlockedStep|TestEphemeralInProgressProbeGatesOnReadiness|TestEphemeralInProgressProbeIgnoresBD105Semantics)$' ./internal/config/` — 3 PASS, 0 FAIL, 0 SKIP.
- `go test -v -count=1 ./internal/config/...` — PASS, including all generated work-query goldens.
- `go test -count=1 ./internal/testpolicy/...` — PASS.
- `make test-ci-policy` — PASS.
- `go vet ./...` — PASS.
- `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true LOCAL_TEST_JOBS=4 GO_TEST_TIMEOUT=30m make test-local-full-parallel` — 33/40 jobs PASS, 7/40 jobs FAIL, 0 tests SKIP. Logs: `/var/tmp/gc-local-tests.S1uNrx`.
- Base comparison evidence at unchanged affected paths: `/var/tmp/gc-local-tests.9SMMVL` on `origin/main@4f4a37b28c9cfaa1ebe1c587576b69663a47f078`; main commits since that run do not touch `internal/bdflags`, `internal/runtime/tmux`, `internal/storebinding/sqlite`, or `test/integration`.

`diff_tests_executed`:

- `TestEphemeralReadyProbeWithholdsBlockedStep` — PASS.
- `TestEphemeralInProgressProbeGatesOnReadiness` — PASS.
- `TestEphemeralInProgressProbeIgnoresBD105Semantics` — PASS.

`policy_lane`: `make test-ci-policy` — PASS.

`waiver_ref`: `ga-6bnc42`, mayor standing authorization dated 2026-08-18, limited to the exact `ga-lpfjhc` / `gastownhall/beads#4566` dirty-table schema-migration signature when the diff cannot reach schema migration or store bootstrap.

## Failure attribution

- `TestBdFlagManifestCurrent` -> `ga-f0uceo`. The exact installed-`bd` manifest skew reproduced on the base. The candidate does not touch `internal/bdflags`.
- `TestGetKeyBinding_CapturesDefaultBinding` and `TestGetKeyBinding_CapturesDefaultBindingWithArgs` -> `ga-afqddr`. Both empty-default-binding failures reproduced on the base. The candidate does not touch `internal/runtime/tmux`.
- `TestSQLiteLegacySnapshotSIGKILLAtBoundaries/legacy-claim-release-after` -> `ga-5dsf6n`.
- `TestSQLiteWriterFenceSIGKILLAtReservationBoundaries/WAL_without_SHM/reservation-open-pending` -> `ga-ggrykt`. For both SQLite failures, focused reruns passed and coverage over the exact top-level tests measured every function in `internal/config/workquery.go` at 0.0%, including every changed function (`/var/tmp/ga-hur55i-sqlite.cover`).
- `TestCleanInstallTutorialPath` -> `ga-hrdd3h`. The failure is the tracked external `bd` circuit-breaker diagnostic contaminating `bd config get issue_prefix` stdout before `tra`; it occurs before any agent work query is executed.
- `TestGraphWorkflowSuccessPath` and `TestGraphWorkflowFailureRunsCleanup` -> `ga-lpfjhc`. Both fail during fixture store initialization with the exact `gastownhall/beads#4566` pending-schema-migrations dirty-table signature, before work-query execution. The success-path signature reproduced on the base. Raw results remain **FAIL — WAIVED** under `ga-6bnc42`, and the occurrence is logged on `ga-lpfjhc` with deploy/build ids and test names.

No failure is diff-owned, every failure has a tracker, pre-existing or structural evidence is recorded above, and no failing execution path reached the changed work-query functions.
