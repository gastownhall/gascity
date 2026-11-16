# Compactor archive-level release gate — ga-gybpf05

**Verdict:** **PASS**

## Coordinates

- Reviewed source: `5f8c1d0fa2b9f01c5957ceef331e9a7f6da61373`; review ga-75e1t01 PASS, build ga-6z06nah, original feature ga-rgwfyg4. All consumed SHAs resolved against Git.
- Review is a fresh delta review of this exact source, not patch-id carryover. No reviewer-added tests. Provenance builder/ga-6z06nah is never a push target.
- DEPLOY_MODE: remote; repository gastownhall/gascity; BASE_REF origin/main.
- merge_state: clean (origin/main@16777d7880f3bc7734bf49e76fc88d158ecbefaa).
- gate_base_kind: current-tip; gate_base_sha: `16777d7880f3bc7734bf49e76fc88d158ecbefaa`.
- Gated synthetic merged head: `d384ae05ae0a7996a6df5d1e58b4bf1cafff380d`; tree `606b614cfe15b20a2bfb2911c03cfe0c206ebff5` equals the read-only predicted tree.
- merge_validation: full clean merged tree built, vetted and freshly tested with normal hooks. No deployer source rebase or conflict resolution. MPR owns merge-time freshness.
- Preflight: exact-source commit/pulls returned HTTP200/empty; original own PR #8327 was OPEN at the retained old head. This bead explicitly requests a fresh isolated branch and new PR, preserving the reviewed source. Prepublication preflight is checked again separately.
- Evidence root: `/var/tmp/deploy-ga-gybpf05.jtn3dt64`. Original BEPs, raw logs and XMLs retained. Prior gate and reviewer passes supply no criterion3 credit.

## Criteria

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | ga-75e1t01 reviewed exact source; zero unresolved HIGH/blocker/major findings; no carryover shortcut. |
| 2 | Acceptance met | PASS | Exactly two constant compactor GC statements carry archive-level=0; shared-history guard remains meaningful; independent historical RED/current full-gate GREEN and all 13 changed tests PASS; docs rendered and synced. |
| 3 | Tests pass | PASS | Complete fresh five-phase remote suite and canonical native floor; uncached first attempts, complete by-name/SKIP audit, no failures or waiver. |
| 4 | No HIGH finding open | PASS | Review security/style/spec findings contain no unresolved HIGH, blocker or major issue. One prose-wrap nit is nonblocking. |
| 5 | Final branch clean | PASS | Gated scratch clean at predicted tree. Publication adds only this record above exact reviewed source and requires a clean branch. |
| 6 | Merge state/build/vet | PASS | Frozen clean current-tip merge, complete bazel build //... (1395 targets) and make vet/nogo PASS, normal hooks active. |
| 7 | Single feature theme | PASS | Six files, +144/-79: compactor GC archive format and its tests/help/recovery docs. Same-theme ancestry check accepts ga-gybpf05, ga-6z06nah, ga-rgwfyg4; no stack. |

## Acceptance and boundaries

The only production behavior changes are `CALL DOLT_GC('--archive-level=0')` for working-set GC and `CALL DOLT_GC('--full', '--archive-level=0')` for full GC. Both write classic table files, avoiding the archive dictionary-duplication path while giving up archive compression. No environment knob is added. Header/progress/help statements match; all three changed help/docs files drop archive-level=1 recommendations. The recovery bullet retains the managed Dolt 2.6.0 floor from #8291 and drops the conflicting default-archive-compression recommendation. static-acceptance-audit.json records exact statements and guard checks.

Shared-history protection still rejects the full-GC prefix while allowing working-set GC. The bare path bypasses flatten/push; gc-only and selected pending-GC recovery use full GC with archive output disabled. Existing allowlists, disk preflight and pending-push holds remain covered by the actual 13 changed tests below. No live store conversion, order enablement, city config, timeout, supervisor ExecGC or quiescence change is included. The mayor owns post-merge operational testing; this gate does not claim a large-store OOM reproduction or a live Dolt conversion.

Historical RED `15257ab6e65b9b00be7e77316660cb45058e2ce3` was independently executed once as a focused acceptance check: both bare-gc and gc-only assertion-FAIL, rc3, uncached first-attempt remote; no compilation/setup failure. The exact new test definition is byte-identical to the current gated version (SHA256 f736c70af8b180adf68b8acfde9b260c5427f6d8077597182019b45d0289ab31). Both cases and their parent PASS in current full-gate unit output (archive-flag-green-evidence.json). Historical logs/XMLs are retained and rehashed; their exact scratch/base are gone. No focused historical result is criterion3 credit.

Both changed user docs pages previewed over localhost, HTTP200, archive flag and version/recovery text rendered: 61 same-page anchors resolve, four TOML examples parse, one frontmatter H1 per page. Original HTML/text and docs-preview-verified.json retained. The owned preview service, socket and throwaway view were stopped/removed. No browser screenshot is claimed. Docs sync ran in the generated fast lane.

## Build and fast lanes

C6 ran first and passed complete bazel build //... plus make vet/nogo on the clean merge. Every subsequent fast lane used fresh-tree-run.sh plus gate-base.sh run before suite launch; generators never wrote the test scratch.

- policy_lane: pinned golangci-lint 2.14.0 fmt-check, full nogo/lint, hooks check, four shell guards and ten required Bazel policy targets PASS.
- drift_lane: make bazel-sync, git diff --exit-code and empty tracked/untracked status PASS. The current CI drift inventory was read independently.
- generated_lane: seven required schema/spec/client/dashboard/lockfile targets plus docs sync PASS. All 36 original fast log/XML artifacts rehashed.
- Optional Python TypeScript-pin subtest SKIP is unchanged and explicitly inventoried in generated-python-skip-audit.json; both required lockfile comparisons run. This is separate from Go result-line counts.
- complexity: PASS under zero-growth form(ii). Base and merged raw checks both return 2 with byte-identical 128-row reports (SHA256 423f115c118cb4b53e654b905c6133ba7d1e7817abe49041ebee2201eed45750); diff check rc0/no growth, thresholds/baseline unchanged. This is not a raw complexity-check PASS.
- ci_lane_run: n/a — no CI job, matrix, timeout or required-check configuration changed.
- heavy_mode: none, verified by classify; no heavy exception or narrowed suite substitution.

## Fresh criterion3

test_cmd_scope: full-suite.
test_exec_mode: remote-exec (BEP-derived); native commands are the canonical supplemental floor.
waiver_ref: none. failure_attribution: none.

Canonical gate-criterion3.sh ran detached under systemd. The five phases cover make check, complete acceptance (sharded and solo), integration packages, full integration minus the canonical latency inventory, then only that inventory under the farm probe. All force --nocache_test_results and --noremote_local_fallback. No cached test credit or farm/local suite stitching. Exact retained commands:

```text
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-gybpf05.jtn3dt64/c3-recovery-run/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-gybpf05.jtn3dt64/c3-recovery-run/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-gybpf05.jtn3dt64/c3-recovery-run/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-gybpf05.jtn3dt64/c3-recovery-run/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-gybpf05.jtn3dt64/c3-recovery-run/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```

| Phase | Result | BEP results | Remote executions | Cached local / remote / retry |
|---|---|---:|---:|---|
| unit | PASS | 375 | 375 | 0 / 0 / 0 |
| acceptance | PASS | 24 | 24 | 0 / 0 / 0 |
| integration-packages | PASS | 85 | 85 | 0 / 0 / 0 |
| integration-full | PASS | 14 | 14 | 0 / 0 / 0 |
| integration-latency | PASS | 1 | 1 | 0 / 0 / 0 |

All 499 terminal results PASS: attempt1, strategy remote, neither cache flag, start within phase window, other strategy0, transport_failure false. All 998 original farm log/XML artifacts rehashed independently.

test_counts: {"FAIL": 0, "PASS": 133954, "SKIP": 377}. These count retained Go result lines, not distinct tests or targets. Historical RED and optional Python SKIP are separate.
diff_tests_executed: all 13 PASS by name, none SKIP/FAIL/MISSING.
test_log_dir: `/var/tmp/deploy-ga-gybpf05.jtn3dt64/c3-recovery-run/evidence/remote/logs`.
test_exec_bep: `/var/tmp/deploy-ga-gybpf05.jtn3dt64/c3-recovery-run/evidence/remote` (original per-phase BEP and predicate reports).
farm_test_seconds: 10846.731 (sum of farm time, not wall time).
fallback_reason: none. owned_topup: [].

```text
GATE_TEST_EVIDENCE logs_dir=/var/tmp/deploy-ga-gybpf05.jtn3dt64/c3-recovery-run/evidence/remote/logs logs=515 logs_with_results=449
test_counts: PASS=133954 FAIL=0 SKIP=377
  (top-level result lines, column 0, summed over the 515 logs; a test that several jobs run counts once per job; sub-tests are not counted)
logs_without_results: floor-13.log floor-14.log floor-15.log unit-02e0324f1ef05430d7e4.log unit-04c4f9e4335a6f1c2dfa.log unit-0787047af2c4dad6aebc.log unit-0eb92a6bd00590d91db6.log unit-0f39cb7e4c03399a72c4.log unit-1a34395a607c0208a005.log unit-1e67f201b300dc5f9e78.log unit-21372f682c4033d14a80.log unit-22d5121476fcaa40b586.log (+54 more)
diff_tests_executed:
  TestCompactScriptBareGCBypassesThresholdAndSkipsFlatten PASS (integration-packages-0708b0d0c2f548d0a722.log, unit-ff6413fa61a58d5e02ec.log)
  TestCompactScriptBareGCHonorsOnlyDBsAllowlist PASS (integration-packages-aa557e5bc0c7045b52d7.log, unit-6b47d0615f42f4ae2b8d.log)
  TestCompactScriptDiskPreflightAllowsOnlySelectedPendingGCRecovery PASS (integration-packages-17007f3ad20cb33b8d6a.log, unit-89d44f035a0dc6a9b6c9.log)
  TestCompactScriptDiskPreflightAllowsSelectedSharedHistoryPendingGCRecovery PASS (integration-packages-a9e12e81511731ef69dd.log, unit-da81bca7c80446c5af28.log)
  TestCompactScriptDiskPreflightHoldsSharedHistoryPendingGCWithoutSelection PASS (integration-packages-0708b0d0c2f548d0a722.log, unit-ff6413fa61a58d5e02ec.log)
  TestCompactScriptDiskPreflightOnlyDBsEnvDoesNotGrantPendingGCRecovery PASS (integration-packages-0708b0d0c2f548d0a722.log, unit-ff6413fa61a58d5e02ec.log)
  TestCompactScriptGCOnlyFlagHonorsOnlyDBFlag PASS (integration-packages-130aa78402132a9cb052.log, unit-98fce3c243b1e6971368.log)
  TestCompactScriptGCOnlyFlagReclaimsBelowThresholdWithFullGC PASS (integration-packages-a9e12e81511731ef69dd.log, unit-da81bca7c80446c5af28.log)
  TestCompactScriptGCStatementsDisableArchives PASS (integration-packages-a9e12e81511731ef69dd.log, unit-89d44f035a0dc6a9b6c9.log)
  TestCompactScriptSharedHistoryGuardHoldsPendingPush PASS (integration-packages-a9e12e81511731ef69dd.log, unit-89d44f035a0dc6a9b6c9.log)
  TestCompactScriptSharedHistoryGuardLeavesReclaimModesAlone PASS (integration-packages-0708b0d0c2f548d0a722.log, unit-9d67ff2f074bc8f49779.log)
  TestCompactScriptSharedHistoryGuardRetriesPendingGCLocally PASS (integration-packages-0708b0d0c2f548d0a722.log, unit-9d67ff2f074bc8f49779.log)
  TestCompactScriptSharedHistoryGuardSkipsFlattenAndPush PASS (integration-packages-17007f3ad20cb33b8d6a.log, unit-98fce3c243b1e6971368.log)
GATE_TEST_EVIDENCE_RESULT ok: 13 diff-owned tests verified by name
```

latency_leg: measured on first attempt under clear probe, unchanged budgets, latency tests excluded from main integration leg. Original probe:

```text
GATE_FARM_PROBE waiting waited=0s load1=57.47 load5=73.78 load15=41.64 cpus=32 threshold=50 max_wait=600s
GATE_FARM_PROBE verdict=clear waited=10s load1=48.64 load5=71.35 load15=41.19 cpus=32 threshold=50 max_wait=600s
```

## Native floor and SKIPs

floor_topup: complete; all 16 steps exit0, including beadstest, Darwin fsys compile, both runner self-tests and every environment-absence selection by package/root. Native logs retained and hashed in native-log-hashes.json. Darwin proves fsys compilation, not execution of changed tests on macOS.

| Package/check | Result | Selection |
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

skip_inventory: {"counts_by_reason_class": {"environment-absence": 71, "other": 292}, "top_level_counts_by_reason_class": {"environment-absence": 42, "other": 220}}.
Original per-test inventory: `/var/tmp/deploy-ga-gybpf05.jtn3dt64/c3-recovery-run/evidence/remote/skip-inventory.json`.
skip_justification: 402 farm/native entries, 39 native residual SKIPs, 273 actual unchanged source sites, unresolved0. Original reasons, source contexts and mechanical --site UNCHANGED proof retained in skip-ownership-audit.json and skip-sites-proof.txt. Actual SKIPs are platform/lane/explicit-opt-in or optional capability/tool guards; none is in either changed test file. No SHARED-HUNKS exist in the two changed test files. The production change only edits two constant SQL argument lists and comment/log text; skip guards and their inputs execute before compactor invocation and are unchanged. Canonical native rechecks address environment absence; residual unrelated native SKIPs are inventoried, never passes. One parallel-log missing reason is recovered only from its unique literal message in the original log.

## CI aggregate accounting

ci_required_closure: `["python3", "/home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py", "/var/tmp/gc-merge-validate.gybpf05.fhzf2zcs", "16777d7880f3bc7734bf49e76fc88d158ecbefaa", "5f8c1d0fa2b9f01c5957ceef331e9a7f6da61373", "origin"]`; merged workflow blob `e5b11c348505dab28dc223e874bc2c61cbbb33c5`; 4 LEAF jobs need a status. Full closure output retained at `/var/tmp/deploy-ga-gybpf05.jtn3dt64/ci-required-account/closure.txt`. The `check` job is an aggregator and takes no separate status. Runner expressions resolve to Blacksmith Ubuntu for the Linux jobs and Windows 2025 for credential-provider-windows.

ci_required_accounting:

| Job | Status | Evidence |
|---|---|---|
| runner-policy | LOCAL-PASS | "EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=/var/tmp/deploy-ga-gybpf05.jtn3dt64/ci-required-account/runner-policy.out python3 .github/workflows/scripts/runner_policy.py"; rc=0; log=/var/tmp/deploy-ga-gybpf05.jtn3dt64/ci-required-account/runner-policy.log |
| changes | LOCAL-PASS | ["python3", "/home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py", "/var/tmp/gc-merge-validate.gybpf05.fhzf2zcs", "16777d7880f3bc7734bf49e76fc88d158ecbefaa", "5f8c1d0fa2b9f01c5957ceef331e9a7f6da61373", "origin"]; rc=0; log=/var/tmp/deploy-ga-gybpf05.jtn3dt64/ci-required-account/closure-origin.txt |
| credential-provider-windows | NOT-TRIGGERED | credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true' |
| pack-gate | NOT-TRIGGERED | pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true' |

windows_reach: n/a (Windows job NOT-TRIGGERED).
deferred_to_pr_ci: none.
not_covered: none.

The gate authorizes PR-open. Merge requires CI / required to be green on the PR head; MPR owns that check.

## Runtime, admission and load

Before C3, Podman rootless socket/pinned Dolt image verified; pinned bd1.3.2/ref9b65b9e19 checked; host ICU74 loader/hash PASS, no overlay or library-path change. The wrapper truthfully reports PASS-THROUGH / NOT ISOLATED for this owned scratch, with unstripped GC_GATE_REMOTE_ENABLED and BEADS_ALLOW_UNREAPED_TESTCONTAINERS; no claim of wrapper-created clean isolation. Farm actions use remote sandboxes and canonical native commands scrub their environment.

An initial recorder mistake appended gc-bd warning stderr to JSON, making the original selector choose local. That attempt was stopped before any test/BEP/log existed; the old pipeline is stopped. Valid stdout-only bead JSON and selector eligibility were rechecked before this single fresh remote run. No failed candidate test was rerun or excused. Original invalid JSON and setup evidence remain retained.

admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 40.20783996582031, "waited_seconds": 0.001}.
rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98", "worker_env_tree": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"}.

Remote admission uses25GiB/cap2 with no host-load or heavy wait. The native floor keeps the ordinary bounded soft wait: threshold15/five-minute load, maximum1800s. Timeout proceeds under the standing rule, without override. Original run/wait summaries are separate:

```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=37.47 run_max_load=109.25 run_mean_load=72.93 run_readings=102 wait_first_load=37.47 wait_max_load=37.47 wait_mean_load=37.47 wait_readings=1 read_errors=0
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=77.02 run_max_load=71.64 run_mean_load=59.03 run_readings=7 wait_first_load=68.54 wait_max_load=108.97 wait_mean_load=92.64 wait_readings=61 read_errors=0
```

Named load fields (remote outer, then native floor):

```json
[
  {
    "run_start_load": "37.47",
    "run_max_load": "109.25",
    "run_mean_load": "72.93",
    "run_readings": "102",
    "wait_first_load": "37.47",
    "wait_max_load": "37.47",
    "wait_mean_load": "37.47",
    "wait_readings": "1",
    "read_errors": "0",
    "load_threshold": "15",
    "load_waited_seconds": "0",
    "load_wait_timed_out": "0"
  },
  {
    "run_start_load": "77.02",
    "run_max_load": "71.64",
    "run_mean_load": "59.03",
    "run_readings": "7",
    "wait_first_load": "68.54",
    "wait_max_load": "108.97",
    "wait_mean_load": "92.64",
    "wait_readings": "61",
    "read_errors": "0",
    "load_threshold": "15",
    "load_waited_seconds": "1801",
    "load_wait_timed_out": "1"
  }
]
```

Only this record is added above reviewed source. Exact-head clearance follows verified PR creation and precedes the peek-verified mayor/MPR merge-request. The deployer never merges; the work remains blocked until MPR lands it.
