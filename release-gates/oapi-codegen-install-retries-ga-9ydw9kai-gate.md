# Retry oapi-codegen installation — ga-9ydw9kai

**Verdict:** **PASS**

Reviewed source: `12c8dd0864122ca97db6bf0f80e1def0a200dff7`. Build ga-dioz9m; latest review ga-753rg5ob CLOSED PASS at this exact resolved source. The two source commits cite original owner ga-6hg5mi, verified from the build/review lineage before widening the ancestry guard. Branch provenance is not a push target. This record is the only deployer addition to the isolated source branch.

DEPLOY_MODE: remote
BASE_REF: origin/main
merge_state: clean (origin/main@e47f4e1bd540983b08f69d221ee019b175666dc7)
gate_base_kind: current-tip
gate_base: e47f4e1bd540983b08f69d221ee019b175666dc7
canonical_merge: 0a6a84dc8fad67feadcfc39d6dabdc144ceee051
canonical_tree: 39bbb73c43e94b745a3892b9e0e619550e510d90
merge_validation: source merged onto pinned current main; independent git merge-tree equality verifies the tested tree. Build of both CLI binaries, full nogo/vet and normal active hooks PASS. Main can advance; MPR and merge-group checks validate the eventual merge. No rebase or conflict resolution by the deployer.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Latest review ga-753rg5ob CLOSED PASS for the exact source; no carryover or reviewer-added test substitution. |
| 2 | Acceptance met | PASS | Transient install failures retry and succeed on attempt3; permanent failure stops after attempt5 with nonzero exit and terminal diagnostic. Installed-tool guard, v2.6.0 pin and 2/4/8/16-second backoff verified in source. Test fakes sleep and Go, making the regression deterministic. |
| 3 | Tests pass | PASS | Five fresh remote phases plus complete mandatory native floor: 149260 PASS / 0 FAIL / 512 SKIP top-level result lines. Owned root and both subcases PASS by name; all skips and four required CI leaves accounted below. |
| 4 | No unresolved HIGH finding | PASS | Latest review read in full; no blocking/high finding remains. |
| 5 | Final source clean | PASS | Canonical source/merge tree clean after checks; final gate-only commit uses normal hooks and must be clean before push. No other source edit. |
| 6 | Merge state and build/vet | PASS | Clean merge and exact tree equality; both CLI builds and full make vet/nogo passed. c6-evidence.log. |
| 7 | Single feature theme | PASS | Makefile retry, its deterministic test and required resource-census/TESTING ledger changes form one installation reliability fix. Six paths, no independent feature or internal prompt contamination. |

The installer retries any failed go-install invocation up to five times, handling transient module-proxy failures. It does not classify error messages. An already available oapi-codegen exits silently before the loop; the tool remains pinned to v2.6.0. The resource ledger adds exactly one medium subprocess call for the new lexical make invocation. The change affects the installer recipe, its test and resource accounting.

## Fresh test evidence

| Root | Scope | Result |
|---|---|---|
| TestMakeInstallOapiCodegenRetriesTransientGoInstallFailure | owned | PASS (1 root observations; all observed subcases PASS) |
| TestMakeInstallFailsClosedWhenCopyFails | existing guard | PASS (1 root observations; all observed subcases PASS) |
| TestRepositoryLedgerMatchesCensusAndDocumentation | existing guard | PASS (1 root observations; all observed subcases PASS) |

The owned root includes explicit `/transient` and `/persistent` PASS verdicts in the canonical full-suite logs. Existing install-copy fail-closed and repository census/documentation consistency guards also pass. The reviewer's mutation and earlier Bazel results are inputs only; none replaces this fresh gate.

test_cmd_scope: full-suite
test_exec_mode: remote-exec (BEP-derived)
test_exec_bep: five phase predicates below; independently recomputed from retained BEP with invocation start/finish bounds
rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98", "worker_env_tree": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"}
fallback_reason: none
admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 78.75223159790039, "waited_seconds": 0.001}
farm_test_seconds: 13173.536
count_scope: top-level result lines; farm 149239 PASS / 498 SKIP plus native floor 21 PASS / 14 SKIP. Native skip inventory includes subtests (39 events), so it differs from the top-level count.
test_counts: 149260 PASS / 0 FAIL / 512 SKIP (top-level per-job result lines; repeated jobs counted separately)
test_log_dir: /var/tmp/deploy-ga-9ydw9kai.myhvd7n4/c3-run/evidence/remote/logs
diff_tests_executed: TestMakeInstallOapiCodegenRetriesTransientGoInstallFailure PASS; /transient PASS; /persistent PASS
heavy_mode: none
failure_attribution: none
waiver_ref: none (Gas City has no waiver path)
owned_topup: []
floor_topup: complete; 16 commands all exit0; beadstest, Darwin compile, runner self-tests and every environment-absence SKIP package (parent selected for subtests)
skip_inventory: farm 498 non-owned rows {'other': 426, 'environment-absence': 72}; native 39 non-owned rows {'environment-absence': 29, 'other': 10}
latency_leg: measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS
city_protocol_commit_at_criterion3_start: 8ab38050b1933a19d897cfcbf8d1aa4c2371ef91

| Phase | Results | Fresh remote | Cache credits | Retries | Verdict |
|---|---:|---:|---:|---:|---|
| unit | 407 | 407 | 0 | 0 | PASS |
| acceptance | 25 | 25 | 0 | 0 | PASS |
| integration-packages | 86 | 86 | 0 | 0 | PASS |
| integration-full | 14 | 14 | 0 | 0 | PASS |
| integration-latency | 1 | 1 | 0 | 0 | PASS |

Every remote result is uncached locally and remotely, attempt1, started inside its phase window and terminally passed. Raw log/XML hashes independently verified. Full integration excludes the registry latency tests, which run separately under a clear probe. No stitched farm/local suite or cache-credited runtime PASS. The mandatory native floor and all its per-job logs remain retained.

policy_lane: fresh-tree-run → gate-base.sh run → remote-lane.sh make lint check-release-dist-ignore check-routed-test-rows check-split-topology-rows check-residency-boundary; exit0 PASS; /var/tmp/deploy-ga-9ydw9kai.myhvd7n4/fast-evidence.log
format_lane: separate fresh view, bazel test --config=remote-exec --noremote_local_fallback //:go_format_test; exit0 PASS
drift_lane: separate fresh-tree-run → gate-base.sh run → make bazel-sync BAZEL_SYNC_FLAGS='--config=remote-exec --noremote_local_fallback'; git diff --exit-code; empty git status --porcelain --untracked-files=all; exit0 PASS; /var/tmp/deploy-ga-9ydw9kai.myhvd7n4/fast-evidence.log
ci_lane_run: n/a (no CI job, matrix, timeout or required-check configuration diff)
ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/deploy-ga-9ydw9kai.myhvd7n4/merge e47f4e1bd540983b08f69d221ee019b175666dc7 12c8dd0864122ca97db6bf0f80e1def0a200dff7 fork; exit0; workflow blob e5b11c348505dab28dc223e874bc2c61cbbb33c5; 4 LEAF jobs need a status
ci_required_accounting: changes | LOCAL-PASS | {"command": ["python3", "/home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py", "/var/tmp/deploy-ga-9ydw9kai.myhvd7n4/merge", "e47f4e1bd540983b08f69d221ee019b175666dc7", "12c8dd0864122ca97db6bf0f80e1def0a200dff7", "fork"], "exit": 0, "log": "/var/tmp/deploy-ga-9ydw9kai.myhvd7n4/ci-required/closure.txt", "status": "LOCAL-PASS"}
ci_required_accounting: runner-policy | LOCAL-PASS | {"command": "EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=/var/tmp/deploy-ga-9ydw9kai.myhvd7n4/ci-required/runner-policy.out python3 .github/workflows/scripts/runner_policy.py", "execution_exception": "Formula 3e STEP2 explicitly prescribes direct read-only invocation", "exit": 0, "log": "/var/tmp/deploy-ga-9ydw9kai.myhvd7n4/ci-required/runner-policy.log", "status": "LOCAL-PASS"}
ci_required_accounting: credential-provider-windows | DEFERRED-TO-PR-CI reason=non-linux-runner | {"command": ["/home/jaword/projects/gc-management/packs/actual/deployer/scripts/windows_reach.sh", "/var/tmp/deploy-ga-9ydw9kai.myhvd7n4/merge", "e47f4e1bd540983b08f69d221ee019b175666dc7", "12c8dd0864122ca97db6bf0f80e1def0a200dff7"], "exit": 0, "log": "/var/tmp/deploy-ga-9ydw9kai.myhvd7n4/ci-required/windows-reach.log", "reason": "non-linux-runner", "status": "DEFERRED-TO-PR-CI", "windows_reach": "none"}
ci_required_accounting: pack-gate | LOCAL-PASS | {"command": ["/home/jaword/projects/gc-management/packs/actual/all/scripts/gate-detached-run.sh", "start", "--run-dir", "/var/tmp/deploy-ga-9ydw9kai.myhvd7n4/ci-required/pack-gate", "--key", "39bbb73c43e94b745a3892b9e0e619550e510d90-ci-pack", "--", "/home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh", "--", "/home/jaword/projects/gc-management/packs/actual/deployer/scripts/gate-base.sh", "run", "--", "/home/jaword/projects/gc-management/packs/actual/all/scripts/gate-admission.py", "--class", "local-heavy", "--record", "/var/tmp/deploy-ga-9ydw9kai.myhvd7n4/ci-required/pack-admission.json", "--max-wait", "1800", "--", "/home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh", "--threshold", "15", "--max-wait", "1800", "--", "/home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh", "--", "python3", "/var/tmp/deploy-ga-9ydw9kai.myhvd7n4/ci-pack-body.py"], "log": "/var/tmp/deploy-ga-9ydw9kai.myhvd7n4/ci-required/pack-output.txt", "output_command_exit": 0, "status": "LOCAL-PASS"}
windows_reach: none
deferred_to_pr_ci: credential-provider-windows (non-linux-runner)
not_covered: none

The closure and runner-policy commands are read-only and run directly as explicitly prescribed by formula3e STEP2. Other job commands run detached in a fresh view behind admission, load wait and isolation, one job at a time. The pack job uses the workflow's exact two run blocks, retry loops, step environments and timeouts; its Go commands are the explicit CI-job exception to ordinary Bazel testing. Local results do not replace PR CI. Merge only when CI / required is green at the current head and each deferred job succeeds there (or is skipped solely because its own path filter is false).

Every non-owned SKIP is mapped to an unchanged source site. Test-import closures determine shared-hunk reach for scripts and the resourcecensus ledger package. Reachable rows require same-command/tag/target BASE comparison at the pinned base; the complete comparison record establishes that requirement. All other packages exclude those shared packages. No owned SKIP is excused; native remaining platform, opt-in, fixture or environment absences are independently inventoried. Skip inventories and source-site proofs are retained as files below.

Runtime preflight: host ICU74 check OK, rootless Podman socket/API active, exact upstream pinned Dolt image2.2.0 cached, and enabled/recent 30-minute stale-testcontainer sweep verified. Native floor, not ambient host assumptions, establishes the environment top-ups. Observed local run load (including any bounded-wait timeout):

```json
[
  {
    "load_scope": "canonical native floor",
    "load_threshold": "15",
    "load_waited_seconds": "1801",
    "load_wait_timed_out": "1",
    "run_start_load": "46.98",
    "run_max_load": "46.44",
    "run_mean_load": "40.45",
    "run_readings": "10",
    "wait_first_load": "93.08",
    "wait_max_load": "93.08",
    "wait_mean_load": "66.08",
    "wait_readings": "61",
    "read_errors": "0"
  },
  {
    "load_scope": "CI pack-gate",
    "load_threshold": "15",
    "load_waited_seconds": "1201",
    "load_wait_timed_out": "0",
    "run_start_load": "14.28",
    "run_max_load": "16.71",
    "run_mean_load": "15.94",
    "run_readings": "8",
    "wait_first_load": "41.31",
    "wait_max_load": "41.31",
    "wait_mean_load": "21.64",
    "wait_readings": "42",
    "read_errors": "0"
  },
  {
    "load_scope": "native BASE comparison",
    "load_threshold": "15",
    "load_waited_seconds": "1801",
    "load_wait_timed_out": "1",
    "run_start_load": "41.39",
    "run_max_load": "46.29",
    "run_mean_load": "44.65",
    "run_readings": "5",
    "wait_first_load": "28.19",
    "wait_max_load": "42.03",
    "wait_mean_load": "29.72",
    "wait_readings": "62",
    "read_errors": "0"
  }
]
```

Canonical target selection (from each phase BEP; the full-integration exclusion and latency-only inclusion are the prescribed complementary lanes):

```text
unit: //...; 
acceptance: //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests; --config=acceptance
integration-packages: //test:integration_packages; --config=integration
integration-full: //test/integration:integration_test; --config=integration --test_arg=-test.skip=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$
integration-latency: //test/integration:integration_test; --config=integration --test_filter=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$
```

Canonical phase commands and flags:

```text
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-9ydw9kai.myhvd7n4/c3-run/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-9ydw9kai.myhvd7n4/c3-run/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-9ydw9kai.myhvd7n4/c3-run/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-9ydw9kai.myhvd7n4/c3-run/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-9ydw9kai.myhvd7n4/c3-run/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```

Compact execution record:

```json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/deploy-ga-9ydw9kai.myhvd7n4/c3-run/evidence/remote","admission":{"class":"remote","floor_gib":25.0,"free_gib":78.75223159790039,"waited_seconds":0.001,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98","worker_env_origin_main":"sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"},"test_exec_bep":{"unit":{"results":407,"remote_executed":407,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":86,"remote_executed":86,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":14,"remote_executed":14,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":13173.536,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=5fd31cdd75b202be3ab9de14bb2514f059b7969d1f14ab7fdc392c2c723e6b7b"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":64.801,"evidence_errors":[],"probe":["GATE_FARM_PROBE verdict=clear waited=0s load1=13.36 load5=5.16 load15=1.96 cpus=32 threshold=50 max_wait=600s"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 12.04 5.77 2.28 29/1069 55498"]}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":30988}},"log_dir":"/var/tmp/deploy-ga-9ydw9kai.myhvd7n4/c3-run/evidence/remote/floor-logs"},"skip_inventory":{"counts_by_reason_class":{"other":426,"environment-absence":72},"top_level_counts_by_reason_class":{"other":233,"environment-absence":43},"tests":{"elided_array":{"items":498,"bytes":177392}}},"test_cmd":{"elided_array":{"items":5,"bytes":2059}},"test_counts":{"PASS":149260,"FAIL":0,"SKIP":512},"diff_tests_executed":"diff_tests_executed:\n  TestMakeInstallOapiCodegenRetriesTransientGoInstallFailure PASS (unit-ee115c844954da53d34d.log)\nGATE_TEST_EVIDENCE_RESULT ok: 1 diff-owned tests verified by name\n"}
```

## Retained evidence

Durable evidence copy: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-9ydw9kai/gate-20261108-myhvd7n4. Final copy manifest and SHA256 readback: final-durable-manifest.json at that path.

The prescribed docs/PROJECT_MANIFEST.md is absent from this repository; the release-criteria protocol and live build/review acceptance records define this gate.

Evidence root: `/var/tmp/deploy-ga-9ydw9kai.myhvd7n4` (protected from scratch reaping; source worktrees/caches may be removed after completion, logs are kept). A durable copy is recorded in the deploy bead before handoff. Full inventories, load metrics, test logs and SHA256 manifests are evidence files, never pasted into this PR record.

Full execution retained: 5044 lines; /var/tmp/deploy-ga-9ydw9kai.myhvd7n4/c3-run/evidence/execution.json; SHA256 eb0480184ce74ed56c3b5fa397525b6444268c25b8627a9702fa5ad27683a730.
Full raw-BEP-recomputed retained: 86 lines; /var/tmp/deploy-ga-9ydw9kai.myhvd7n4/bep-recomputed.json; SHA256 1f06407d152672b928e50582f738f929baca73ab55ea2a626eba80a17af86512.
Full owned-and-neighbors retained: 42 lines; /var/tmp/deploy-ga-9ydw9kai.myhvd7n4/acceptance-by-name.json; SHA256 684e94dca83abac9ad30a828f68f5459427a0d84a05d80f6ada947a64e020892.
Full farm-skip-inventory-and-reach retained: 7138 lines; /var/tmp/deploy-ga-9ydw9kai.myhvd7n4/farm-skip-site-mapping.json; SHA256 073fb3c54ec25156a200b740bd8614cbf328a5c3292ab1d501a3ca1a2ead7950.
Full farm-BASE-comparisons retained: 216 lines; /var/tmp/deploy-ga-9ydw9kai.myhvd7n4/base-skip-comparison-proof.json; SHA256 ccadd7dfb28984a33311eee60ea4dd2d829d7ed7863c3e77ee8489bca2987718.
Full native-skip-audit retained: 785 lines; /var/tmp/deploy-ga-9ydw9kai.myhvd7n4/native-skip-audit.json; SHA256 9177f0e3733d61b547dbfb10905158c6de9de8666574258f246f1f73f3aaa22b.
Full CI-required-closure retained: 15 lines; /var/tmp/deploy-ga-9ydw9kai.myhvd7n4/ci-required/closure.txt; SHA256 579bea1a365fd27ad754b317f6f23682b7041cb4bee200f8da24729c1263cbb8.
Full CI-required-accounting retained: 92 lines; /var/tmp/deploy-ga-9ydw9kai.myhvd7n4/ci-required/accounting.json; SHA256 3108e20cafc86ac4e2545d6860bc9693a524b069c6c33b0ae780b31acbb7fe89.
Full runtime-proof retained: 12 lines; /var/tmp/deploy-ga-9ydw9kai.myhvd7n4/runtime-proof.json; SHA256 bc57f1e4238d14b780163186c8ea6a9dc0acbab66d8d70677d65175a978ef8b5.

Criterion6 command/log: /var/tmp/deploy-ga-9ydw9kai.myhvd7n4/c6.sh; /var/tmp/deploy-ga-9ydw9kai.myhvd7n4/c6-evidence.log; exit0.
Fast lane command/log: /var/tmp/deploy-ga-9ydw9kai.myhvd7n4/fast-lane.sh; /var/tmp/deploy-ga-9ydw9kai.myhvd7n4/fast-evidence.log; exit0.
C3 command/log: gate-criterion3.sh --repo /var/tmp/deploy-ga-9ydw9kai.myhvd7n4/merge --base e47f4e1bd540983b08f69d221ee019b175666dc7 --bead-json /var/tmp/deploy-ga-9ydw9kai.myhvd7n4/bead.json --owned /var/tmp/deploy-ga-9ydw9kai.myhvd7n4/owned.tsv --run-dir /var/tmp/deploy-ga-9ydw9kai.myhvd7n4/c3-run/evidence; /var/tmp/deploy-ga-9ydw9kai.myhvd7n4/c3-output.txt; exit0.
Final integrity audit: /var/tmp/deploy-ga-9ydw9kai.myhvd7n4/final-audit.py; /var/tmp/deploy-ga-9ydw9kai.myhvd7n4/final-audit.log; exit0.

Full canonical-phase-invocations retained: 120 lines; /var/tmp/deploy-ga-9ydw9kai.myhvd7n4/canonical-phase-invocations.json; SHA256 ca3feba75af64d10fe2329b4c735805ecea921e9e09913ad85a494bccfa5017a.
