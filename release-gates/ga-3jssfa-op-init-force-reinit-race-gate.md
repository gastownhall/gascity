**Verdict:** **FAIL**

# Release Gate: `ga-3jssfa` — `op_init` force-reinit race

- Deploy bead: `ga-3jssfa`
- Review bead: `ga-6jg4xk`
- Reviewed commit: `e3005da5ed3c1a3d897e908433595ca3b321a4e7`
- Base checked: `origin/main@9cfb391f6f379a626a34748aa7dbcaa2f67f5cf2`
- Deploy mode: `remote`; push target resolved as `fork`
- Gate evaluated: 2026-09-21

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 6 | Branch diverges cleanly from main | **PASS** | `origin/main` is the reviewed commit's direct merge base. `git merge-tree --write-tree origin/main e3005da5ed3c1a3d897e908433595ca3b321a4e7` exited 0 and produced tree `d548f9edf8b39d4c5c77e0b8e1d5f171ba3373b9`; `git diff --check` was clean. No self-rebase was needed. |
| 1 | Review PASS present | **PASS** | Review bead `ga-6jg4xk` records PASS for the exact reviewed commit above. The recorded SHA resolves locally to that full commit. |
| 2 | Acceptance criteria met | **FAIL** | The change must stop random `vNN -> v66` forced reinitializations when another initializer advances `schema_migrations`. The full suite reproduced that exact outcome: `TestGraphWorkflowFailureRunsCleanup` reached forced `bd init`, which refused 13 pending migrations (`v53 -> v66`). The failure traverses the changed `op_init` script and is the feature's explicit target, so passing the deterministic lock tests does not satisfy the live acceptance path. |
| 3 | Tests pass | **FAIL** | The required full-suite run completed 40 jobs: 36 PASS, 4 FAIL, 0 SKIP. One failure is the candidate-owned acceptance-path reproduction above. Two other root conditions are attributed below. `waiver_ref: none`. |
| 3a | Pre-existing failures attributed | **PARTIAL / does not clear criterion 3** | `TestCustomTypesCheck_ServerBackedStoreIgnoresAmbientEndpoint` (two jobs) maps to `ga-3t5hvu` via clause 3(b), an identical unrelated-head failure predating this run with no `internal/doctor` path overlap. `TestPinnedIntegrationBeadsModuleVersion` maps to `ga-rnwg5u` via clause 3(a): the candidate changes neither the dependency pin nor `test/integration`. The schema refusal maps to condition tracker `ga-n75ap3`, but attribution is refused because the failing test reaches the changed production script, the diff adds declared subprocess test load, and this condition is the acceptance target. Sightings were appended to all three trackers. |
| 3b | Policy/lint lane | **PASS** | `make test-ci-policy`, `LINT_CHANGED_REF=origin/main make lint-affected`, and `LINT_CHANGED_REF=origin/main make fmt-check-changed` all passed against the exact reviewed SHA. Logs: `/var/tmp/ga-3jssfa-test-ci-policy.log`, `/var/tmp/ga-3jssfa-lint-affected.log`, and `/var/tmp/ga-3jssfa-fmt-check-changed.log`. |
| 3c | CI-config lane | **PASS (not applicable)** | The diff changes no workflow, CI matrix, timeout, or required-check configuration. |
| 4 | No high-severity review findings open | **PASS** | Review bead `ga-6jg4xk` records no blocking style or security findings and no unresolved HIGH finding. |
| 5 | Final branch is clean | **PASS** | `git status --porcelain=v1` was empty at the exact reviewed commit before this gate record was created. |
| 7 | Single feature theme | **PASS** | All six changed paths implement or verify the same managed-beads initialization race fix and its declared test-resource accounting. |

## Criterion 3 evidence

- `test_cmd`: `make test-local-full-parallel`, run through `isolated-test-run.sh`
- `test_cmd_scope`: `full-suite`
- Container setup: rootless Podman socket configured, Ryuk disabled, and the repository-pinned Dolt image tag was present in the local cache.
- `test_counts`: 36 job PASS, 4 job FAIL, 0 job SKIP across the complete 40-job union.
- Aggregate log: `/var/tmp/ga-3jssfa-test-local-full-parallel.log`
- Job logs: `/var/tmp/gc-local-tests.WIaCkR/`
- `policy_lane`: PASS, as recorded in criterion 3b.
- `waiver_ref`: none.

The four failed job occurrences were:

- `TestCustomTypesCheck_ServerBackedStoreIgnoresAmbientEndpoint` in `unit-core` and `integration-packages-core-3-of-4`: attributed to `ga-3t5hvu` by cross-PR evidence.
- `TestPinnedIntegrationBeadsModuleVersion` in `integration-rest-full-6-of-8`: attributed to `ga-rnwg5u` by mechanism; the stale expectation is `v1.3.0-rc.2` while the current pin resolves to `v1.3.0`.
- `TestGraphWorkflowFailureRunsCleanup` in `integration-rest-full-7-of-8`: **blocking candidate-owned failure**. Its `gc init` invocation selected forced reinitialization, then bd refused 13 pending shared-server migrations (`v53 -> v66`). This is the exact race the diff is intended to close.

## Diff-owned test resolution

All 11 diff-added or modified top-level tests were explicitly named by the
full-suite shard selectors, appeared in green containing jobs, and emitted no
`--- FAIL:` or `--- SKIP:` result:

- `TestGCBeadsBDScript_UsesPortableSleepMS`
- `TestStoreHoldsBdTablesDistinguishesEmptyFromUndetermined`
- `TestBdRuntimeBdTableCountRejectsUnsafeDatabaseNames`
- `TestGcBeadsBdInitRefusesForcedReinitWhenDatabaseHoldsBdTables`
- `TestStoreHoldsBdTablesConsidersMigrationCursor`
- `TestForceReinitGuardWaitOutlastsCursorAdvancing`
- `TestForceReinitGuardWaitGivesUpOnStalledCursor`
- `TestGcBeadsBdInitRefusesForcedReinitWhenMigrationCursorIsAdvancing`
- `TestGcBeadsBdInitRefusesForcedReinitWhenCursorAdvancesBetweenClassificationAndForce`
- `TestGcBeadsBdScriptDocumentsSchemaSettleTimeoutOverride`
- `TestGcBeadsBdInitConcurrentInvocationsDoNotBothForceReinit`

Their deterministic PASS results do not override the live suite's candidate-owned
schema-refusal reproduction.

## Pre-flight and disposition

The reviewed commit has not been pushed. GitHub's commit-to-PR lookup found no
target PR, so there is no already-merged work to reconcile.

No deploy branch, push, pull request, deploy-clearance status, or merge-request
was created. This is a technical implementation failure: route `ga-3jssfa` back
to the builder with failed criteria 2 and 3 and the exact full-suite log paths.
