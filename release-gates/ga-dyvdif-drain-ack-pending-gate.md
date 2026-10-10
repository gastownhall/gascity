**Verdict:** **PASS**

# Release gate: require a pending drain before hook acknowledgement

- Deploy bead: `ga-dyvdif`
- Review bead: `ga-o58lao`
- Build bead: `ga-tbc2zq.1`
- Reviewed source: `38abbe039a490f9ed2fafebd3d04a646bbbb4fdb`
- Source branch (provenance only): `builder/ga-tbc2zq.1`
- Base: `origin/main@3618fc23aa06b07c9da6617190951ea5d9f267f5`
- Merge base: `bb011fa7b48d24482291af02e7e688a9b44d05e4`
- Deploy mode: remote; push target: `fork`
- Evaluated: 2026-09-19

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-o58lao` records PASS for the exact resolved commit `38abbe039a490f9ed2fafebd3d04a646bbbb4fdb`. No review carryover was used. |
| 2 | Acceptance criteria met | **PASS** | The no-work hook path now asks the existing drain-pending probe before honoring `--drain-ack`: a definite not-pending answer suppresses the acknowledgement callback and state write, while a real pending drain retains the existing acknowledgement behavior. The probe's established fail-open contract remains intact. The inaccurate `Agent.Nudge` pool-only comment was corrected and regenerated reference files match their source. Named-session backstop coverage, always-mode special casing, claimable-work narrowing, and the mayor mitigation were not changed. The reviewer accepted the colocated, feature-specific test file as a non-blocking deviation from the request to extend one of three named files. |
| 3 | Tests pass | **PASS** | The documented full-suite command ran through the isolation wrapper with rootless Podman configured: 40 jobs, 34 PASS, 6 raw FAIL, 0 SKIP. Every raw failure satisfies criterion 3a and is attributed below; none is diff-owned. All 12 diff-owned tests were selected by the full-suite shard enumerations and their owning `cmd/gc` shards completed cleanly, with no FAIL or SKIP for any named test. No waiver was used. |
| 3a | Pre-existing failures may be attributed | **PASS** | Each raw failure is outside the diff, has a tracker covering the root condition, has a direct mechanism proof that the candidate cannot cause it, and has no path overlap. Current sightings were appended to the pre-existing trackers. The previously untracked supervisor-readiness condition uses the discovering-run escape with landed mechanism proof in `ga-ch1pfr`. Details follow. |
| 3b | Policy/lint lane | **PASS** | `make test-ci-policy` PASS; `go vet ./...` PASS; `make check-hooks` PASS; `make check-docs` PASS; `go run ./cmd/genschema` followed by a generated-file diff check PASS. `make lint` reported only three known findings in ignored ambient `internal/api/dashboardspa/web/node_modules/flatted/golang/pkg/flatted/flatted.go`; pre-run tracker `ga-tcdrnz` covers the exact condition, the current sighting was appended, and the candidate has no dashboard dependency or lint-configuration overlap. |
| 3c | CI-config diff needs its own lane | **PASS (N/A)** | The diff changes no workflow, required-check list, CI matrix, or CI timeout. `ci_lane_run: n/a`. |
| 4 | No high-severity review findings open | **PASS** | Review verdict is PASS with no unresolved HIGH finding. The sole security observation was explicitly non-blocking and concerns event-bus parity for a logged fail-open path. |
| 5 | Final branch is clean | **PASS** | `git status --short` was empty at the reviewed commit before this gate record was written. |
| 6 | Branch diverges cleanly from main | **PASS** | `git merge-tree --write-tree origin/main 38abbe039a490f9ed2fafebd3d04a646bbbb4fdb` exited 0 and produced tree `1dcc1871a4d02bf3a5b24d539cc58e80042bedf6`. No bounded self-rebase was needed. |
| 7 | Single feature theme | **PASS** | Both commits and all eight changed files serve one behavior: preventing an unrequested hook drain acknowledgement while preserving genuine pending-drain handling and keeping its generated configuration documentation accurate. |

## Criterion 3 evidence

- `test_cmd`: `/home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- bash -c 'make test-local-full-parallel'`
- `test_cmd_scope`: `full-suite`
- Environment: `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock`, `TESTCONTAINERS_RYUK_DISABLED=true`; Podman/crun were healthy and cached `dolthub/dolt:2.1.7` plus `dolthub/dolt-sql-server:2.1.7` matched the repository pin.
- Result: 40 jobs; 34 PASS, 6 raw FAIL, 0 SKIP. The make aggregate ended with `Error 123`; the six raw failures are all attributed below.
- Full log: `/var/tmp/ga-dyvdif-gate.lS5VYm/full-suite.log`
- Per-job logs: `/var/tmp/gc-local-tests.9Oebfa/`
- Isolation tripwire: 0 hits.
- `waiver_ref`: none.
- `ci_lane_run`: n/a — no CI configuration changed.

### Diff-owned tests

All 12 tests in `cmd/gc/cmd_hook_claim_drain_pending_test.go` were present in the full-suite shard enumerations. All six `cmd-gc-process` shards and all six `integration-packages-cmd-gc` shards passed; the full output contains no FAIL or SKIP for any of these names:

1. `TestHookClaimRefusesADrainingSessionBeforeAnyMutation`
2. `TestHookClaimDrainPendingRefusalNamesTheExplicitAckCommand`
3. `TestHookClaimNotDrainingSessionClaimsAsBefore`
4. `TestHookClaimDrainPendingProbeErrorFailsOpen`
5. `TestHookClaimRefusesADrainingSessionHoldingAnExistingAssignment`
6. `TestHookClaimDrainPendingWithoutDrainAckExitsOne`
7. `TestHookClaimDrainPendingProbeErrorEmitsTheFenceUnavailableEvent`
8. `TestHookClaimDrainPendingEmitsNoEventWhenTheProbeAnswers`
9. `TestHookClaimDrainPendingWritesTheDrainRecordEvenWhenTheAckFails`
10. `TestHookClaimDrainPendingSkipsWhenNoSessionIDIsKeyed`
11. `TestHookClaimNoWorkDoesNotHonorDrainAckWithoutAPendingDrain`
12. `TestHookClaimNoWorkHonorsDrainAckWithAPendingDrain`

`diff_tests_executed`: 12 PASS, 0 FAIL, 0 SKIP.

### Failure attribution

1. `internal/doctor.TestCustomTypesCheck_ServerBackedStoreIgnoresAmbientEndpoint` failed in `unit-core` and `integration-packages-core-4-of-4` with the same concurrent `bd` JSON-corruption signature. Pre-run tracker `ga-x5wacn` covers the exact test and condition and contains prior independent sightings. The candidate's executable change is confined to the `cmd/gc` hook-claim path; `internal/doctor` cannot import `cmd/gc`, and the remaining `internal/config` change is comment-only. No path overlaps. **Mechanism proof; attributed.**
2. `test/integration.TestHumaBinary_CityCreateAsync` and `test/integration.TestRetryManagedPooledWorkerRecoversClaimedAttemptAfterCrash` failed during `gc init` because a shared Dolt sql-server refused schema migrations (`v53 -> v66` and `v57 -> v66`). Pre-run tracker `ga-n75ap3` covers the shared-schema condition and prior independent sightings. The failure occurs before the changed hook-claim helper can run, and the candidate touches no initialization, store, or schema-migration path. No path overlaps. **Mechanism proof; attributed.**
3. `test/integration.TestGastown_EventsMailLifecycle` failed while `gc supervisor install` checked preserve-mode readiness: the active supervisor control socket was unavailable and the supervisor did not become ready. No open tracker covered this exact condition, so discovering-run tracker `ga-ch1pfr` was filed with the reproduction and direct mechanism trace. The failure is in `gc init` supervisor install/start, while the candidate only changes hook claim acknowledgement gating; its `internal/config` delta is comment-only. The test is not diff-owned, the candidate adds no new test target or census load, and no path overlaps. **Discovering-run mechanism proof; attributed.**
4. `test/integration.TestPinnedIntegrationBeadsModuleVersion` failed because the installed module reports `v1.3.0` while the test still expects `v1.3.0-rc.2`. Pre-run tracker `ga-rnwg5u` covers the exact stale-literal condition and contains multiple independent sightings. The candidate changes neither `go.mod` nor the version assertion. No path overlaps. **Mechanism and cross-run proof; attributed.**

`failure_attribution`: `TestCustomTypesCheck_ServerBackedStoreIgnoresAmbientEndpoint -> ga-x5wacn`; shared-schema `gc init` failures -> `ga-n75ap3`; `TestGastown_EventsMailLifecycle` supervisor readiness -> `ga-ch1pfr`; `TestPinnedIntegrationBeadsModuleVersion -> ga-rnwg5u`.

## Additional verification

- Generated reference files were regenerated from `internal/config/config.go`; the generated-file diff check was clean. This follows the documentation source-of-truth rule rather than editing generated prose directly.
- `make check-docs` passed.
- `make test-ci-policy` passed.
- `go vet ./...` passed.
- `make check-hooks` passed.

## Disposition

All seven release criteria pass. Prepare `deploy/ga-dyvdif-gate` from the exact reviewed source, commit this gate record, push only the isolated branch, open the PR, publish deploy clearance on the exact gated head, and route the merge-request to the merge authority. The deployer does not merge.
