**Verdict:** **PASS**

# Release gate: named-session runtime-name assignee collection

- Deploy bead: `ga-vbpckm`
- Build bead: `ga-6jhitn`
- Reviewed commit: `fc781bf59d2548485139928ca0b73a04ac13374d`
- Base ref: `origin/main@a5e8598acbc808992786e7c8015b093d2121d3b1`
- Deploy mode: remote; push remote: `fork`
- Gate date: 2026-09-18

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Review bead `ga-t200ja` records an unambiguous PASS for reviewed commit `fc781bf59d2548485139928ca0b73a04ac13374d`, with no review carryover. |
| 2 | Acceptance criteria met | PASS | The sole production change threads `cityStore` into `readyAssignedWorkAssignees`, keeps canonical identity collection unconditional, and adds the runtime-name assignee only when `findClosedNamedSessionBead(cityStore, identity)` proves prior session existence. The corrected closed-session regression, no-session control, and pre-existing broad-identity guard all pass by exact test name. |
| 3 | Tests pass | PASS | Independent full-suite run used the isolation wrapper with rootless Podman, Ryuk disabled, and cached pinned Dolt 2.1.7: `make test-local-full-parallel`. Counts across the 40 captured job logs: 48,819 top-level PASS, 6 FAIL, 228 SKIP. All six failures are non-diff-owned and attributed below under criterion 3a. None of the skips is diff-owned; they are the suite's pre-existing platform, live-provider, opt-in persistence, root-only, helper-process, and resource-conditional skips. `test_cmd_scope: full-suite`; `waiver_ref: none`; `ci_lane_run: n/a (no CI configuration changed)`. |
| 3a | Pre-existing failures may be attributed | PASS | Five external-`bd init` schema-migration refusals are tracked by predating tracker `ga-esyijp`; the stale Beads module-version assertion is tracked by predating tracker `ga-rnwg5u`. Both tracker sightings from this run were appended and read back. Full four-clause evidence is recorded below. |
| 3b | Policy/lint lane | PASS | `make test-ci-policy` PASS; `LINT_BASE=origin/main make lint-new` PASS with 0 issues; `go vet ./...` PASS; `go build ./...` PASS; changed Go files are `gofmt` clean; `make check-hooks` confirms `.githooks` ownership. |
| 3c | CI-config diff lane | PASS | Not applicable: the diff changes only `cmd/gc/build_desired_state.go` and its tests; no workflow, matrix, timeout, or required-check configuration changed. |
| 4 | No high-severity review findings open | PASS | Reviewer recorded no style or security findings and no unresolved high-severity findings. |
| 5 | Final branch is clean | PASS | `git status --porcelain` was empty at the exact reviewed commit before the gate record was added. |
| 6 | Branch diverges cleanly from main | PASS | Preflight found no PR carrying the reviewed commit. `git merge-tree --write-tree origin/main fc781bf59d2548485139928ca0b73a04ac13374d` exited 0 and produced tree `1934e618eb56588e062d8d0a396027f3308b1272`; no self-rebase was needed. |
| 7 | Single feature theme | PASS | Both commits and all three changed files belong to one `cmd/gc` desired-state feature: recovering on-demand named-session work recorded under the runtime-name assignee after a prior session closes. |

## Criterion 3 evidence

- `test_cmd`: `/home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- bash -c 'make test-local-full-parallel'`
- `test_cmd_scope`: `full-suite`
- `test_environment`: `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock`, `TESTCONTAINERS_RYUK_DISABLED=true`, cached `dolthub/dolt:2.1.7` and `dolthub/dolt-sql-server:2.1.7`
- `test_counts`: 48,819 PASS / 6 FAIL / 228 SKIP across 40 job logs
- `test_logs`: `/var/tmp/ga-vbpckm-full.Zc6f8G/shards`
- `diff_tests_executed`:
  - `TestNamedSessionDemand_OpenReadyWorkUnderRuntimeName_SurvivesPhantomClose`: PASS in the process and integration-package lanes
  - `TestNamedSessionDemand_OpenReadyWorkUnderQualifiedIdentity_NoSessionBead`: PASS in the process and integration-package lanes
  - `TestReadyAssignedWorkAssigneesExcludeBroadIdentities`: PASS in the process and integration-package lanes
- `skip_justification`: all 228 top-level skips are pre-existing conditional tests (platform-specific, live-provider/credential opt-ins, helper subprocesses, root-only cases, or explicitly opt-in persistence coverage); none is added or modified by this diff, while every diff-owned test reports a real PASS.
- `waiver_ref`: none
- `ci_lane_run`: n/a (no CI-config change in this diff)

### Failure attribution

- `TestAdoptPRFormulaCompileAndRun`, `TestPersonalWorkFormulaCompileAndRun`, `TestRetryManagedPooledWorkerRecoversClaimedAttemptAfterCrash`, `TestCleanInstallTutorialPath`, and `TestGCLiveContract_BeadsAndEvents` -> `ga-esyijp`.
  - Clause 1: PASS — the diff does not add or modify `test/integration/**`.
  - Clause 2: PASS — `ga-esyijp` was created 2026-08-29, predates this run, names these tests/root condition, and was opened before attribution; this run's sighting was appended and verified.
  - Clause 3: PASS via (b) CROSS-PR — the tracker records the identical shared-server pending-schema-migration refusal on many unrelated candidate heads. In this run each failure's explicit error is emitted by external `bd init`; the scenario either never starts or fails at the independent rig-store bootstrap boundary.
  - Clause 4: PASS — no failing test file/package overlaps the diff; there is no resource-census change, new suite target, or integration test added.
- `TestPinnedIntegrationBeadsModuleVersion` -> `ga-rnwg5u`.
  - Clause 1: PASS — neither `test/integration/integration_test.go` nor the Beads version pin is diff-owned.
  - Clause 2: PASS — `ga-rnwg5u` predates this run, tracks this exact stale `v1.3.0-rc.2` expectation, and was opened before attribution; this run's sighting was appended and verified.
  - Clause 3: PASS via (a) MECHANISM — the check only compares `go list -m` output with a hard-coded test literal; the candidate's named-session collection code cannot change either input. The tracker also carries identical unrelated-head sightings.
  - Clause 4: PASS — no failing test file/package, pin, census, or test-target overlap.

The attributed failures do not alter the gate verdict under the repository's non-diff-owned failure protocol.
