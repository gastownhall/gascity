**Verdict:** **PASS**

## Managed Dolt preflight wait bound

A tick, startup reconciliation pass, control dispatcher, or orders pass waits at most the shared 30-second recovery window. One recovery runs per normalized city path, continuing after the wait expires and ending on orchestrator shutdown. Refs #7170.

Evaluated 2026-10-15T05:36:57.866480+00:00 for deploy ga-imt64ec, build ga-hkx3ab, review ga-r2ictfs. DEPLOY_MODE=remote; push target=fork (quad341/gascity), selected by origin dry-run failure. No waiver applies.

| Identity | Verified value |
|---|---|
| Reviewed source | `1c9844fdc1a427dfb3c0f6a52987521894907d58` |
| Pinned gate BASE_REF | `d8adec76e8634fa82582c4054e9b924f30d1f126` |
| Base tree | `66b317227fd9ee9b768bb432a2a83ad2b51e4408` |
| Canonical merge candidate | `0daeaefe04590ab820e8a49373d845d75b4c3a8c` |
| Candidate tree | `f9dba3ceeb88106226a7c25fa632aff59def0d78` |
| Candidate parents | base, then reviewed source, exactly as above |

The canonical merge was materialized with active normal hooks and remained clean through the complete gate. A fresh authenticated commit-to-PR lookup returned no PR; the publication-time fetch still resolved origin/main to the same pinned base. Historical interrupted gates supply no evidence for this verdict.

| # | Criterion | Result and evidence |
|---|---|---|
| 1 | Review PASS present | PASS: ga-r2ictfs round 2, exact source above. Style/security clean; the round-1 API harness signature finding was fixed. Mayor gm-wisp-ecrd9s5 deferred the full suite to this gate, without waiving it. This run supplies that complete suite. |
| 2 | Acceptance criteria met | PASS: blocking health leaves the caller at its deadline; concurrent callers share one recovery; later ticks observe completion; shutdown cancels the recovery; orders do not queue past the shared window; healthy/published ports retain their behavior. The 32 changed tests pass by name, including the acceptance cases below. |
| 3 | Tests pass | PASS: complete canonical `gate-criterion3.sh` execution, exit 0, `criterion3_valid=true`, `test_cmd_scope=full-suite`. Four forced remote phases plus all 16 prescribed native-floor steps passed. Log-reader counts: 110082 PASS / 0 FAIL / 335 SKIP events, including repeats across jobs. All 32 diff-owned tests PASS; none SKIP, FAIL, or MISSING. |
| 4 | No high-severity review findings open | PASS: zero unresolved blocker/major findings in the round-2 style/security/verdict records; remaining observations explicitly non-blocking. |
| 5 | Final branch is clean | PASS: canonical candidate tracked/untracked status clean; publication adds only this gate record to the exact reviewed source. Hook and staged-change checks run for that commit. |
| 6 | Branch diverges cleanly from main | PASS: conflict-free canonical merge matches the predicted tree; `bazel build --config=remote-exec --jobs=16 --remote_download_outputs=minimal --rewind_lost_inputs --keep_going //cmd/gc:gc_test` and `make vet`/whole-tree nogo exit 0. No self-rebase needed. |
| 7 | Single feature theme | PASS: five ga-hkx3ab commits and nine changed paths cover managed-Dolt preflight flights, callers, tests, BUILD registrations, and the matching orders architecture paragraph. Ancestry/scope guards pass; source branch is provenance only. |

## Test execution and fast lane

The required Bazel lanes in `.github/workflows/bazel.yml` and `TESTING.md` are covered. Each phase ends with `test_rc=0 predicate_rc=0`; all tests use `--config=remote-exec --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1`.

| Phase | Command scope | First-attempt uncached remote results |
|---|---|---:|
| unit | `make check` (shell guards + `bazel test //...`) | 366 |
| acceptance | `bazel test --config=acceptance //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests` | 24 |
| integration-packages | `bazel test --config=integration //test:integration_packages` | 77 |
| integration-full | `bazel test --config=integration --test_arg=-test.v //test/integration:integration_test`, no smoke filter | 14 |

`test_exec_mode: remote-exec`; `test_exec_bep`: 481/481 strategy=remote, cached-remotely=0, cached-locally=0, other-strategy=0, retried=0, every start in its phase window, every result terminal. `farm_test_seconds: 7947.109`. `fallback_reason: none`; `owned_topup: []`. RBE marker present, kill switch absent, certificate expires January 9 2027; candidate/base worker-env pins match. Remote admission: floor 25 GiB, cap 2, free 100.108 GiB, waited 0.001 seconds.

`policy_lane: PASS` before runtime setup/suite: pinned golangci-lint 2.12.0 formatting, whole-tree lint/nogo, shell/core/residency/CI/hook guards, and 11/11 policy Bazel targets. `drift_lane: PASS`: isolated `make bazel-sync` plus empty tracked/untracked drift, six generated-file drift targets, and exact pinned-base `make openapi-breaking-check` (1/1). Fresh views do not modify the suite scratch. All 36 fast log/XML artifacts and 962 farm log/XML artifacts were independently checked against retained manifest/CAS digests. `ci_lane_run: n/a (no CI job/matrix/timeout/required-check change)`. `heavy_mode: none` (mandatory classifier returned mode=none); `coverage_check: n/a`, `waiver_ref: none`, `failure_attribution: none`.

`floor_topup: complete`, all 16 steps exit 0: every one of 34 environment-absence parent obligations, the pinned beads-module check, Darwin fsys compile, push-gate-lock self-test, and local-concurrency self-test. Mount-namespace cases and the real systemd drift restart pass on the host. Native-floor load is recorded separately from remote execution:

```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=58.03 run_max_load=88.58 run_mean_load=76.01 run_readings=12 wait_first_load=92.49 wait_max_load=99.39 wait_mean_load=66.00 wait_readings=61 read_errors=0
```

The soft load wait expired and the prescribed ordinary floor proceeded; no result was rerun into PASS. Outer remote-wrapper host samples are retained in recovered-run.stderr; they include the native wait and do not describe farm worker load.

`skip_inventory`: 321 farm entries (71 environment-absence, 250 other), plus 36 native entries including subtests; raw log-reader SKIP counts above have their own event/repeat convention. All 246 unique farm skip sites and all native skip sites resolve `UNCHANGED`; no skip is in a changed test/helper file, and no skip is owned. Existing process-unit skips are exercised by integration; remaining skips cover platform branches, subprocess helpers, optional providers/live-service credentials, legacy/diagnostic fixtures, and deliberate negative-start fixtures. Native opt-ins/optional Postgres/br/live-herdr and personal-tmux-dependent cases remain inventoried. These unchanged conditions do not control this diff's preflight behavior. Shared helper hunks only adapt the health-hook signature; the new flight-test helpers serve the passing owned tests. See the complete reason/site audits retained below.

## Amended complexity exit contract

PASS under base-relative-no-new-regression-v1 (PM contract gm-wisp-l2pp2xd, mayor authorizations gm-wisp-lgfhay6/gm-wisp-6i95bts), on the exact candidate/base and trees above. Original shipped-code allowlist/exclusions, analyzer, CCN threshold, and checked-in baseline are unchanged. Existing protected functions acquire no new/regressed threshold row and no new large helper is introduced.

```sh
make complexity-diff COMPLEXITY_BASE_REF=d8adec76e8634fa82582c4054e9b924f30d1f126 COMPLEXITY_TOOL=/home/jaword/go/bin/gocyclo COMPLEXITY_THRESHOLD=20
```

Executed via a fresh canonical view and pinned `gate-base.sh run`. Tool: `github.com/fzipp/gocyclo/cmd/gocyclo`, module v0.6.0; binary SHA256 `9e8e74255442d2daf91c8766f7c91f7c7d04313ab4bca611e630fffc9c43182a`. Actual exit code: 0. Full Go build-version output is retained in contract-complexity.json. Stdout:

```text
complexity: diff against d8adec76e8634fa82582c4054e9b924f30d1f126
complexity: no threshold changes
```

Stderr contains only the fresh-view/pinned-base records (retained verbatim with hashes below), with no scan error. `new:` rows=0; `regressed:` rows=0; valid scan=true. Diff mode itself always exits 0 after reporting changes; this PASS also explicitly judges the report. Historical absolute-check failures/holds remain historical; the open hygiene tracker is not closed by this scoped contract.

## Retained evidence

Root: `/var/tmp/deploy-ga-imt64ec.fresh.7p7eq_9b`; driver: `/var/tmp/gc-heavy-gate/runs/deploy-ga-imt64ec.fresh.7p7eq_9b/chain`; final trailer `GATE_RUN_EXIT rc=0 state=complete`.

- `context.json`, `c6-independent-proof.json`, `acceptance-map.json`, `contract-complexity.json`, `complexity-proof/candidate.stdout` and `.stderr` retain exact identities, command, tool/version/threshold and actual exit.
- `recovery-audit-20261015T052x.json` contains 1,016 independently verified hashes, raw BEP predicate results, and canonical-clean proof.
- `remote-skip-site-audit.json`, `native-floor-audit.json`, `native-floor-skip-audit.json` retain each skip reason/site and floor result.
- `recovered-run.stdout`/`.stderr` retain the detached command and trailer. Per-job `test_log_dir: /var/tmp/gc-heavy-gate/runs/deploy-ga-imt64ec.fresh.7p7eq_9b/criterion3/evidence/remote/logs`; immutable CAS copies/manifests, four BEPs, floor logs/record, by-name output and `execution.json` are under `/var/tmp/gc-heavy-gate/runs/deploy-ga-imt64ec.fresh.7p7eq_9b/criterion3/evidence`.

Diff-owned results (from the completed full-suite union):

```text
diff_tests_executed:
  TestCityRuntimeControlDispatcherPreflightsManagedDoltBeforeSessionSnapshot PASS (integration-packages-bc482d2ae768251ad47f.log, unit-493754ea2d5b4d8858c8.log)
  TestCityRuntimeEnsureManagedDoltPublishedForTickCallsHealthWhenManagedPortMissing PASS (integration-packages-c2ab4e7d4735e1bd66a7.log, unit-f679cd2551fcc64525aa.log)
  TestCityRuntimeEnsureManagedDoltPublishedForTickLogsOwnershipError PASS (integration-packages-251f1cf8cede4438b2d5.log, unit-2bde55fab377f50b4739.log)
  TestCityRuntimeEnsureManagedDoltPublishedForTickSkipsHealthWhenManagedPortPresent PASS (integration-packages-509ef0a6cd81866833cc.log, unit-23e7d9fc763496dd8095.log)
  TestCityRuntimeRunStartupPreflightsManagedDoltBeforeSessionSnapshot PASS (integration-packages-cb110414c557c0928e82.log, unit-64014bc0bb3833bc198c.log)
  TestCityRuntimeTickPreflightUsesResolvableProviderStateByDefault PASS (integration-packages-830685c60d0c2dd88567.log, unit-27b23e708b477fce4dad.log)
  TestCityRuntimeTickPreflightsManagedDoltBeforeSessionSnapshot PASS (integration-packages-568812e4d525bdd1f974.log, unit-4d20b3d28497f844aa39.log)
  TestCityRuntimeTickSkipsBeforeManagedDoltAndDemandUnderFSPressure PASS (integration-packages-18745ddf7d62465654f5.log, unit-64014bc0bb3833bc198c.log)
  TestManagedDoltFlightDeliversItsErrorToEveryWaiter PASS (integration-packages-074fc6c34a1fb2577127.log, unit-f93a0ca96ccc61d0cae9.log)
  TestManagedDoltFlightIsRemovedWhenItFinishes PASS (integration-packages-75af79934639abaae69c.log, unit-912ab643330b35bd0b6b.log)
  TestManagedDoltFlightJoinsAcrossSpellingsOfOneCityPath PASS (integration-packages-e4245ddf6a37030111bc.log, unit-752a0bf25b15b465517a.log)
  TestManagedDoltFlightRunMayConsultTheRegistry PASS (integration-packages-2ab73b52d8fd4dc346df.log, unit-cd6930bb0d0da8646500.log)
  TestManagedDoltFlightRunsOnceForConcurrentCallers PASS (integration-packages-9326181a736eb30b71ca.log, unit-d9e667428f972d067bd3.log)
  TestManagedDoltFlightRunsUnderTheStartersContext PASS (integration-packages-821a75e4a503601af052.log, unit-1cc8c8ab55e59a9e03a9.log)
  TestManagedDoltFlightStartedAtReportsTheRunningFlight PASS (integration-packages-514989cc035c0e9a8ff5.log, unit-088cc38d26d094729229.log)
  TestManagedDoltFlightWaitEndsWithItsContextAndLeavesTheFlightRunning PASS (integration-packages-bf8ed8ba46004086b3b9.log, unit-abd97d2cb9b450225d57.log)
  TestManagedDoltFlightWindowIsThirtySeconds PASS (integration-packages-12697e2ea0d32c278115.log, unit-1ab526405ace21879f85.log)
  TestManagedDoltFlightsAreSeparatePerCity PASS (integration-packages-56f04454dd9158a9ee8a.log, unit-fd986cf5bab8709b8d45.log)
  TestManagedDoltPreflightContainsAHealthHookPanic PASS (integration-packages-830685c60d0c2dd88567.log, unit-0e1499ad8e8a0a6b9d81.log)
  TestManagedDoltPreflightJoinerDoesNotLogTheOwnersFailureAgain PASS (integration-packages-75f7330b9b673df65e4f.log, unit-2bde55fab377f50b4739.log)
  TestManagedDoltPreflightLaterTickSeesTheFinishedRecover PASS (integration-packages-251f1cf8cede4438b2d5.log, unit-f679cd2551fcc64525aa.log)
  TestManagedDoltPreflightLogsARecoverFailureOnceAndRetriesOnTheNextTick PASS (integration-packages-568812e4d525bdd1f974.log, unit-23e7d9fc763496dd8095.log)
  TestManagedDoltPreflightLogsTheExpiryOncePerRecover PASS (integration-packages-509ef0a6cd81866833cc.log, unit-10cc7795c0ec199a5530.log)
  TestManagedDoltPreflightReturnsAtTheWindowWhileTheRecoverKeepsRunning PASS (integration-packages-c2ab4e7d4735e1bd66a7.log, unit-5a9df316125cd0aeff2e.log)
  TestManagedDoltPreflightShutdownCancelsTheRecover PASS (integration-packages-cb110414c557c0928e82.log, unit-4d20b3d28497f844aa39.log)
  TestManagedDoltPreflightStartsNoFlightWhenThereIsNothingToRecover PASS (integration-packages-b72a67a2f9444af705e3.log, unit-3d6d3036a2f5902472b5.log)
  TestManagedDoltPreflightWindowExpiryLeavesTheRecoverRunning PASS (integration-packages-bc482d2ae768251ad47f.log, unit-2ef13e1b6898c5544764.log)
  TestManagedDoltPreflightWindowIsAnchoredToTheRecoversStart PASS (integration-packages-18745ddf7d62465654f5.log, unit-64014bc0bb3833bc198c.log)
  TestNewCityRuntimePreflightUsesResolvableProviderStateByDefault PASS (integration-packages-9c8cd94da10f0dd04b00.log, unit-3d6d3036a2f5902472b5.log)
  TestNewCityRuntimePreflightsManagedDoltPublicationBeforeStartupStoreWork PASS (integration-packages-18745ddf7d62465654f5.log, unit-0e1499ad8e8a0a6b9d81.log)
  TestOrdersLanePassDoesNotQueueBehindARecoverPastTheWindow PASS (integration-packages-9c8cd94da10f0dd04b00.log, unit-493754ea2d5b4d8858c8.log)
  TestOrdersLanePassPreflightsManagedDoltBeforeDueOrderDispatch PASS (integration-packages-75f7330b9b673df65e4f.log, unit-2ef13e1b6898c5544764.log)
GATE_TEST_EVIDENCE_RESULT ok: 32 diff-owned tests verified by name
```

No production gc build/install/deploy was performed. Publication is from the isolated deploy branch; MPR owns merging. The PR uses Refs #7170; no issue #7170/PR #6164 comment or upstream issue is part of this release.

Publication-hook setup: the first ordinary gate-record commit invoked docs-sync without an execution config and began a cold local build. Its owned Bazel client was interrupted before any test verdict (0 tests executed); the partial log and verified client identity remain in gate-commit.log and hook-environment-correction.json. The same active hook is then run with explicit remote execution and no local fallback; its separate command/BEP/log and actual exit are retained. This setup interruption supplies no PASS or failure attribution for the canonical suite above.
