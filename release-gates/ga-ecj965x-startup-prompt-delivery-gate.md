# Startup-prompt delivery fresh release gate — ga-ecj965x

**Verdict:** **PASS**

- Evaluated: 2026-10-29T01:29:49.071419+00:00.
- Reviewed source: `894e708f9beb43515c3af30257fc233e2f55a627`; review ga-p5fdcfm; build ga-mfp1no; feature ga-zh3hdj.
- Deploy mode remote; base ref origin/main; frozen base `5b2d538d459fd581d3556c23ee512433da80fdcd`.
- Materialized merge `f14c0d8875e8093c3d5b44292e7345aa0a177baa`; tree `5abc64300f0b287a4f0297998bd6975eaae574f9`.
- `merge_state: clean (origin/main@5b2d538d459fd581d3556c23ee512433da80fdcd)`; `gate_base_kind: current-tip`.
- Merge-time validation belongs to MPR. No deployer rebase or merge.
- Evidence root: `/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j`. All current raw logs/BEP/XML, runtime proof, native inventories and preview HTML are retained.
- Already-merged preflight: reviewed source not on main; own deploy-head PR query empty. Source/pulls422 is recorded as unpublished, never an empty-success result.
- docs/PROJECT_MANIFEST.md is absent at this source; the supplied seven criteria and repository Bazel/TESTING.md lanes define this gate.

| Criterion | Result | Evidence |
| --- | --- | --- |
| 1. Review PASS | PASS | Live reviewer explicitly approves the exact resolved source. No carryover or waiver; all corrections after the TDD green commit are included in that review. |
| 2. Acceptance | PASS | Fifteen changed test roots plus the expressly permitted R1-R3 V4 live evidence carry below. Full prompt on the same native key for both origins; hook context absent, never called a verified beacon. |
| 3. Full CI-equivalent tests | PASS | C6 first, all fast policy/drift/OpenAPI lanes before runtime/suite, then five fresh remote phases and 16 successful native floor steps. No owned FAIL/SKIP/MISSING, attribution or waiver. |
| 4. Open HIGH findings | PASS | Zero unresolved blocking HIGH findings. Repeated rendering/probes/sole-carrier warning overhead remains nonblocking follow-up ga-vytkfv. |
| 5. Final tree clean | PASS | Canonical merge tree and clean tracked/untracked status verified after the suite. Publication permits only the gate-record commit on an isolated branch at the reviewed source and verifies clean state before push. |
| 6. Merge/build/vet | PASS | Clean normal merge equals predicted tree. Full Bazel build, make vet/nogo and check-hooks pass. Initial reservation-only setup failure was corrected before any candidate checkout/build/test. |
| 7. Single feature | PASS | Seventeen files implement startup-prompt delivery parity and its regression/warning tests under cmd/gc and internal/api. No unrelated feature, CI config change or private-doc contamination. |

## Acceptance under the current contract

AC1: shared delivery planning pairs payload and marker for reconciler starts and worker-factory resumes. Target identity is rendered independently of caller environment. Empty prompts and transports unable to carry a nudge retain their existing behavior. API worker resolution asks controller state for the plan and surfaces preparation errors. The original reviewed RED/green sequence is recorded in the review; this gate independently executes all15 changed tests in its full suite.

AC2 uses the architect's explicit R1-R3 ruling on ga-4k4zfk, 2026-10-28T23:33Z, delivered in verified mail gm-wisp-y6h6kgk. Tracker ga-6fcmq29 closed through stamped gc bd because that clarification is its named exit; this is not a test waiver. The fresh gate carries only V4's live observations, exactly as the ruling allows, after proving every one of its12 enumerated delivery-path files is byte-identical between old canary tree `e7feed21397c9ed85aec87882442c72af33974a2` and this newly gated tree. No old automated suite result carries.

- R1: original real Claude Code2.1.312 stopped-session API session.submit and CLI gc session attach traces were rehashed and the actual first-user records re-read. Each contains the entire20518-character rendered prompt exactly once (20576 including restart wrapper), on the same native key. Stop witnesses each end with three runtime-absent observations.
- R2: both resumed transcripts contain zero SessionStart attachments. That is ABSENT and satisfies the clarified rule; no prompt or preview carried by resumed hook context was observed.
- R3: **beacon not observed on resume; hook execution on resume not established** (ga-861hcby). No absence is relabeled as beacon verified.

Exact carry proof and native transcript hashes:

```json
{
  "ruling": "ga-4k4zfk R1-R3 at2026-10-28T23:33Z; mail gm-wisp-y6h6kgk",
  "old_merge": "13ca2acace0a49aa2e55920c281f40f13928bacb",
  "old_tree": "e7feed21397c9ed85aec87882442c72af33974a2",
  "new_merge": "f14c0d8875e8093c3d5b44292e7345aa0a177baa",
  "new_tree": "5abc64300f0b287a4f0297998bd6975eaae574f9",
  "enumerated_files": [
    {
      "path": "cmd/gc/api_state.go",
      "old_blob": "ec13418ddea7186dd2e9b7749d7d2c8712de1de2",
      "new_blob": "ec13418ddea7186dd2e9b7749d7d2c8712de1de2",
      "unchanged": true
    },
    {
      "path": "cmd/gc/cmd_prime.go",
      "old_blob": "58d4f375edfc72bfb92240f9fdb900940b683e4d",
      "new_blob": "58d4f375edfc72bfb92240f9fdb900940b683e4d",
      "unchanged": true
    },
    {
      "path": "cmd/gc/prompt_delivery.go",
      "old_blob": "18f67b88d435077e866e9470b547fed3fe4beaac",
      "new_blob": "18f67b88d435077e866e9470b547fed3fe4beaac",
      "unchanged": true
    },
    {
      "path": "cmd/gc/session_lifecycle_parallel.go",
      "old_blob": "a8222410eef4430b2c32d8816c98be326acef121",
      "new_blob": "a8222410eef4430b2c32d8816c98be326acef121",
      "unchanged": true
    },
    {
      "path": "cmd/gc/session_startup_prompt.go",
      "old_blob": "3c20edb18ac216fa98d532805382c5e46b85ab24",
      "new_blob": "3c20edb18ac216fa98d532805382c5e46b85ab24",
      "unchanged": true
    },
    {
      "path": "cmd/gc/startup_prompt_delivery.go",
      "old_blob": "4dff5bd325f2e922121412157152c89e51a71722",
      "new_blob": "4dff5bd325f2e922121412157152c89e51a71722",
      "unchanged": true
    },
    {
      "path": "cmd/gc/template_resolve.go",
      "old_blob": "51b01530dffc3f679bfb3ee56996deb198f3e0c7",
      "new_blob": "51b01530dffc3f679bfb3ee56996deb198f3e0c7",
      "unchanged": true
    },
    {
      "path": "cmd/gc/worker_handle.go",
      "old_blob": "f0be7507e7f7aee30c9322b1f99b775c0786762a",
      "new_blob": "f0be7507e7f7aee30c9322b1f99b775c0786762a",
      "unchanged": true
    },
    {
      "path": "internal/api/dashport_support.go",
      "old_blob": "9f049de3bbf2e1f7777d0eb455fab9425df671bb",
      "new_blob": "9f049de3bbf2e1f7777d0eb455fab9425df671bb",
      "unchanged": true
    },
    {
      "path": "internal/api/session_runtime.go",
      "old_blob": "6a8cfe9e30b91ac64e12481525e4c64748212212",
      "new_blob": "6a8cfe9e30b91ac64e12481525e4c64748212212",
      "unchanged": true
    },
    {
      "path": "internal/api/state.go",
      "old_blob": "a56e16a74daa20c8c622cbe064d949537be18a3f",
      "new_blob": "a56e16a74daa20c8c622cbe064d949537be18a3f",
      "unchanged": true
    },
    {
      "path": "internal/session/chat.go",
      "old_blob": "9db4a3b59520a0ff47edb478601eb850bc19d030",
      "new_blob": "9db4a3b59520a0ff47edb478601eb850bc19d030",
      "unchanged": true
    }
  ],
  "carry_forward_allowed": true,
  "V4_transcripts_rehashed": [
    {
      "origin": "v4-api",
      "native_session": "1fd97ad1-d2f5-407e-9c22-86dd3eb95977",
      "uuid": "88805614-c02a-474c-89d8-6593ea8f47ef",
      "transcript": "/var/tmp/deploy-ga-ecj965x.57ig8_rp/live-canary-v4-evidence/api-complete-native-retained.jsonl",
      "sha256": "63fecb04b74df5de692fbc2cf57bfb5f62428f9f7820fd6abb04d93cba4be9d0",
      "full_prompt_chars": 20518,
      "first_turn_chars": 20576,
      "SessionStart": "absent",
      "first_turn_reread": true,
      "entire_rendered_prompt_matches": true,
      "prompt_sha256": "3485875276eb4d84c1ea3de48aa7e439583c9a57ccc4c58415e264188c7310b3",
      "last_three_runtime_observations_absent": true
    },
    {
      "origin": "v4-attach",
      "native_session": "1fd97ad1-d2f5-407e-9c22-86dd3eb95977",
      "uuid": "c16e97eb-4300-48be-8b99-18dc88889596",
      "transcript": "/var/tmp/deploy-ga-ecj965x.57ig8_rp/live-canary-v4-evidence/attach-complete-native-retained.jsonl",
      "sha256": "63fecb04b74df5de692fbc2cf57bfb5f62428f9f7820fd6abb04d93cba4be9d0",
      "full_prompt_chars": 20518,
      "first_turn_chars": 20576,
      "SessionStart": "absent",
      "first_turn_reread": true,
      "entire_rendered_prompt_matches": true,
      "prompt_sha256": "3485875276eb4d84c1ea3de48aa7e439583c9a57ccc4c58415e264188c7310b3",
      "last_three_runtime_observations_absent": true
    }
  ],
  "beacon_not_observed_on_resume": true,
  "hook_execution_on_resume_not_established": "ga-861hcby",
  "historical_8f84242e": "UNATTRIBUTED; class not reported fixed"
}
```

The original canary audit's INCOMPLETE verdict under the older, narrower beacon requirement remains intact. V5/V6 and their private matcher fixture are not current PASS evidence and were not rerun. V4 is accepted only under the explicit current contract and exact file equality above. Its existing private-city supervisor/socket cleanup was independently verified in that audit. No active fleet session was interrupted for this fresh gate and no new canary provider was launched.

AC3: historical launch8f84242e remains UNATTRIBUTED. The managed worker-factory path is covered; the whole truncation class is not reported fixed. Parent ga-4k4zfk remains open pending landing and its required AC2 record. A later truncation still needs its actual launch origin recorded there. Started-create delivery behavior remains tracked separately in ga-vytkfv.

AC4: TestPrimeHookWarnsWhenItIsTheSoleCarrierOfALargePrompt passes by name. The warning uses the existing structured logging sink, with session/size context; no public wire/event schema is added. The delivery marker retains the existing selected/attempted meaning rather than asserting transport receipt.

AC5: current canonical Bazel CI-equivalent lanes, build/nogo/vet, format and generated checks pass. make dashboard-check passes all ten targets in a fresh independent view. Actual npm preview serves HTTP200, HTML hash 86e1c6434496852e1c4659fa5163a89ee7c1bdcb24fa2778d402a30cf2781930, and its own captured process group is stopped.

## Full-suite evidence

- `test_cmd_scope: full-suite`; `heavy_mode: none` (computed classifier).
- `test_exec_mode: remote-exec` (BEP-derived).
- `test_exec_bep: /var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/{unit,acceptance,integration-packages,integration-full,integration-latency}.bep.json`.
- `test_log_dir: /var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs`.
- `test_counts: PASS=131775 FAIL=0 SKIP=373` (top-level per-job result lines, repeats counted).
- `diff_tests_executed: 15/15 PASS by name`.
- `waiver_ref: none`; `failure_attribution: none`; `ci_lane_run: n/a (no CI-config diff)`.
- `rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98", "worker_env_tree": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"}`.
- `fallback_reason: null`.
- `admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 45.53066635131836, "waited_seconds": 0.001}`.
- `farm_test_seconds: 10256.415`.
- `owned_topup: []`.
- `floor_topup: complete; 16 steps; /var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/floor-topup.json`.
- `skip_inventory: /var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/skip-inventory.json; /var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/farm-skip-inventory.json; /var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/native-skip-inventory.json`.

| Phase | Fresh remote result events | Farm action seconds | Verdict |
| --- | --- | --- | --- |
| unit | 374 | 2335.818 | PASS |
| acceptance | 24 | 998.587 | PASS |
| integration-packages | 85 | 3586.002 | PASS |
| integration-full | 14 | 3217.241 | PASS |
| integration-latency | 1 | 118.767 | PASS |

All 498 remote result events use remote strategy, neither cache flag, first attempt, a start inside their recorded phase window and terminal PASS. All 996 downloaded log/XML artifacts independently hash-match. No farm/local suite stitching. Full integration excludes exactly the canonical latency list; its separate latency leg measures both budgets on the first clear-probe attempt. The native floor covers beadstest, Darwin compilation, runner self-tests and every environment-absence package. Remaining non-owned optional/platform/environment skips are inventoried by exact reason/site; every site is UNCHANGED with zero shared hunks and no changed test is excused. TestTutorial01 reports PASS in the integration-package logs.

Exact detached command:

```sh
env GC_GATE_REMOTE_ENABLED=1 GC_REMOTE_GATE_FLOOR_GIB=25 GC_REMOTE_GATE_CAP=2 ISOLATED_TEST_BEAD=ga-ecj965x /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-criterion3.sh --repo /var/tmp/gc-merge-validate.hg54hzw0 --base 5b2d538d459fd581d3556c23ee512433da80fdcd --bead-json /var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/bead-current.json --owned /var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/owned.tsv --run-dir /var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence -- bash /var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/local-full.sh
```

Resolved full phase commands:

```text
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```

Native floor:

```json
{
  "complete": true,
  "steps": [
    {
      "package": "cmd/gc",
      "tests": [
        "TestReplay",
        "TestSessionEventPumpLiveHerdr",
        "TestSimCoverageGate",
        "TestSweepStaleTmuxServers_KeepsServerWhoseRootExistsInItsOwnMountNamespace",
        "TestSweepStaleTmuxServers_ReapsServerWhoseRootIsGoneInItsOwnMountNamespace"
      ],
      "skip_reasons": [
        {
          "package": "cmd/gc",
          "test": "TestReplay",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/integration-packages-bf8ed8ba46004086b3b9.log",
          "reason": "reconcile_replay_test.go:574: GC_V2_REPLAY_DIR is not set",
          "class": "environment-absence"
        },
        {
          "package": "cmd/gc",
          "test": "TestReplay",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/unit-2ef13e1b6898c5544764.log",
          "reason": "reconcile_replay_test.go:574: GC_V2_REPLAY_DIR is not set",
          "class": "environment-absence"
        },
        {
          "package": "cmd/gc",
          "test": "TestSessionEventPumpLiveHerdr",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/integration-packages-56f04454dd9158a9ee8a.log",
          "reason": "session_event_pump_live_test.go:23: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "cmd/gc",
          "test": "TestSessionEventPumpLiveHerdr",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/unit-23e7d9fc763496dd8095.log",
          "reason": "session_event_pump_live_test.go:23: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "cmd/gc",
          "test": "TestSimCoverageGate",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/integration-packages-e4245ddf6a37030111bc.log",
          "reason": "reconcile_sim_coverage_test.go:305: GC_V2_SIM_COVERAGE_FILES unset",
          "class": "environment-absence"
        },
        {
          "package": "cmd/gc",
          "test": "TestSimCoverageGate",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/unit-2bde55fab377f50b4739.log",
          "reason": "reconcile_sim_coverage_test.go:305: GC_V2_SIM_COVERAGE_FILES unset",
          "class": "environment-absence"
        },
        {
          "package": "cmd/gc",
          "test": "TestSweepStaleTmuxServers_KeepsServerWhoseRootExistsInItsOwnMountNamespace",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/integration-packages-75af79934639abaae69c.log",
          "reason": "tmux_leak_guard_mntns_test.go:76: bwrap not installed; cannot build a private-/tmp mount namespace",
          "class": "environment-absence"
        },
        {
          "package": "cmd/gc",
          "test": "TestSweepStaleTmuxServers_KeepsServerWhoseRootExistsInItsOwnMountNamespace",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/unit-64014bc0bb3833bc198c.log",
          "reason": "tmux_leak_guard_mntns_test.go:76: bwrap not installed; cannot build a private-/tmp mount namespace",
          "class": "environment-absence"
        },
        {
          "package": "cmd/gc",
          "test": "TestSweepStaleTmuxServers_ReapsServerWhoseRootIsGoneInItsOwnMountNamespace",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/integration-packages-514989cc035c0e9a8ff5.log",
          "reason": "tmux_leak_guard_mntns_test.go:98: bwrap not installed; cannot build a private-/tmp mount namespace",
          "class": "environment-absence"
        },
        {
          "package": "cmd/gc",
          "test": "TestSweepStaleTmuxServers_ReapsServerWhoseRootIsGoneInItsOwnMountNamespace",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/unit-493754ea2d5b4d8858c8.log",
          "reason": "tmux_leak_guard_mntns_test.go:98: bwrap not installed; cannot build a private-/tmp mount namespace",
          "class": "environment-absence"
        }
      ],
      "command": [
        "go",
        "test",
        "-tags",
        "integration",
        "-count=1",
        "-v",
        "-run",
        "^(TestReplay|TestSessionEventPumpLiveHerdr|TestSimCoverageGate|TestSweepStaleTmuxServers_KeepsServerWhoseRootExistsInItsOwnMountNamespace|TestSweepStaleTmuxServers_ReapsServerWhoseRootIsGoneInItsOwnMountNamespace)$",
        "./cmd/gc"
      ],
      "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/floor-logs/floor-0.log",
      "exit_code": 0
    },
    {
      "package": "examples/gastown",
      "tests": [
        "TestAttachedRigScopeShellToken",
        "TestJsonlExportPushRetryDelayIsLocaleIndependent"
      ],
      "skip_reasons": [
        {
          "package": "examples/gastown",
          "test": "TestAttachedRigScopeShellToken/zsh",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/integration-packages-be91eec86b4dbf7e66f1.log",
          "reason": "gastown_test.go:4153: zsh not installed",
          "class": "environment-absence"
        },
        {
          "package": "examples/gastown",
          "test": "TestAttachedRigScopeShellToken/zsh",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/unit-ef24b9dbe59f5d04fac1.log",
          "reason": "gastown_test.go:4153: zsh not installed",
          "class": "environment-absence"
        },
        {
          "package": "examples/gastown",
          "test": "TestJsonlExportPushRetryDelayIsLocaleIndependent",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/integration-packages-a0f68450216c5e4d04af.log",
          "reason": "maintenance_scripts_test.go:9321: awk does not emit a comma decimal under de_DE.UTF-8 (locale missing?): \"1.50\", <nil>",
          "class": "environment-absence"
        },
        {
          "package": "examples/gastown",
          "test": "TestJsonlExportPushRetryDelayIsLocaleIndependent",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/unit-df0dcd5ff9a717b4f2a3.log",
          "reason": "maintenance_scripts_test.go:9321: awk does not emit a comma decimal under de_DE.UTF-8 (locale missing?): \"1.50\", <nil>",
          "class": "environment-absence"
        }
      ],
      "command": [
        "go",
        "test",
        "-tags",
        "integration",
        "-count=1",
        "-v",
        "-run",
        "^(TestAttachedRigScopeShellToken|TestJsonlExportPushRetryDelayIsLocaleIndependent)$",
        "./examples/gastown"
      ],
      "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/floor-logs/floor-1.log",
      "exit_code": 0
    },
    {
      "package": "internal/beads",
      "tests": [
        "TestMetadataEmptyStringClearContract",
        "TestProbeVersionsAgainstHostBinaries"
      ],
      "skip_reasons": [
        {
          "package": "internal/beads",
          "test": "TestMetadataEmptyStringClearContract/Postgres/BatchClearsOneKeyKeepsAnother",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/integration-packages-20cfdbabba8f52b16e0b.log",
          "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
          "class": "environment-absence"
        },
        {
          "package": "internal/beads",
          "test": "TestMetadataEmptyStringClearContract/Postgres/BatchClearsOneKeyKeepsAnother",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/unit-ea4511196531bb4b8c55.log",
          "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
          "class": "environment-absence"
        },
        {
          "package": "internal/beads",
          "test": "TestMetadataEmptyStringClearContract/Postgres/SetMetadata",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/integration-packages-20cfdbabba8f52b16e0b.log",
          "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
          "class": "environment-absence"
        },
        {
          "package": "internal/beads",
          "test": "TestMetadataEmptyStringClearContract/Postgres/SetMetadata",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/unit-ea4511196531bb4b8c55.log",
          "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
          "class": "environment-absence"
        },
        {
          "package": "internal/beads",
          "test": "TestMetadataEmptyStringClearContract/Postgres/SetMetadataBatch",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/integration-packages-20cfdbabba8f52b16e0b.log",
          "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
          "class": "environment-absence"
        },
        {
          "package": "internal/beads",
          "test": "TestMetadataEmptyStringClearContract/Postgres/SetMetadataBatch",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/unit-ea4511196531bb4b8c55.log",
          "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
          "class": "environment-absence"
        },
        {
          "package": "internal/beads",
          "test": "TestMetadataEmptyStringClearContract/Postgres/Update",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/integration-packages-20cfdbabba8f52b16e0b.log",
          "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
          "class": "environment-absence"
        },
        {
          "package": "internal/beads",
          "test": "TestMetadataEmptyStringClearContract/Postgres/Update",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/unit-ea4511196531bb4b8c55.log",
          "reason": "metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance",
          "class": "environment-absence"
        },
        {
          "package": "internal/beads",
          "test": "TestProbeVersionsAgainstHostBinaries/bd",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/unit-3ee1ae2eaa26dfd63a88.log",
          "reason": "binary_versions_test.go:66: bd not probeable in this environment: locate bd: exec: \"bd\": executable file not found in $PATH",
          "class": "environment-absence"
        }
      ],
      "command": [
        "go",
        "test",
        "-tags",
        "integration",
        "-count=1",
        "-v",
        "-run",
        "^(TestMetadataEmptyStringClearContract|TestProbeVersionsAgainstHostBinaries)$",
        "./internal/beads"
      ],
      "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/floor-logs/floor-2.log",
      "exit_code": 0
    },
    {
      "package": "internal/beads/beadstest",
      "tests": [
        "TestPinnedBeadsModuleDirResolvesThisBuildsOwnCache"
      ],
      "skip_reasons": [],
      "command": [
        "go",
        "test",
        "-tags",
        "integration",
        "-count=1",
        "-v",
        "-run",
        "^(TestPinnedBeadsModuleDirResolvesThisBuildsOwnCache)$",
        "./internal/beads/beadstest"
      ],
      "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/floor-logs/floor-3.log",
      "exit_code": 0
    },
    {
      "package": "internal/beads/contract",
      "tests": [
        "TestMigrateJournalFileMatchesPinnedBeads"
      ],
      "skip_reasons": [
        {
          "package": "internal/beads/contract",
          "test": "TestMigrateJournalFileMatchesPinnedBeads",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/unit-8544388e16a0d6b0cdb0.log",
          "reason": "migrate_journal_test.go:37: pinned beads v1.3.2 is not unpacked in the local module cache: open /tmp/bt/_tmp/35731ca362c311940500020a6c8a2007/go/pkg/mod/github.com/steveyegge/beads@v1.3.2/cmd/bd/migrate_dolt_mode.go: no such file or directory",
          "class": "environment-absence"
        }
      ],
      "command": [
        "go",
        "test",
        "-tags",
        "integration",
        "-count=1",
        "-v",
        "-run",
        "^(TestMigrateJournalFileMatchesPinnedBeads)$",
        "./internal/beads/contract"
      ],
      "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/floor-logs/floor-4.log",
      "exit_code": 0
    },
    {
      "package": "internal/beads/exec",
      "tests": [
        "TestBrProviderConformance"
      ],
      "skip_reasons": [
        {
          "package": "internal/beads/exec",
          "test": "TestBrProviderConformance",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/integration-packages-64046beff95fb28ccd66.log",
          "reason": "br_test.go:18: br not available",
          "class": "environment-absence"
        }
      ],
      "command": [
        "go",
        "test",
        "-tags",
        "integration",
        "-count=1",
        "-v",
        "-run",
        "^(TestBrProviderConformance)$",
        "./internal/beads/exec"
      ],
      "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/floor-logs/floor-5.log",
      "exit_code": 0
    },
    {
      "package": "internal/doctor",
      "tests": [
        "TestCustomTypesCheck_ServerBackedStoreIgnoresAmbientEndpoint",
        "TestCustomTypesCheck_TableDrift",
        "TestCustomTypesCheck_TableDriftUsesTestOwnedDoltContext"
      ],
      "skip_reasons": [
        {
          "package": "internal/doctor",
          "test": "TestCustomTypesCheck_ServerBackedStoreIgnoresAmbientEndpoint",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/unit-7bce503b7c011c7475f3.log",
          "reason": "checks_custom_types_test.go:467: bd binary not on PATH",
          "class": "environment-absence"
        },
        {
          "package": "internal/doctor",
          "test": "TestCustomTypesCheck_TableDrift",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/unit-7bce503b7c011c7475f3.log",
          "reason": "checks_custom_types_test.go:297: bd binary not on PATH",
          "class": "environment-absence"
        },
        {
          "package": "internal/doctor",
          "test": "TestCustomTypesCheck_TableDriftUsesTestOwnedDoltContext",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/unit-7bce503b7c011c7475f3.log",
          "reason": "checks_custom_types_test.go:399: bd binary not on PATH",
          "class": "environment-absence"
        }
      ],
      "command": [
        "go",
        "test",
        "-tags",
        "integration",
        "-count=1",
        "-v",
        "-run",
        "^(TestCustomTypesCheck_ServerBackedStoreIgnoresAmbientEndpoint|TestCustomTypesCheck_TableDrift|TestCustomTypesCheck_TableDriftUsesTestOwnedDoltContext)$",
        "./internal/doctor"
      ],
      "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/floor-logs/floor-6.log",
      "exit_code": 0
    },
    {
      "package": "internal/mail/exec",
      "tests": [
        "TestMCPMailBridgeSourceable",
        "TestMCPMailConformance",
        "TestMCPMailCrossPodNameResolution",
        "TestMCPMailProjectKeyIsolation",
        "TestMCPMailReplyMissingOriginalIsNotFound"
      ],
      "skip_reasons": [
        {
          "package": "internal/mail/exec",
          "test": "TestMCPMailBridgeSourceable",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
          "reason": "mcp_conformance_test.go:159: MCP mail script not found: file does not exist",
          "class": "environment-absence"
        },
        {
          "package": "internal/mail/exec",
          "test": "TestMCPMailConformance",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
          "reason": "mcp_conformance_test.go:26: MCP mail script not found: file does not exist",
          "class": "environment-absence"
        },
        {
          "package": "internal/mail/exec",
          "test": "TestMCPMailCrossPodNameResolution",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
          "reason": "mcp_conformance_test.go:195: MCP mail script not found: file does not exist",
          "class": "environment-absence"
        },
        {
          "package": "internal/mail/exec",
          "test": "TestMCPMailProjectKeyIsolation",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
          "reason": "mcp_conformance_test.go:286: MCP mail script not found: file does not exist",
          "class": "environment-absence"
        },
        {
          "package": "internal/mail/exec",
          "test": "TestMCPMailReplyMissingOriginalIsNotFound",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/unit-6c42f61796d33a7f0757.log",
          "reason": "mcp_conformance_test.go:80: MCP mail script not found: file does not exist",
          "class": "environment-absence"
        }
      ],
      "command": [
        "go",
        "test",
        "-tags",
        "integration",
        "-count=1",
        "-v",
        "-run",
        "^(TestMCPMailBridgeSourceable|TestMCPMailConformance|TestMCPMailCrossPodNameResolution|TestMCPMailProjectKeyIsolation|TestMCPMailReplyMissingOriginalIsNotFound)$",
        "./internal/mail/exec"
      ],
      "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/floor-logs/floor-7.log",
      "exit_code": 0
    },
    {
      "package": "internal/runtime/herdr",
      "tests": [
        "TestActivityLive",
        "TestHerdrConformance",
        "TestProviderLive",
        "TestProviderLiveClaudeKindPath",
        "TestProviderLiveOccupantSwapKeepsLiveness",
        "TestSessionEventsLive"
      ],
      "skip_reasons": [
        {
          "package": "internal/runtime/herdr",
          "test": "TestActivityLive",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
          "reason": "activity_live_test.go:20: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestActivityLive",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/unit-d920ddbd08781200929a.log",
          "reason": "activity_live_test.go:20: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestHerdrConformance",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
          "reason": "conformance_test.go:18: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestHerdrConformance",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/unit-d920ddbd08781200929a.log",
          "reason": "conformance_test.go:18: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestProviderLive",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
          "reason": "provider_live_test.go:15: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestProviderLive",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/unit-d920ddbd08781200929a.log",
          "reason": "provider_live_test.go:15: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestProviderLiveClaudeKindPath",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
          "reason": "kindpath_live_test.go:105: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestProviderLiveClaudeKindPath",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/unit-d920ddbd08781200929a.log",
          "reason": "kindpath_live_test.go:105: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestProviderLiveOccupantSwapKeepsLiveness",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
          "reason": "panebinding_live_test.go:21: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestProviderLiveOccupantSwapKeepsLiveness",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/unit-d920ddbd08781200929a.log",
          "reason": "panebinding_live_test.go:21: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestSessionEventsLive",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/integration-packages-0ba2c1cd80ef2ec12177.log",
          "reason": "events_live_test.go:40: herdr not installed",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/herdr",
          "test": "TestSessionEventsLive",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/unit-d920ddbd08781200929a.log",
          "reason": "events_live_test.go:40: herdr not installed",
          "class": "environment-absence"
        }
      ],
      "command": [
        "go",
        "test",
        "-tags",
        "integration",
        "-count=1",
        "-v",
        "-run",
        "^(TestActivityLive|TestHerdrConformance|TestProviderLive|TestProviderLiveClaudeKindPath|TestProviderLiveOccupantSwapKeepsLiveness|TestSessionEventsLive)$",
        "./internal/runtime/herdr"
      ],
      "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/floor-logs/floor-8.log",
      "exit_code": 0
    },
    {
      "package": "internal/runtime/runtimetest",
      "tests": [
        "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors"
      ],
      "skip_reasons": [
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Interrupt_ConcurrentDistinctSessions",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:238: Start(skip-start-14): provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/IsRunning_ConcurrentDistinctSessions",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:271: Start(skip-start-18): provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_ConcurrentDistinctPrefixes",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:367: Start(skip-start-25): provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_EmptyPrefix",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:349: Start: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_ExcludesStopped",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:333: Start: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_FindsSessions",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:294: Start skip-start-21: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ListRunning_PrefixFiltering",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:313: Start skip-start-22: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ProcessAlive_ConcurrentDistinctSessions",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:421: Start(skip-start-30): provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ProcessAlive_EmptyNamesReturnsTrue",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:395: Start: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/ProcessAlive_FalseAfterStop",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:404: Start: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/SharedSession",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:61: Start shared session: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Start_ConcurrentDistinctSessions",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:124: concurrent Start(skip-start-3): provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Start_CreatesRunningSession",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:84: Start: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Start_DuplicateReturnsError",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:93: first Start: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Stop_ConcurrentDistinctSessions",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:209: Start(skip-start-11): provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Stop_Idempotent_AlreadyStopped",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:193: Start: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Stop_Idempotent_NotRunning",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:170: Start sentinel: provider environment unavailable",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/runtimetest",
          "test": "TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors/Stop_MakesSessionNotRunning",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/unit-4310d3ec85c9c9d53cb6.log",
          "reason": "conformance.go:134: Start: provider environment unavailable",
          "class": "environment-absence"
        }
      ],
      "command": [
        "go",
        "test",
        "-tags",
        "integration",
        "-count=1",
        "-v",
        "-run",
        "^(TestRunProviderTestsWithOptionsSkipsClassifiedStartErrors)$",
        "./internal/runtime/runtimetest"
      ],
      "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/floor-logs/floor-9.log",
      "exit_code": 0
    },
    {
      "package": "internal/runtime/tmux",
      "tests": [
        "TestGetKeyBinding_CapturesUserBinding",
        "TestGetKeyBinding_SkipsGasTownBindings",
        "TestIsGTBinding_DetectsGasTownBindings",
        "TestSetBindings_PreserveFallbackOnRepeatedCalls"
      ],
      "skip_reasons": [
        {
          "package": "internal/runtime/tmux",
          "test": "TestGetKeyBinding_CapturesUserBinding",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/integration-packages-5a078b24388ef3c79748.log",
          "reason": "tmux_test.go:3663: not inside tmux \u2014 need server for bind-key",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/tmux",
          "test": "TestGetKeyBinding_SkipsGasTownBindings",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/integration-packages-15c091b746137e5c2921.log",
          "reason": "tmux_test.go:3637: not inside tmux \u2014 need server for bind-key",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/tmux",
          "test": "TestIsGTBinding_DetectsGasTownBindings",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/integration-packages-f1416b63f1c12109ea7e.log",
          "reason": "tmux_test.go:3689: not inside tmux \u2014 need server for bind-key",
          "class": "environment-absence"
        },
        {
          "package": "internal/runtime/tmux",
          "test": "TestSetBindings_PreserveFallbackOnRepeatedCalls",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/integration-packages-15c091b746137e5c2921.log",
          "reason": "tmux_test.go:3719: not inside tmux \u2014 need server for bind-key",
          "class": "environment-absence"
        }
      ],
      "command": [
        "go",
        "test",
        "-tags",
        "integration",
        "-count=1",
        "-v",
        "-run",
        "^(TestGetKeyBinding_CapturesUserBinding|TestGetKeyBinding_SkipsGasTownBindings|TestIsGTBinding_DetectsGasTownBindings|TestSetBindings_PreserveFallbackOnRepeatedCalls)$",
        "./internal/runtime/tmux"
      ],
      "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/floor-logs/floor-10.log",
      "exit_code": 0
    },
    {
      "package": "test/acceptance/helpers",
      "tests": [
        "TestBdLatestSchemaVersionIsolatesHOMEFromSharedServerConfig",
        "TestBdLatestSchemaVersionLeavesNothingInItsTempRoot",
        "TestRealBdProvisionLeavesTheHostHomeAlone"
      ],
      "skip_reasons": [
        {
          "package": "test/acceptance/helpers",
          "test": "TestBdLatestSchemaVersionIsolatesHOMEFromSharedServerConfig",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/unit-2af37a65322c095289c3.log",
          "reason": "bd_schema_parity_test.go:121: bd not available",
          "class": "environment-absence"
        },
        {
          "package": "test/acceptance/helpers",
          "test": "TestBdLatestSchemaVersionLeavesNothingInItsTempRoot",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/unit-2af37a65322c095289c3.log",
          "reason": "bd_schema_parity_test.go:144: bd not available",
          "class": "environment-absence"
        },
        {
          "package": "test/acceptance/helpers",
          "test": "TestRealBdProvisionLeavesTheHostHomeAlone",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/unit-2af37a65322c095289c3.log",
          "reason": "tool_home_test.go:315: no bd available (set GC_ACCEPTANCE_BD_BIN)",
          "class": "environment-absence"
        }
      ],
      "command": [
        "go",
        "test",
        "-tags",
        "integration",
        "-count=1",
        "-v",
        "-run",
        "^(TestBdLatestSchemaVersionIsolatesHOMEFromSharedServerConfig|TestBdLatestSchemaVersionLeavesNothingInItsTempRoot|TestRealBdProvisionLeavesTheHostHomeAlone)$",
        "./test/acceptance/helpers"
      ],
      "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/floor-logs/floor-11.log",
      "exit_code": 0
    },
    {
      "package": "test/integration",
      "tests": [
        "TestStartDrift_SystemdManaged_RestartsToNewBuildID"
      ],
      "skip_reasons": [
        {
          "package": "test/integration",
          "test": "TestStartDrift_SystemdManaged_RestartsToNewBuildID",
          "status": "SKIP",
          "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/logs/integration-full-23490fe556f84649266a.log",
          "reason": "start_drift_test.go:123: XDG_RUNTIME_DIR not set; user systemd not addressable",
          "class": "environment-absence"
        }
      ],
      "command": [
        "go",
        "test",
        "-tags",
        "integration",
        "-count=1",
        "-v",
        "-run",
        "^(TestStartDrift_SystemdManaged_RestartsToNewBuildID)$",
        "./test/integration"
      ],
      "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/floor-logs/floor-12.log",
      "exit_code": 0
    },
    {
      "check": "fsys-darwin-compile",
      "tests": [],
      "skip_reasons": [],
      "command": [
        "make",
        "test-fsys-darwin-compile"
      ],
      "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/floor-logs/floor-13.log",
      "exit_code": 0
    },
    {
      "check": "push-gate-lock-selftest",
      "tests": [],
      "skip_reasons": [],
      "command": [
        "bash",
        "scripts/test-push-gate-lock.sh"
      ],
      "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/floor-logs/floor-14.log",
      "exit_code": 0
    },
    {
      "check": "local-concurrency-selftest",
      "tests": [],
      "skip_reasons": [],
      "command": [
        "bash",
        "scripts/test-local-concurrency.sh"
      ],
      "log": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/floor-logs/floor-15.log",
      "exit_code": 0
    }
  ],
  "log_dir": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/c3-run/evidence/remote/floor-logs"
}
```

Skip audit:

```json
{
  "farm": {
    "rows": 359,
    "unique_tests": 302,
    "site_candidates": 282,
    "all_sites_unchanged": true,
    "shared_hunks_at_skip_sites": 0,
    "classes": {
      "other": 288,
      "environment-absence": 71
    },
    "proof": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/farm-skip-sites-proof.txt"
  },
  "native": {
    "rows": 39,
    "unique_tests": 39,
    "site_candidates": 36,
    "all_sites_unchanged": true,
    "shared_hunks_at_skip_sites": 0,
    "classes": {
      "environment-absence": 29,
      "other": 10
    },
    "proof": "/var/tmp/deploy-ga-ecj965x-fresh-tywyiq1j/native-skip-sites-proof.txt"
  },
  "diff_shared_hunk": "Added startup-prompt tests/helpers contain no skip, TestMain/init change, subprocess, sleep or listener. The fakeState startupPromptFn defaults nil and its added method returns unchanged hints in existing fixtures. New launch delivery maps are cloned and written into runtime.Config, not the parent test process environment. No changed helper or production path changes the retained environment/platform/deferred skip gates; each exact site is checked separately below."
}
```

## Changed tests by name

- `cmd/gc :: TestBuildPrimeContextForReadsSessionIdentityFromTheEnvironment` — PASS by name.
- `cmd/gc :: TestPlanStartupPromptTruthTable` — PASS by name.
- `cmd/gc :: TestPrimeHookWarnsWhenItIsTheSoleCarrierOfALargePrompt` — PASS by name.
- `cmd/gc :: TestStartupPromptAPIWorkerResumeOfLargePromptSuppressesTheHookPrompt` — PASS by name.
- `cmd/gc :: TestStartupPromptContractEmptyPromptDeliversNothing` — PASS by name.
- `cmd/gc :: TestStartupPromptContractEveryLaunchBuilderDeliversPrompt` — PASS by name.
- `cmd/gc :: TestStartupPromptContractEveryLaunchableStateGetsThePrompt` — PASS by name.
- `cmd/gc :: TestStartupPromptContractNothingToDeliverLeavesHintsAlone` — PASS by name.
- `cmd/gc :: TestStartupPromptContractRendersSessionIdentityNotCallerEnvironment` — PASS by name.
- `cmd/gc :: TestStartupPromptContractRuntimeThatCannotCarryANudgeIsLeftAlone` — PASS by name.
- `cmd/gc :: TestStartupPromptContractWorkerResumeCarriesPromptInNudgeNotArgv` — PASS by name.
- `cmd/gc :: TestStartupPromptContractWorkerResumeLeavesCallerEnvUntouched` — PASS by name.
- `cmd/gc :: TestStartupPromptPlanApplyOwnsPayloadAndMarker` — PASS by name.
- `internal/api :: TestResolveWorkerSessionRuntimeAsksStateToDeliverTheStartupPrompt` — PASS by name.
- `internal/api :: TestResolveWorkerSessionRuntimeSurfacesStartupPromptFailure` — PASS by name.

## Fast lanes, runtime and load

Pinned lint/format, shell guards and11 policy targets PASS. make bazel-sync followed by git diff --exit-code and clean tracked/untracked status PASS, then all seven generated drift targets PASS. OpenAPI breaking check PASS. Each lane runs in a fresh committed view before runtime/classifier/suite launch. The15 changed tests include both API resolver cases; the dashboard and actual preview supplement the canonical fresh unit coverage.

ICU74 global loader/hashes, rootless Podman socket and pinned bd preflight pass. Isolation reports PASS-THROUGH/NOT ISOLATED and retains ambient-name disclosures; no clean-environment claim, library override, overlay or tool shim. Remote admission25GiB/cap2; the native floor retains its prescribed local-heavy soft wait. No downgrade occurred. Preview uses its own40GiB/local-heavy admission.

- remote-host: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=23.39 run_max_load=26.57 run_mean_load=18.38 run_readings=53 wait_first_load=23.39 wait_max_load=23.39 wait_mean_load=23.39 wait_readings=1 read_errors=0`
- native-floor: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=60 wait_timed_out=0 run_start_load=14.89 run_max_load=23.34 run_mean_load=20.85 run_readings=8 wait_first_load=15.92 wait_max_load=15.92 wait_mean_load=15.43 wait_readings=3 read_errors=0`
- dashboard: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=870 wait_timed_out=0 run_start_load=14.78 run_max_load=21.04 run_mean_load=21.04 run_readings=1 wait_first_load=23.92 wait_max_load=23.92 wait_mean_load=17.15 wait_readings=31 read_errors=0 prewait_s=870`

Setup history retained: the initial C6 reservation directory violated materialize_merge_tree's absent-path precondition and stopped before checkout/build/test; only that empty, unregistered reservation was removed, and ready-run completed on the same pinned source/base. The initial supplementary dashboard command reached an eight-minute remote-admission timeout with explicit no command ran; ready-run used the documented longer wait, unchanged cap/floor, and then passed. No failed candidate test verdict was retried or attributed.
