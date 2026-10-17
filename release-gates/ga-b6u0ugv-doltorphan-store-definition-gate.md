# Dolt store-definition release gate — ga-b6u0ugv

**Verdict:** **PASS**

Reviewed source: fcb8a7600d4bb22cc52469c316cc643f2c5112be
Frozen base: b11f2a52489a11e753681b7513fe8fc632a8f6a7
Canonical merge commit: 0e261646128188f207270c99a2ccdd527be2460f
Canonical merge tree: ca0ee95042260edc69b48a106fabb96717836d28
gate_base: b11f2a52489a11e753681b7513fe8fc632a8f6a7
gate_base_kind: current-tip
merge_state: clean (origin/main@b11f2a52489a11e753681b7513fe8fc632a8f6a7)
merge_validation: merge result built and vetted here; no rebase
DEPLOY_MODE: remote; BASE_REF: origin/main

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Reviewer PASS | PASS | ga-dzmgn7b records verdict pass on the resolved reviewed source. No source substitution or review carryover. |
| 2 | Acceptance | PASS | AC1 RED/GREEN independently reproduced; AC2–11 inspected and exercised, including the native real-Dolt canary; AC12 uses the contract owner's amended complexity condition and normal hooks. The prepared PR body carries AC13's main base, RED/GREEN output and prior builder hook-bypass disclosure; exact public body readback is required before clearance and handoff. |
| 3 | Full CI-equivalent suite | PASS | All four forced-fresh remote phases, all required native floor steps, all eleven diff-owned tests, and the path-selected live Pack compatibility gate pass. Detailed evidence below. |
| 4 | No unresolved HIGH findings | PASS | Review has zero HIGH/blocker findings; minor follow-ups remain outside this scope. |
| 5 | Clean candidate | PASS | Canonical merge has clean tracked and untracked status. Publication checks that only this gate record is added, commits it through the normal hook, and requires a clean branch before pushing. |
| 6 | Merge state and merged build/vet | PASS | Merge-tree prediction exactly equals the canonical merge tree; Bazel build //... and full-tree make vet/nogo pass. Real pre-commit hooks ran during materialization. |
| 7 | One feature theme | PASS | One store-definition rule, its unit regressions and its real-Dolt canary, across three source files. |

The already-merged preflight returned no associated PR. The deploy-ref interlock and ancestry guard passed with ga-b6u0ugv and source build ga-47vvd8. The reviewed source stays pinned; the provenance branch is never a push target. No production binary deployment is requested.

`docs/PROJECT_MANIFEST.md` is absent. Release commands were resolved from AGENTS.md, TESTING.md, the Makefile, the Bazel workflow and the mandatory deploy protocol.

## Acceptance evidence

The prior absolute-baseline HOLD is historical. PM and mayor explicitly adopted base-relative-no-new-regression-v1 for this source and deploy. This is a contract amendment, with no waiver. The candidate runs `make complexity-diff` against this same gate's full-SHA base, verified gocyclo v0.6.0 and CCN20, with unchanged analyzer, production exclusions and baseline. Exit0 AND zero new/regressed rows were verified. Result: no threshold changes. Tool hash/buildinfo, both commit/tree pairs, command and raw output are retained in /var/tmp/deploy-ga-b6u0ugv.df62p7zv.

Independent RED: candidate tests with the original source-base sweep implementation yield 34 PASS, 13 FAIL, 0 SKIP result lines, including subtests (Bazel exit3). Candidate restoration is byte-clean; GREEN yields 47 PASS, 0 FAIL, 0 SKIP. The failing cases include the multi-pass ancestor, HOME-shaped tree, six invalid-store table cases, probe errors, depth limit and `.dolt` traversal. The focused native test `TestSweep_ReapsRealDoltDataDirAfterSIGKILL` also passes with `/usr/local/bin/dolt`, integration tags and count1; raw output is in focused-real-dolt.log.

## Fast lane

fast_lane: PASS 2026-10-17T11:19:06Z
policy_lane: PASS — format check, full-tree nogo/vet, changed-package nogo, required shell guards and all nine remote static policy/format targets
drift_lane: PASS — make bazel-sync, git diff --exit-code and clean untracked status
heavy_mode: none
ci_lane_run: n/a (no CI-config change)
waiver_ref: none (gascity has no waiver path)

Every fast lane ran in a fresh view of the committed merge. Per-lane FRESH_TREE_RECORD lines and raw logs are retained. Build/nogo cache use is permitted; no cached test result is credited.

## Required workflow coverage

The three changed paths select Bazel unit, acceptance, integration-packages and integration-smoke (the full integration run covers the smoke inventory), BUILD-file sync, and CI's Pack compatibility gate. `examples/gastown/**` is the `packs` path filter in `.github/workflows/ci.yml`. The registry-pin command `scripts/update-bundled-gastown-pack --check` exited 0; `GC_TEST_GASCITY_PACKS_REGISTRY=main GOFLAGS=-v make test-pack-registry-live` ran both named tests with real registry access and neither failed nor skipped. Per-test output, command and load measurements are retained in pack-live-verbose.log. No new workflow was introduced by this diff.

## Full-suite execution

test_cmd_scope: full-suite
test_exec_mode: remote-exec (BEP-derived farm phases; native floor recorded separately)
test_log_dir: /var/tmp/deploy-ga-b6u0ugv.df62p7zv/c3-run/evidence/remote/logs
test_counts: PASS=116744 FAIL=0 SKIP=370
farm_test_seconds: 8119.391
fallback_reason: none
test_exec_bep: {"unit":{"results":371,"remote_executed":371,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":24,"remote_executed":24,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":81,"remote_executed":81,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":14,"remote_executed":14,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}}
owned_topup: []
floor_topup: complete, 16 required steps, all exit0
skip_inventory: {"other":285,"environment-absence":71}; full inventory in retained execution.json
native_skip_inventory: 39 rows, all non-owned and with unchanged source sites; complete reasons and proofs in native-skip-proof.json
rbe_state: {"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:0c6c05c2196c246dba37e106a3d028e2f47f2e3d7eae0e41c2fe2bd9b2e4f6f0","worker_env_origin_main":"sha256:0c6c05c2196c246dba37e106a3d028e2f47f2e3d7eae0e41c2fe2bd9b2e4f6f0"}
admission: {"class":"remote","floor_gib":25.0,"free_gib":110.69868469238281,"waited_seconds":0.001,"cap":2}

| Phase | Verdict | Results | Fresh remote | Cached | Retried |
|---|---|---:|---:|---:|---:|
| unit | PASS | 371 | 371 | 0 | 0 |
| acceptance | PASS | 24 | 24 | 0 | 0 |
| integration-packages | PASS | 81 | 81 | 0 | 0 |
| integration-full | PASS | 14 | 14 | 0 | 0 |

Every BEP result uses strategy remote, neither cached flag, attempt1, a start inside its phase window, and a terminal verdict. No mixed execution or failed-result retry. Phase4 is full integration without a smoke filter. The isolation wrapper was used; its per-repository pass-through behavior is recorded in raw output.

Phase commands (with the forced-fresh/no-local-fallback flags recorded below):

```text
unit: make check
acceptance: bazel test --config=acceptance //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests
integration-packages: bazel test --config=integration //test:integration_packages
integration-full: bazel test --config=integration --test_arg=-test.v //test/integration:integration_test
```

Actual phase flags:

```text
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-b6u0ugv.df62p7zv/c3-run/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-b6u0ugv.df62p7zv/c3-run/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-b6u0ugv.df62p7zv/c3-run/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-b6u0ugv.df62p7zv/c3-run/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```

The counts are Go result lines from the retained log reader; target/shard verdicts, including non-Go tests, are in the BEP table. Remaining SKIPs are inventoried repository opt-ins, platform/provider limitations and configuration cases. No remote SKIP belongs to this diff or cites a changed file. All environment-absence cases were selected for the required native floor; non-owned native SKIPs stay inventoried, while native failures would fail this gate. All eleven owned tests and their subtests pass by name:

```text
GATE_TEST_EVIDENCE logs_dir=/var/tmp/deploy-ga-b6u0ugv.df62p7zv/c3-run/evidence/remote/logs logs=506 logs_with_results=440
test_counts: PASS=116744 FAIL=0 SKIP=370
  (top-level result lines, column 0, summed over the 506 logs; a test that several jobs run counts once per job; sub-tests are not counted)
logs_without_results: floor-13.log floor-14.log floor-15.log unit-02e0324f1ef05430d7e4.log unit-04c4f9e4335a6f1c2dfa.log unit-0787047af2c4dad6aebc.log unit-0eb92a6bd00590d91db6.log unit-0f39cb7e4c03399a72c4.log unit-1a34395a607c0208a005.log unit-1e67f201b300dc5f9e78.log unit-21372f682c4033d14a80.log unit-22d5121476fcaa40b586.log (+54 more)
diff_tests_executed:
  TestSweep_ReapsRealDoltDataDirAfterSIGKILL PASS (integration-packages-b51d44b27e6601511c78.log)
  TestSweep_AncestorMarkerNeverBecomesTarget PASS (unit-961301b9c2db4db553db.log)
  TestSweep_AncestorThatIsAStoreGoesWhole PASS (unit-961301b9c2db4db553db.log)
  TestSweep_DoltDirIsNeverDescendedInto PASS (unit-961301b9c2db4db553db.log)
  TestSweep_DoltDirWithoutNomsIsNotAStore PASS (unit-961301b9c2db4db553db.log)
  TestSweep_HomeShapedTreeIsNeverACandidate PASS (unit-961301b9c2db4db553db.log)
  TestSweep_SiblingStoresConvergeOnePerAge PASS (unit-961301b9c2db4db553db.log)
  TestSweep_SiblingStoresNeverWidenToContainer PASS (unit-961301b9c2db4db553db.log)
  TestSweep_StoreAtTopLevelIsRemovedWhole PASS (unit-961301b9c2db4db553db.log)
  TestSweep_StoreBeyondTheSearchWindowIsLeftAlone PASS (unit-961301b9c2db4db553db.log)
  TestSweep_UnreadableDoltDirIsNotAStore PASS (unit-961301b9c2db4db553db.log)
GATE_TEST_EVIDENCE_RESULT ok: 11 diff-owned tests verified by name
```

Load measurements (the remote suite has no local load wait; native checks retain the bounded soft wait):

```text
Remote-suite host observation: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=35.59 run_max_load=94.08 run_mean_load=50.19 run_readings=102 wait_first_load=35.59 wait_max_load=35.59 wait_mean_load=35.59 wait_readings=1 read_errors=0
Native floor: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1803 wait_timed_out=1 run_start_load=38.33 run_max_load=36.41 run_mean_load=33.65 run_readings=14 wait_first_load=26.96 wait_max_load=94.06 wait_mean_load=61.71 wait_readings=61 read_errors=0
Focused real Dolt: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1800 wait_timed_out=1 run_start_load=83.33 run_max_load=88.40 run_mean_load=87.21 run_readings=2 wait_first_load=39.61 wait_max_load=83.33 wait_mean_load=44.71 wait_readings=61 read_errors=0
load_threshold: 15
load_waited_seconds: 0
load_wait_timed_out: 0
run_start_load: 35.59
run_max_load: 94.08
run_mean_load: 50.19
run_readings: 102
```

Required live registry lane load:

```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1802 wait_timed_out=1 run_start_load=25.67 run_max_load=27.81 run_mean_load=27.27 run_readings=4 wait_first_load=62.75 wait_max_load=93.93 wait_mean_load=56.54 wait_readings=61 read_errors=0
```

All 356 remote SKIP events (including subtests) and all 39 native SKIP events have per-site proof against the pinned base. None is diff-owned. Reasons cover unchanged opt-ins, unavailable providers/tools, platform checks and classified-start-error test cases. Environment-absence cases received the required native top-up; residual native SKIPs remain inventoried. The aggregate count above counts top-level results once per log; it includes the native floor, and subtest SKIPs are retained separately in these inventories.

## Run history and publication

The earlier gate's unexplained native diff-owned failure remains a FAIL in its retained evidence. Builder investigation changed no source and returned it as nonreproducible. This is a new complete gate against a new frozen base, with a private TMPDIR/GOTMPDIR and captured directory/server diagnostics for the native canary. No failed result was retried into this run's PASS; no attribution or waiver was used.

Publication resumes deploy/ga-b6u0ugv-gate without resetting it: its only change above the reviewed source is the previous FAIL gate record. This commit updates that record with this fresh PASS; source history stays intact. AC13's exact public PR-body readback is required before clearance and handoff.

The finalization helper was stopped after all three test drivers reported complete1/1 failed0: it expected status exit0, while the runner reports exit10 on completion. Final verification and record writing were then run directly and each exited0. No tests were stopped or relaunched.

## Retained evidence

Evidence root: /var/tmp/deploy-ga-b6u0ugv.df62p7zv

Each record below is retained with the raw logs. SHA256 values bind the compact gate to the complete inventories and execution record:

| Record | SHA256 |
|---|---|
| final-verification.json | 78096fe1dabaebfc17fd0da32cf9b5610f60e5cb42cd9c32ec3d2532be9ea457 |
| static-evidence.json | 3836bf02b7817000f2c8ca429aeedbe908cc14af6381094774e7d956854c37bf |
| red-green-proof.json | b3a31785914484eb94c76260911e23a80578bff6b05441831d55bccc79a9e87d |
| remote-skip-proof.json | 9f431f3246953770c7730590f75d5625983398a9eb38ce5d3fb30bff3348a36a |
| native-skip-proof.json | fd53d872018bd7298d1657aa7c25e1e909e942c2610bb18519f5efc2867b321b |
| load-evidence.json | b1eca24214ebe135a093130c929fd13bd8b9c3f36a5c97ee7b374dabf3ddf4da |
| focused-native-result.json | a5cc130b6378418d3a9f3438197cdc03541fe00fce320ced4b88359ecdeebde0 |
| c3-run/evidence/execution.json | 221ddfbd54746c65e64e7fa7e14df99b00a726494748edb0e931a5e5aaecdf3a |

Publication hook setup: the first pre-commit attempt selected local Bazel and exited127 before any test execution because libicui18n.so.74 is missing on the host. Its log is retained as precommit-local-icu74.log. Publication uses the configured remote-execution wrapper for the same normal hook; no hook bypass is used. The four criterion3 phases above already ran on the configured remote executor.
