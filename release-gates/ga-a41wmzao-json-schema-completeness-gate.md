**Verdict:** **PASS**

# JSON result schema completeness — release gate

Bead: `ga-a41wmzao`; review: `ga-69dfx80e`; confirmed build lineage: `ga-a3ypyj`, `ga-gkn43p2`.

Reviewed source: `6a4f237bec5568170ffa943438cd3a2687b9a891`. Tested merge: `306df60ecaf9045f4cfe9c6f8facb2623342a312`; tree: `d9845f530862af66a21f1589b1c8b33c84ed9d0c`.

gate_base: `bd8cf85d76d3f622f0d72b9005a996a878330468`

merge_state: clean (`origin/main@bd8cf85d76d3f622f0d72b9005a996a878330468`)

gate_base_kind: current-tip

merge_validation: exact merge result built and vetted here; merge-time checks remain the merge authority’s responsibility.

deploy_mode: remote; push_remote: fork; deploy branch: `deploy/ga-a41wmzao-gate`.

| # | Result | Evidence |
|---|---|---|

| 1 | PASS | Live closed review records PASS for the exact resolved reviewed source. No carryover or branch-tip substitution. |

| 2 | PASS | All JSON-declaring commands resolve valid result/failure schemas; 12 new result schemas are embedded. Four changed tests and eight contract/regression checks PASS by name. Existing example-city callers remain executable after test-directory isolation. Routed-test policy fix is an ancestor of the reviewed source. |

| 3 | PASS | Canonical full-suite chain is complete and valid: 156,297 PASS / 0 FAIL / 613 SKIP across 557 retained logs. All skips accounted for below; six required CI leaf jobs accounted for. |

| 4 | PASS | Zero unresolved high-severity review findings. Minor schema/output-test follow-ups are tracked in ga-8b86ec33. |

| 5 | PASS | Tested runtime tree is clean. The isolated branch carries the reviewed source plus this gate record; commit cleanliness is verified before push. |

| 6 | PASS | `bazel build --config=remote-exec --keep_going --noremote_local_fallback //...`, then the same command with `--output_groups=nogo_fix`, both exit 0 and BEP overallSuccess=true. |

| 7 | PASS | One feature: complete the CLI JSON schema catalog and align its output envelopes, including the test isolation needed to verify it. All six source commits cite the confirmed build lineage; ancestry and internal-path guards passed again before publication. |


## Test evidence

`test_cmd_scope: full-suite`; `test_exec_mode: remote-exec`; `heavy_mode: none`; `failure_attribution: none`; `waiver_ref: none` (gascity has no waiver path).

`test_cmd`: canonical `gate-criterion3.sh` on the exact merged tree, forced uncached remote phases followed by its computed native floor. Phase scopes: `make -k check`; acceptance test plus acceptance solo tests; `--config=integration //test:integration_packages`; full integration minus the registered latency tests; then only the registered latency tests under the farm probe.

Actual per-phase flags (targets/scopes above; complete launch receipt retained with the run):

```text
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-a41wmzao.nss6ogs1/c3-run/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-a41wmzao.nss6ogs1/c3-run/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-a41wmzao.nss6ogs1/c3-run/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-a41wmzao.nss6ogs1/c3-run/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-a41wmzao.nss6ogs1/c3-run/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```

All 541 BEP test results used remote execution, attempt 1, no local or remote test-result cache, no fallback, and valid phase windows. Per-phase counts: unit 415; acceptance 25; integration-packages 86; integration-full 14; latency 1. Farm test seconds: 14580.264.

`TestGcBdLatencyBudget` and `TestGcSeatHookLatencyBudget` each PASS in the measured latency lane under probe `clear`; neither ran in the main integration leg.

Changed tests, each PASS in both unit and integration-package phases:

- `TestGastownExamplePathResolvesAfterClearGCEnv`: PASS.
- `TestJSONResultSchemasRequireSuccessDiscriminator`: PASS.
- `TestJSONSchemasDeclaredForCommandsWithJSONOutput`: PASS.
- `TestSuccessDiscriminatorExemptionsAreNotStale`: PASS.


The eight additional contract names also PASS; complete by-name receipts are retained. The local floor completed all 16 steps, including pinned beads-module resolution, Darwin compilation and both runner self-tests. ICU74 check: OK. Rootless podman ping: OK. Pinned test bd: v1.3.4. The isolation wrapper reported pass-through for this rig; remote test actions were hermetic, and native floor commands scrubbed live-store selectors and pinned Git config and shell.


Compact execution record (full arrays retained by pointers):

```json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/deploy-ga-a41wmzao.nss6ogs1/c3-run/evidence/remote","admission":{"class":"remote","floor_gib":25.0,"free_gib":58.85831832885742,"waited_seconds":0.0,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"},"test_exec_bep":{"unit":{"results":415,"remote_executed":415,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":86,"remote_executed":86,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":14,"remote_executed":14,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":14580.264,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=5fd31cdd75b202be3ab9de14bb2514f059b7969d1f14ab7fdc392c2c723e6b7b"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":62.678,"evidence_errors":[],"probe":["GATE_FARM_PROBE verdict=clear waited=0s load1=13.21 load5=10.52 load15=6.74 cpus=32 threshold=50 max_wait=600s"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 16.05 11.49 7.22 14/1327 56766"]}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":38616}},"log_dir":"/var/tmp/deploy-ga-a41wmzao.nss6ogs1/c3-run/evidence/remote/floor-logs"},"skip_inventory":{"counts_by_reason_class":{"other":509,"environment-absence":90},"top_level_counts_by_reason_class":{"other":251,"environment-absence":43},"tests":{"elided_array":{"items":599,"bytes":214981}}},"test_cmd":{"elided_array":{"items":5,"bytes":2059}},"test_counts":{"PASS":156297,"FAIL":0,"SKIP":613},"diff_tests_executed":"diff_tests_executed:\n  TestGastownExamplePathResolvesAfterClearGCEnv PASS (integration-packages-bc482d2ae768251ad47f.log, unit-752a0bf25b15b465517a.log)\n  TestJSONResultSchemasRequireSuccessDiscriminator PASS (integration-packages-9326181a736eb30b71ca.log, unit-10cc7795c0ec199a5530.log)\n  TestJSONSchemasDeclaredForCommandsWithJSONOutput PASS (integration-packages-12697e2ea0d32c278115.log, unit-5a9df316125cd0aeff2e.log)\n  TestSuccessDiscriminatorExemptionsAreNotStale PASS (integration-packages-56f04454dd9158a9ee8a.log, unit-f679cd2551fcc64525aa.log)\nGATE_TEST_EVIDENCE_RESULT ok: 4 diff-owned tests verified by name\n"}
```


## Skip integrity

Inventories contain 599 remote and 72 native skip events (including subtests). Every event maps to an unchanged source location: 316 remote sites and 45 native sites. The only shared-helper hunks are in cmd/gc/init_from_hosted_dolt_test.go, cmd/gc/json_schema_test.go, and cmd/gc/testenv_test.go. Default/integration test-binary closure unions for the other skipped packages exclude cmd/gc (162/164 module directories checked). The schema assets and three JSON writers do not control the recorded OS, privilege, missing-tool, opt-in, or storage-fixture skip predicates.

All CLI skips were measured independently at the pinned gate base with matching job settings: 66 integration-package, 4 sim-exec, 187 unit, and 36 native events, all pre-existing with identical reasons; zero new, changed-reason, diff-owned, or reasonless skips. This includes main’s TestRalphFinalizeOuterExhaustsInnerPasses. Named base measurements are focused evidence for skips and are excluded from the full-suite test counts.

integration-packages //cmd/gc:gc_test: COMPARE-SUMMARY head_skips=66 judged=66 pre-existing=66 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
COMPARE-NOTE the remote worker's account differs between the runs in 1 SKIP(s), read as the same reason because the account is the only difference (an account is rbe-, a letter and two digits, standing alone; the logs are not changed): TestInfraScopedManagedDoltPIDIdentityUnderRealSystemd (base rbe-a11, head rbe-a12)
unit //cmd/gc:gc_sim_exec_test: COMPARE-SUMMARY head_skips=4 judged=4 pre-existing=4 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
unit //cmd/gc:gc_test: COMPARE-SUMMARY head_skips=187 judged=187 pre-existing=187 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING


One initial remote comparison was refused before any test ran because its evidence parent directory was not mode 0700. The permissions were corrected and the refusal log retained; all actual base executions were uncached first attempts. The canonical suite was run once.


## Fast lane and load

Fast lane completed 14:46:23Z, before criterion-3 launch at 14:47:02Z. Policy/format: shell guards plus 14 Bazel policy targets, rc 0. Generated artifact drift: seven Bazel targets, rc 0. OpenAPI breaking check: rc 0. `make bazel-sync BAZEL_SYNC_FLAGS=--config=remote-exec` plus `git diff --exit-code`: rc 0. Each lane used its own fresh view and the pinned gate base. Known-red precheck: proceed, routed-test guard green.

Observed native-floor load (bounded wait timed out; every floor command then passed):

```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1800 wait_timed_out=1 run_start_load=37.49 run_max_load=36.04 run_mean_load=32.03 run_readings=7 wait_first_load=74.64 wait_max_load=77.28 wait_mean_load=56.16 wait_readings=61 read_errors=0
```

Observed native base-comparison load:

```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1470 wait_timed_out=0 run_start_load=14.23 run_max_load=14.62 run_mean_load=14.41 run_readings=2 wait_first_load=25.01 wait_max_load=46.77 wait_mean_load=25.62 wait_readings=50 read_errors=0
```

Observed own CI compile-job load:

```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=811 wait_timed_out=0 run_start_load=14.90 run_max_load=18.62 run_mean_load=17.17 run_readings=4 wait_first_load=29.09 wait_max_load=46.78 wait_mean_load=29.41 wait_readings=28 read_errors=0
```


## Required-job accounting

ci_required_closure: `python3 ci_required_triggers.py <tested-merge> bd8cf85d76d3f622f0d72b9005a996a878330468 6a4f237bec5568170ffa943438cd3a2687b9a891 fork`; merged workflow blob `fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612`; 6 LEAF jobs need a status. Filter result: packs/credential_provider/shared no match; suite_mode filtered.

Raw closure and path-filter result:

```text
diff bd8cf85d76d3f622f0d72b9005a996a878330468...6a4f237bec5568170ffa943438cd3a2687b9a891: 18 changed files
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

| Job | Status | Evidence |
|---|---|---|

| runner-policy | LOCAL-PASS | EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=file python3 .github/workflows/scripts/runner_policy.py; rc 0; `/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-a41wmzao/gate-20261118T152828Z-s19mkt2s/ci-required/runner-policy.log` |

| changes | LOCAL-PASS | Closure command and matcher result; `/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-a41wmzao/gate-20261118T152828Z-s19mkt2s/ci-required/closure.txt` |

| credential-provider-windows | NOT-TRIGGERED | `if: needs.changes.outputs.credential_provider == 'true'`; credential_provider and shared filters no match. |

| pack-gate | NOT-TRIGGERED | `if: needs.changes.outputs.packs == 'true'`; packs and shared filters no match. |

| windows-lane | LOCAL-PASS | Exact job build/test-compile commands below, rc 0; `/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-a41wmzao/gate-20261118T152828Z-s19mkt2s/ci-required/windows-lane-terminal.stdout` and `.stderr`. |

| windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | Resolved runner blacksmith-4vcpu-windows-2025. Its own GOOS=windows test-binary closure over the three allowlisted package sets has no changed inputs. |


windows_reach: windows-lane-test none (own closure computed, not the credential-only helper).

deferred_to_pr_ci: windows-lane-test (non-linux-runner).

not_covered: none.

ci_lane_run: n/a (no CI-config change in this diff).

Exact windows-lane commands; test invocations compile without executing foreign binaries, as in CI:

```bash
#!/usr/bin/env bash
set -euo pipefail
# Build the Windows allowlist
GOOS=windows go build $(cat .github/windows-lane/build.txt)
# Compile the Windows allowlist's tests
GOOS=windows go test -count=1 -vet=off -run '^$' -exec /bin/true $(cat .github/windows-lane/test-build.txt)
# Build the nogo analyzers and their tests for Windows
(
cd tools/nogo
GOOS=windows go build ./analyzers/... && GOOS=windows go test -count=1 -vet=off -run '^$' -exec /bin/true ./analyzers/...
)
# Build every package and its tests for macOS
GOOS=darwin go build ./... && GOOS=darwin go test -count=1 -vet=off -run '^$' -exec /bin/true ./...
```


This PASS authorizes PR opening. Merge requires CI / required green on the exact PR head and the deferred native Windows job successful there.


## Retained evidence

Durable evidence mirror: `/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-a41wmzao/gate-20261118T152828Z-s19mkt2s`. Original run root: `/var/tmp/deploy-ga-a41wmzao.nss6ogs1`. The tested merge is preserved in a verified Git bundle. Raw inventories, site proofs, per-job logs and full execution data are kept outside this committed record.


Full execution.json retained: 5999 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-a41wmzao/gate-20261118T152828Z-s19mkt2s/c3-run/evidence/execution.json; SHA256 9e287facde70d1fc5f9f607b626315c965d94d067af4c177b235ceb66cedd4d4.


Full remote skip inventory retained: 4794 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-a41wmzao/gate-20261118T152828Z-s19mkt2s/c3-run/evidence/remote/skip-inventory.json; SHA256 f0110f3f1c1d02b64ee5a1d6adc23deb73eb6fa97a0129d92fd5606ddb166525.


Full native skip inventory retained: 578 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-a41wmzao/gate-20261118T152828Z-s19mkt2s/native-skip-inventory.json; SHA256 8e827733a790802879a7408781afed62e58f814cd9cbdc452f75531e69bd1651.


Full remote skip-site proofs retained: 1898 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-a41wmzao/gate-20261118T152828Z-s19mkt2s/skip-site-results.json; SHA256 57984c4900f2a10b4ad02f3d4f0a7417f3e256a985acc65c267ec136b8b7d585.


Full native skip-site proofs retained: 272 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-a41wmzao/gate-20261118T152828Z-s19mkt2s/native-skip-site-results.json; SHA256 3cb44fb048a62597718ad397e3011157087efb4aaaafbf22db37136032c5c7a7.


Full native skip comparison retained: 37 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-a41wmzao/gate-20261118T152828Z-s19mkt2s/native-base/comparison.stdout; SHA256 20718ca174b4326e6dd20863f0f9837d3b040471accf063729bc34d9f9935a52.


Full floor steps retained: 1056 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-a41wmzao/gate-20261118T152828Z-s19mkt2s/c3-run/evidence/remote/floor-topup.json; SHA256 23fd44b168f52da589c7dfd29497da50f177488f19043ffdacd84e8b45cbb73c.


Full required-job accounting retained: 50 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-a41wmzao/gate-20261118T152828Z-s19mkt2s/ci-required/accounting.json; SHA256 56f99e9b63fe7f05e9cd5d38feb4330be6fbc9f9be3bdc4f64ed0c0c04ec1c2c.


Full Windows reach proof retained: 36 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-a41wmzao/gate-20261118T152828Z-s19mkt2s/ci-required/windows-lane-test-reach.json; SHA256 6c9388d1f0e6071b4afc41133e4a9c84147efacd9d7435992ebc39572e94276d.


Full core log manifest retained: 1940 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-a41wmzao/gate-20261118T152828Z-s19mkt2s/SHA256SUMS_CORE; SHA256 925beb349d26ffd700660dc1be912a149bc2bcab8f670906aae25c728f164ae2.
