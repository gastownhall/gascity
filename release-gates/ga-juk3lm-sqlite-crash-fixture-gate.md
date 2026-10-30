# SQLite crash-fixture release gate: ga-juk3lm

**Verdict:** **PASS**

Evaluated 2026-09-27 on resolved reviewed source `cd6e84bdaa8b9bbeff33a2d509193279bbfc2863`. The complete suite has seven attributed failures outside the deploy diff; every test in the changed fixture passed. This gate uses the documented non-diff-owned failure rule, including verified typed cross-fix eligibility for the initialization recurrence. `waiver_ref: none`.

## Source and tested trees

- Deploy branch: `deploy/ga-juk3lm-gate`, cut from the exact reviewed source. Builder branch `builder/ga-5skods` is provenance only. Review ga-o5on1k PASS; build ga-5skods fixes tracker ga-22dskp.
- Full-suite base: `9e6a0babb93678bf91b755e9858217564c2aac0d`; sanctioned canonical merge `474f7f76064995602f3bfcf4cd9032e413986c65`; tree `cc6389705a4e8f3a4546860f9431757c066eed44`.
- Remaining required lanes, build and vet: base `3596cd351f310a3deaab5d3b9488207456f1f256`; sanctioned merge `4fac7e3cb4868c1988d5fd179cad39bee5928499`; tree `c93262d2d78d336568234cb872c16528569cd70d`. Exact parents and expected merge-tree equality verified.
- Final freshness check: `origin/main` at `8cc1d366331cb7a7a27f09ceaf11e9ce974c5848` still merges cleanly; merge-tree `0906d8e8cccd0d76cb1ee9fe8a30234091c39054`. Later main commits change neither internal/storebinding/sqlite, go.mod/go.sum nor CI coverage. Completed runs remain pinned to their named snapshots; no claim that the full sweep ran again on later main.
- Remote mode; publication remote fork; base repository gastownhall/gascity. Own-PR preflight found no existing PR for this deploy. Scope guard accepts only ga-juk3lm, confirmed build ga-5skods and review ga-o5on1k; no stack or prohibited paths.
- Evidence: `/var/tmp/gc-ga-juk3lm-r2.iu2n4ol0`. docs/PROJECT_MANIFEST.md is absent; criteria come from the supplied release-gate protocol, TESTING.md, Makefile and applicable current CI jobs.

| Criterion | Result | Evidence |
|---|---|---|
| 1. Review PASS present | PASS | ga-o5on1k records PASS at the resolved source. No review carryover required. |
| 2. Acceptance criteria met | PASS | SIGKILL precedes stdin close, preventing EOF-triggered rollback from deleting the hot journal. The parent still waits for exit. The deterministic order regression and real journal/process-composition checks each pass ten fresh repetitions; the full suite also passes every root in the changed file. No retries or sleeps added. |
| 3. Tests pass | PASS | Fresh documented full suite completed all 40 jobs: 35 PASS, 5 FAIL; all seven raw test failures satisfy four-clause attribution below. All applicable additional lanes completed. Changed-file tests have zero FAIL/SKIP/missing results. Counts and skips remain explicit below. |
| 3b. Policy/lint | PASS | Pinned golangci-lint 2.12.0; affected lint, formatting, CI policy, module/native dependency/DoltLite/export/core/doc boundaries pass. Whole-tree build and vet pass. Private-cache environment correction is disclosed below. |
| 3c. CI-config diff | PASS | ci_lane_run: n/a (no CI-config change in this deploy diff). Current topology coverage, including shared-server isolation, was run. |
| 4. No HIGH findings open | PASS | Reviewer records zero unresolved HIGH findings. |
| 5. Branch clean | PASS | Both tested trees are clean. The release record is the only addition to the reviewed source and is committed with the normal pre-commit hook; final clean status is verified before push. |
| 6. Clean divergence from main | PASS | Both tested merges and final freshness merge succeed without conflicts. No bounded self-rebase needed. |
| 7. Single feature theme | PASS | One Linux SQLite test-support file; two build commits belong to ga-5skods. No independent feature or internal-document ancestry rides along. |

## Full-suite evidence

`test_cmd: make test-local-full-parallel LOCAL_TEST_JOBS=4`

`test_cmd_scope: full-suite`

Fresh load-gate then isolation-wrapper run, actual exit 2, all 40 jobs completed; no TRIPWIRE or partial result. This includes the cmd/gc process and full integration shards required for internal/**. Tutorial clean-install, graph cleanup and all changed fixture tests executed. Go 1.26.6, pinned bd 1.3.0/Dolt 2.1.7, verified rootless Podman and cached test image dolthub/dolt-sql-server:2.2.0; Ryuk disabled with BEADS_ALLOW_UNREAPED_TESTCONTAINERS=1. Temporary/build files stay on disk under /var/tmp. Live bead writes use host bd 1.1.0.

- Top-level Go executions: **53,503 PASS / 7 FAIL / 232 SKIP**.
- All Go root/subtest result events: **95,720 PASS / 7 FAIL / 322 SKIP**. Executions across profiles, not distinct-test counts.
- `diff_tests_executed`: eight roots in the modified file, each PASS twice; **16 PASS / 0 FAIL / 0 SKIP / 0 missing**. Unchanged hot-journal acceptance root also PASSes twice. Together: 18 roots and 132 root/subtest PASS events.
- `skip_justification`: all skips are outside the SQLite test file and its package-local helper. They cover platform/permission assumptions, helper modes, optional external services, process-tier separation (covered by the process profile), and explicitly retired/unavailable fixtures, including the existing bd conditional-write and herdr gaps. Full names and emitted reasons are retained in audit.json and skip-audit-final.json. No changed SQLite test was skipped.

| Changed-file root | Full-suite result |
|---|---|
| TestSQLiteFenceHelperProcess | PASS in unit-core and integration-packages-core-2-of-4 |
| TestSQLiteLegacySnapshotSIGKILLAtBoundaries | PASS in unit-core and integration-packages-core-2-of-4 |
| TestSQLiteGraphSnapshotSIGKILLAtBoundaries | PASS in unit-core and integration-packages-core-2-of-4 |
| TestSQLiteWriterFenceSIGKILLAtReservationBoundaries | PASS in unit-core and integration-packages-core-2-of-4 |
| TestSQLiteWriterFenceUsesExactKernelLockModes | PASS in unit-core and integration-packages-core-2-of-4 |
| TestSQLiteSourceCensusPermitsContinuousReaderMarkChurn | PASS in unit-core and integration-packages-core-2-of-4 |
| TestSQLiteWriterFenceProcessComposition | PASS in unit-core and integration-packages-core-2-of-4 |
| TestSQLiteFenceChildKillOrder | PASS in unit-core and integration-packages-core-2-of-4 |

TestLegacyCombinedSourceRecoversHotRollbackJournalInPrivateSnapshot PASSes in both profiles. Supplemental `go test -count=10 -v -timeout=5m ./internal/storebinding/sqlite` with only the order, hot-journal and process-composition roots is **focused**, not the criterion-3 substitute: **30 roots / 120 root-subtest events PASS, 0 FAIL, 0 SKIP**.

## Required lanes beyond the full sweep

Results below are raw Go root/subtest events, except the two explicitly named frontend/browser rows. Exact commands are retained in required-lanes.sh and its corrected continuation scripts; results and logs are in required/ and required-counts.json.

| Lane | Command result | PASS / FAIL / SKIP |
|---|---|---|
| current-policy | PASS | 103 / 0 / 0 |
| sqlite-repeat | PASS | 120 / 0 / 0 |
| acceptance-a | PASS | 396 / 0 / 15 |
| bd-cli-prev | PASS | 37 / 0 / 0 |
| topology-proxied | PASS | 23 / 0 / 0 |
| topology-shared-server | PASS | 1 / 0 / 0 |
| topology-migrate | PASS | 0 / 0 / 2 |
| topology-matrix | PASS | 11 / 0 / 1 |
| proxied-native | PASS | 17 / 0 / 0 |
| release-config | PASS | command exited 0 |
| generated | PASS | 204 / 0 / 0 |
| generated-docs | PASS | command exited 0 |
| dashboard-vitest | PASS | 932 / 0 / 0; 96 files |
| dashboard-fixture | PASS | command exited 0 |
| dashboard-browser | PASS | command exited 0 |
| dashboard-render | PASS | 19 / 0 / 0; retries=0 |

Acceptance-A matches CI's no-bd/no-dolt environment: 133 top-level PASS and 15 SKIP. Strict-tool topology lanes execute proxied-default, shared-server isolation, lifecycle, safety and M1. Two migration roots and M5 skip for the documented CI gap: no pre-journal legacy gc binary. Other acceptance-A skips are existing selfhost placeholders and the opt-in live pack registry. These tests are unchanged by this package-local fixture patch. Path-filtered worker phase1/phase2 jobs do not apply; their summaries require no fan-in. The conditional current-bd CAS lane does not apply to this path; the always-required previous-bd CLI contract ran.

The generated lane is `make dashboard-ci spec-ci`; generated-doc drift, Vitest and Playwright seeded dashboard rendering also ran. Playwright served the app and its API with HTTP 200 and passed all 19 checks with retries disabled. No source API/dashboard/schema change requires a separate manual preview.

## Failure attribution

| Failed test | Full-suite job | Attribution |
|---|---|---|
| TestGastown_MailArchive | integration-rest-full-1-of-8 | ga-4w6d2r; proof (a) |
| TestGastown_PipelineMailChain | integration-rest-full-1-of-8 | ga-4w6d2r; proof (a) |
| TestMail_BashAgent | integration-rest-full-2-of-8 | ga-4w6d2r; proof (a) |
| TestGCLiveContract_BeadsAndEvents | integration-rest-full-5-of-8 | ga-n75ap3; proof (a) |
| TestGastown_MailRoundTrip | integration-rest-full-6-of-8 | ga-4w6d2r; proof (a) |
| TestGastown_PipelineMailAndWork | integration-rest-full-6-of-8 | ga-4w6d2r; proof (a) |
| TestGastown_PipelineConvoyTracking | integration-rest-full-8-of-8 | ga-4w6d2r; proof (a) |

All four clauses hold for every row:

1. None is diff-owned: failures are in examples/gastown or test/integration; the sole changed path is internal/storebinding/sqlite/sqlite_fence_process_linux_test.go.
2. Both opened trackers predate this run and cover these exact conditions. Prefix-only repair and untouched-base reproduction are recorded on ga-4w6d2r. Final sightings were written and exact-text verified: `fd376b09-5c3e-54a8-b62c-42c078883dfe` and `c00965eb-1a05-55c9-a6e8-bd0ba923a878`.
3. Proof (a), mechanism: Go excludes this unexported package-local _test.go helper from real gc and other packages' binaries. Those failing tests cannot execute the changed call order. No production file, import, dependency, test target or census baseline changes.
4. No failing-package/path overlap with the SQLite test-support file.

Prefix symptoms include valid fk-16/yt-19 IDs rejected and mail acknowledgment/reply timeouts. This is this lineage's first prefix hit; designated fix ga-c2atlu (gc.fixes_tracker=ga-4w6d2r) remains unlanded.

The live contract's rig-alpha init left v48 -> v66 (18 pending) after bd's writable five-second force preflight; it returned HTTP 500 instead of 201. It is attributed to ga-n75ap3, not the closed ga-lejnse condition. Treating it conservatively as a repeat, typed cross-fix eligibility is verified: this deploy's build_bead ga-5skods has gc.fixes_tracker=ga-22dskp; condition fix ga-h8haz6 has gc.fixes_tracker=ga-n75ap3, outcome blocked and commit c98dbc740ee61b86226bf29a686cfa37bb3f3d62 still unlanded. Final main still calls the force path on verified-empty scopes and lacks its plain-init helper. The former ga-lejnse named hold exit is fulfilled by #6658 and remains closed. No waiver, failure suppression or full-suite rerun is used.

## Setup attempts and load

The initial policy attempt returned 2 while replaying diagnostics from deleted sibling checkouts. ga-039od0 was opened; sighting bfd9578b-44ab-56be-b378-005f0d8792e5 verified. A private on-disk golangci cache fixed the environment without changing code, rules or shared caches; fresh original and current policy lanes exited 0.

Two supplemental invocations stopped before tests: acceptance-A lacked gcc behind its filtered ccache wrappers; topology lost a dollar-sign regex terminator to Make quoting. Both actual exit-2 logs are preserved separately. The compiler PATH was repaired while still excluding bd/dolt; Make escaping was validated with bash -n and make -n. Only those unexecuted lanes resumed, then untouched remaining lanes ran. Completed full/policy/repetition/acceptance/compatibility evidence was never discarded or rerun to obtain green.

- Full suite: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=751 wait_timed_out=0 load_start=31.16 load_max=31.16 load_mean=19.76 samples=135 read_errors=0`
- Policy / repetition / compiler setup attempt: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=540 wait_timed_out=0 load_start=19.55 load_max=23.93 load_mean=20.10 samples=32 read_errors=0`
- Acceptance / compatibility / quoting setup attempt: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=150 wait_timed_out=0 load_start=19.11 load_max=19.11 load_mean=15.73 samples=9 read_errors=0`
- Corrected topology / generated / dashboard lanes: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 load_start=12.54 load_max=12.60 load_mean=10.23 samples=33 read_errors=0`

Load summaries include the wait phase; load_start is not a claim that tests started at that load. The earlier supplemental wait canceled for a base refresh returned 143 before any lane/test began; its separate canceled log has no summary and supplies no test evidence.

## Publication

The normal pre-commit hook owns .githooks and runs on this gate commit. Publish only the isolated deploy branch after clean status, scope, reviewed-SHA and fresh ownership checks. The documented non-diff-owned failure protocol permits an attributed-head push with --no-verify; the ownership guard is still run explicitly immediately before any such push. Clearance is success on this newly gated PR head in gastownhall/gascity, before a peek-verified merge-request to mayor. Merge belongs to MPR; this record does not merge the change.
