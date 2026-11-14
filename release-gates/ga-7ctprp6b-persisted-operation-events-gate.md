# Persisted identity for worker operation events

**Verdict:** **PASS**

Bead: ga-7ctprp6b; build: ga-smj6p7; review: ga-c28t4x7k.
Reviewed source: `e2db2355ce723d6beb592d8c32cdf3e3ea2d32dc`; frozen base: `6bd609f5bfb41f43f444843b1220d0cde4473446`.
Clean validation merge: `ca4f7680c2907e7f28633fccfe4c02e693cf0e9e`; tree: `54ddbb9e154e3d77a29512d00b340c9a5019e60b`.
Evaluated: 2026-11-11T17:57:41.272160+00:00

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Reviewer PASS and reviewer-only test addition mechanically verified. |
| 2 | Acceptance criteria met | PASS | Provider-call parity, final Stop, persisted identity/error/type-heal/payload controls and per-method ACP routing verified. Eight regression roots fail on unchanged main production with a test-only adapter; all 14 pass on the branch. Optional StartResolved reuse was dropped; no live hung-provider reproduction. |
| 3 | Tests pass | PASS | Five fresh remote phases, full native floor, fast lanes, by-name audit, skip proofs and six-leaf CI accounting complete. |
| 4 | No high-severity findings open | PASS | Review security/style/spec findings have zero unresolved HIGH issues. |
| 5 | Final branch clean | PASS | Reviewed source, validation tree and deployment checkout clean before adding this intentional gate record; final committed cleanliness checked before publication. |
| 6 | Merge state recorded; build/vet pass | PASS | Frozen clean merge builds and passes whole nogo/vet; terminal BEPs and source/parents/tree verified. |
| 7 | Single feature theme | PASS | Persisted worker operation-event identity only; code, tests and owning invariant documentation. |

merge_state: clean (origin/main@6bd609f5bfb41f43f444843b1220d0cde4473446)
gate_base_kind: current-tip
merge_validation: clean merged tree tested; MPR retains exact-head and merge-time validation.
deploy_mode: remote; push_remote: fork
project_manifest: docs/PROJECT_MANIFEST.md absent; supplied release criteria, TESTING.md, Makefile and actual CI workflows determine this gate.

## Test evidence

test_cmd: /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-criterion3.sh --repo /var/tmp/deploy-ga-7ctprp6b.joyuumzs/merge --base 6bd609f5bfb41f43f444843b1220d0cde4473446 --bead-json /var/tmp/deploy-ga-7ctprp6b.joyuumzs/ga-7ctprp6b.json --owned /var/tmp/deploy-ga-7ctprp6b.joyuumzs/owned.tsv --run-dir /var/tmp/deploy-ga-7ctprp6b.joyuumzs/c3-run/evidence; exit 0
test_cmd_scope: full-suite
test_counts: PASS=150752 FAIL=0 SKIP=515
Counts are top-level Go result lines per job; subtest skip events are inventoried separately.
test_log_dir: /var/tmp/deploy-ga-7ctprp6b.joyuumzs/c3-run/evidence/remote/logs
test_exec_mode: remote-exec
test_exec_bep: {"acceptance": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 25, "results": 25, "retried": 0, "window_ok": true}, "integration-full": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 14, "results": 14, "retried": 0, "window_ok": true}, "integration-latency": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 1, "results": 1, "retried": 0, "window_ok": true}, "integration-packages": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 86, "results": 86, "retried": 0, "window_ok": true}, "unit": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 408, "results": 408, "retried": 0, "window_ok": true}}
rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98", "worker_env_tree": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"}
fallback_reason: none
admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 51.803775787353516, "waited_seconds": 0.002}
farm_test_seconds: 12799.99
skip_inventory: /var/tmp/deploy-ga-7ctprp6b.joyuumzs/c3-run/evidence/remote/skip-inventory.json; SHA256 57ab355e330b5249bd18e22bf0cf99cdaac5582cba383a1888bc50816d0ca6e7
owned_topup: /var/tmp/deploy-ga-7ctprp6b.joyuumzs/c3-run/evidence/remote/owned-topup.json; SHA256 37517e5f3dc66819f61f5a7bb8ace1921282415f10551d2defa5c3eb0985b570
floor_topup: complete=true, 16/16 commands exit 0; /var/tmp/deploy-ga-7ctprp6b.joyuumzs/c3-run/evidence/remote/floor-topup.json; SHA256 31d77ec700e49d5af3ab52d07e37072e1f2ff3c1685c979a741993fb83898672
latency_leg: measured, complete=true, rc=0; both named tests PASS in a clear-probe leg; /var/tmp/deploy-ga-7ctprp6b.joyuumzs/c3-run/evidence/remote/latency-leg.json; SHA256 b17c20a17921629e03564106e8e3456dbfaba7ad5b5236939c2e5cc9f86a6e2c
heavy_mode: none (computed)
waiver_ref: none (gascity has no waiver path)
failure_attribution: none
ci_lane_run: n/a (no CI-config change)

diff_tests_executed:
```text
GATE_TEST_EVIDENCE logs_dir=/var/tmp/deploy-ga-7ctprp6b.joyuumzs/c3-run/evidence/remote/logs logs=550 logs_with_results=482
test_counts: PASS=150752 FAIL=0 SKIP=515
  (top-level result lines, column 0, summed over the 550 logs; a test that several jobs run counts once per job; sub-tests are not counted)
logs_without_results: floor-13.log floor-14.log floor-15.log unit-02e0324f1ef05430d7e4.log unit-04c4f9e4335a6f1c2dfa.log unit-0787047af2c4dad6aebc.log unit-0eb92a6bd00590d91db6.log unit-0f39cb7e4c03399a72c4.log unit-1a34395a607c0208a005.log unit-1e67f201b300dc5f9e78.log unit-21372f682c4033d14a80.log unit-22d5121476fcaa40b586.log (+56 more)
diff_tests_executed:
  TestOperationEventIdentityDoesNotWaitOnWedgedProvider PASS (unit-481d1d821b2235b34d5b.log)
  TestOperationEventIdentityIssuesNoProviderCall PASS (unit-481d1d821b2235b34d5b.log)
  TestSessionRecordPersistedHealsEmptyType PASS (unit-481d1d821b2235b34d5b.log)
  TestSessionRecordPersistedKeepsErrorContract PASS (unit-481d1d821b2235b34d5b.log)
  TestSessionRecordPersistedMatchesEnrichedIdentity PASS (unit-481d1d821b2235b34d5b.log)
  TestSessionRecordPersistedMatchesEnrichedIdentityMatrix PASS (unit-481d1d821b2235b34d5b.log)
  TestWorkerOperationEventAddsNoProviderCall PASS (unit-481d1d821b2235b34d5b.log)
  TestWorkerOperationEventAddsNoTransportProbe PASS (unit-481d1d821b2235b34d5b.log)
  TestWorkerOperationEventKeepsProviderCallTrail PASS (unit-481d1d821b2235b34d5b.log)
  TestWorkerOperationEventPayloadIdentityUnchanged PASS (unit-481d1d821b2235b34d5b.log)
  TestWorkerOperationEventPayloadNamesPersistedIdentity PASS (unit-481d1d821b2235b34d5b.log)
  TestWorkerOperationKillEventEndsOnStop PASS (unit-481d1d821b2235b34d5b.log)
  TestWorkerOperationStartEventAddsNoProviderCall PASS (unit-481d1d821b2235b34d5b.log)
  TestWorkerOperationStartResolvedEventAddsNoTransportProbe PASS (unit-481d1d821b2235b34d5b.log)
GATE_TEST_EVIDENCE_RESULT ok: 14 diff-owned tests verified by name
```

## Acceptance and review

reviewer_tests_verified: f34460e1ad470c41ebe78351cd04e72f2d0706b5 -> e2db2355ce723d6beb592d8c32cdf3e3ea2d32dc files=internal/worker/operation_events_persisted_test.go
AC1/AC2: recorder parity over lifecycle operations, detector/ACP counters and final Kill Stop. AC3/AC4/AC5: six identity fields plus RunID, live-overlay contrast, missing/non-session errors, empty-type heal and unchanged payload. AC6: not applicable; optional reuse dropped. AC7: baseline RED/GREEN and no-live-reproduction wording included in PR draft.
ACP routing was checked per manager method. ResetFenced/CheckResetFenced use CLI providers seeded at construction and fail closed when a runtime-only route is unavailable. Retained enriched reads and prompt-finish telemetry are explicitly described in the PR.
Baseline: /var/tmp/deploy-ga-7ctprp6b.joyuumzs/baseline-audit.json; SHA256 7a888a038c455d7ce2f2e23649fd51a1094b13655d4b58023a27b862171e88f9

## Fast lanes

policy_lane: fresh-tree-run.sh -> gate-base.sh run -> bash /var/tmp/deploy-ga-7ctprp6b.joyuumzs/fast-lane.sh policy; exit 0; run-pinned-lint.sh -- make fmt-check-changed, make lint, make check-release-dist-ignore check-routed-test-rows check-split-topology-rows check-residency-boundary check-hooks, and all 13 explicit policy targets PASS.
drift_lane: fresh-tree-run.sh -> gate-base.sh run -> bash /var/tmp/deploy-ga-7ctprp6b.joyuumzs/fast-lane.sh drift; exit 0; make bazel-sync, git diff --exit-code, empty git status --porcelain --untracked-files=all, and all seven generated/client/lockfile targets PASS.
openapi_lane: fresh-tree-run.sh -> gate-base.sh run -> bash /var/tmp/deploy-ga-7ctprp6b.joyuumzs/fast-lane.sh openapi; make openapi-breaking-check against pinned GATE_BASE_SHA exit 0.
storeclass_policy: fourth fresh view; trusted-base generator, strict baseline and growth checks exit 0.
known_red_precheck: proceed, no known-red blocker.

## Skip justification

All 14 diff-owned test roots and their subtests PASS, with no FAIL or SKIP. Remaining skips are unchanged platform guards, suite-lane/process guards, explicit fixture/capability limitations, manual/legacy scenarios or unavailable external tools/data. All 327 remote/native source-site checks (291 distinct sites) are UNCHANGED and their file blobs equal the pinned base. The only SHARED-HUNKS package is internal/worker: new test-only fixtures, with no init/TestMain/package mutable globals or references from other files. Import closures reach it from cmd/gc, internal/api, internal/runtime/tmux and internal/worker/workertest; all 312 remote and seven native reachable skip events reproduce by selected test name at the pinned base with the identical reason. The other 189 remote and 32 native events cannot reach those helper hunks. Both native baseline commands exit0; the load wait timed out after1801s and ran at86.00/89.46/87.96 (start/max/mean), recorded without excusing a failure. The dispatch missing reason is proven by the exact adjacent raw CONT/message/SKIP sequence; program_status.go:141 resolves to the imported runtimetest helper, the only candidate with a Skip at that line. Raw logs are unchanged. These non-owned skips do not certify changed behavior; that is covered by the full-suite owned tests and acceptance controls.
Full skip site, source/blob, import-closure and same-reason baseline proofs: /var/tmp/deploy-ga-7ctprp6b.joyuumzs/skip-proof-automated.json; SHA256 c951eb7afaca6ebe03aa991f381c8e375f39bccbe2dc55614116cb4d9485d24c
Non-Go skip review: Bridge lockfile unittest has one configured optional skip (no --rules-ts-typescript); unchanged macro default and bridge BUILD/data/source prove non-applicability. Dashboard opts in and passes all three checks. See non-go-skip-reviewed.json.

## Required-job accounting

ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/deploy-ga-7ctprp6b.joyuumzs/merge 6bd609f5bfb41f43f444843b1220d0cde4473446 e2db2355ce723d6beb592d8c32cdf3e3ea2d32dc fork; exit 0; workflow_blob=fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs.
```text
diff 6bd609f5bfb41f43f444843b1220d0cde4473446...e2db2355ce723d6beb592d8c32cdf3e3ea2d32dc: 7 changed files
  filter packs                no match
  filter credential_provider  no match
  filter shared               no match
suite_mode: filtered  (shared filter did not match)

ci-required needs 7 jobs; transitive closure 7 jobs
job key                                  kind       runs-on                    trigger
runner-policy                            LEAF       linux                      EVERY-PR
changes                                  LEAF       linux                      EVERY-PR
check                                    AGGREGATOR linux                      EVERY-PR
  credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'
  pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'
  windows-lane                           LEAF       linux                      EVERY-PR
  windows-lane-test                      LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} EVERY-PR

6 LEAF jobs need a status; the rest are rolled up through their needs.
```

ci_required_accounting:

- changes | LOCAL-PASS | {"evidence": "/var/tmp/deploy-ga-7ctprp6b.joyuumzs/ci-required/closure.stdout", "status": "LOCAL-PASS"}
- runner-policy | LOCAL-PASS | {"evidence": "/var/tmp/deploy-ga-7ctprp6b.joyuumzs/ci-required/runner-policy.stdout", "status": "LOCAL-PASS"}
- windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | {"evidence": "/var/tmp/deploy-ga-7ctprp6b.joyuumzs/ci-required/windows-native-reach.json", "reason": "non-linux-runner", "status": "DEFERRED-TO-PR-CI", "windows_reach": "none"}
- credential-provider-windows | NOT-TRIGGERED | {"evidence": "  credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'", "status": "NOT-TRIGGERED", "windows_reach": "none"}
- windows-lane | LOCAL-PASS | {"command": ["/home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh", "--from", "/var/tmp/deploy-ga-7ctprp6b.joyuumzs/merge", "--", "/home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh", "--threshold", "15", "--max-wait", "1800", "--", "/home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh", "--", "python3", "/var/tmp/deploy-ga-7ctprp6b.joyuumzs/run-ci-job.py", "windows-lane"], "rc": 0, "status": "LOCAL-PASS", "stderr": "/var/tmp/deploy-ga-7ctprp6b.joyuumzs/ci-required/windows-lane.stderr", "stdout": "/var/tmp/deploy-ga-7ctprp6b.joyuumzs/ci-required/windows-lane.stdout"}
- pack-gate | NOT-TRIGGERED | {"evidence": "  pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'", "status": "NOT-TRIGGERED"}

windows_reach: {"credential": [], "native": []}
deferred_to_pr_ci: windows-lane-test (non-linux-runner)
not_covered: none
host_gaps: none
Merge requires current-head CI / required and each deferred job to pass. This gate authorizes PR-open only.

## Observed load

c3-run/evidence/remote/floor-topup.stdout: LOAD_GATE_SUMMARY threshold=15 waited_seconds=390 wait_timed_out=0 run_start_load=14.39 run_max_load=19.67 run_mean_load=16.76 run_readings=8 wait_first_load=21.78 wait_max_load=21.78 wait_mean_load=17.62 wait_readings=14 read_errors=0
c3-run/units/gate.w1.stderr.log: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=94.68 run_max_load=113.90 run_mean_load=52.82 run_readings=61 wait_first_load=94.68 wait_max_load=94.68 wait_mean_load=94.68 wait_readings=1 read_errors=0
storeclass-proof/wrapper.stderr: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=89.87 run_max_load=94.77 run_mean_load=91.80 run_readings=6 wait_first_load=47.23 wait_max_load=94.35 wait_mean_load=70.56 wait_readings=61 read_errors=0
ci-required/windows-lane.stderr: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=139.04 run_max_load=143.02 run_mean_load=141.53 run_readings=6 wait_first_load=23.27 wait_max_load=150.14 wait_mean_load=103.52 wait_readings=61 read_errors=0
skip-base-evidence/native.stderr: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=86.00 run_max_load=89.46 run_mean_load=87.96 run_readings=4 wait_first_load=92.82 wait_max_load=93.61 wait_mean_load=73.49 wait_readings=61 read_errors=0
load_threshold: 15
load_waited_seconds: 390
load_wait_timed_out: 0
run_start_load: 14.39
run_max_load: 19.67
run_mean_load: 16.76
run_readings: 8

## Retained evidence

C6 build/vet: /var/tmp/deploy-ga-7ctprp6b.joyuumzs/c6-verified.json; SHA256 0e147ddf1b046992bd32a2b2bdf1144e2a6d44aa2e00bb7d20cc66718a1e81dc
Fast lane: /var/tmp/deploy-ga-7ctprp6b.joyuumzs/fast-verified.json; SHA256 58c46c600d660c27df5d7f853c9cd5cf8bb287a7433aa605f914475ab905e525
Raw full-suite and artifact audit: /var/tmp/deploy-ga-7ctprp6b.joyuumzs/resume-c3-audit.json; SHA256 ea6aa4045dbe3a11b29d5a605db5b15c6bd93b78dafa0a925d56b27952af483c
Package-bound owned results: /var/tmp/deploy-ga-7ctprp6b.joyuumzs/owned-package-verified.json; SHA256 7d77f83b1127fc2e61a63021c8c64592f14b1676e1fa77285016a36e47fb5cfd
Required CI jobs: /var/tmp/deploy-ga-7ctprp6b.joyuumzs/ci-required/accounting-verified.json; SHA256 1ff2cd18f17e20d5cde9956ee2b6de83791d0eb7b1fdaa1e8de741a82459f592
Manual acceptance/skip review: /var/tmp/deploy-ga-7ctprp6b.joyuumzs/final-manual-review.json; SHA256 1fc2fef5baa7d5dbfe88578bedcdd0f120956d86d2653d334e394b4306bb9a04
