# Verified-empty bd initialization release gate: ga-qircmx

**Verdict:** **PASS**

Evaluated 2026-09-27. All seven criteria pass under the documented attribution rule for six unrelated file-provider fixture failures. No waiver. All required supplemental lanes completed.

## Source and tested snapshots

Reviewed source: `c98dbc740ee61b86226bf29a686cfa37bb3f3d62`, resolved against git. Review ga-hmdodw PASS, build ga-h8haz6 fixes ga-n75ap3. `builder/ga-h8haz6` is provenance only. Publication branch will be mechanically derived as `deploy/ga-qircmx-gate` at that source, plus only this gate record.

Full-sweep base `8cc1d366331cb7a7a27f09ceaf11e9ce974c5848`; canonical merge `2aa3fac2a21b2d379d0295a558deb072f88ae66e`; tree `211e14155058c35fdb2a9ca6d0afefbc2fad7808`. Required supplemental lanes, whole-tree build and vet use base `b193fc79eec8c1b3af2a1a366e82a860d145910e`, canonical merge `08074d323313b06629b8347e5d4f45ab009a4582`, tree `29f9b6144330cf54c032fb295c770eb5238a795a`. Both merge trees and parent SHAs are verified. Between these bases main changed only an unrelated Linux SQLite fixture and its prior gate record. Completed full evidence remains pinned to its recorded snapshot; it was not rerun on the new base.

Mode remote, publication remote fork, base repository gastownhall/gascity. docs/PROJECT_MANIFEST.md is absent; supplied release criteria, TESTING.md, Makefile and required CI jobs define this evaluation. Evidence retained at `/var/tmp/gc-ga-qircmx-apg4m0yd`.

| Criterion | Result | Evidence |
|---|---|---|
| 1. Review PASS | PASS | ga-hmdodw records PASS at the resolved reviewed source; single pass permitted, no carryover needed. |
| 2. Acceptance criteria | PASS | Force-free verified-empty init and metadata rollback tests pass; unchanged guards pass; 147 initialization roots have passing executions; ten fresh real session-message repetitions pass. |
| 3. Tests pass | PASS | Complete 40-job full suite, all diff-owned roots pass, six unrelated failures meet every attribution clause below. All 27 supplemental commands pass, with explicit unchanged legacy/opt-in skips. |
| 3b. Policy/lint | PASS | Pinned private-cache golangci-lint 2.12.0, formatting, CI policy, module/native/core/export/doc boundaries, full build and full vet pass. |
| 3c. CI-config diff | PASS | ci_lane_run: n/a; no CI job, matrix, timeout or required-check change. Applicable existing lanes ran. |
| 4. No HIGH findings | PASS | Zero unresolved HIGH, blocker, major or security findings in review. |
| 5. Branch clean | PASS | Tested trees have no tracked changes; only this gate record is added to the reviewed source. Normal pre-commit hook runs; final clean status is verified before push. |
| 6. Clean divergence | PASS | Both tested merges and final main freshness merge are conflict-free; no self-rebase. |
| 7. Single theme | PASS | Two paths implement and test verified-empty direct bd initialization. No unrelated feature or prohibited ancestry. |

## Acceptance verification

The plain-init helper is selected only after post-lock revalidation returns genuinely empty. It uses the pinned server/database argv and closes lock fd 8 in the child. It sets aside the metadata stub and restores it byte-identically on failure; success removes the aside. Existing tables/cursor refusals and the undetermined-probe force arm remain. Lock release, schema readiness, proxied/DoltLite/hosted paths are unchanged. The broader readiness-probe repair is separate work.

Two new tests prove force-free initialization and stub restoration. All fourteen roots in the modified test file passed twice in the fresh full sweep: 28 PASS, 0 FAIL, 0 SKIP, 0 missing. The unchanged forced-fallback and preseeded-metadata guards also passed twice. The acceptance regex matches 147 roots in the full process/integration profiles: every root has PASS, 280 PASS executions, 0 FAIL, 14 duplicate-profile SKIP. These fourteen skips each have a passing process-profile execution. Ten fresh real asynchronous session-message repetitions passed 10/10, no FAIL/SKIP, 298.787s; no pending-schema refusal strings.

| Modified-file root | Fresh full-suite execution |
|---|---|
| TestStoreHoldsBdTablesDistinguishesEmptyFromUndetermined | cmd-gc-process-6-of-6: PASS; integration-packages-cmd-gc-3-of-6: PASS |
| TestBdRuntimeBdTableCountRejectsUnsafeDatabaseNames | cmd-gc-process-1-of-6: PASS; integration-packages-cmd-gc-4-of-6: PASS |
| TestGcBeadsBdInitRefusesForcedReinitWhenDatabaseHoldsBdTables | cmd-gc-process-2-of-6: PASS; integration-packages-cmd-gc-5-of-6: PASS |
| TestStoreHoldsBdTablesConsidersMigrationCursor | cmd-gc-process-3-of-6: PASS; integration-packages-cmd-gc-6-of-6: PASS |
| TestStoreHoldsBdTablesConsidersParkedMigrationHistory | cmd-gc-process-4-of-6: PASS; integration-packages-cmd-gc-1-of-6: PASS |
| TestForceReinitGuardWaitOutlastsCursorAdvancing | cmd-gc-process-5-of-6: PASS; integration-packages-cmd-gc-2-of-6: PASS |
| TestForceReinitGuardWaitGivesUpOnStalledCursor | cmd-gc-process-6-of-6: PASS; integration-packages-cmd-gc-3-of-6: PASS |
| TestForceReinitGuardWaitOutlastsSettleTimeoutWhileCursorAdvances | cmd-gc-process-1-of-6: PASS; integration-packages-cmd-gc-4-of-6: PASS |
| TestGcBeadsBdInitRefusesForcedReinitWhenMigrationCursorIsAdvancing | cmd-gc-process-2-of-6: PASS; integration-packages-cmd-gc-5-of-6: PASS |
| TestGcBeadsBdInitRefusesForcedReinitWhenCursorAdvancesBetweenClassificationAndForce | cmd-gc-process-3-of-6: PASS; integration-packages-cmd-gc-6-of-6: PASS |
| TestGcBeadsBdInitVerifiedEmptyStoreInitializesWithoutForce | cmd-gc-process-4-of-6: PASS; integration-packages-cmd-gc-1-of-6: PASS |
| TestGcBeadsBdInitRestoresMetadataStubWhenPlainInitFails | cmd-gc-process-5-of-6: PASS; integration-packages-cmd-gc-2-of-6: PASS |
| TestGcBeadsBdScriptDocumentsSchemaSettleTimeoutOverride | cmd-gc-process-6-of-6: PASS; integration-packages-cmd-gc-3-of-6: PASS |
| TestGcBeadsBdInitConcurrentInvocationsDoNotBothForceReinit | cmd-gc-process-1-of-6: PASS; integration-packages-cmd-gc-4-of-6: PASS |

## Full-scope result

`test_cmd: make test-local-full-parallel LOCAL_TEST_JOBS=4`

`test_cmd_scope: full-suite`

`diff_tests_executed: 28 PASS / 0 FAIL / 0 SKIP / 0 missing (14 roots, twice)`

`waiver_ref: none`

All 40 jobs completed: 36 PASS / 4 FAIL jobs; raw command exit 2, no TRIPWIRE or partial result. Top-level results: 53,561 PASS / 6 FAIL / 233 SKIP. All Go root/subtest result events: 95,879 PASS / 6 FAIL / 328 SKIP. These count executions across profiles, not distinct test names. TestGCLiveContract_BeadsAndEvents PASS (57.97s), TestHumaBinary_SessionMessageAsync PASS, and zero pending-schema-migration refusal strings. All six failures are outside the diff and attributed below; no failed tests were rerun.

`skip_justification`: no modified-file test skipped. Other skips cover duplicate-tier/helper entrypoints, platform/UID assumptions, optional live services/credentials and explicit opt-in persistence tests, intentional ambient-cwd refusal, and the existing Huma unregister gap. Per-test emitted contexts are retained in audit.json. Rootless Podman socket and pinned dolthub/dolt-sql-server:2.2.0 image were verified before execution; Ryuk disabled with BEADS_ALLOW_UNREAPED_TESTCONTAINERS=1. Go 1.26.6, bd 1.3.0 pin f45, Dolt 2.1.7; host bd used for the ledger was not replaced. Disk scratch under /var/tmp, shared Go object cache untouched.

## Six unrelated failure attributions

| Failure | Full-suite shard | Duration | Tracker/proof |
|---|---|---|---|
| TestGastown_MailArchive | rest-full-1-of-8 | 2.05s | ga-4w6d2r, mechanism (a) |
| TestGastown_PipelineMailChain | rest-full-1-of-8 | 16.80s | ga-4w6d2r, mechanism (a) |
| TestMail_BashAgent | rest-full-2-of-8 | 17.01s | ga-4w6d2r, mechanism (a) |
| TestGastown_MailRoundTrip | rest-full-6-of-8 | 16.62s | ga-4w6d2r, mechanism (a) |
| TestGastown_PipelineMailAndWork | rest-full-6-of-8 | 16.73s | ga-4w6d2r, mechanism (a) |
| TestGastown_PipelineConvoyTracking | rest-full-8-of-8 | 2.28s | ga-4w6d2r, mechanism (a) |

All four clauses are verified for each failure:

1. No failing test is modified; source changes only examples/bd/assets/scripts/gc-beads-bd.sh and cmd/gc/gc_beads_bd_force_reinit_guard_test.go.
2. Opened ga-4w6d2r predates this run (2026-09-26T23:55:25Z) and names all six signatures, with untouched-main reproduction and prefix-only fix evidence. Final sighting 4419f3de-61f9-5c0d-a258-deb112302ef4 and helper-name correction 36ed00f9-5362-5034-92a9-17abe6a5b248 were read back with exact-text verification.
3. Mechanism (a): Mail_BashAgent uses writeAgentsToml with beads.provider=file (helpers_test.go:468); five Gastown cases use renderGasTownToml with provider=file (gastown_helpers_test.go:112). initBeadsForDirWithExecutor dispatches file directly to initFileStoreForDir before the exec-provider path (beads_provider_lifecycle.go:1812), so the changed bd op_init never executes in these six tests. The changed cmd/gc _test.go is excluded from the real gc binary and integration package. Unchanged helpers and loop-mail.sh/mayor-dispatch.sh still reject non-gc fixture IDs. Actual symptoms include valid tk-16 and ci-19 IDs rejected and acknowledgment/work timeouts.
4. Failing package test/integration has no production changes in this diff. This is conclusive mechanism attribution, not the inconclusive or same-package path.

First prefix sighting on this deploy. Typed cross-fix eligibility also independently qualifies: own build ga-h8haz6 has gc.fixes_tracker=ga-n75ap3; blocking fix ga-c2atlu has gc.fixes_tracker=ga-4w6d2r, closed blocked but source 66b9671a38491c723479ee56d13fc3cb3bfb8732 not landed at the named base. No census bump, new suite target or new test file; the two new unit cases are appended to the existing file. No waiver or suppressed result.

## Required lanes and load

All 27 supplemental commands passed. Lanes include pinned private-cache policy/lint, real Huma repetitions, acceptance A, minimum bd CLI contract, all four worker phase-2 profiles and their required report fan-in, strict proxied/shared/native topology, release config, generated/spec/docs drift, Vitest/Playwright with retries 0, and Bazel Gazelle/repo-tree regeneration with zero tracked drift.


| Lane | Result | Go root/subtest PASS / FAIL / SKIP |
|---|---|---|
| current-policy | PASS | 103 / 0 / 0 |
| huma-repeat | PASS | 10 / 0 / 0 |
| acceptance-a | PASS | 396 / 0 / 15 |
| bd-cli-prev | PASS | 37 / 0 / 0 |
| worker-phase2-claude | PASS | 51 / 0 / 0 |
| worker-phase2-codex | PASS | 51 / 0 / 0 |
| worker-phase2-cursor | PASS | 51 / 0 / 0 |
| worker-phase2-gemini | PASS | 51 / 0 / 0 |
| topology-proxied | PASS | 23 / 0 / 0 |
| topology-shared-server | PASS | 1 / 0 / 0 |
| topology-migrate | PASS | 0 / 0 / 2 |
| topology-matrix | PASS | 11 / 0 / 1 |
| proxied-native | PASS | 17 / 0 / 0 |
| release-config | PASS | command exit 0 |
| generated | PASS | 204 / 0 / 0 |
| generated-docs | PASS | command exit 0 |
| dashboard-vitest | PASS | 932 / 0 / 0 frontend tests; 96 files |
| dashboard-fixture | PASS | command exit 0 |
| dashboard-browser | PASS | command exit 0 |
| dashboard-render | PASS | 19 / 0 / 0 browser tests; retries=0 |
| bazel-sync | PASS | command exit 0 |

Worker phase 2: 52/52 reports pass across exactly four required profiles, zero failures/unsupported/environment errors and no missing profiles. Phase 1 path filter is false. Acceptance A uses the CI no-bd/no-dolt PATH: 133 top-level PASS / 15 SKIP; unchanged selfhost placeholders and opt-in live registry explain its skips. Strict-tool lanes exercise proxied default, shared-server isolation, native lifecycle/safety and M1; two migration roots and M5 skip for CI's documented missing pre-journal gc binary. These legacy fixtures do not execute the new source. No diff-owned test skips. Current-bd conditional CAS path filter is false; the minimum-bd CLI lane ran.

- Full: LOAD_GATE_SUMMARY threshold=15 waited_seconds=60 wait_timed_out=0 load_start=15.52 load_max=37.23 load_mean=20.67 samples=121 read_errors=0.
- Supplement: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1201 wait_timed_out=0 load_start=28.39 load_max=33.56 load_mean=22.03 samples=104 read_errors=0.

Values are wrapper samples of five-minute load, including wait; they are not an absolute host-load maximum. Policy uses golangci-lint 2.12.0 and a private disk cache. Bazelisk is functional; --repo_env=CC=/usr/bin/gcc bypasses sandbox-incompatible ccache. No import/package changes require new BUILD content, but synchronization is independently checked.

## Final freshness and publication

Final origin/main `c288ab2d31b3c468a42482b3cdcf48b5ada3124d` merges cleanly with reviewed source; expected merge tree `c441dfbd68ff016826b81f3530bc868b5bc74db6`. Fresh scope guard passes for ga-qircmx and confirmed build/review ga-h8haz6/ga-hmdodw; no stack. Completed test runs remain pinned to their named snapshots.

Normal pre-commit hook owns .githooks and runs on the gate-only commit. The documented attribution protocol permits --no-verify for this specific gated head; the live ownership guard and safe-ref/reviewed-content guards still run immediately before push. Publish success-only deploy clearance to the base repository on the exact newly gated PR head before the verified merge-request. Merge authority belongs to MPR; the deployer does not merge.

After the supplemental snapshot, main added changelog entries and test-only HOME/server-cleanup fixtures plus their resource-census ledger changes. No application production, bd-wrapper, owned-unit-test, module/dependency or CI path changed. Both deploy-owned files at the final expected merge tree are byte-identical to the supplemental tested tree. The completed full sweep was not rerun or claimed to execute the later upstream tests.
