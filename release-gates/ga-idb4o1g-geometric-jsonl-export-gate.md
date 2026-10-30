# Geometric JSONL archive maintenance — ga-idb4o1g

**Verdict:** **PASS**

Reviewed source: `6a142fda521e75765e3ac818da59becefb47ad8c`. Build ga-l694csh; round-2 review ga-rap4kfc PASS on this exact source.
Canonical merge: `209cdd45ce4ba8dbfad9a3a101f2b42883fe1118`; tree `75ee60bea6dbcad9ba04029c3bf5d2f7ba40063c`.
DEPLOY_MODE: remote. BASE_REF: origin/main.
merge_state: clean (origin/main@ff7194cf9ccf4c1bd2dfcfcad3c8666af7aeb083)
gate_base_kind: current-tip
gate_base: ff7194cf9ccf4c1bd2dfcfcad3c8666af7aeb083
merge_validation: this exact merged tree built/vetted with normal hooks; MPR and the required merge-group checks validate the eventual merge.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Closed round-2 review ga-rap4kfc binds PASS to the resolved reviewed source. No carryover or reviewer-added-test exception. |
| 2 | Acceptance met | PASS | All 18 owned roots PASS by name: export/push before failed or wedged maintenance, halt/no-change paths, geometric packing/pack identity, free-space modes, temporary Git config and telemetry. The troubleshooting text matches the script. DS-1/2/3 pass the prepared-body checks; publication must verify that same body through the PR API before close/clearance and report each clause to mayor. |
| 3 | Tests pass | PASS | Five forced remote phases plus all native-floor jobs completed. Recorded result entries: 126521 PASS / 0 FAIL / 370 SKIP, counting job observations rather than unique tests.18 diff-owned roots PASS, no FAIL/SKIP/MISSING. |
| 4 | No unresolved HIGH finding | PASS | Round-2 style/security/spec verdict is PASS, with no blocking finding. The survived failed-DB-exit mutant remains the disclosed advisory; AC2 requested other exit paths. |
| 5 | Final source clean | PASS | Exact canonical merge is clean at the audit. The isolated deploy branch contains reviewed source plus this sole record; publication additionally requires a clean committed checkout. |
| 6 | Merge state; build/vet | PASS | Clean merge onto pinned main; full Bazel build and make vet/nogo passed, active normal pre-commit hooks ran, and independent merge-tree output matches the committed tree. |
| 7 | Single feature theme | PASS | Five source files implement/test/document archive maintenance that follows snapshot delivery. Ancestry citation and denylist checks PASS for the confirmed build/review chain; no stack. |

## Acceptance and PR-open clauses

Maintenance now follows snapshot export/commit/push, including no-change exits. Failure still exits1 without completion. The next run exports before retrying maintenance. A single-threaded low-priority geometric repack preserves the large pack until geometric merging requires it. Existing free-space thresholds select normal, keep-largest or refusal; commit-local flags disable automatic maintenance without persisting config. Stderr records mode, wall time, loose/pack counts and packed size. No Go production change, GC fallback, periodic GC, new knob, retention policy or export-side disk valve is added.

DS-1: prepared PR text records the builder's equal-seed comparison:1,300,987,904B/22.7s old versus151,625,728B/10.4s new;8.6x BELOW the expected10x. The mostly-incompressible random-base64 backlog explains the shortfall, contrasted with the earlier roughly130x live-backlog compression. These are builder measurements, not a new deploy benchmark.

DS-2: prepared PR text includes all seven architect residuals. It identifies the estimates as the2026-10-22 model: retention decision by late November, first large merge2026-11-27..2026-12-11, disk wall2026-12-08..2026-12-20; telemetry to watch, absent disk valve/follow-up escalation, compression/manual GC, omitted ancillary GC work, temporary-pack garbage and manual-GC overlap. This PR MUST merge before ga-aa8ysou.

DS-3: prepared PR text explicitly distinguishes the verified combined --keep-pack/--geometric version Git 2.56.0 from the advertised Git 2.32+ floor, unverified on older versions. The API-verified actual PR body and per-clause report are mandatory publication evidence retained on the bead.

Documentation reviewed with gascity-docs conventions: no new TOML fence, link, heading, navigation or generated reference changed. Shell examples and operational ordering match the script; fresh docsync is part of the unit suite. `docs/PROJECT_MANIFEST.md` is absent; coverage comes from TESTING.md, Makefile, .github/workflows/bazel.yml and the supplied seven-criterion protocol.

## Criterion 3 evidence

test_cmd_scope: full-suite
test_exec_mode: remote-exec (BEP-derived)
waiver_ref: none (Gas City has no waiver path)
failure_attribution: none
heavy_mode: none
ci_lane_run: n/a (no CI configuration change)
fallback_reason: none
farm_test_seconds: 10282.16
test_log_dir: /var/tmp/deploy-ga-idb4o1g.zcfdw62f/c3/evidence/remote/logs
test_counts: 126521 PASS / 0 FAIL / 370 SKIP
test_exec_bep: {"acceptance": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 24, "results": 24, "retried": 0, "window_ok": true}, "integration-full": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 14, "results": 14, "retried": 0, "window_ok": true}, "integration-latency": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 1, "results": 1, "retried": 0, "window_ok": true}, "integration-packages": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 85, "results": 85, "retried": 0, "window_ok": true}, "unit": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 373, "results": 373, "retried": 0, "window_ok": true}}
rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:7065d0a100bca411f10804c9ea0e2af60a34be153e94f7cb86f81135e1b98b45", "worker_env_tree": "sha256:7065d0a100bca411f10804c9ea0e2af60a34be153e94f7cb86f81135e1b98b45"}
admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 49.972625732421875, "waited_seconds": 0.001}
owned_topup: []
floor_topup: complete; 16 steps; commands/verdicts in execution.json
skip_inventory: {"environment-absence": 71, "other": 285}
latency_leg: {"attempts": [{"attempt": 1, "evidence_errors": [], "farm_test_seconds": 77.61, "phase": ["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$", "GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=5fd31cdd75b202be3ab9de14bb2514f059b7969d1f14ab7fdc392c2c723e6b7b"], "probe": ["GATE_FARM_PROBE waiting waited=0s load1=68.18 load5=72.85 load15=36.53 cpus=32 threshold=50 max_wait=600s", "GATE_FARM_PROBE verdict=clear waited=20s load1=48.83 load5=68.13 load15=35.75 cpus=32 threshold=50 max_wait=600s"], "probe_verdict": "clear", "rc": 0, "results": {"TestGcBdLatencyBudget": {"PASS": 1}, "TestGcSeatHookLatencyBudget": {"PASS": 1}}, "test": ["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 30.53 61.44 34.71 5/927 50227"]}], "attempts_allowed": 2, "complete": true, "main_leg": ["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"], "main_leg_verdicts": {"TestGcBdLatencyBudget": {}, "TestGcSeatHookLatencyBudget": {}}, "message": "measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS", "phase": "integration-latency", "rc": 0, "retired_farm_test_seconds": 0.0, "tests": ["TestGcBdLatencyBudget", "TestGcSeatHookLatencyBudget"], "verdict": "measured", "verdicts": {"TestGcBdLatencyBudget": {"PASS": 1}, "TestGcSeatHookLatencyBudget": {"PASS": 1}}}

| Phase | Results | Fresh remote | Cache credits | Retries | Verdict |
|---|---:|---:|---:|---:|---|
| unit | 373 | 373 | 0 | 0 | PASS |
| acceptance | 24 | 24 | 0 | 0 | PASS |
| integration-packages | 85 | 85 | 0 | 0 | PASS |
| integration-full | 14 | 14 | 0 | 0 | PASS |
| integration-latency | 1 | 1 | 0 | 0 | PASS |

The canonical runner executes unit make check, acceptance, integration-packages, full integration excluding the two latency names, then the separate load-held latency leg. Each phase forces uncached tests and forbids local fallback; all results are remote attempt1 inside their phase windows with terminal verdicts. The native floor includes beadstest, Darwin compile, runner self-tests and every environment-absence top-up. No partial farm/local suite is stitched together.

Actual commands:

```text
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-idb4o1g.zcfdw62f/c3/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-idb4o1g.zcfdw62f/c3/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-idb4o1g.zcfdw62f/c3/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-idb4o1g.zcfdw62f/c3/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-idb4o1g.zcfdw62f/c3/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```

Independent by-name readback from retained per-job logs:

```text
GATE_TEST_EVIDENCE logs_dir=/var/tmp/deploy-ga-idb4o1g.zcfdw62f/c3/evidence/remote/logs logs=513 logs_with_results=447
test_counts: PASS=126521 FAIL=0 SKIP=370
  (top-level result lines, column 0, summed over the 513 logs; a test that several jobs run counts once per job; sub-tests are not counted)
logs_without_results: floor-13.log floor-14.log floor-15.log unit-02e0324f1ef05430d7e4.log unit-04c4f9e4335a6f1c2dfa.log unit-0787047af2c4dad6aebc.log unit-0eb92a6bd00590d91db6.log unit-0f39cb7e4c03399a72c4.log unit-1a34395a607c0208a005.log unit-1e67f201b300dc5f9e78.log unit-21372f682c4033d14a80.log unit-22d5121476fcaa40b586.log (+54 more)
diff_tests_executed:
  TestJsonlExportArchiveMaintenanceKeepsFreeSpaceFloor PASS (integration-packages-b51d44b27e6601511c78.log, unit-3634f1b18e9c3836a1c1.log)
  TestJsonlExportArchiveMaintenanceKeepsLargePackAsIs PASS (integration-packages-a0f68450216c5e4d04af.log, unit-df0dcd5ff9a717b4f2a3.log)
  TestJsonlExportArchiveMaintenanceMergesSmallPacksBesideLargePack PASS (integration-packages-b51d44b27e6601511c78.log, unit-3634f1b18e9c3836a1c1.log)
  TestJsonlExportArchiveMaintenanceNamesGitVersionWhenRepackRejectsGeometric PASS (integration-packages-be91eec86b4dbf7e66f1.log, unit-a8efc2750d58b465ffbf.log)
  TestJsonlExportArchiveMaintenanceRefusesWithoutFreeSpace PASS (integration-packages-ff9cb121652191f033f5.log, unit-29937721dc700c4b8d8b.log)
  TestJsonlExportArchiveMaintenanceReportsTelemetry PASS (integration-packages-ff9cb121652191f033f5.log, unit-29937721dc700c4b8d8b.log)
  TestJsonlExportArchiveRepackCommandAndGitAutoMaintenance PASS (integration-packages-c2440ecc93e2da4ed97f.log, unit-ef24b9dbe59f5d04fac1.log)
  TestJsonlExportCommitsAndPushesSnapshotBeforeArchiveMaintenanceFails PASS (integration-packages-b51d44b27e6601511c78.log, unit-3634f1b18e9c3836a1c1.log)
  TestJsonlExportCommitsSnapshotBeforeRepackThatNeverReturns PASS (integration-packages-c2440ecc93e2da4ed97f.log, unit-ef24b9dbe59f5d04fac1.log)
  TestJsonlExportHaltSendsSpikeAlertBeforeArchiveMaintenance PASS (integration-packages-be91eec86b4dbf7e66f1.log, unit-a8efc2750d58b465ffbf.log)
  TestJsonlExportKeepsSpikeAlertWhileArchiveCannotBePacked PASS (integration-packages-a0f68450216c5e4d04af.log, unit-df0dcd5ff9a717b4f2a3.log)
  TestJsonlExportLeavesSmallArchiveLooseUnderDefaultLimits PASS (integration-packages-a0f68450216c5e4d04af.log, unit-df0dcd5ff9a717b4f2a3.log)
  TestJsonlExportPacksArchiveUnderIoniceAndNice PASS (integration-packages-be91eec86b4dbf7e66f1.log, unit-a8efc2750d58b465ffbf.log)
  TestJsonlExportPacksArchiveWhenLooseSizeLimitReached PASS (integration-packages-ff9cb121652191f033f5.log, unit-29937721dc700c4b8d8b.log)
  TestJsonlExportPacksBacklogWhenThereIsNothingToExport PASS (integration-packages-ff9cb121652191f033f5.log, unit-29937721dc700c4b8d8b.log)
  TestJsonlExportPushesPendingSnapshotWhenArchiveCannotBePacked PASS (integration-packages-ff9cb121652191f033f5.log, unit-29937721dc700c4b8d8b.log)
  TestJsonlExportPushesSnapshotWhenArchivePackingFails PASS (integration-packages-be91eec86b4dbf7e66f1.log, unit-a8efc2750d58b465ffbf.log)
  TestJSONLExportMaintainsArchiveObjectDatabase PASS (unit-731da2e3326e685ba0eb.log)
GATE_TEST_EVIDENCE_RESULT ok: 18 diff-owned tests verified by name
```

policy_lane: PASS — pinned format2.12.0, all-tree nogo, eight shell guards and seven policy targets.
drift_lane: PASS — CI's make bazel-sync then git diff --exit-code plus empty tracked/untracked status, and seven generated schema/client/bundle/lockfile checks, each lane in a fresh view.

```text
GLT_RECORD: {"golangci_lint":{"pin":"2.12.0","resolved":"2.12.0","mismatch":false}}
FRESH_TREE_RECORD from=/var/tmp/deploy-ga-idb4o1g.zcfdw62f/merge-scratch head=209cdd45ce4b tree=75ee60bea6db dir=/var/tmp/gc-fresh-tree.OaY4Km modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-merge-scratch,bazel-out,bazel-testlogs
GATE_BASE_RECORD base=ff7194cf9ccf4c1bd2dfcfcad3c8666af7aeb083 knobs=CI_BEADS_DIFF_BASE,BASE_SHA,COMPLEXITY_BASE_REF,LINT_BASE dir=/var/tmp/gc-fresh-tree.OaY4Km
FRESH_TREE_RECORD from=/var/tmp/deploy-ga-idb4o1g.zcfdw62f/merge-scratch head=209cdd45ce4b tree=75ee60bea6db dir=/var/tmp/gc-fresh-tree.boBFhR modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-merge-scratch,bazel-out,bazel-testlogs
GATE_BASE_RECORD base=ff7194cf9ccf4c1bd2dfcfcad3c8666af7aeb083 knobs=CI_BEADS_DIFF_BASE,BASE_SHA,COMPLEXITY_BASE_REF,LINT_BASE dir=/var/tmp/gc-fresh-tree.boBFhR
FRESH_TREE_RECORD from=/var/tmp/deploy-ga-idb4o1g.zcfdw62f/merge-scratch head=209cdd45ce4b tree=75ee60bea6db dir=/var/tmp/gc-fresh-tree.ynEeU7 modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-merge-scratch,bazel-out,bazel-testlogs
GATE_BASE_RECORD base=ff7194cf9ccf4c1bd2dfcfcad3c8666af7aeb083 knobs=CI_BEADS_DIFF_BASE,BASE_SHA,COMPLEXITY_BASE_REF,LINT_BASE dir=/var/tmp/gc-fresh-tree.ynEeU7
```

Remote admission has no local wait. Native floor retains its soft wait. Original load summaries:

```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=14.75 run_max_load=15.47 run_mean_load=11.39 run_readings=38 wait_first_load=14.75 wait_max_load=14.75 wait_mean_load=14.75 wait_readings=1 read_errors=0
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=8.71 run_max_load=11.68 run_mean_load=11.07 run_readings=6 wait_first_load=8.71 wait_max_load=8.71 wait_mean_load=8.71 wait_readings=1 read_errors=0
```

Native ICU provenance:

```text
GATE_ICU74 lib=libicui18n.so.74 file="/usr/local/lib64/icu74-compat/libicui18n.so.74.2" sha256=d90d62f0d7b6c6fbc279ae5e433cc7004aa47cdb106a71a02350099f8df9ae51 recorded=yes
GATE_ICU74 lib=libicuuc.so.74 file="/usr/local/lib64/icu74-compat/libicuuc.so.74.2" sha256=33499dd97a5393926b32e0b535f5340f7dae14b35e59025b572e6bd9ad48a2ae recorded=yes
GATE_ICU74 lib=libicudata.so.74 file="/usr/local/lib64/icu74-compat/libicudata.so.74.2" sha256=c891c5fdb45e3cf756b728abe256be7af1bfc97f00ca2f2fac450c96c09bd4b7 recorded=yes
GATE_ICU74 verdict=OK provenance=recorded sonames="libicui18n.so.74 libicuuc.so.74 libicudata.so.74"
```

The isolation wrapper reported PASS-THROUGH, not isolation; native floor commands perform their own session/live-store environment scrubbing. No library-path override, bwrap overlay, floor/admission override, source rewrite or test-verdict retry was used.

## Skip ownership and remaining inventory

All 18 owned roots and every owned subtest execute and PASS. Skips in the edited maintenance test file are verified UNCHANGED at their actual sites; shared imports/helpers are answered in the retained ownership audit. Their locale/tool prerequisites occur before, or outside, this JSONL exporter. Other skips are unchanged environment/platform/configuration/live-provider prerequisites; environment-absence cases receive native package top-ups, whose remaining non-owned skips stay inventoried and are not credited as passes.

Ownership audit:

```json
{
  "complete": true,
  "diff_owned_skips": [],
  "site_results": [
    {
      "site": "examples/gastown/gastown_test.go:492",
      "evidence": "SITE examples/gastown/gastown_test.go:492  func=TestTmuxKeybindingsAlternateScreenPassthrough kind=test lines=484-499  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED"
    },
    {
      "site": "examples/gastown/gastown_test.go:4153",
      "evidence": "SITE examples/gastown/gastown_test.go:4153  func=TestAttachedRigScopeShellToken kind=test lines=4148-4175  line_in_hunk=no  func_touched=no  shared_hunks_in_file=0  => UNCHANGED"
    },
    {
      "site": "examples/gastown/maintenance_scripts_test.go:9266",
      "evidence": "SITE examples/gastown/maintenance_scripts_test.go:9266  func=TestJsonlExportPushRetryDelayIsLocaleIndependent kind=test lines=9258-9313  line_in_hunk=no  func_touched=no  shared_hunks_in_file=18  => UNCHANGED"
    }
  ],
  "shared_hunks": [
    "hunk +4,2      is outside every function  [shared: a //go:build line, an import, a package-level var or a comment; a build constraint or package-level state can change every test in the file]",
    "hunk +8,1      is outside every function  [shared: a //go:build line, an import, a package-level var or a comment; a build constraint or package-level state can change every test in the file]",
    "hunk +19,1     is outside every function  [shared: a //go:build line, an import, a package-level var or a comment; a build constraint or package-level state can change every test in the file]",
    "hunk +6813,6   touches helper runScriptResultInDir (lines 6811-6814)  [shared: tests that call it are affected but not owned]",
    "hunk +6813,6   touches helper scriptCmd (lines 6818-6823)  [shared: tests that call it are affected but not owned]",
    "hunk +6822,1   touches helper scriptCmd (lines 6818-6823)  [shared: tests that call it are affected but not owned]",
    "hunk +7430,3   touches helper initSeedArchive (lines 7408-7447)  [shared: tests that call it are affected but not owned]",
    "hunk +10263,1  is outside every function  [shared: a //go:build line, an import, a package-level var or a comment; a build constraint or package-level state can change every test in the file]",
    "hunk +10284,2  touches helper readArchiveObjectStats (lines 10267-10291)  [shared: tests that call it are affected but not owned]",
    "hunk +10307,1  touches helper commitArchiveBloat (lines 10297-10309)  [shared: tests that call it are affected but not owned]",
    "hunk +10424,2  is outside every function  [shared: a //go:build line, an import, a package-level var or a comment; a build constraint or package-level state can change every test in the file]",
    "hunk +10437,1  touches helper lookPathGit (lines 10437-10444)  [shared: tests that call it are affected but not owned]",
    "hunk +10443,151 touches helper lookPathGit (lines 10437-10444)  [shared: tests that call it are affected but not owned]",
    "hunk +10443,151 touches method failGitRepack (lines 10446-10449)  [shared: tests that call it are affected but not owned]",
    "hunk +10443,151 touches method runSeparate (lines 10452-10459)  [shared: tests that call it are affected but not owned]",
    "hunk +10443,151 touches method gcCalls (lines 10462-10469)  [shared: tests that call it are affected but not owned]",
    "hunk +10443,151 touches method startNewGCCallLog (lines 10472-10475)  [shared: tests that call it are affected but not owned]",
    "hunk +10443,151 touches method runKilledAtRepack (lines 10481-10519)  [shared: tests that call it are affected but not owned]",
    "hunk +10443,151 touches helper writeGitArgvLogStub (lines 10524-10531)  [shared: tests that call it are affected but not owned]",
    "hunk +10443,151 touches helper largestArchivePack (lines 10534-10551)  [shared: tests that call it are affected but not owned]",
    "hunk +10443,151 touches helper fileSHA256 (lines 10553-10560)  [shared: tests that call it are affected but not owned]",
    "hunk +10443,151 touches method seedLargePackAndBacklog (lines 10567-10585)  [shared: tests that call it are affected but not owned]",
    "hunk +10443,151 touches method exportOnce (lines 10589-10594)  [shared: tests that call it are affected but not owned]",
    "hunk +10596,2  is outside every function  [shared: a //go:build line, an import, a package-level var or a comment; a build constraint or package-level state can change every test in the file]",
    "hunk +10811,5  touches helper writeArchiveFreeSpaceDFStub (lines 10815-10830)  [shared: tests that call it are affected but not owned]",
    "hunk +10817,3  touches helper writeArchiveFreeSpaceDFStub (lines 10815-10830)  [shared: tests that call it are affected but not owned]",
    "hunk +10822,5  touches helper writeArchiveFreeSpaceDFStub (lines 10815-10830)  [shared: tests that call it are affected but not owned]",
    "hunk +10828,2  touches helper writeArchiveFreeSpaceDFStub (lines 10815-10830)  [shared: tests that call it are affected but not owned]",
    "hunk +11098,1  is outside every function  [shared: a //go:build line, an import, a package-level var or a comment; a build constraint or package-level state can change every test in the file]"
  ],
  "shared_hunk_answers": [
    {
      "names": [
        "imports bufio, bytes, crypto/sha256, syscall",
        "archiveObjectStats.packs"
      ],
      "why": "No new TestMain, build constraint or mutable state is added to this test file. Standard-library initialization and an added statistics field do not probe or change locale, tool availability or embedded tmux content."
    },
    {
      "names": [
        "runScriptResultInDir",
        "scriptCmd"
      ],
      "why": "The shared refactor preserves exec.Command(script), cmd.Dir, mergeTestEnv(env), and CombinedOutput exactly. The locale precondition still runs its standalone awk probe with the same LC_ALL/POSIXLY_CORRECT; its missing-locale outcome precedes exporter execution."
    },
    {
      "names": [
        "initSeedArchive",
        "readArchiveObjectStats",
        "commitArchiveBloat",
        "limitPackingToSnapshot"
      ],
      "why": "Archive Git setup/statistics/commit helpers and comment changes are used after the locale precondition or in archive fixtures; no helper can change the awk probe, LookPath(zsh) or the external tmux pack-content check."
    },
    {
      "names": [
        "lookPathGit",
        "failGitRepack",
        "runSeparate",
        "gcCalls",
        "startNewGCCallLog",
        "runKilledAtRepack",
        "writeGitArgvLogStub",
        "largestArchivePack",
        "fileSHA256",
        "seedLargePackAndBacklog",
        "exportOnce",
        "writeArchiveFreeSpaceDFStub"
      ],
      "why": "These new/changed fixture helpers are invoked by the owned archive-maintenance cases, not by the three skipped prerequisites. The locale test skips before creating an archive or invoking jsonl-export. They write only each test temporary fixture, not installed locale/tool/embedded-pack availability."
    },
    {
      "names": [
        "outside-function hunks at10263,10424,10596,11098"
      ],
      "why": "Added struct field or comments/spacing near fixture methods/tests, not executable init or a build tag. No global environment setting or skip predicate changed."
    }
  ],
  "reasons": {
    "TestJsonlExportPushRetryDelayIsLocaleIndependent": "Worker awk prints 1.50, not1,50; exporter has not executed when this unchanged locale check skips. Native floor checks the package as required.",
    "TestAttachedRigScopeShellToken/zsh": "Worker zsh is absent; file/function unchanged and independent of the JSONL script. Native floor selects parent.",
    "TestTmuxKeybindingsAlternateScreenPassthrough": "Pinned external gascity-packs lacks alternate_on; go.mod and that asset are untouched."
  },
  "raw_inventory": "Original skip inventory/logs remain unchanged; all18 owned roots must independently PASS before the final audit.",
  "blank_skip_clarifications": []
}
```

| Class | Entries | Original recorded reason |
|---|---:|---|
| environment-absence | 2 | activity_live_test.go:20: herdr not installed |
| environment-absence | 1 | bd_schema_parity_test.go:121: bd not available |
| environment-absence | 1 | bd_schema_parity_test.go:144: bd not available |
| environment-absence | 1 | binary_versions_test.go:66: bd not probeable in this environment: locate bd: exec: "bd": executable file not found in $PATH |
| environment-absence | 1 | br_test.go:18: br not available |
| environment-absence | 1 | checks_custom_types_test.go:297: bd binary not on PATH |
| environment-absence | 1 | checks_custom_types_test.go:399: bd binary not on PATH |
| environment-absence | 1 | checks_custom_types_test.go:467: bd binary not on PATH |
| environment-absence | 1 | conformance.go:124: concurrent Start(skip-start-3): provider environment unavailable |
| environment-absence | 1 | conformance.go:134: Start: provider environment unavailable |
| environment-absence | 1 | conformance.go:170: Start sentinel: provider environment unavailable |
| environment-absence | 1 | conformance.go:193: Start: provider environment unavailable |
| environment-absence | 1 | conformance.go:209: Start(skip-start-11): provider environment unavailable |
| environment-absence | 1 | conformance.go:238: Start(skip-start-14): provider environment unavailable |
| environment-absence | 1 | conformance.go:271: Start(skip-start-18): provider environment unavailable |
| environment-absence | 1 | conformance.go:294: Start skip-start-21: provider environment unavailable |
| environment-absence | 1 | conformance.go:313: Start skip-start-22: provider environment unavailable |
| environment-absence | 1 | conformance.go:333: Start: provider environment unavailable |
| environment-absence | 1 | conformance.go:349: Start: provider environment unavailable |
| environment-absence | 1 | conformance.go:367: Start(skip-start-25): provider environment unavailable |
| environment-absence | 1 | conformance.go:395: Start: provider environment unavailable |
| environment-absence | 1 | conformance.go:404: Start: provider environment unavailable |
| environment-absence | 1 | conformance.go:421: Start(skip-start-30): provider environment unavailable |
| environment-absence | 1 | conformance.go:61: Start shared session: provider environment unavailable |
| environment-absence | 1 | conformance.go:84: Start: provider environment unavailable |
| environment-absence | 1 | conformance.go:93: first Start: provider environment unavailable |
| environment-absence | 2 | conformance_test.go:18: herdr not installed |
| environment-absence | 2 | events_live_test.go:40: herdr not installed |
| environment-absence | 2 | gastown_test.go:4153: zsh not installed |
| environment-absence | 2 | kindpath_live_test.go:105: herdr not installed |
| environment-absence | 2 | maintenance_scripts_test.go:9266: awk does not emit a comma decimal under de_DE.UTF-8 (locale missing?): "1.50", <nil> |
| environment-absence | 1 | mcp_conformance_test.go:159: MCP mail script not found: file does not exist |
| environment-absence | 1 | mcp_conformance_test.go:195: MCP mail script not found: file does not exist |
| environment-absence | 1 | mcp_conformance_test.go:26: MCP mail script not found: file does not exist |
| environment-absence | 1 | mcp_conformance_test.go:286: MCP mail script not found: file does not exist |
| environment-absence | 1 | mcp_conformance_test.go:80: MCP mail script not found: file does not exist |
| environment-absence | 8 | metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance |
| environment-absence | 1 | migrate_journal_test.go:37: pinned beads v1.3.2 is not unpacked in the local module cache: open /tmp/bt/_tmp/35731ca362c311940500020a6c8a2007/go/pkg/mod/github.com/steveyegge/beads@v1.3.2/cmd/bd/migrate_dolt_mode.go: no such file or directory |
| environment-absence | 2 | panebinding_live_test.go:21: herdr not installed |
| environment-absence | 2 | provider_live_test.go:15: herdr not installed |
| environment-absence | 2 | reconcile_replay_test.go:574: GC_V2_REPLAY_DIR is not set |
| environment-absence | 2 | reconcile_sim_coverage_test.go:305: GC_V2_SIM_COVERAGE_FILES unset |
| environment-absence | 2 | session_event_pump_live_test.go:23: herdr not installed |
| environment-absence | 1 | start_drift_test.go:123: XDG_RUNTIME_DIR not set; user systemd not addressable |
| environment-absence | 2 | tmux_leak_guard_mntns_test.go:76: bwrap not installed; cannot build a private-/tmp mount namespace |
| environment-absence | 2 | tmux_leak_guard_mntns_test.go:98: bwrap not installed; cannot build a private-/tmp mount namespace |
| environment-absence | 1 | tmux_test.go:3637: not inside tmux — need server for bind-key |
| environment-absence | 1 | tmux_test.go:3663: not inside tmux — need server for bind-key |
| environment-absence | 1 | tmux_test.go:3689: not inside tmux — need server for bind-key |
| environment-absence | 1 | tmux_test.go:3719: not inside tmux — need server for bind-key |
| environment-absence | 1 | tool_home_test.go:315: no bd available (set GC_ACCEPTANCE_BD_BIN) |
| other | 2 | activity_poke_integration_test.go:21: set GC_TMUX_INTEGRATION=1 to run this real-tmux dogfood (spins a throwaway tmux server) |
| other | 1 | api_state_test.go:582: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage |
| other | 1 | api_state_test.go:622: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage |
| other | 1 | atomic_test.go:185: filesystem did not preserve setuid bit in test mode: -rwxr-xr-x |
| other | 1 | bd_env_test.go:1396: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | bd_env_test.go:1643: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | bdstore_conditional_integration_test.go:69: installed bd lacks --if-revision (pre-beads#4682); the conformance row skips cleanly and becomes the authoritative guard for the provisional body codes once #4682 lands |
| other | 1 | bdstore_exec_internal_test.go:266: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1 |
| other | 1 | bdstore_test.go:54: BdStore fails ParentIDNamesARowThisStoreDoesNotHave: bd refuses an unresolvable --parent (ga-6od57); un-skip is ga-x4j5ka |
| other | 1 | beads_migrate_proxied_test.go:135: there is no legacy gc available; set GC_ACCEPTANCE_LEGACY_GC_BIN to a gc that still initialises the GC-managed direct topology |
| other | 1 | beads_migrate_proxied_test.go:357: there is no legacy gc available; set GC_ACCEPTANCE_LEGACY_GC_BIN to a gc that still initialises the GC-managed direct topology |
| other | 1 | beads_provider_dirty_tables_test.go:345: requires a real Dolt server; run make test-cmd-gc-process for full coverage |
| other | 1 | beads_provider_lifecycle_test.go:10308: starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage |
| other | 1 | beads_provider_lifecycle_test.go:10494: starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage |
| other | 1 | beads_provider_lifecycle_test.go:10777: starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage |
| other | 1 | beads_provider_lifecycle_test.go:11056: starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage |
| other | 1 | beads_provider_lifecycle_test.go:11536: starts the materialized gc-beads-bd shell fallback; run make test-cmd-gc-process for full coverage |
| other | 1 | beads_provider_lifecycle_test.go:11680: starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage |
| other | 1 | beads_provider_lifecycle_test.go:11767: starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage |
| other | 1 | beads_provider_lifecycle_test.go:11932: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | beads_provider_lifecycle_test.go:12978: starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage |
| other | 1 | beads_provider_lifecycle_test.go:14190: runs the real bd provider lifecycle; run make test-cmd-gc-process for full coverage |
| other | 1 | beads_provider_lifecycle_test.go:1723: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | beads_provider_lifecycle_test.go:5144: starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage |
| other | 1 | beads_provider_lifecycle_test.go:5695: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | beads_provider_lifecycle_test.go:6677: runs the materialized gc-beads-bd init script with GC_BIN helper; run make test-cmd-gc-process for full coverage |
| other | 1 | beads_provider_lifecycle_test.go:9693: starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage |
| other | 1 | beads_provider_script_scope_lifecycle_linux_test.go:352: runs the real bd provider init; run make test-cmd-gc-process for full coverage |
| other | 3 | beads_topology.go:632: there is no pre-journal gc binary; set GC_ACCEPTANCE_LEGACY_GC_BIN to a gc built from a commit before the ownership journal |
| other | 1 | build_desired_state_test.go:10104: uses a live managed-dolt port probe for scale_check coverage; run make test-cmd-gc-process for full coverage |
| other | 1 | build_desired_state_test.go:6331: uses real bd subprocesses for routed-work scale checks; run make test-cmd-gc-process for full coverage |
| other | 1 | call_budget_test.go:536: ga-vnycm2.27: interrupted abort-scope converges with different skip metadata |
| other | 1 | city_runtime_test.go:4327: runs a full runtime tick/reconcile path; run make test-cmd-gc-process for full coverage |
| other | 1 | city_status_snapshot_test.go:200: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1 |
| other | 1 | cityinit_impl_test.go:259: runs the full local init scaffold/finalize path; run make test-cmd-gc-process for full coverage |
| other | 2 | close_notification_conformance.go:54: CloseIfMatch: conditional writes unsupported |
| other | 2 | close_notification_conformance.go:57: UpdateIfMatch(status=closed): conditional writes unsupported |
| other | 2 | close_notification_conformance.go:89: backing *beads.FileStore lacks the capability UpdateIfAssignmentStatusClosed needs |
| other | 2 | close_notification_conformance.go:89: backing *beads.MemStore lacks the capability UpdateIfAssignmentStatusClosed needs |
| other | 2 | close_notification_conformance.go:89: backing *beads.NativeDoltStore lacks the capability UpdateIfAssignmentStatusClosed needs |
| other | 2 | close_notification_conformance.go:89: backing *beads.SQLiteStore lacks the capability UpdateIfAssignmentStatusClosed needs |
| other | 1 | cmd_bd_test.go:1314: requires a managed bd lifecycle city; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_bd_test.go:1478: requires a managed bd lifecycle city; run make test-cmd-gc-process for full coverage |
| other | 2 | cmd_bd_test.go:1851: ga-klo4gz: this test's purpose is exercising resolveContextFromDir's ambient cwd walk-up (step 10), which is now unconditionally refused inside test binaries; an explicit GC_CITY/GC_CITY_PATH/GC_CITY_ROOT override would make it a no-op test rather than a fix |
| other | 1 | cmd_beads_city_test.go:188: exercises managed bd provider transition behavior; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_beads_city_test.go:485: exercises managed bd provider transition behavior; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_beads_city_test.go:677: exercises managed bd provider transition behavior; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_beads_city_test.go:90: exercises managed bd provider transition behavior; run make test-cmd-gc-process for full coverage |
| other | 2 | cmd_commands_test.go:814: ga-klo4gz: this test's purpose is exercising ambient cwd-based city resolution (resolveContextFromDir step 10) to distinguish the ambient city's commands from an explicitly-selected one, which is now unconditionally refused inside test binaries; an explicit override would make it a no-op test rather than a fix |
| other | 1 | cmd_convoy_dispatch_test.go:3899: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage |
| other | 1 | cmd_doctor_test.go:479: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage |
| other | 1 | cmd_doctor_test.go:599: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage |
| other | 1 | cmd_dolt_state_test.go:1023: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:1184: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:1307: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:1347: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 2 | cmd_dolt_state_test.go:1407: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:142: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:1445: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:1535: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:1598: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 2 | cmd_dolt_state_test.go:1661: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:1720: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:1888: spawns managed dolt holder processes; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:189: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:2780: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:2888: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:3010: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:3051: spawns managed dolt recovery processes; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:3216: spawns a delayed TCP listener process to emulate managed dolt recovery; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:3313: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:3436: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 2 | cmd_dolt_state_test.go:3511: spawns a delayed TCP listener process to emulate managed dolt recovery; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:398: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:447: spawns a delayed TCP listener process to emulate managed dolt recovery; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:485: spawns a delayed TCP listener process to emulate managed dolt recovery; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:526: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:560: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:582: spawns a TCP listener process and verifies cleanup; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:694: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:722: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:760: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:801: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:853: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:905: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:960: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_mail_test.go:1849: requires a real Dolt server; run make test-cmd-gc-process for full coverage |
| other | 2 | cmd_pack_registry_test.go:758: set GC_TEST_GASCITY_PACKS_REGISTRY to a gascity-packs registry.toml source to run this live catalog canary |
| other | 1 | cmd_prime_test.go:1269: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage |
| other | 1 | cmd_prime_test.go:1336: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage |
| other | 1 | cmd_reload_test.go:540: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage |
| other | 1 | cmd_reload_test.go:606: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage |
| other | 1 | cmd_reload_test.go:740: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage |
| other | 1 | cmd_rig_endpoint_test.go:1651: requires a managed external dolt endpoint; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_rig_endpoint_test.go:1689: requires a managed external dolt endpoint; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_session_logs_test.go:388: probes provider transcript lookup before workdir fallback; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_stop_test.go:1376: exercises managed bd provider shutdown; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_supervisor_city_test.go:317: exercises supervisor registration retry behavior; run make test-cmd-gc-process for scenario coverage |
| other | 2 | cmd_supervisor_test.go:1473: unsupported-os test only applies outside darwin/linux |
| other | 1 | cmd_supervisor_test.go:1691: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1 |
| other | 1 | cmd_supervisor_test.go:1851: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1 |
| other | 2 | cmd_supervisor_test.go:2527: launchd path only applies on darwin |
| other | 2 | cmd_supervisor_test.go:2577: launchd path only applies on darwin |
| other | 2 | cmd_supervisor_test.go:2642: launchd path only applies on darwin |
| other | 2 | cmd_supervisor_test.go:2695: launchd path only applies on darwin |
| other | 2 | cmd_supervisor_test.go:2742: launchd path only applies on darwin |
| other | 2 | cmd_supervisor_test.go:2819: launchd path only applies on darwin |
| other | 2 | cmd_supervisor_test.go:6604: launchd path only applies on darwin |
| other | 1 | cmd_wait_test.go:777: builds a real bd binary from source; run make test-cmd-gc-process for full coverage |
| other | 1 | cmdline_portable_test.go:141: PID 1 argv is readable in this environment; cannot exercise the fail-closed path this way |
| other | 1 | compile_test.go:1359: tooling formula not present: stat /home/ubuntu/tooling/formulas/mol-bug-report-flow-v2.toml: permission denied |
| other | 1 | condition_test.go:594: darwin-only: the /private/{tmp,var} alias collapse is a no-op on other platforms |
| other | 1 | conformance.go:124: concurrent Start(preexisting-concurrent-session): session already exists |
| other | 5 | conformance.go:508: provider cannot fence session input |
| other | 5 | conformance.go:525: provider cannot fence session input |
| other | 5 | conformance.go:551: provider cannot fence session input |
| other | 1 | conformance_skips_test.go:60: skipping LongLapsed (bead ga-example, expires 2000-01-01): example |
| other | 1 | conformance_test.go:147: Start: session already exists |
| other | 1 | controller_test.go:1746: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage |
| other | 1 | controller_test.go:1808: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage |
| other | 2 | controller_watcher_fd_test.go:27: darwin-only: the kqueue Close() FD leak (fsnotify#732) is a Darwin/kqueue bug |
| other | 1 | delivery_lookup_test.go:133: start grace is raw-bridge bookkeeping |
| other | 1 | discovery_test.go:65: macOS-only /tmp <-> /private/tmp Claude project path alias |
| other | 1 | dolt_identity_test.go:156: requires a real Dolt server; run make test-cmd-gc-process for full coverage |
| other | 1 | dolt_lifecycle_race_test.go:148: spawns real processes; run via make test-cmd-gc-process |
| other | 1 | dolt_lifecycle_race_test.go:170: spawns real processes; run via make test-cmd-gc-process |
| other | 1 | dolt_lifecycle_race_test.go:238: spawns real processes; run via make test-cmd-gc-process |
| other | 1 | dolt_persistence_test.go:449: GC_INTEGRATION_BD_PERSISTENCE!=1; requires bd >= gastownhall/beads@1cf8337 |
| other | 1 | dolt_persistence_test.go:490: GC_INTEGRATION_BD_PERSISTENCE!=1; requires bd >= gastownhall/beads@1cf8337 |
| other | 1 | dolt_persistence_test.go:536: GC_INTEGRATION_BD_PERSISTENCE!=1; requires bd >= gastownhall/beads@1cf8337 |
| other | 1 | dolt_persistence_test.go:585: GC_INTEGRATION_BD_PERSISTENCE!=1; requires bd >= gastownhall/beads@1cf8337 |
| other | 1 | dolt_persistence_test.go:636: GC_INTEGRATION_BD_PERSISTENCE!=1; requires bd >= gastownhall/beads@1cf8337 |
| other | 1 | dolt_persistence_test.go:679: GC_INTEGRATION_BD_PERSISTENCE!=1; requires bd >= gastownhall/beads@1cf8337 |
| other | 1 | dolt_persistence_test.go:722: GC_INTEGRATION_BD_PERSISTENCE!=1; requires bd >= gastownhall/beads@1cf8337 |
| other | 1 | dolt_persistence_test.go:766: GC_INTEGRATION_BD_PERSISTENCE!=1; requires bd >= gastownhall/beads@1cf8337 |
| other | 1 | dolt_persistence_test.go:818: GC_INTEGRATION_BD_PERSISTENCE!=1; requires bd >= gastownhall/beads@1cf8337 |
| other | 1 | dolt_project_id_test.go:184: requires a real Dolt server; run make test-cmd-gc-process for full coverage |
| other | 1 | dolt_project_id_test.go:212: requires a real Dolt server; run make test-cmd-gc-process for full coverage |
| other | 1 | dolt_project_id_test.go:225: requires a managed dolt server; run make test-cmd-gc-process for full coverage |
| other | 1 | dolt_project_id_test.go:281: requires a managed dolt server; run make test-cmd-gc-process for full coverage |
| other | 1 | dolt_runtime_publication_test.go:130: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | dolt_runtime_publication_test.go:204: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | dolt_runtime_publication_test.go:28: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | dolt_runtime_publication_test.go:294: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 2 | dolt_scope_watchdog_test.go:176: helper process only |
| other | 1 | dolt_sql_health_test.go:525: requires a real Dolt server; run make test-cmd-gc-process for full coverage |
| other | 1 | dolt_start_managed_test.go:1635: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1 |
| other | 2 | dolt_start_managed_test.go:647: helper process only |
| other | 1 | evaluate_test.go:189: darwin-only: the /private/{tmp,var} alias collapse is a no-op on other platforms |
| other | 2 | exec_timeout_linux_test.go:66: harness process |
| other | 2 | gastown_test.go:492: embedded gascity-packs does not yet include #{alternate_on} in WheelUpPane (gastownhall/gascity-packs#136); test activates once go.mod is bumped to that release |
| other | 1 | gc_beads_bd_config_commit_test.go:457: requires a real Dolt server; run make test-cmd-gc-process for full coverage |
| other | 1 | golden_regen_test.go:15: set RUNPROJ_UPDATE_GOLDENS=1 to rewrite testdata goldens |
| other | 1 | handler_status_scoped_store_test.go:141: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1 |
| other | 1 | handler_status_scoped_store_test.go:507: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1 |
| other | 1 | handler_status_scoped_store_test.go:544: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1 |
| other | 1 | huma_binary_test.go:508: flaky on CI under load — 5s supervisor shutdown budget races with in-flight mayor-session-start; tracked in #2090 |
| other | 1 | implicit_agent_patch_repro_test.go:68: bug ga-91uk5m fixed; delete this characterization test |
| other | 1 | integration_selfhost_ux_test.go:100: placeholder for ga-q0bf.1 |
| other | 1 | integration_selfhost_ux_test.go:104: placeholder for ga-r8iz.1 |
| other | 1 | integration_selfhost_ux_test.go:108: placeholder for ga-6wrr.1 |
| other | 1 | integration_selfhost_ux_test.go:112: placeholder for ga-xxqx |
| other | 1 | integration_selfhost_ux_test.go:16: pending ga-cwdaqv: supervised gc start does not validate config pre-registration |
| other | 1 | integration_selfhost_ux_test.go:54: pending merge of builder/ga-xxqx-1 (PR#2219): drift auto-restart not on origin/main |
| other | 1 | integration_selfhost_ux_test.go:96: placeholder for ga-q0bf.1, ga-r8iz.1, ga-6wrr.1, and post-PR#2219 assertions |
| other | 1 | integration_test.go:2828: host has /proc; this test targets the no-/proc (macOS) fallback path |
| other | 1 | main_test.go:375: runs tutorial testscript scenarios; run make test-cmd-gc-process for full coverage |
| other | 2 | main_test.go:836: ga-klo4gz: this subtest's purpose is exercising resolveCity's ambient cwd-based fallback (step 10), which is now unconditionally refused inside test binaries; an explicit override would make it a no-op test rather than a fix |
| other | 1 | mcp_live_test.go:35: set GC_TEST_MCP_MAIL=1 to run (or use make test-mcp-mail) |
| other | 1 | molecule_test.go:30: tooling formula not present: stat /home/ubuntu/tooling/formulas/mol-bug-report-flow-v2.formula.toml: permission denied |
| other | 2 | native_dolt_store_test.go:651: upstream beads storage unavailable: Dolt server unreachable at 127.0.0.1:0: dial tcp 127.0.0.1:0: connect: connection refused |
| other | 1 | nudge_confirm_lag_integration_test.go:174: set GC_TMUX_INTEGRATION=1 to run this real-tmux measurement |
| other | 1 | nudge_confirm_lag_integration_test.go:36: set GC_TMUX_INTEGRATION=1 to run this real-tmux reproduction (spins a throwaway tmux server) |
| other | 1 | nudge_poke_integration_test.go:27: set GC_TMUX_INTEGRATION=1 to run this real-tmux dogfood (spins a throwaway tmux server) |
| other | 1 | nudge_staged_draft_integration_test.go:26: set GC_TMUX_INTEGRATION=1 to run this real-tmux reproduction (spins a throwaway tmux server) |
| other | 1 | order_dispatch_test.go:3695: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1 |
| other | 1 | order_dispatch_test.go:3718: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1 |
| other | 1 | orphan_sweep_sigkill_test.go:29: not invoked as a subprocess helper |
| other | 1 | pack_registry_live_test.go:38: set GC_TEST_GASCITY_PACKS_REGISTRY to a gascity-packs registry.toml source to run this live import smoke test |
| other | 1 | path_helpers_test.go:1054: spawns subprocesses to simulate still-running dolt sql-servers; run make test-cmd-gc-process for full coverage |
| other | 7 | phase1_conformance_test.go:100: provider family "" has no invocation-usage extractor (out of scope) |
| other | 7 | phase1_conformance_test.go:110: provider family "" has no invocation-usage extractor (out of scope) |
| other | 1 | pinned_id_fence_conformance.go:467: store has no guarded batcher |
| other | 1 | pinned_id_fence_conformance.go:533: store has no guarded batcher |
| other | 1 | pool_test.go:108: uses real bd and jq for default scale_check coverage; run make test-cmd-gc-process for full coverage |
| other | 1 | pool_test.go:154: uses real bd and jq for default scale_check coverage; run make test-cmd-gc-process for full coverage |
| other | 1 | pool_test.go:215: uses real bd to prove test-owned HOME isolation; run make test-cmd-gc-process for full coverage |
| other | 1 | processgroup_unix_test.go:71: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1 |
| other | 2 | prompt_test.go:1420: crew.template.md not readable at ../../examples/gastown/packs/gastown/assets/prompts/crew.template.md: open ../../examples/gastown/packs/gastown/assets/prompts/crew.template.md: no such file or directory |
| other | 1 | proxy_process_test.go:1160: harness process |
| other | 1 | proxy_process_test.go:167: helper process |
| other | 4 | ready_parity_conformance.go:69: skipping CachedReadyParity (bead ga-gmf8r, expires 2026-12-15): MemStore and FileStore Ready block on a missing or foreign blocker and on a closed blocker with gc.work_outcome=blocked, which a primed CachingStore cannot see without a ready projection, and return insertion order with Limit applied mid-scan instead of the canonical (priority, created_at, id) order. Replacement proof: MemStore implements enrichReadyProjectionForCache and canonical ready order, and TestMemStoreReadyParityConformance and TestFileStoreReadyParityConformance run this suite unwaived |
| other | 2 | rig_anywhere_test.go:377: ga-klo4gz: this subtest's purpose is exercising resolveContext's ambient cwd walk-up (step 10), which is now unconditionally refused inside test binaries; an explicit override would make it a no-op test rather than a fix |
| other | 2 | rig_anywhere_test.go:401: ga-klo4gz: this subtest's purpose is exercising resolveContext's ambient cwd walk-up (step 10) followed by a rig match, which is now unconditionally refused inside test binaries; an explicit override would make it a no-op test rather than a fix |
| other | 2 | rig_anywhere_test.go:488: ga-klo4gz: this subtest's purpose is exercising the fallthrough from an ambiguous registered-rig match (step 9) to resolveContext's ambient cwd walk-up (step 10), which is now unconditionally refused inside test binaries; an explicit override would make it a no-op test rather than a fix |
| other | 1 | rules_test.go:186: needs root to chown the fixture to the Secret-mount shape |
| other | 1 | scoped_store_test.go:364: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1 |
| other | 1 | script_resolve_test.go:313: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage |
| other | 1 | sessionlog_test.go:1084: macOS-only /tmp <-> /private/tmp Claude project path alias |
| other | 1 | sessionlog_test.go:1104: macOS-only /tmp <-> /private/tmp Claude project path alias |
| other | 1 | sessionlog_test.go:1195: macOS-only /tmp <-> /private/tmp Claude project path alias |
| other | 1 | sessionlog_test.go:1215: macOS-only /tmp <-> /private/tmp Claude project path alias |
| other | 1 | sessionlog_test.go:1397: macOS-only /tmp <-> /private/tmp Claude project path alias |
| other | 1 | sessionlog_test.go:2170: macOS /private/tmp path aliases only apply on darwin |
| other | 1 | sessionlog_test.go:2315: macOS-only /tmp <-> /private/tmp Gemini project path alias |
| other | 1 | ssh_localhost_integration_test.go:24: passwordless ssh to localhost unavailable |
| other | 1 | start_drift_test.go:275: permission-denied scenario requires root to drop to a secondary uid |
| other | 1 | starttime_portable_test.go:86: on linux /proc answers directly, so a ps stub cannot make the identity unreadable |
| other | 2 | state_cache_test.go:926: Darwin-specific syntax guard |
| other | 1 | structured_conformance_test.go:153: profile fixture has no edit result to evaluate |
| other | 1 | tmux_test.go:1615: Skipping: GT_TEST_ALLOW_CLEANUP_TEST=1 required (this test kills sessions) |
| other | 1 | tmux_test.go:1713: Skipping: GT_TEST_ALLOW_CLEANUP_TEST=1 required (this test kills sessions) |
| other | 1 | triggers_test.go:464: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1 |
| other | 1 | triggers_test.go:495: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1 |
| other | 2 | v2_g1_test.go:120: gate G1 times this host: set GC_V2_G1=1 |
| other | 2 | v2_g1_test.go:178: gate G1b reads a store copy: set GC_V2_G1B_STORE |
| other | 1 | worker_handle_test.go:1266: waits through stale session-key detection; run make test-cmd-gc-process for full coverage |
| other | 1 | worker_handle_test.go:36: waits through stale session-key detection; run make test-cmd-gc-process for full coverage |
| other | 1 | write_test.go:376: the test user has no secondary group and is not root |
| other | 1 | write_test.go:475: needs root to own a file as another user |
| other | 1 | x6_test.go:248: DES-O1's literal X6 line (5,000 notices in 60 s) is a manual measurement: --test_env=GC_LANE_X6_FULL=1 |

Original evidence: `/var/tmp/deploy-ga-idb4o1g.zcfdw62f`. All994 canonical raw log/XML artifacts independently SHA256-verified. Execution-record SHA256: `78961c1c732f92216e1f3193d61bb85f4ae5881025861805e56efde05abc1f34`. Original verdicts, per-job logs, BEPs and ownership proofs remain through scratch cleanup.
