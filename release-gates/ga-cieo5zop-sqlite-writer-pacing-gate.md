**Verdict:** **PASS**

Deploy: `ga-cieo5zop`; build: `ga-4bdlrjw4`; review: `ga-2ie6jfja`.
Reviewed source: `f89006d175781cb520e2fad62363ac1f2c87a927`.
Gated merge: `80d7e790c2e099a9eacf015a7728a6e9f6fdabea`; tree: `ecbbd8a8b2184186e4a44539e93ae4bbbb639746`.
Fresh run: `/var/tmp/gc-regate-ga-cieo5zop-gs_d9bjo`. Earlier gates and abandoned supplemental local measurements receive no credit.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS | PASS | Exact resolved source is the commit named by closed review ga-2ie6jfja; reviewer verdict pass, no carryover. |
| 2 | Acceptance criteria | PASS | Only the SQLite fixture file changes (+99/-12). Code independently confirms 150 ms post-commit pacing and the 20 ms lock-hold regression with a 5 s apply ceiling. All three contract test names PASS. Independently restoring the ticker makes the new regression FAIL with SQLITE_BUSY after 35.014 s; verifier exit 0. Architect accepted the time.After spelling on the build bead. |
| 3 | Tests and CI evidence | PASS | Canonical full-scope run valid, all five remote phases and 16 native-floor steps complete; latency probe clear. Both owned tests PASS by name. Required CI leaves accounted below; every remaining SKIP justified in the retained proof. |
| 4 | No unresolved HIGH finding | PASS | Fresh read of reviewer style/security/spec sections found no unresolved HIGH finding. |
| 5 | Clean input and deploy record | PASS | Assigned checkout and gated input are clean. The gate record is the only addition committed for publication; the clean-branch guard runs before push. |
| 6 | Merge/build validation | PASS | Clean merge at the pinned base; make lint built/vetted all targets with nogo, exit 0. Scratch hooks off, raw merge-tree equality asserted. No source rebase. |
| 7 | Single feature theme | PASS | The reviewed net diff is internal/beads/sqlite_store_graph_cook_contention_test.go only. All three source commits cite build ga-4bdlrjw4; ancestry check accepted this build and deploy id, with no stack. |

merge_state: clean (origin/main@a7f6e8ea9fc4d095d26ae8961a0df20519dc8a0f)
gate_base: a7f6e8ea9fc4d095d26ae8961a0df20519dc8a0f
gate_base_kind: current-tip
merge_validation: merge result built and vetted here

```text
test_cmd_scope: full-suite
test_exec_mode: remote-exec
fallback_reason: none
test_counts: PASS=157756 FAIL=0 SKIP=617
diff_tests_executed:
  TestPacedSQLiteWriterLeavesTheWriteLockToAWaitingWriter PASS (integration-packages-ad823e3d7eb71467b6af.log, unit-b398920224b769833cd5.log)
  TestSQLiteStoreGraphCookUnderConcurrentWriter PASS (integration-packages-b6a233fef6b5b347dcfa.log, unit-ea4511196531bb4b8c55.log)
GATE_TEST_EVIDENCE_RESULT ok: 2 diff-owned tests verified by name

test_log_dir: /var/tmp/gc-regate-ga-cieo5zop-gs_d9bjo/c3-run/evidence/remote/logs
native_floor_log_dir: /var/tmp/gc-regate-ga-cieo5zop-gs_d9bjo/c3-run/evidence/remote/floor-logs
waiver_ref: none
heavy_mode: none
ci_lane_run: n/a (no CI-config change)
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-regate-ga-cieo5zop-gs_d9bjo/c3-run/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-regate-ga-cieo5zop-gs_d9bjo/c3-run/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-regate-ga-cieo5zop-gs_d9bjo/c3-run/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-regate-ga-cieo5zop-gs_d9bjo/c3-run/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-regate-ga-cieo5zop-gs_d9bjo/c3-run/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
failure_attribution: none (canonical FAIL=0)
format_lane: gate-base.sh run -- run-pinned-lint.sh -- make fmt-check; exit=0; log=/var/tmp/gc-regate-ga-cieo5zop-gs_d9bjo/format-output.txt
policy_lane: gate-base.sh run -- isolated-test-run.sh --rbe -- make lint; exit=0; log=/var/tmp/gc-regate-ga-cieo5zop-gs_d9bjo/policy-output.txt
drift_lane: make bazel-sync BAZEL_SYNC_FLAGS=--config=remote-exec; git diff --exit-code; exit=0; log=/var/tmp/gc-regate-ga-cieo5zop-gs_d9bjo/drift-output.txt
fast_lane: PASS (format, policy and drift in sequential fresh views, before the suite)
known_red_precheck: proceed reason=fix-carrying; build ga-4bdlrjw4 is stamped for ga-rh8jgr
```

The phase invocations were make -k check; acceptance test plus acceptance_solo_tests; integration_packages; the full integration_test minus the latency names; then integration_test filtered only to gate-latency-tests.txt under the farm probe. Exact flags are above. Native-floor commands are retained in execution.json, not substituted for the suite.

```json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/gc-regate-ga-cieo5zop-gs_d9bjo/c3-run/evidence/remote","gate_base":"a7f6e8ea9fc4d095d26ae8961a0df20519dc8a0f","gate_head":"f89006d175781cb520e2fad62363ac1f2c87a927","admission":{"class":"remote","floor_gib":25.0,"free_gib":61.82874298095703,"waited_seconds":0.001,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"},"delta_gate":{"mode":"shadow","decision":"full","reason":"not-gate-pr","detail":"no PR record (pr.json)","applied":false,"base":"a7f6e8ea9fc4d095d26ae8961a0df20519dc8a0f","head":"f89006d175781cb520e2fad62363ac1f2c87a927","merge":"80d7e790c2e099a9eacf015a7728a6e9f6fdabea","prior":null,"delta":{},"changed_labels":[],"changed_label_count":0,"tests":{},"phases":{},"plan_sha256":"9006fe8aeebf482c9be14031f6e8453f29e213f41752e4242ba397867c335339"},"test_exec_bep":{"unit":{"results":422,"remote_executed":422,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":86,"remote_executed":86,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":14,"remote_executed":14,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":15118.966,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=5fd31cdd75b202be3ab9de14bb2514f059b7969d1f14ab7fdc392c2c723e6b7b"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":123.352,"evidence_errors":[],"probe":["GATE_FARM_PROBE verdict=clear waited=0s load1=13.38 load5=11.73 load15=10.93 cpus=32 threshold=50 max_wait=600s"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 37.87 19.78 13.80 33/1960 56901"]}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":38934}},"log_dir":"/var/tmp/gc-regate-ga-cieo5zop-gs_d9bjo/c3-run/evidence/remote/floor-logs"},"skip_inventory":{"counts_by_reason_class":{"other":513,"environment-absence":90},"top_level_counts_by_reason_class":{"other":255,"environment-absence":43},"tests":{"elided_array":{"items":603,"bytes":218244}}},"test_cmd":{"elided_array":{"items":5,"bytes":2074}},"test_counts":{"PASS":157756,"FAIL":0,"SKIP":617},"diff_tests_executed":"diff_tests_executed:\n  TestPacedSQLiteWriterLeavesTheWriteLockToAWaitingWriter PASS (integration-packages-ad823e3d7eb71467b6af.log, unit-b398920224b769833cd5.log)\n  TestSQLiteStoreGraphCookUnderConcurrentWriter PASS (integration-packages-b6a233fef6b5b347dcfa.log, unit-ea4511196531bb4b8c55.log)\nGATE_TEST_EVIDENCE_RESULT ok: 2 diff-owned tests verified by name\n"}
```

```text
ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-validate.ffXWGF a7f6e8ea9fc4d095d26ae8961a0df20519dc8a0f f89006d175781cb520e2fad62363ac1f2c87a927 fork; exit=0; workflow_blob=fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs; log=/var/tmp/gc-regate-ga-cieo5zop-gs_d9bjo/ci-required-corrected/closure.txt
ci_required_accounting:
  runner-policy | LOCAL-PASS | /home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-merge-validate.ffXWGF -- /home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh --threshold 15 --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- bash /var/tmp/gc-regate-ga-cieo5zop-gs_d9bjo/ci-runner-policy.sh; exit=0; log=/var/tmp/gc-regate-ga-cieo5zop-gs_d9bjo/ci-required-corrected/runner-policy-output.txt
  changes | LOCAL-PASS | python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-validate.ffXWGF a7f6e8ea9fc4d095d26ae8961a0df20519dc8a0f f89006d175781cb520e2fad62363ac1f2c87a927 fork; exit=0; log=/var/tmp/gc-regate-ga-cieo5zop-gs_d9bjo/ci-required-corrected/closure.txt
  credential-provider-windows | DEFERRED-TO-PR-CI reason=non-linux-runner | windows_reach=none; proof=/var/tmp/gc-regate-ga-cieo5zop-gs_d9bjo/ci-required-corrected/credential-windows-reach.txt
  pack-gate | LOCAL-PASS | /home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-merge-validate.ffXWGF -- /home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh --threshold 15 --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- bash /var/tmp/gc-regate-ga-cieo5zop-gs_d9bjo/ci-pack-gate.sh; exit=0; log=/var/tmp/gc-regate-ga-cieo5zop-gs_d9bjo/ci-required-corrected/pack-gate-output.txt
  windows-lane | LOCAL-PASS | /home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-merge-validate.ffXWGF -- /home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh --threshold 15 --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- bash /var/tmp/gc-regate-ga-cieo5zop-gs_d9bjo/ci-windows-lane.sh; exit=0; log=/var/tmp/gc-regate-ga-cieo5zop-gs_d9bjo/ci-required-corrected/windows-lane-output.txt
  windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | windows_reach=none; proof=/var/tmp/gc-regate-ga-cieo5zop-gs_d9bjo/ci-required-corrected/native-windows-reach.json
windows_reach: none (both measured closures)
deferred_to_pr_ci: credential-provider-windows (non-linux-runner), windows-lane-test (non-linux-runner)
not_covered: none
host_gaps: none
```

```text
/var/tmp/gc-regate-ga-cieo5zop-gs_d9bjo/c3-run/units/gate.w1.stderr.log: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=25.85 run_max_load=27.20 run_mean_load=14.56 run_readings=52 wait_first_load=25.85 wait_max_load=25.85 wait_mean_load=25.85 wait_readings=1 read_errors=0
/var/tmp/gc-regate-ga-cieo5zop-gs_d9bjo/ci-required-corrected/runner-policy-output.txt: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=12.55 run_max_load=NA run_mean_load=NA run_readings=0 wait_first_load=12.55 wait_max_load=12.55 wait_mean_load=12.55 wait_readings=1 read_errors=0
/var/tmp/gc-regate-ga-cieo5zop-gs_d9bjo/ci-required-corrected/pack-gate-output.txt: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=12.02 run_max_load=12.39 run_mean_load=12.20 run_readings=3 wait_first_load=12.02 wait_max_load=12.02 wait_mean_load=12.02 wait_readings=1 read_errors=0
/var/tmp/gc-regate-ga-cieo5zop-gs_d9bjo/ci-required-corrected/windows-lane-output.txt: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=12.30 run_max_load=15.54 run_mean_load=14.29 run_readings=3 wait_first_load=12.30 wait_max_load=12.30 wait_mean_load=12.30 wait_readings=1 read_errors=0
```

Skip justification: the change owns two contention tests and neither skips. The retained audit checks every printed skip reason/site and the test-binary import closures. Reached shared/helper hunks are the fixture-local pacing helper, called only by the two owned tests, with sqliteBusySleep restored by cleanup; no skip predicate or package initializer changes. Every required baseline comparison is pre-existing. Baseline test runs and the expected mutation failure are excluded from canonical counts.
Skip proof: 675 records; 318 sites; 540 records measured; 35 remote target comparison groups and 4 native groups; zero failed comparisons; no message bytes changed.

```text
COMPARE-NOTE the remote worker's account differs between the runs in 1 SKIP(s), read as the same reason because the account is the only difference (an account is rbe-, a letter and two digits, standing alone; the logs are not changed): TestInfraScopedManagedDoltPIDIdentityUnderRealSystemd (base rbe-a05, head rbe-a06)
COMPARE-NOTE a generated path name differs between the runs in 1 SKIP(s), read as the same reason because it is the only difference and the same file and line were printed (a generated name is the digits Go appends to a temp directory's name, the process id in the name of gascity's temp root gct<pid>-<digits>, or a UUID right after a /work/ directory, which is how a Bazel remote worker names its work directory; the logs are not changed): TestProxyProcessReapRegistryMatchesAnEquivalentSpelling (base 2631084387, head 3905246502)
```

Full execution.json retained: 6050 lines; /var/tmp/gc-regate-ga-cieo5zop-gs_d9bjo/c3-run/evidence/execution.json; SHA256 b5937d7c69c33557cc6ceef8ed45855c7089f3d6ed4f3397b6ddf86d51e228ae.
Full CI accounting and exact scripts retained: 120 lines; /var/tmp/gc-regate-ga-cieo5zop-gs_d9bjo/ci-accounting.json; SHA256 65c208a55aeab864011ea2e8122bf179be3cc625e287403a9ba6cdcdc53201f6.
Full skip reasons, sites, reach and baseline judgments retained: 18573 lines; /var/tmp/gc-regate-ga-cieo5zop-gs_d9bjo/skip-proof/final-justification.json; SHA256 fe53e9c9f7d6a17b82f1f5242a20c34fa2ef5fe60a1cc4b2d33dc72be3bebb66.
Full skip comparison results retained: 702 lines; /var/tmp/gc-regate-ga-cieo5zop-gs_d9bjo/skip-proof/compare-results.json; SHA256 371944e6d815015e23ce6e810824013b7544a9ca6d91da1b1b6c9231746940d7.
Full native skip comparison results retained: 34 lines; /var/tmp/gc-regate-ga-cieo5zop-gs_d9bjo/skip-proof/native-compare-results.json; SHA256 7fd536c5c9b5d7c47d82a04253c9e855c3b5573b9e1926cb3c97fc9c1f403bc0.
Full named canonical evidence retained: 8 lines; /var/tmp/gc-regate-ga-cieo5zop-gs_d9bjo/canonical-by-name-evidence.txt; SHA256 5a99c93995c7463fa2a140b83605fe4dfba6ef169ec2fa79d2aebfc97db463e8.
Full independent ticker mutation retained: 65 lines; /var/tmp/gc-regate-ga-cieo5zop-gs_d9bjo/acceptance-ticker-mutant-test.log; SHA256 7ebd733c4d10cc25892a908baa289e879e15ab427ceb8517c37b9dfcc4d7ca92.

The supplemental baseline setup initially ran locally because its fresh view lacked remote definitions; those logs are retained under skip-proof/base-skips and are uncredited. Corrected measurements under base-skips-remote-v2 installed RBE inside the actual view and passed per-phase BEP predicates. Native measurements used the canonical floor environment and a bounded host-load wait. The canonical suite, CI jobs and independent mutation were not repeated.

This PASS authorizes opening the PR. Merge remains with MPR and requires green CI on the PR head, including the deferred native Windows jobs. Tracker ga-rh8jgr stays open until the fixture fix lands.

Counts are top-level result lines per job. The skip-proof record also includes subtests and individual lane entries.

```text
load_threshold: 15
load_waited_seconds: 0
load_wait_timed_out: 0
run_start_load: 25.85
run_max_load: 27.20
run_mean_load: 14.56
run_readings: 52
native_base_load: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=13.62 run_max_load=15.71 run_mean_load=15.10 run_readings=3 wait_first_load=13.62 wait_max_load=13.62 wait_mean_load=13.62 wait_readings=1 read_errors=0
```

```text
FRESH_TREE_RECORD from=/var/tmp/gc-merge-validate.ffXWGF head=80d7e790c2e0 tree=ecbbd8a8b218 dir=/var/tmp/gc-fresh-tree.pumFkX modified=0 untracked=0 ignored=6 ignored_names=.bazelrc.local,.github/workflows/scripts/__pycache__/,bazel-bin,bazel-gc-merge-validate.ffXWGF,bazel-out
GLT_RECORD: {"golangci_lint":{"pin":"2.14.0","resolved":"2.14.0","mismatch":false}}
FRESH_TREE_RECORD from=/var/tmp/gc-merge-validate.ffXWGF head=80d7e790c2e0 tree=ecbbd8a8b218 dir=/var/tmp/gc-fresh-tree.M2F7Uv modified=0 untracked=0 ignored=6 ignored_names=.bazelrc.local,.github/workflows/scripts/__pycache__/,bazel-bin,bazel-gc-merge-validate.ffXWGF,bazel-out
FRESH_TREE_RECORD from=/var/tmp/gc-merge-validate.ffXWGF head=80d7e790c2e0 tree=ecbbd8a8b218 dir=/var/tmp/gc-fresh-tree.xBWk7p modified=0 untracked=0 ignored=6 ignored_names=.bazelrc.local,.github/workflows/scripts/__pycache__/,bazel-bin,bazel-gc-merge-validate.ffXWGF,bazel-out
GATE_ICU74 lib=libicui18n.so.74 file="/usr/local/lib64/icu74-compat/libicui18n.so.74.2" sha256=d90d62f0d7b6c6fbc279ae5e433cc7004aa47cdb106a71a02350099f8df9ae51 recorded=yes
GATE_ICU74 lib=libicuuc.so.74 file="/usr/local/lib64/icu74-compat/libicuuc.so.74.2" sha256=33499dd97a5393926b32e0b535f5340f7dae14b35e59025b572e6bd9ad48a2ae recorded=yes
GATE_ICU74 lib=libicudata.so.74 file="/usr/local/lib64/icu74-compat/libicudata.so.74.2" sha256=c891c5fdb45e3cf756b728abe256be7af1bfc97f00ca2f2fac450c96c09bd4b7 recorded=yes
GATE_ICU74 verdict=OK provenance=recorded sonames="libicui18n.so.74 libicuuc.so.74 libicudata.so.74"
runtime_probe: systemctl --user is-active podman.socket; exit=0 (active)
runtime_probe: curl --fail --silent --unix-socket /run/user/1000/podman/podman.sock http://localhost/_ping; exit=0 (OK)
```

Additional contract test (not diff-owned):

```text
  unit-9a6a09bab414814cab53.log: --- PASS: TestSQLiteStoreWriteTxTakesWriteLockAtBegin (0.01s)
  integration-packages-8d820cd4c52192de043e.log: --- PASS: TestSQLiteStoreWriteTxTakesWriteLockAtBegin (0.01s)
```
