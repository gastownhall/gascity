# Release gate: orphan-scan PID assertion

**Verdict:** **PASS**

- Bead: `ga-36zmpai`; build: `ga-4xc5934`; review: `ga-d7tz9xc` (PASS).
- Reviewed source: `856817ab47d317b7e7e106b3a3e3128c73eeab9a`. Builder branch is provenance only.
- Deploy branch: `deploy/ga-36zmpai-gate`; the gate record is its only deploy-time addition.
- Evaluated: 2026-10-27T00:53:49.166989+00:00.
- `DEPLOY_MODE=remote`; `BASE_REF=origin/main`; pinned base: `37bd09351ac80604606366872b4efeed63b624b8`.
- Canonical scratch merge: `aefbacccb5a8bb56a4a7a0175291c8fbf399a676`; tree: `0eda9272be5a55652e4a1e716fec7ae687fd921c`.
- `merge_state: clean (origin/main@37bd09351ac80604606366872b4efeed63b624b8)`
- `gate_base_kind: current-tip`
- `merge_validation: merge result built, fully vetted and tested here; later base changes belong to the merge authority.`
- Evidence: `/var/tmp/gc-ga-36zmpai-regate.p78j4wd3`. Original raw logs remain retained outside the city.

| # | Criterion | Result and evidence |
|---|---|---|
| 1 | Review PASS present | PASS — reviewer ga-d7tz9xc records PASS on the exact resolved reviewed source. No carryover or reviewer-added commit is substituted. |
| 2 | Acceptance criteria met | PASS — independent code inspection confirms typed EntryError traversal and exactly PID104, with both random TempDir and fixed pid103 cases. The fresh suite passes the parent and both cases by name, while retaining candidate-error and non-candidate laziness assertions. The post-landing action below remains pending the merge. |
| 3 | Full CI-equivalent suite | PASS — all five forced-fresh remote phases and every mandatory native step completed. Combined Go result lines: 126832 PASS, 0 FAIL, 372 SKIP. Full-scope commands, per-name evidence and skip disposition below. |
| 4 | No unresolved HIGH findings | PASS — 0; style, security and specification reviews have no blocking finding. |
| 5 | Final branch clean | PASS — reviewed source plus this gate record only; clean status is independently checked after the gate commit and before publication. |
| 6 | Merge state and build/vet | PASS — clean canonical merge at the pinned base; CLI build and full nogo/vet exit0, with normal commit hooks active. |
| 7 | Single feature theme | PASS — two reviewed build commits change only internal/runtime/proctable/scan_linux_test.go, one assertion-stability fix. Ancestry scope accepts only this deploy and its build bead. |

## Fast checks

All lanes ran before the suite, each in its own fresh-tree view of the same merge. No lane changed the suite checkout.

- Build: `bazel build --config=remote-exec --noremote_local_fallback //cmd/gc:gc` — PASS.
- Vet: `make vet NOGO_BUILD_FLAGS='--keep_going --output_groups=nogo_fix --config=remote-exec --noremote_local_fallback'` — PASS.
- Format: pinned `run-pinned-lint.sh -- make fmt-check` — PASS, golangci-lint2.14.0 matched its pin.
- Policy: pinned-base `gate-base.sh run -- make lint` with full nogo, plus check-release-dist-ignore, check-routed-test-rows, check-split-topology-rows and check-residency-boundary — PASS.
- Drift: `make bazel-sync` followed by `git diff --exit-code` — PASS; normal Gazelle merge-expression diagnostics produced no drift.
- Complexity: literal `make complexity-check` exits2 on both candidate and pristine pinned base. The entire 74-finding population is identical, SHA256 `2ac4f0a851ec91ed7aae01aa2dcf2f7c69be041f395fc1625349e8e338f43642`; fresh `make complexity-diff` against that same base exits0 and reports no threshold changes. Existing opened tracker `ga-q7p6is`, sighting `ff9fbb2a-568c-5ee2-bda2-744246488b44` (readback verified). The diff changes only a test file excluded by the production scanner. `policy_attribution: complexity-check -> ga-q7p6is; proof=d (same-run BASE_REF), no path overlap; zero growth per the tracker's mayor disposition.` No baseline edit or waiver.
- `make check-hooks` confirms `.githooks` is active. The deploy gate commit uses normal hooks.

## Criterion 3 execution

- `test_cmd_scope: full-suite`
- `heavy_mode: none` — mandatory classifier reports no selected heavy package.
- `test_exec_mode: remote-exec` — derived from BEP, not eligibility.
- `fallback_reason: None`; `waiver_ref: none`.
- `farm_test_seconds: 11032.397`.
- `test_log_dir: /var/tmp/gc-ga-36zmpai-regate.p78j4wd3/criterion3/evidence/remote/logs`.
- `test_exec_bep`: each phase below; every result has a terminal verdict, first attempt, a valid phase start window, remote strategy and neither cached flag.

| Phase | Results | Remote executions | Cached / local / retries | Window | Verdict |
|---|---:|---:|---|---|---|
| unit | 373 | 373 | 0 / 0 / 0 | valid | PASS |
| acceptance | 24 | 24 | 0 / 0 / 0 | valid | PASS |
| integration-packages | 85 | 85 | 0 / 0 / 0 | valid | PASS |
| integration-full | 14 | 14 | 0 / 0 / 0 | valid | PASS |
| integration-latency | 1 | 1 | 0 / 0 / 0 | valid | PASS |

Total: 497 forced-fresh remote results; 994 retained raw farm log/XML artifacts, 67658594 bytes, all SHA256-verified against the retained phase manifests. Compile/action-cache hits are not credited as test results.

Actual canonical commands (including the separate full-integration and latency phases):

```text
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-ga-36zmpai-regate.p78j4wd3/criterion3/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-ga-36zmpai-regate.p78j4wd3/criterion3/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-ga-36zmpai-regate.p78j4wd3/criterion3/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-ga-36zmpai-regate.p78j4wd3/criterion3/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-ga-36zmpai-regate.p78j4wd3/criterion3/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```

Owned-test evidence, derived with diff-owned-tests.py and read from the retained full-suite logs:

- TestOrphanCandidateScanReadsOnlyReparentedProcesses — PASS.
- TestOrphanCandidateScanReadsOnlyReparentedProcesses/tempdir — PASS.
- TestOrphanCandidateScanReadsOnlyReparentedProcesses/path_containing_103 — PASS.

The shared hunks are the standard-library slices import and entryErrorPIDs, which is called only by this owned test; other test bodies and skip conditions are unchanged. No owned FAIL, SKIP or missing case is excused. Required process-tier TestTutorial01 PASS in integration-packages-aa6cf5c138f0b61b866a.log. Former revision-fence failure TestGCLiveContract_BeadsAndEvents PASS in integration-full-65412ec6e874f54aa912.log.

`latency_leg`: measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS; complete=true, rc0. Both TestGcBdLatencyBudget and TestGcSeatHookLatencyBudget PASS on the first probe-cleared attempt. No busy-farm attempt or deferred latency result is credited.

`admission`: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 41.56550598144531, "waited_seconds": 0.001}.

`rbe_state`: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98", "worker_env_tree": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"}.

## Native floor and skips

`floor_topup`: complete=true; every environment-absence selection and the permanent beadstest, Darwin compile and two runner self-tests ran. `owned_topup`: [].

| Native step | Result |
|---|---|
| cmd/gc | PASS (exit 0); retained floor-0.log |
| examples/gastown | PASS (exit 0); retained floor-1.log |
| internal/beads | PASS (exit 0); retained floor-2.log |
| internal/beads/beadstest | PASS (exit 0); retained floor-3.log |
| internal/beads/contract | PASS (exit 0); retained floor-4.log |
| internal/beads/exec | PASS (exit 0); retained floor-5.log |
| internal/doctor | PASS (exit 0); retained floor-6.log |
| internal/mail/exec | PASS (exit 0); retained floor-7.log |
| internal/runtime/herdr | PASS (exit 0); retained floor-8.log |
| internal/runtime/runtimetest | PASS (exit 0); retained floor-9.log |
| internal/runtime/tmux | PASS (exit 0); retained floor-10.log |
| test/acceptance/helpers | PASS (exit 0); retained floor-11.log |
| test/integration | PASS (exit 0); retained floor-12.log |
| fsys-darwin-compile | PASS (exit 0); retained floor-13.log |
| push-gate-lock-selftest | PASS (exit 0); retained floor-14.log |
| local-concurrency-selftest | PASS (exit 0); retained floor-15.log |

Farm Go result lines: 126811 PASS, 0 FAIL, 358 SKIP. Native Go result lines: 21 PASS, 0 FAIL, 14 SKIP. Counts sum to the combined inventory above; subtests are resolved by name and excluded from these top-level counts. Compile and shell assertions are additional checks, not Go result lines.

`skip_inventory` farm reason classes: {"environment-absence": 71, "other": 287}. All environment-absence SKIPs received the canonical native selection by package, selecting parents for subtests. The inventory includes unit-tier process skips, whose process-tier coverage is provided by integration-tagged packages, plus optional unavailable fixtures/tools or providers and platform/group/tmux conditions; exact test/reason records remain in skip-inventory.json and floor logs. Native top-ups inventory unavailable optional facilities rather than hiding them; no failure or diff-owned skip is accepted. The selected floor's successful completion is mechanically checked by gate-floor-topup.py.

Host load (native floor, standard soft wait; no override):

```text
load_threshold=15
load_waited_seconds=1800
load_wait_timed_out=1
run_start_load=35.33
run_max_load=48.85
run_mean_load=44.22
run_readings=12
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1800 wait_timed_out=1 run_start_load=35.33 run_max_load=48.85 run_mean_load=44.22 run_readings=12 wait_first_load=66.32 wait_max_load=73.69 wait_mean_load=58.49 wait_readings=61 read_errors=0
```

Wrapped runtime preflight proved Dolt2.6.0 matches deps.env, test bd1.3.2 is pinned, Podman socket/_ping is healthy, and ICU74 provenance is recorded/OK. No test-container image pin was found in executable test setup; the Dolt-image string hits are a deployment-manifest pin check and documentation URLs. No image was substituted. Evidence and native TMPDIR are private directories under /var/tmp, outside any city, with an empty outside-city probe. The workload uses its normal umask and scrubbed live-store selectors.

`ci_lane_run: n/a (no CI-config change in this diff)`.

## History, landing and authority

The two 2026-10-21 terminal failed/incomplete runs remain preserved at /var/tmp/deploy-ga-36zmpai.6qCH36ex, /var/tmp/deploy-ga-36zmpai-fresh.oTvJBxx5; none of their verdicts is reused. The mayor explicitly authorized this complete fresh gate after ga-kjf435d's fix actually landed as PR8299 squash1036b2cb20c15f0a6532fbd04fbb7d63ae42e2b3 and ga-ilq37o8's bounded-latency exit was met. This run independently re-proved all coverage including the native floor. Gas City has no waiver path.

The change is not shipped until MPR merges it. Keep source tracker ga-wlmmmmw OPEN until the remedy is verified on main; then close that tracker and re-run the SAME held deploy ga-951bwb9 fresh. PR creation, gate PASS or review closure does not satisfy that landing action. The deployer publishes exact-head clearance and routes the merge-request via mayor; it never merges.

