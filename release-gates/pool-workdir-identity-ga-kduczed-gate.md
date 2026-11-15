# Release gate: preserve pool work-directory ownership

**Verdict:** **PASS**

- Bead: ga-kduczed; build: ga-pkdvjn.1; review: ga-y9guymn (verdict: pass).
- Evaluated: 2026-11-02T01:15:15.071292+00:00
- deploy_mode: remote; base_ref: origin/main; push_remote: fork.
- reviewed_commit: 68983936bf177647c1c8a66a5cbed6fba15b5a15
- gated_merge_head: 240048cd65abe27d4bc2398bdc78a6d26fa19f96
- gated_merge_tree: 19535580199fbd844283d733bbfde4692a855c64
- merge_state: clean (origin/main@d5261665dcd8f12e8a5a7ed4fdf9343f849f8c37)
- gate_base_kind: current-tip
- gate_base: d5261665dcd8f12e8a5a7ed4fdf9343f849f8c37
- merge_validation: the canonical merge result was built, vetted, and tested here; the merge authority validates the actual merge-time result.

| Criterion | Result | Evidence |
|---|---|---|
| 1 Review PASS | PASS | Review ga-y9guymn names the resolved reviewed commit. No carryover or reviewer-added test repin is needed. |
| 2 Acceptance criteria | PASS | Both stamping sites use isPoolManagedSessionInfo. Both pool-slot-only regression tests passed. Existing pool-managed and non-pool task/root stamping tests passed by name; see acceptance-by-name.txt. |
| 3 Tests and required CI coverage | PASS | Five full-scope farm phases passed with 514 first-attempt, uncached remote test results. All 16 local-floor steps passed. Latency was measured under a clear probe. All four CI leaf jobs are accounted for below. |
| 4 High-severity review findings | PASS | Zero unresolved blocker/major/high findings in review ga-y9guymn. |
| 5 Clean branch | PASS | git status --porcelain was empty before this gate record was written; the record is the sole planned addition. Post-commit cleanliness is verified in the bead notes. |
| 6 Merge validation | PASS | Clean merge onto the pinned base; predicted-tree equality; bazel build --config=remote-exec --noremote_local_fallback //... passed (1,448 targets; 160.147 s). |
| 7 Single feature theme | PASS | Two files, +56/-2: pool work-directory stamping and its regression tests. Ancestry scope check passed for deploy ga-kduczed and source build ga-pkdvjn.1; no stacked PR. |

## Criterion 3 execution

- test_cmd_scope: full-suite
- test_exec_mode: remote-exec
- tests_green: true
- test_counts: PASS=140109 FAIL=0 SKIP=481 (top-level terminal result lines summed across retained job logs; repeated configurations count separately).
- test_bd: isolated-test-run:   pinned test bd (gm-i3csn7): GASCITY_TEST_BD version=v1.3.2 bin=/home/jaword/.local/bd-versions/v1.3.2/bd reports='bd version 1.3.2 (9b65b9e19: HEAD@9b65b9e198ac)' ref_check=match:9b65b9e19 origin=preexisting pin_source=/var/tmp/gc-merge-validate.CBAiNc/deps.env displaces='bd version 1.3.2 (9b65b9e19)'
- criterion3_city_evidence_commit: df3ee8ace2f4836850221b9cc690d363f8b52832
- test_log_dir: /var/tmp/deploy-ga-kduczed.5foGu3wg/c3-run/evidence/remote/logs
- heavy_mode: none; no composite required.
- fallback_reason: none; no farm/local suite stitching.
- farm_test_seconds: 11017.838
- owned_topup: [] (both diff-owned tests passed in unit and integration-packages).
- floor_topup: 16/16 passed; canonical environment-absence top-ups, beadstest, Darwin compile, and both runner self-tests. Native commands are the unmodified mandatory floor, separate from the all-Bazel suite.
- latency_leg: measured; complete=true; attempt=1; both registered tests PASS. Probe cleared after 50 s, load1=47.80, threshold=50. No latency test ran in integration-full.
- waiver_ref: none (Gas City has no waiver path).
- failure_attribution: none; zero failures.
- ci_lane_run: n/a (no CI configuration changed).

diff_tests_executed:
```text
diff_tests_executed:
  TestStampRunSessionIdentityPoolSlotOnlySessionDoesNotManufactureWorktreeEvidence PASS (integration-packages-75f7330b9b673df65e4f.log, unit-f679cd2551fcc64525aa.log)
  TestStampRunSessionIdentityPoolSlotOnlySessionDoesNotStampRunRootWorkDir PASS (integration-packages-cb110414c557c0928e82.log, unit-23e7d9fc763496dd8095.log)
GATE_TEST_EVIDENCE_RESULT ok: 2 diff-owned tests verified by name
```

### Exact suite commands and flags

The detached command was gate-criterion3.sh --repo /var/tmp/gc-merge-validate.CBAiNc --base d5261665dcd8f12e8a5a7ed4fdf9343f849f8c37 --bead-json /var/tmp/deploy-ga-kduczed.5foGu3wg/bead.json --owned /var/tmp/deploy-ga-kduczed.5foGu3wg/owned.tsv --run-dir /var/tmp/deploy-ga-kduczed.5foGu3wg/c3-run/evidence -- python3 /var/tmp/deploy-ga-kduczed.5foGu3wg/local-bazel-transport.py. Selection and BEP both resolved to remote-exec; the local fallback command was never used.

```text
unit: make check
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-kduczed.5foGu3wg/c3-run/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
acceptance: bazel test $BAZEL_FLAGS --config=acceptance //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-kduczed.5foGu3wg/c3-run/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
integration-packages: bazel test $BAZEL_FLAGS --config=integration //test:integration_packages
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-kduczed.5foGu3wg/c3-run/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
integration-full: bazel test $BAZEL_FLAGS --config=integration --test_arg=-test.v --test_arg=-test.skip=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$ //test/integration:integration_test
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-kduczed.5foGu3wg/c3-run/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
integration-latency: bazel test $BAZEL_FLAGS --config=integration --test_arg=-test.v --test_filter=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$ --test_sharding_strategy=disabled --run_under=<gate-farm-probe.sh threshold=50 max_wait=600 poll=10; text SHA256 in latency-leg.json> //test/integration:integration_test
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-kduczed.5foGu3wg/c3-run/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```

test_exec_bep:
```json
{
  "unit": {
    "results": 389,
    "remote_executed": 389,
    "cached_remotely": 0,
    "cached_locally": 0,
    "other_strategy": 0,
    "retried": 0,
    "window_ok": true
  },
  "acceptance": {
    "results": 24,
    "remote_executed": 24,
    "cached_remotely": 0,
    "cached_locally": 0,
    "other_strategy": 0,
    "retried": 0,
    "window_ok": true
  },
  "integration-packages": {
    "results": 86,
    "remote_executed": 86,
    "cached_remotely": 0,
    "cached_locally": 0,
    "other_strategy": 0,
    "retried": 0,
    "window_ok": true
  },
  "integration-full": {
    "results": 14,
    "remote_executed": 14,
    "cached_remotely": 0,
    "cached_locally": 0,
    "other_strategy": 0,
    "retried": 0,
    "window_ok": true
  },
  "integration-latency": {
    "results": 1,
    "remote_executed": 1,
    "cached_remotely": 0,
    "cached_locally": 0,
    "other_strategy": 0,
    "retried": 0,
    "window_ok": true
  }
}
```

rbe_state:
```json
{
  "rc_marker_present": true,
  "kill_switch_absent": true,
  "cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT",
  "worker_env_tree": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98",
  "worker_env_origin_main": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"
}
```

admission:
```json
{
  "class": "remote",
  "floor_gib": 25.0,
  "free_gib": 78.5018539428711,
  "waited_seconds": 0.0,
  "cap": 2
}
```

### Load observations

Remote admission used its 25 GiB floor and cap 2, with no heavy-slot or host-load wait. The outer wrapper still sampled the client host. The native floor and runner-policy job retained their separate local waits.

Remote client monitor:
```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=14.19 run_max_load=13.94 run_mean_load=7.81 run_readings=44 wait_first_load=14.19 wait_max_load=14.19 wait_mean_load=14.19 wait_readings=1 read_errors=0
```

Native floor:
```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=5.13 run_max_load=7.43 run_mean_load=6.62 run_readings=6 wait_first_load=5.13 wait_max_load=5.13 wait_mean_load=5.13 wait_readings=1 read_errors=0
```

Runner policy:
```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=7.05 run_max_load=NA run_mean_load=NA run_readings=0 wait_first_load=7.05 wait_max_load=7.05 wait_mean_load=7.05 wait_readings=1 read_errors=0
```

load_threshold: 15 (local); load_waited_seconds: 0; load_wait_timed_out: 0. Native-floor run_start_load=5.13, run_max_load=7.43, run_mean_load=6.62, run_readings=6.

### Skips

skip_inventory: 506 farm/floor entries including subtests: 406 other and 100 environment-absence entries. This differs from the top-level SKIP result-line count because inventory includes subtests and the suite repeats configurations. All environment-absence selections ran in the local floor; every floor command returned 0. Non-owned residual skips remain inventoried.

skip_justification: existing platform restrictions, explicit unit/process lane switches, provider conformance outside declared scope or awaiting its existing implementation, missing optional live fixtures/tools/credentials, and explicit baseline contract skips. These conditions and skip sites are unchanged by this two-line production predicate fix. The only changed test bodies are the two passing pool-slot tests; no shared/helper test hunk exists. A conservative source-site audit checked all 293 possible source locations for the 506 entries and every site returned UNCHANGED. Neither owned test has a SKIP. Full reasons and source-site proofs are in the hashed artifacts below.

## Fast lane

- fast_lane: PASS before suite launch; merge_head=240048cd65abe27d4bc2398bdc78a6d26fa19f96
- policy_lane: fresh-tree-run.sh -- gate-base.sh run -- policy.sh: diff whitespace check; pinned make fmt-check; make lint using nogo; release-dist-ignore, routed-test-rows, split-topology-rows, residency-boundary, and check-hooks all passed.
- drift_lane: fresh-tree-run.sh -- gate-base.sh run -- drift.sh: make bazel-sync followed by git diff --exit-code passed.
- Additional required policy/generated/OpenAPI targets: 19/19 passed, verified from BEP summaries. The suite itself used no cached test result.

```text
FRESH_TREE_RECORD from=/var/tmp/gc-merge-validate.CBAiNc head=240048cd65ab tree=19535580199f dir=/var/tmp/gc-fresh-tree.6zsfA2 modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-gc-merge-validate.CBAiNc,bazel-out,bazel-testlogs
GATE_BASE_RECORD base=d5261665dcd8f12e8a5a7ed4fdf9343f849f8c37 knobs=CI_BEADS_DIFF_BASE,BASE_SHA,COMPLEXITY_BASE_REF,LINT_BASE dir=/var/tmp/gc-fresh-tree.6zsfA2
GLT_RECORD: {"golangci_lint":{"pin":"2.14.0","resolved":"2.14.0","mismatch":false}}
FRESH_TREE_RECORD from=/var/tmp/gc-merge-validate.CBAiNc head=240048cd65ab tree=19535580199f dir=/var/tmp/gc-fresh-tree.OnbEOq modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-gc-merge-validate.CBAiNc,bazel-out,bazel-testlogs
GATE_BASE_RECORD base=d5261665dcd8f12e8a5a7ed4fdf9343f849f8c37 knobs=CI_BEADS_DIFF_BASE,BASE_SHA,COMPLEXITY_BASE_REF,LINT_BASE dir=/var/tmp/gc-fresh-tree.OnbEOq
FRESH_TREE_RECORD from=/var/tmp/gc-merge-validate.CBAiNc head=240048cd65ab tree=19535580199f dir=/var/tmp/gc-fresh-tree.V7VS6A modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-gc-merge-validate.CBAiNc,bazel-out,bazel-testlogs
GATE_BASE_RECORD base=d5261665dcd8f12e8a5a7ed4fdf9343f849f8c37 knobs=CI_BEADS_DIFF_BASE,BASE_SHA,COMPLEXITY_BASE_REF,LINT_BASE dir=/var/tmp/gc-fresh-tree.V7VS6A
FRESH_TREE_RECORD from=/var/tmp/gc-merge-validate.CBAiNc head=240048cd65ab tree=19535580199f dir=/var/tmp/gc-fresh-tree.eDq7ko modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-gc-merge-validate.CBAiNc,bazel-out,bazel-testlogs
GATE_BASE_RECORD base=d5261665dcd8f12e8a5a7ed4fdf9343f849f8c37 knobs=CI_BEADS_DIFF_BASE,BASE_SHA,COMPLEXITY_BASE_REF,LINT_BASE dir=/var/tmp/gc-fresh-tree.eDq7ko
FRESH_TREE_RECORD from=/var/tmp/gc-merge-validate.CBAiNc head=240048cd65ab tree=19535580199f dir=/var/tmp/gc-fresh-tree.S2T3Bs modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-gc-merge-validate.CBAiNc,bazel-out,bazel-testlogs
GATE_BASE_RECORD base=d5261665dcd8f12e8a5a7ed4fdf9343f849f8c37 knobs=CI_BEADS_DIFF_BASE,BASE_SHA,COMPLEXITY_BASE_REF,LINT_BASE dir=/var/tmp/gc-fresh-tree.S2T3Bs
```

## CI dependency accounting

ci_required_closure: python3 ci_required_triggers.py /var/tmp/gc-merge-validate.CBAiNc d5261665dcd8f12e8a5a7ed4fdf9343f849f8c37 68983936bf177647c1c8a66a5cbed6fba15b5a15 fork; workflow blob e5b11c348505dab28dc223e874bc2c61cbbb33c5; 4 LEAF jobs need a status.

| Job | Status | Evidence |
|---|---|---|
| changes | LOCAL-PASS | ci_required_triggers.py returned 0; packs, credential_provider and shared filters did not match. ci-closure.txt contains the tool output. |
| runner-policy | LOCAL-PASS | EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=<retained file> python3 .github/workflows/scripts/runner_policy.py; exit 0, through fresh-tree-run.sh, load-gate-run.sh and isolated-test-run.sh, detached. runner-policy-output.txt and runner-policy.env retained. |
| credential-provider-windows | NOT-TRIGGERED | Tool row: NON-LINUX; needs.changes.outputs.credential_provider == true did not match. |
| pack-gate | NOT-TRIGGERED | Tool row: needs.changes.outputs.packs == true did not match. |

- windows_reach: none; windows_reach.sh exit 0 with no changed paths in its output.
- deferred_to_pr_ci: none
- not_covered: none
- This gate authorizes PR opening. Merge authority remains MPR; the PR head must satisfy its required CI before merging.

## Retained evidence

Bulky inventories, BEP records and raw logs stay outside this record. Each entry below gives path, SHA256, line count and byte count.

| Artifact | SHA256 | Lines | Bytes |
|---|---|---:|---:|
| /var/tmp/deploy-ga-kduczed.5foGu3wg/c3-run/evidence/execution.json | 21dd7d613c42dbb83c1dabdf3197e66c06c59c2814c357308a4d8ecd982b9f1a | 4789 | 249452 |
| /var/tmp/deploy-ga-kduczed.5foGu3wg/c3-run/evidence/remote/skip-inventory.json | 8435d8204d825265a1916da49c00edfdd64f782af25032220a6ca312027a1b13 | 3738 | 186282 |
| /var/tmp/deploy-ga-kduczed.5foGu3wg/skip-inventory-all.json | 3f8a0618940a8e6f37cea26d3701e97500993bcda05b9e055b6d3157f8ac26aa | 4050 | 201181 |
| /var/tmp/deploy-ga-kduczed.5foGu3wg/skip-site-proof.json | 085141c4b237a61e3ef5d5fa28c172f2cb6a08e609c52da76a77b0848d5ea144 | 4906 | 277234 |
| /var/tmp/deploy-ga-kduczed.5foGu3wg/c3-run/evidence/remote/floor-topup.json | ecad25a90c190e9c322f1e7dcc6d46a36c6efd2d770cb68b90f81d5592278b40 | 901 | 39786 |
| /var/tmp/deploy-ga-kduczed.5foGu3wg/c3-run/evidence/remote/latency-leg.json | d9a479cb4d314002ceb414194c023173ffadbcb9d70e658684158ceb90f53a52 | 56 | 1813 |
| /var/tmp/deploy-ga-kduczed.5foGu3wg/c3-run/evidence/remote/owned-topup.json | 37517e5f3dc66819f61f5a7bb8ace1921282415f10551d2defa5c3eb0985b570 | 1 | 3 |
| /var/tmp/deploy-ga-kduczed.5foGu3wg/acceptance-by-name.txt | 73253349394b7949d49f91fc66f0fffd4f6ca5d216aace43907384b585bb1a28 | 11 | 1482 |
| /var/tmp/deploy-ga-kduczed.5foGu3wg/diff-owned-explain.txt | a04b5cf53ad8f1be543aa581ce65ffa3b7d7c26ef0869db7cba7fe62532f9694 | 20 | 2137 |
| /var/tmp/deploy-ga-kduczed.5foGu3wg/fast-verified.json | 850fe59eb0f86e05a0091f034d2019c9611501be0de6ee1ca6ea19b8a83059af | 44 | 1905 |
| /var/tmp/deploy-ga-kduczed.5foGu3wg/ci-closure.txt | 456200021576e5270893e518c344ececf18409358d7b414c8f4a33dcbdf2f0f9 | 15 | 1070 |
| /var/tmp/deploy-ga-kduczed.5foGu3wg/runner-policy-output.txt | 914d941beae352719d1ae5fe7d2a7471460c04ff6b8383d8609beb06c35e61c8 | 11 | 1930 |
| /var/tmp/deploy-ga-kduczed.5foGu3wg/build-nogo.log | 6a7b8aa2691fd7355a06784c05b472eda212d52970af09773063d0d4241f7da9 | 115 | 18665 |
| /var/tmp/deploy-ga-kduczed.5foGu3wg/icu74.log | dc1ab2e8ccf94bcf9a0c827a085e548134cafef5ec8378acc25ae4c948c21196 | 4 | 616 |
| /var/tmp/deploy-ga-kduczed.5foGu3wg/c3-run/evidence/remote/unit.bep.json | 826e18535082dc849a0d1addc65aadb3eb58fe8871b02e0f1675f6edd6e96225 | 10678 | 12226516 |
| /var/tmp/deploy-ga-kduczed.5foGu3wg/c3-run/evidence/remote/unit.evidence.json | f16fb16175b9f6545c48d6d493cca009dcbdf4b4659ab9027eda2202b20ec9ca | 16 | 324 |
| /var/tmp/deploy-ga-kduczed.5foGu3wg/c3-run/evidence/remote/unit-artifacts.json | 9724d4aaf2bfce8386b5934ad50235fb03a63283afaf5042a6a8f0145f6ecbd2 | 10894 | 463943 |
| /var/tmp/deploy-ga-kduczed.5foGu3wg/c3-run/evidence/remote/acceptance.bep.json | 1a20d3fd5f95b17e10e70653c5bccbb5d49dd75627488acce1d5a0fa01b41177 | 78 | 180749 |
| /var/tmp/deploy-ga-kduczed.5foGu3wg/c3-run/evidence/remote/acceptance.evidence.json | 7e83768beb6547f8c060d2e08ca3180693e3431dbe7007c1abc1dd71600f6593 | 16 | 322 |
| /var/tmp/deploy-ga-kduczed.5foGu3wg/c3-run/evidence/remote/acceptance-artifacts.json | d7c5e19a9b49957d3b741954ca39c8f2008f36ad98ad898e2f1a2b5d87f7d424 | 674 | 28988 |
| /var/tmp/deploy-ga-kduczed.5foGu3wg/c3-run/evidence/remote/integration-packages.bep.json | 5ecd5ccc22eccf102f3a0c6b6242e343b9f4c809a93b19a215cdc4409910f357 | 290 | 339452 |
| /var/tmp/deploy-ga-kduczed.5foGu3wg/c3-run/evidence/remote/integration-packages.evidence.json | 0b6c4baee29669df9d1d3e721ddbf386e5e055c4ffe62cd4aafea9caaaa252a9 | 16 | 323 |
| /var/tmp/deploy-ga-kduczed.5foGu3wg/c3-run/evidence/remote/integration-packages-artifacts.json | 57bfd457d8b61a28ab0d9f44b9ea88302a886062e80cc6b03dbdd57d833f7e42 | 2410 | 104407 |
| /var/tmp/deploy-ga-kduczed.5foGu3wg/c3-run/evidence/remote/integration-full.bep.json | eb80c6cb4c11baa383b83c4d496b6fd582eeaa83a00bba600a01fa532a0344aa | 38 | 115248 |
| /var/tmp/deploy-ga-kduczed.5foGu3wg/c3-run/evidence/remote/integration-full.evidence.json | 1f14b00ac3bcf7fbb3251df903c92e71dcb5c64c07ea7ce500af56bcaecd9a15 | 16 | 323 |
| /var/tmp/deploy-ga-kduczed.5foGu3wg/c3-run/evidence/remote/integration-full-artifacts.json | d5fd5608e2ac4c3ffad0a5e71e17e2af43ba4bee1cb16dce9feba0d148497485 | 394 | 17011 |
| /var/tmp/deploy-ga-kduczed.5foGu3wg/c3-run/evidence/remote/integration-latency.bep.json | c308088257e3f380aca22a05a75dc48a7cb76782e49015184bffd8308b6ac885 | 25 | 127363 |
| /var/tmp/deploy-ga-kduczed.5foGu3wg/c3-run/evidence/remote/integration-latency.evidence.json | bf1616cd15555328c2b731727828dd8478b4c8d8914b895d77bdaea52632dd05 | 16 | 319 |
| /var/tmp/deploy-ga-kduczed.5foGu3wg/c3-run/evidence/remote/integration-latency-artifacts.json | 534ea2c1b0fb5051616c375acdee9832e591833033c967ccf099383acd62daf8 | 30 | 1222 |

| /var/tmp/deploy-ga-kduczed.5foGu3wg/owned.tsv | 3c83acf340514cbec02b05b62e94e82e59b2452d1f0ab8e6aca5ba37c679bc8d | 2 | 168 |

| /var/tmp/deploy-ga-kduczed.5foGu3wg/acceptance-names.tsv | e2a78c7b03ec692c17d4a12840226ec560820580212508f155fc1f2a36d8775b | 5 | 354 |

| /var/tmp/deploy-ga-kduczed.5foGu3wg/c3-output.txt | 61d848ae5fa4c0d59042f3a94b959ba3b563f48ff26dd3a3300fb64bda3689cf | 13 | 211930 |

ICU 74 loader probe: OK, all three libraries match recorded provenance. Per-phase artifact manifests retain original CAS authority and verified raw-log digests. The deployer notifier confirmed service completion; all test logs are retained.
