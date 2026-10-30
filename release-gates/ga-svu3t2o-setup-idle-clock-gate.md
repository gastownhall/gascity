# Setup idle-clock release gate — ga-svu3t2o

**Verdict:** **PASS**

## Coordinates and scope

- Reviewed source: `7ec3e645df8ab30ac9b96223cf60ef1bdef331bf`; review ga-1j20n2g explicitly PASS on this source; build ga-autw1v7.
- Provenance only: builder/ga-19kjfbd. Deploy branch: deploy/ga-svu3t2o-gate, cut from the exact reviewed source before the gate record is committed. No builder branch is a push target.
- Remote mode, gastownhall/gascity. Frozen origin/main: `997378712049631c349bfc982f4f3d0f347277b9`.
- merge_state: clean (origin/main@997378712049631c349bfc982f4f3d0f347277b9).
- gate_base_kind: current-tip; gate_base_sha: `997378712049631c349bfc982f4f3d0f347277b9`.
- Synthetic gated head: `6bec760eca00118eee8d82cd92e3176f3da1129f`; tree: `9188db40bb6f04559a6174ea394efa378e54fecf`. Normal merge hooks ran; actual tree equals the read-only prediction.
- merge_validation: the clean merged tree built, vetted and passed the fresh gate. No source rebase, reset or conflict resolution was performed by the deployer.
- Evidence root: `/var/tmp/deploy-ga-svu3t2o.s6t5r6ny`. Original earlier gate evidence and reviewer test results are inputs only and carry no gate credit here.

## Criteria

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | ga-1j20n2g independently reviewed the exact source. No review carryover or reviewer-added test pin is needed. All consumed SHAs resolve to real commits. |
| 2 | Acceptance criteria met | PASS | Source inspection and three changed tests cover the injected streaming clock, silent-command cancellation and real-clock default. They PASS by name in this fresh gate. The original scheduling stall was not reproduced; the builder/reviewer classify the old test timing from contract analysis. |
| 3 | Tests pass | PASS | All five canonical farm phases execute fresh, with no cached verdict, retry, failed result or missing changed test. Native floor and SKIP audit complete. Details below. |
| 4 | No open HIGH review finding | PASS | Reviewer style/security report zero blockers or majors. Known non-blocking findings and residual test-clock window remain disclosed. |
| 5 | Final branch clean | PASS | Gated scratch clean after tests; isolated deploy branch contains reviewed source plus only this committed record. Clean status verified after commit. |
| 6 | Merge state, build and vet | PASS | Current-tip clean merge above; complete Bazel build //... and nogo/vet PASS, normal hooks active. Required prior fixes are ancestors of source and frozen main. |
| 7 | One feature theme | PASS | One idle-monitor clock seam and deterministic setup regression/control tests; seven source files, +350/-33. Ancestry scope guard PASS. |

## Build and fast lane

Build/vet: complete `bazel build //...` and `make vet`, remote execution with local fallback disabled. Logs: build.log, vet.log, merge-commit.log and c6-run/.

fast_lane: PASS before full-suite launch. Each lane used a separate fresh-tree view and the canonical gate-base adapter, so generation never changed the suite scratch.

- policy_lane: PASS — pinned golangci-lint 2.14.0 formatter, full nogo/lint, shell guards, and all ten explicit policy targets. Twenty original log/XML artifacts retained and rehashed.
- drift_lane: PASS — `make bazel-sync`, `git diff --exit-code` and empty tracked/untracked status. Original Gazelle warnings about absent generated embed assets are retained; sync exited zero and changed no file.
- generated_lane: PASS — seven schema/spec/client/dashboard/lockfile checks; fourteen original log/XML artifacts retained and rehashed.
- ci_lane_run: n/a (no CI job, matrix, timeout or required-check configuration changed).
- heavy_mode: none. No composite or coverage exception used.

## Fresh criterion 3

test_cmd_scope: full-suite.
test_exec_mode: remote-exec, derived from BEP; native checks are the required supplemental floor.
waiver_ref: none. failure_attribution: none.

Canonical gate-criterion3.sh ran under gate-detached-run.sh as a systemd service on the frozen merged tree. Each farm invocation forces no cached results and no local fallback. Actual commands:

```bash
make check
bazel test $BAZEL_FLAGS --config=acceptance //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests
bazel test $BAZEL_FLAGS --config=integration //test:integration_packages
bazel test $BAZEL_FLAGS --config=integration --test_arg=-test.v "--test_arg=-test.skip=$latency_re" //test/integration:integration_test
bazel test $BAZEL_FLAGS --config=integration --test_arg=-test.v "--test_filter=$latency_re" --test_sharding_strategy=disabled "--run_under=$probe" //test/integration:integration_test
```

These are the canonical phase cases, not a selected package subset. Per-phase flag expansions follow. The latency expression is read from gate-latency-tests.txt; the worker probe text and digest, parameters and clear verdict are retained in latency-leg.json and integration-latency.stdout.

```text
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-svu3t2o.s6t5r6ny/c3-run/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-svu3t2o.s6t5r6ny/c3-run/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-svu3t2o.s6t5r6ny/c3-run/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-svu3t2o.s6t5r6ny/c3-run/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-svu3t2o.s6t5r6ny/c3-run/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```

| Phase | Result | BEP results | Remote executions | Cached local / cached remote / retry |
|---|---|---:|---:|---|
| unit | PASS | 375 | 375 | 0 / 0 / 0 |
| acceptance | PASS | 24 | 24 | 0 / 0 / 0 |
| integration-packages | PASS | 85 | 85 | 0 / 0 / 0 |
| integration-full | PASS | 14 | 14 | 0 / 0 / 0 |
| integration-latency | PASS | 1 | 1 | 0 / 0 / 0 |

All results are terminal PASS, attempt 1, with starts inside their phase window; other strategy = 0 and transport_failure = false. The latency tests are absent from the main integration leg and measured only in the separate clear-probe lane. Both latency tests PASS on its first attempt; the budget is unchanged.

test_counts: {"PASS": 133721, "FAIL": 0, "SKIP": 375}. These are result lines summed over retained job logs, not distinct tests or targets.
test_log_dir: `/var/tmp/deploy-ga-svu3t2o.s6t5r6ny/c3-run/evidence/remote/logs`.
test_exec_bep: `/var/tmp/deploy-ga-svu3t2o.s6t5r6ny/c3-run/evidence/remote` (one original BEP and predicate report per phase).
farm_test_seconds: 11028.16 (sum of farm test time, not wall time).
fallback_reason: none. owned_topup: [].

```text
diff_tests_executed:
  TestMonitorNilClockUsesRealTime PASS (unit-98e159f45800f37b6a67.log)
  TestRunSetupCommandActivityIdleFiresOnInjectedClock PASS (integration-packages-f1416b63f1c12109ea7e.log, unit-ce5f199695f6832daef7.log)
  TestRunSetupCommandActivityStreamingSurvivesIdleWindow PASS (integration-packages-5a078b24388ef3c79748.log, unit-4bcc997199c6a7426526.log)
GATE_TEST_EVIDENCE_RESULT ok: 3 diff-owned tests verified by name
```

Original integrity: 998 raw log/XML artifacts rehashed against canonical retained manifests. Original verdicts and bytes are unchanged. Canonical normalized logs remove the rules_go output framing; raw CAS artifacts remain retained. Proof remote-integrity-audit.json.

## Native floor and SKIPs

floor_topup: complete; every step exited zero. Each environment-absence selection ran by owning package/root, with the mandatory beadstest, Darwin compile and runner self-tests. Any non-owned native SKIP is retained, not hidden; no changed test or subtest skipped.

| Package or check | Result | Selection |
|---|---|---|
| cmd/gc | PASS | 5 selected roots |
| examples/gastown | PASS | 2 selected roots |
| internal/beads | PASS | 2 selected roots |
| internal/beads/beadstest | PASS | 1 selected roots |
| internal/beads/contract | PASS | 1 selected roots |
| internal/beads/exec | PASS | 1 selected roots |
| internal/doctor | PASS | 3 selected roots |
| internal/mail/exec | PASS | 5 selected roots |
| internal/runtime/herdr | PASS | 6 selected roots |
| internal/runtime/runtimetest | PASS | 1 selected roots |
| internal/runtime/tmux | PASS | 4 selected roots |
| test/acceptance/helpers | PASS | 3 selected roots |
| test/integration | PASS | 1 selected roots |
| fsys-darwin-compile | PASS | 0 selected roots |
| push-gate-lock-selftest | PASS | 0 selected roots |
| local-concurrency-selftest | PASS | 0 selected roots |

Native original log hashes: native-log-hashes.json; canonical floor-topup.json retains commands, selected roots and reasons. The Darwin floor is the fsys compile check, not a Darwin run of the changed tests.

skip_inventory: {"other": 290, "environment-absence": 71}. Original inventory: `/var/tmp/deploy-ga-svu3t2o.s6t5r6ny/c3-run/evidence/remote/skip-inventory.json`.

skip_justification: all 272 inspected source sites/bodies are mechanically UNCHANGED; every inventory entry is mapped, including shared runtime conformance callers and the parallel-log reason recovered from its original dispatch log. Existing platform, lane, optional-capability and explicit destructive-test guards precede changed monitor code. The new test clock/FIFO helpers have no init/TestMain and are called only by the two passing new tests; the startup-test shared hunk removes only the former streaming test/comments. No unchanged SKIP can be caused by those test helpers. All environment-absence selections receive the canonical native floor; remaining unrelated SKIPs and reasons stay inventoried. Proof skip-ownership-audit.json and skip-sites-proof.txt.

## Runtime, admission and load

Rootless podman socket and exact deps.env image verified before launch; pinned bd v1.3.2 supplied by the standard wrapper. Host ICU74 hashes/loader check PASS without an overlay. The wrapper transparently reports PASS-THROUGH for this existing throwaway scratch; no wrapper-created isolation is claimed. Farm tests execute in their remote sandboxes, and native commands use the canonical scrubbed test environment.

admission: {"class": "remote", "floor_gib": 25.0, "free_gib": 57.903202056884766, "waited_seconds": 0.001, "cap": 2}.
rbe_state: {"rc_marker_present": true, "kill_switch_absent": true, "cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "worker_env_tree": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98", "worker_env_origin_main": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"}.
The native load gate uses the five-minute host load. Its ordinary bounded wait and every threshold, timeout and budget are unchanged. Original load records (all wait/run fields retained):

```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=63.97 run_max_load=82.63 run_mean_load=56.24 run_readings=116 wait_first_load=63.97 wait_max_load=63.97 wait_mean_load=63.97 wait_readings=1 read_errors=0
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1800 wait_timed_out=1 run_start_load=58.89 run_max_load=82.66 run_mean_load=74.30 run_readings=15 wait_first_load=42.73 wait_max_load=72.52 wait_mean_load=49.19 wait_readings=61 read_errors=0
```

## Review provenance, limits and handoff

The reviewer independently executed green repeats and mutations, but both review rounds inspected RED statically. The builder's recorded RED/GREEN patches and their refreshed-history equivalents have matching stable patch IDs, recomputed in red-green-provenance.json. No original scheduling-stall reproduction or reviewer-executed RED is claimed.

No arbitrary timing window was widened. The runtime-tmux manifest remains derived; no inventory constants are typed here or in the PR. The test clock's residual signal-before-store window stays tracked as ga-neaunf4, and ga-cp6g5nv / ga-3i8bckt are untouched. No Darwin or Windows test execution is claimed for these changed tests.

Tracker ga-886gn1 remains OPEN until this change actually lands on main. No reopening of ga-q422865 or ga-od4rcxy. The deployer opens only an isolated PR, publishes clearance for its exact head and routes the merge request to mayor/MPR; only the merge authority lands it.
