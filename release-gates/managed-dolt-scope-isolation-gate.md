# Managed Dolt scope isolation release gate

**Verdict:** **PASS**

Evaluated 2026-11-05 22:13 UTC for ga-24p06a5; build ga-tuqlqr, reviewed rework ga-yzvki7, review ga-rcgt2m8.

- Reviewed source: `646ac250dfc0b9a49ca37d9bf68ab2e96d03408f` (resolved as a commit; provenance branch is never a push target).
- gate_base: `9f807760c1914a79326c35029e0a9c1600d14ee8`; gate_base_kind: current-tip.
- Canonical merge: `79fd1a4006d3965455d9f26f6c49fe0c19e67600`; tree: `69f2b6dc7547c7ccc95022a3349575d963d1e0da`.
- merge_state: clean (origin/main@9f807760c1914a79326c35029e0a9c1600d14ee8).
- merge_validation: exact merged tree built, vetted and tested here; MPR checks the actual merge result and required CI before landing.
- DEPLOY_MODE: remote; PUSH_REMOTE: fork; isolated branch: deploy/ga-24p06a5-gate; no stack.

| # | Criterion | Result and evidence |
| --- | --- | --- |
| 1 | Review PASS | PASS. Live ga-rcgt2m8 records PASS for the resolved reviewed SHA; no carryover or reviewer-added tests. |
| 2 | Acceptance | PASS for AC1–AC4 and AC6: city/rig runtime and session env projections, persistent config pins, ownership exclusions and existing proxied regressions. Six owned roots and ten subtests pass in the fresh suite. AC5 real-bd shared-root gate-lock acceptance is explicitly descoped to open ga-9p8m2q. |
| 3 | CI-equivalent tests | PASS. Complete canonical five-phase runner plus all 16 native floor steps; full counts and by-name results below. Policy, formatting, generated-file drift and CI leaf accounting pass. |
| 4 | Open HIGH findings | PASS. Zero unresolved HIGH findings. Review style result has one low naming nit; security result has no blocker, major or minor defect. |
| 5 | Final branch clean | PASS. Source and canonical scratch are clean; the isolated deploy adds only this gate record. Commit/push publication checks require a clean deploy tree and the reviewed SHA as an ancestor. |
| 6 | Build and merge state | PASS. `make check-hooks` and whole-repository `bazel build --config=remote-exec --noremote_local_fallback //...` complete successfully (1,456 targets), including nogo lint/vet. Exact canonical parents, merge-tree prediction and clean scratch independently verified. |
| 7 | Single theme | PASS. Five source files in cmd/gc implement one managed-scope shared-server isolation change. Final ancestry guard accepts the deploy/build/rework lineage and rejects unrelated ancestry; no forbidden paths or stacked PR. |

## Full-suite evidence

test_cmd_scope: full-suite; test_exec_mode: remote-exec (BEP-derived); fallback_reason: none; heavy_mode: none; waiver_ref: none; failure_attribution: none.

test_counts: PASS=147031 FAIL=0 SKIP=507 across 537 retained logs. These are top-level Go result lines summed across jobs, not unique tests. Non-Go test runners and compile/runner self-check logs also remain retained.

test_log_dir: `/var/tmp/gc-heavy-gate/runs/ga-24p06a5.20261105-jf5mf1b3/c3/evidence/remote/logs`.
farm_test_seconds: 12390.044.
admission: remote, cap 2, 25 GiB floor; free 47.101 GiB; wait 0.001 seconds.
rbe_state: generated scratch RC present; kill switch absent; certificate expires 2027-01-09; worker-env pin matches the base (`sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98`).
owned_topup: none needed; every owned test passed in the forced full suite.
floor_topup: complete, 16/16 exit 0: every environment-absence selection by package/parent, beadstest, Darwin compile and both runner self-tests.
latency_leg: measured, first attempt, probe clear after 30s (load1 49.91, threshold 50); both listed budgets PASS, absent from the main leg as required.
ci_lane_run: n/a (no CI-config change).

Actual detached invocation (exit 0):

```text
env GC_GATE_REMOTE_ENABLED=1 GC_REMOTE_GATE_FLOOR_GIB=25 GC_REMOTE_GATE_CAP=2 ISOLATED_TEST_BEAD=ga-24p06a5 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/gate-base.sh run --dir /var/tmp/gc-merge-24p06a5.4ek4l4ep -- /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-criterion3.sh --repo /var/tmp/gc-merge-24p06a5.4ek4l4ep --base 9f807760c1914a79326c35029e0a9c1600d14ee8 --bead-json /var/tmp/gc-heavy-gate/runs/ga-24p06a5.20261105-jf5mf1b3/bead-current.json --owned /var/tmp/gc-heavy-gate/runs/ga-24p06a5.20261105-jf5mf1b3/owned.tsv --run-dir /var/tmp/gc-heavy-gate/runs/ga-24p06a5.20261105-jf5mf1b3/c3/evidence -- python3 /var/tmp/gc-heavy-gate/runs/ga-24p06a5.20261105-jf5mf1b3/local-bazel.py
```

Phases reproduce `make -k check` (unit), both acceptance targets under `--config=acceptance`, `//test:integration_packages` under `--config=integration`, full `//test/integration:integration_test` minus the canonical latency list, then only that list with the worker load probe. Main and latency partitions are computed from `gate-latency-tests.txt`; no coverage is omitted.

| Phase | Verdict | test_exec_bep |
| --- | --- | --- |
| unit | PASS (301 targets) | 395 fresh remote results; cached=0, retries=0; window and terminal verdicts verified |
| acceptance | PASS (8 targets) | 25 fresh remote results; cached=0, retries=0; window and terminal verdicts verified |
| integration-packages | PASS (37 targets) | 86 fresh remote results; cached=0, retries=0; window and terminal verdicts verified |
| integration-full | PASS (1 targets) | 14 fresh remote results; cached=0, retries=0; window and terminal verdicts verified |
| integration-latency | PASS (1 targets) | 1 fresh remote results; cached=0, retries=0; window and terminal verdicts verified |

Every phase's actual flags and BEP path (all exit 0):

```text
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-24p06a5.20261105-jf5mf1b3/c3/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-24p06a5.20261105-jf5mf1b3/c3/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-24p06a5.20261105-jf5mf1b3/c3/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-24p06a5.20261105-jf5mf1b3/c3/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-heavy-gate/runs/ga-24p06a5.20261105-jf5mf1b3/c3/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```

```text
diff_tests_executed:
  TestEnsureGCOwnedProxiedScopeSharedServerOff PASS (integration-packages-821a75e4a503601af052.log, unit-fd986cf5bab8709b8d45.log)
  TestEnsureGCOwnedScopeSharedServerOffPinsManagedServerScopes PASS (integration-packages-f7fba3a97bc02cffff0b.log, unit-0e1499ad8e8a0a6b9d81.log)
  TestExplicitEndpointRigDoesNotInheritManagedCityBdSharedServerPin PASS (integration-packages-aa6cf5c138f0b61b866a.log, unit-27b23e708b477fce4dad.log)
  TestManagedServerRuntimeEnvPinsBdSharedServerOff PASS (integration-packages-830685c60d0c2dd88567.log, unit-64014bc0bb3833bc198c.log)
  TestManagedServerSessionEnvPinsBdSharedServerOff PASS (integration-packages-b72a67a2f9444af705e3.log, unit-493754ea2d5b4d8858c8.log)
  TestUnownedServerScopesKeepBdSharedServerResolution PASS (integration-packages-e5330b8c981927473156.log, unit-3d6d3036a2f5902472b5.log)
```

All ten owned subtests PASS in unit and integration. The full source fixture map and ownership analysis are retained by pointer below.

## Skips, runtime and load

skip_inventory: 493 classified remote entries: 421 other and 72 environment-absence. Configuration-gated process tests are exercised by integration; remaining rows cover platform/fixture restrictions, optional live providers, explicit manual contracts and upstream placeholders. Every environment-absence selection was run through the native floor; non-owned remaining SKIPs are inventoried. None of this diff's tests skipped.

The two comment-only shared hunks have no runtime effect; the new test helpers/constants are confined to their new test file and used only by its owned tests. All 27 remaining cmd/gc skip sites are UNCHANGED. Pinned-base unit/integration comparisons establish all 54 selected remote skip events have the same names and reasons: 54 pre-existing, zero new/changed/diff-owned. This focused comparison is skip proof, separate from full-suite evidence. Other-package rows are retained with their reasons and fall outside these test-only shared hunks.

Runtime probes passed: pinned bd v1.3.4 matched this canonical tree's deps.env; pinned Dolt image and rootless podman socket available; ICU74 recorded sonames OK. The wrapper accurately reported PASS-THROUGH; actual remote execution is proven by BEP. No host gc binary was installed.

Native-floor host load (the required local soft wait, no timeout):

```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=781 wait_timed_out=0 run_start_load=14.95 run_max_load=15.50 run_mean_load=15.17 run_readings=7 wait_first_load=26.74 wait_max_load=41.53 wait_mean_load=26.70 wait_readings=27 read_errors=0
```

This supplies load_threshold=15, load_waited_seconds=781, load_wait_timed_out=0, run_start_load=14.95, run_max_load=15.50, run_mean_load=15.17, run_readings=7.

Outer remote-chain host-load context (remote admission has no local-heavy load wait):

```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=46.81 run_max_load=48.14 run_mean_load=28.15 run_readings=77 wait_first_load=46.81 wait_max_load=46.81 wait_mean_load=46.81 wait_readings=1 read_errors=0
```

The initial C6 server exited rc37 with “Socket closed”, no BuildFinished, failed action or test verdict. TESTING.md permits a documented pre-test service-outage retry; one recovery completed, preserving the interrupted run. No failing product test was retried. The optional baseline unit collector initially refused evidence-root mode 0755; its passing test's original stdout and core BEP remain retained, with no raw-CAS/XML retention claim for that variant. Mode 0700 was applied before the distinct integration variant; that collector and both skip comparisons pass. No gate result uses a waiver.

## Fast lane and CI-required accounting

fast_lane: PASS before runtime setup/suite, each lane in a fresh view of the canonical merge.
policy_lane: pinned format check, shell guards, hooks and 11 policy/format/doc-sync targets PASS; full-tree nogo covered by criterion 6 and the unit lane.
drift_lane: `make bazel-sync BAZEL_SYNC_FLAGS='--config=remote-exec --noremote_local_fallback'`, `git diff --exit-code`, empty tracked/untracked status, and seven schema/client/dashboard/lockfile drift targets: PASS (exit 0).
OpenAPI: `make openapi-breaking-check OPENAPI_BREAKING_BASE=9f807760c1914a79326c35029e0a9c1600d14ee8` with remote flags: PASS (exit 0).

```text
policy: FRESH_TREE_RECORD from=/var/tmp/gc-merge-24p06a5.4ek4l4ep head=79fd1a4006d3 tree=69f2b6dc7547 dir=/var/tmp/gc-fresh-tree.mifXUx modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-gc-merge-24p06a5.4ek4l4ep,bazel-out,bazel-testlogs
policy: GATE_BASE_RECORD base=9f807760c1914a79326c35029e0a9c1600d14ee8 knobs=CI_BEADS_DIFF_BASE,BASE_SHA,COMPLEXITY_BASE_REF,LINT_BASE dir=/var/tmp/gc-fresh-tree.mifXUx
policy: GLT_RECORD: {"golangci_lint":{"pin":"2.14.0","resolved":"2.14.0","mismatch":false}}
drift: FRESH_TREE_RECORD from=/var/tmp/gc-merge-24p06a5.4ek4l4ep head=79fd1a4006d3 tree=69f2b6dc7547 dir=/var/tmp/gc-fresh-tree.ubfLN5 modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-gc-merge-24p06a5.4ek4l4ep,bazel-out,bazel-testlogs
drift: GATE_BASE_RECORD base=9f807760c1914a79326c35029e0a9c1600d14ee8 knobs=CI_BEADS_DIFF_BASE,BASE_SHA,COMPLEXITY_BASE_REF,LINT_BASE dir=/var/tmp/gc-fresh-tree.ubfLN5
openapi: FRESH_TREE_RECORD from=/var/tmp/gc-merge-24p06a5.4ek4l4ep head=79fd1a4006d3 tree=69f2b6dc7547 dir=/var/tmp/gc-fresh-tree.ieJssa modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-gc-merge-24p06a5.4ek4l4ep,bazel-out,bazel-testlogs
openapi: GATE_BASE_RECORD base=9f807760c1914a79326c35029e0a9c1600d14ee8 knobs=CI_BEADS_DIFF_BASE,BASE_SHA,COMPLEXITY_BASE_REF,LINT_BASE dir=/var/tmp/gc-fresh-tree.ieJssa
```

ci_required_closure: `python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-24p06a5.4ek4l4ep 9f807760c1914a79326c35029e0a9c1600d14ee8 646ac250dfc0b9a49ca37d9bf68ab2e96d03408f fork` rc=0; merged ci.yml blob `e5b11c348505dab28dc223e874bc2c61cbbb33c5`; 4 LEAF jobs. The check/ci-required aggregators take no independent leaf status.
ci_required_accounting:

- changes | LOCAL-PASS | closure command above, exit 0; `/var/tmp/gc-heavy-gate/runs/ga-24p06a5.20261105-jf5mf1b3/ci-required/closure.txt`.
- runner-policy | LOCAL-PASS | `EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=/var/tmp/gc-heavy-gate/runs/ga-24p06a5.20261105-jf5mf1b3/ci-required/runner-policy.out python3 .github/workflows/scripts/runner_policy.py`, exit 0; log `/var/tmp/gc-heavy-gate/runs/ga-24p06a5.20261105-jf5mf1b3/ci-required/runner-policy.log` and nonempty output retained. Runner outputs resolve Linux jobs to Blacksmith Ubuntu.
- credential-provider-windows | NOT-TRIGGERED | `credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'`.
- pack-gate | NOT-TRIGGERED | `pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'`.

windows_reach: none (`/home/jaword/projects/gc-management/packs/actual/deployer/scripts/windows_reach.sh /var/tmp/gc-merge-24p06a5.4ek4l4ep 9f807760c1914a79326c35029e0a9c1600d14ee8 646ac250dfc0b9a49ca37d9bf68ab2e96d03408f`, rc=0, zero changed paths in the Windows closure).
deferred_to_pr_ci: none.
not_covered: none.

## Compact execution record

```json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/gc-heavy-gate/runs/ga-24p06a5.20261105-jf5mf1b3/c3/evidence/remote","admission":{"class":"remote","floor_gib":25.0,"free_gib":47.10131072998047,"waited_seconds":0.001,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98","worker_env_origin_main":"sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"},"test_exec_bep":{"unit":{"results":395,"remote_executed":395,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":86,"remote_executed":86,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":14,"remote_executed":14,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":12390.044,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=5fd31cdd75b202be3ab9de14bb2514f059b7969d1f14ab7fdc392c2c723e6b7b"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":98.673,"evidence_errors":[],"probe":["GATE_FARM_PROBE waiting waited=0s load1=78.61 load5=62.30 load15=27.83 cpus=32 threshold=50 max_wait=600s","GATE_FARM_PROBE verdict=clear waited=30s load1=49.91 load5=56.91 load15=27.14 cpus=32 threshold=50 max_wait=600s"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 32.31 51.74 26.51 6/977 55480"]}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":32394}},"log_dir":"/var/tmp/gc-heavy-gate/runs/ga-24p06a5.20261105-jf5mf1b3/c3/evidence/remote/floor-logs"},"skip_inventory":{"counts_by_reason_class":{"other":421,"environment-absence":72},"top_level_counts_by_reason_class":{"other":230,"environment-absence":43},"tests":{"elided_array":{"items":493,"bytes":183798}}},"test_cmd":{"elided_array":{"items":5,"bytes":2139}},"test_counts":{"PASS":147031,"FAIL":0,"SKIP":507},"diff_tests_executed":"diff_tests_executed:\n  TestEnsureGCOwnedProxiedScopeSharedServerOff PASS (integration-packages-821a75e4a503601af052.log, unit-fd986cf5bab8709b8d45.log)\n  TestEnsureGCOwnedScopeSharedServerOffPinsManagedServerScopes PASS (integration-packages-f7fba3a97bc02cffff0b.log, unit-0e1499ad8e8a0a6b9d81.log)\n  TestExplicitEndpointRigDoesNotInheritManagedCityBdSharedServerPin PASS (integration-packages-aa6cf5c138f0b61b866a.log, unit-27b23e708b477fce4dad.log)\n  TestManagedServerRuntimeEnvPinsBdSharedServerOff PASS (integration-packages-830685c60d0c2dd88567.log, unit-64014bc0bb3833bc198c.log)\n  TestManagedServerSessionEnvPinsBdSharedServerOff PASS (integration-packages-b72a67a2f9444af705e3.log, unit-493754ea2d5b4d8858c8.log)\n  TestUnownedServerScopesKeepBdSharedServerResolution PASS (integration-packages-e5330b8c981927473156.log, unit-3d6d3036a2f5902472b5.log)\nGATE_TEST_EVIDENCE_RESULT ok: 6 diff-owned tests verified by name\n"}
```

## Retained evidence

Full complete criterion-3 execution retained: 5005 lines; /var/tmp/gc-heavy-gate/runs/ga-24p06a5.20261105-jf5mf1b3/c3/evidence/execution.json; SHA256 3f1169b1da1946193d4c63e39401af7b8106c71269f620a58142c9e6c987c922.
Full retained raw artifact SHA256 manifest retained: 14590 lines; /var/tmp/gc-heavy-gate/runs/ga-24p06a5.20261105-jf5mf1b3/raw-artifact-manifest.json; SHA256 526bf4afae8e4c28bb6b0b8619939cf2ae7f1395077005c5e68812ed0eb616ec.
Full remote skip inventory retained: 3946 lines; /var/tmp/gc-heavy-gate/runs/ga-24p06a5.20261105-jf5mf1b3/c3/evidence/remote/skip-inventory.json; SHA256 6b52e1ec838aab284d3448e8985441df22d885369f470d605f568d1ed550b5c5.
Full native floor steps and outcomes retained: 909 lines; /var/tmp/gc-heavy-gate/runs/ga-24p06a5.20261105-jf5mf1b3/c3/evidence/remote/floor-topup.json; SHA256 f718417efcaeee6a7e8c9dba8623fe60e8de89a441de005b14db7dfcd3cf16bb.
Full by-name results retained: 12 lines; /var/tmp/gc-heavy-gate/runs/ga-24p06a5.20261105-jf5mf1b3/final-by-name.txt; SHA256 13cf25dd15710097015056bcab258d679c454f03dced8d7feeb199c0796f49dd.
Full diff ownership and shared hunks retained: 33 lines; /var/tmp/gc-heavy-gate/runs/ga-24p06a5.20261105-jf5mf1b3/owned-explain.txt; SHA256 6f32d56c7ee1fef98439b2479524598866b39507c637fb2ac5fc2856ece0317c.
Full skip-site proof retained: 27 lines; /var/tmp/gc-heavy-gate/runs/ga-24p06a5.20261105-jf5mf1b3/base-skip-proof/skip-sites.txt; SHA256 4310a663aa5d6d9dd6fed016ace8f35bd6672ce93deb2618ad38a77d3695edff.
Full pinned-base skip comparisons retained: 56 lines; /var/tmp/gc-heavy-gate/runs/ga-24p06a5.20261105-jf5mf1b3/base-skip-proof/final-farm-skip-comparison.txt; SHA256 903c3aab3ce29f598246fdac61b6235cf0aa24da266db51e257517d62e06dab4.
Full baseline skip proof provenance retained: 35 lines; /var/tmp/gc-heavy-gate/runs/ga-24p06a5.20261105-jf5mf1b3/base-skip-proof/SKIP_PROOF_VERIFIED.json; SHA256 86246865388d0a517e0e96e01b35aa780ae641a2b56efc0224922432f90a4fc0.
Full CI job accounting retained: 62 lines; /var/tmp/gc-heavy-gate/runs/ga-24p06a5.20261105-jf5mf1b3/ci-required/accounting.json; SHA256 6e40f138f4c4d85d030f42fa9e6a3f0e83350c4be4b952f4f043603901d7d65c.
Full C6 build verification retained: 17 lines; /var/tmp/gc-heavy-gate/runs/ga-24p06a5.20261105-jf5mf1b3/C6_VERIFIED.json; SHA256 16fff6e90737e6e70b3002945715082fdcba00177d9520bd5f8478fee6501f0d.
Full pre-test build outage evidence retained: 11 lines; /var/tmp/gc-heavy-gate/runs/ga-24p06a5.20261105-jf5mf1b3/c6-infrastructure-audit.json; SHA256 d53fa0fd44efb1b5a005b8c25547dc79f971db2d04c12251215928882d812483.
