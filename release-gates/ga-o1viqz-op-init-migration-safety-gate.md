# Release gate: safe beads database initialization

**Verdict:** **PASS**

- Bead: ga-o1viqz; designated build/fix bead: ga-3jssfa (gc.fixes_tracker=ga-lejnse).
- Reviewed source: `0330b86507e9101f015dc91159fc66fe4ec5f329`; provenance branch: builder/ga-3jssfa.
- Deploy branch: deploy/ga-o1viqz-gate; DEPLOY_MODE=remote; PUSH_REMOTE=fork.
- Evaluated: 2026-09-26T04:48:09.978227+00:00.
- Tested merge: `3f38693d6da822c1c40cf47e929107678ec3e8ca`, tree `83cba798e5efd85dcaef842782dafdbf080f84f1`, against base `a158e519c8d25ac476c4750257185bea65d7f3e7`.
- Latest criterion-6 base: `2d7d33954459e0da1aec2feacfb60b810f02afb5`; clean merge tree `a4b3356374904f9de01ebf53729ddca921966199`.
- The source stayed pinned throughout. Main advanced by the independently gated transport deadline fix (#6657); the full-suite evidence names its original merge snapshot explicitly.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Fresh full-diff reviewer PASS at 2026-09-26 03:17Z on the exact reviewed source, recorded on ga-o1viqz. Historical carryover mismatch is superseded by that fresh review. |
| 2 | Acceptance criteria met | PASS | Existing tables, active migration cursors, and parked migration history prevent treating a store as fresh. Progress resets the wait deadline; stalled progress stops the wait. Forced init serializes by database and revalidates inside the lock. All relevant named regressions below PASS, plus the live beads/events contract and failure-cleanup workflow. |
| 3 | Tests pass | PASS | Fresh documented `make test-local-full-parallel LOCAL_TEST_JOBS=4`: all 40 jobs PASS, 0 FAIL, 0 job SKIP; wrapper exit 0. 95427 PASS / 0 FAIL / 327 SKIP test/subtest events across repeated suite coverage. All 13 relevant top-level tests and 21 subtests PASS; none FAIL or SKIP. |
| 4 | No unresolved HIGH finding | PASS | Reviewer explicitly reports zero unresolved HIGH findings. The shared temporary lock-directory default and warn/proceed on an indeterminate revalidation remain two nonblocking findings. |
| 5 | Final branch clean | PASS | Reviewed checkout was clean. This record is the only final staged update; publishing is guarded by a clean status check after the gate commit. |
| 6 | Branch merges cleanly with main | PASS | `git merge-tree --write-tree` succeeds against the latest base above. No self-rebase or source substitution. |
| 7 | Single feature theme | PASS | One op_init migration-safety theme: state classification, progress-based settling, serialized/revalidated force, regression proofs, and matching resource-census ratchets. Confirmed related ancestry IDs: ga-o1viqz, ga-3jssfa, ga-m1qxc8, ga-nw3yms, ga-clv1sz, ga-e2z1zb; no stack or denied paths. |

`test_cmd_scope: full-suite`

`test_cmd: load-gate-run.sh --threshold 15 --max-wait 1800 -- isolated-test-run.sh -- make test-local-full-parallel LOCAL_TEST_JOBS=4`

`waiver_ref: none`; `failure_attribution: none`; `policy_attribution: none`.

Runtime: rootless Podman socket configured, Ryuk disabled per fleet recipe, pinned cached images verified before running. Short private HOME (19 bytes), empty .zshrc, normal shared Go/module caches, TMPDIR=/var/tmp, GOFLAGS=-v.

skip_justification: Unchanged platform/permission probes, helper-process entries, optional live profiles/services and test-category routing account for the remaining skips. No relevant init test skipped. TestTutorial01 PASS in its process shard; its integration-category skip does not replace that proof. Native DoltLite tests also ran separately and PASS.

policy_lane: `make test-ci-policy` PASS; `make lint-affected fmt-check-changed LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=a158e519c8d25ac476c4750257185bea65d7f3e7` PASS (0 issues and exit 0, including standalone vet of the affected closure). The first static run is discarded because its cached diagnostics named removed scratch trees; a private on-disk lint cache resolved the stale paths without code edits or shared Go-cache clearing.

Additional preflight: `make check-gomod-replace check-native-dependency-surface check-eventexport-isolation check-core-boundary test-native-doltlite-beads` PASS. Native dependency binary: 177692187 bytes. Native DoltLite package: 17.947s. Canonical merge materialization ran the active pre-commit hook; `go build ./...` and `go vet ./...` PASS. `make check-hooks` PASS.

CI coverage: documented full local union covers cmd/gc process, productmetrics-testhook, integration package/runtime shards, review-formula basic/retry/recovery, beads-store, REST smoke and full REST lanes; independent static/policy/native guards cover preflight. `ci_lane_run: n/a (no CI-config change in this diff)`.

load_threshold: 15
load_waited_seconds: 0
load_wait_timed_out: 0
load_start: 10.49
load_max: 33.74
load_mean: 22.09

Load sampler: samples=74, read_errors=0.

diff_tests_executed:

- `TestBdRuntimeBdTableCountRejectsUnsafeDatabaseNames` — PASS
- `TestForceReinitGuardWaitGivesUpOnStalledCursor` — PASS
- `TestForceReinitGuardWaitOutlastsCursorAdvancing` — PASS
- `TestForceReinitGuardWaitOutlastsSettleTimeoutWhileCursorAdvances` — PASS
- `TestGCBeadsBDScript_UsesPortableSleepMS` — PASS
- `TestGcBeadsBdInitConcurrentInvocationsDoNotBothForceReinit` — PASS
- `TestGcBeadsBdInitRefusesForcedReinitWhenCursorAdvancesBetweenClassificationAndForce` — PASS
- `TestGcBeadsBdInitRefusesForcedReinitWhenDatabaseHoldsBdTables` — PASS
- `TestGcBeadsBdInitRefusesForcedReinitWhenMigrationCursorIsAdvancing` — PASS
- `TestGcBeadsBdScriptDocumentsSchemaSettleTimeoutOverride` — PASS
- `TestStoreHoldsBdTablesConsidersMigrationCursor` — PASS
- `TestStoreHoldsBdTablesConsidersParkedMigrationHistory` — PASS
- `TestStoreHoldsBdTablesDistinguishesEmptyFromUndetermined` — PASS

Acceptance within the full suite:

- `TestGCLiveContract_BeadsAndEvents` — --- PASS: TestGCLiveContract_BeadsAndEvents (49.05s)
- `TestGraphWorkflowFailureRunsCleanup` — --- PASS: TestGraphWorkflowFailureRunsCleanup (296.91s)

Evidence: `/var/tmp/ga-o1viqz-full-r4.log`, `/var/tmp/ga-o1viqz-full-shards-r4/`, `/var/tmp/ga-o1viqz-policy-r4.log`, `/var/tmp/ga-o1viqz-static-r5.log`, `/var/tmp/ga-o1viqz-preflight-r4.log`. Historical partial/failing gate records are not evidence for this run.
