# Release gate: tmux cleanup across mount namespaces

**Verdict:** **PASS**

Bead: ga-70xvj1. Build: ga-wz67y1. Review: ga-g3zll3.
Reviewed source: c70d0a12d034f4a5884a35ad4a337ac50e9d4181
Pinned base: f4196f0b538df37f306a8409c25efd8fdecda0c8
Canonical merge tested: f6574f1885999f07bce1c287d229c68e5b36fa56
Canonical tree tested: 078c3b4c5fea8b16ba2de4e63a4f18ca47176595
Deploy mode: remote. Base ref: origin/main. No waiver.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS | PASS | Full review ga-g3zll3 pins the resolved source; unresolved HIGH findings: 0. |
| 2 | Acceptance criteria | PASS | Both owned private-tmp namespace regressions actually PASS in unit and integration; root-present/missing/relative/exited cases PASS. Real //cmd/gc:gc_test passes with new source registered. Local Bazel path (a), mayor ruling gm-wisp-v0vf6d5; required PR-side check remains an exact-head merge prerequisite. |
| 3 | Full required coverage | PASS | Full commands and retained PASS/FAIL/SKIP counts below. All four owned roots/descendants and all 13 leak-guard consumers PASS in both applicable lanes. One unrelated acceptance failure is attributed under all four clauses; the raw FAIL remains recorded. |
| 4 | No open HIGH finding | PASS | Review reports HIGH=0; two minor observations remain nonblocking. |
| 5 | Clean final source | PASS | Canonical scratch has no tracked/untracked changes. Deploy branch adds only this evidence record; clean-tree check required again after commit. |
| 6 | Main divergence | PASS | Full-suite merge f6574f1885999f07bce1c287d229c68e5b36fa56 built 1219 targets. P0-specific ruling gm-wisp-nsjlvxh permits a fresh supplemental merge a569a0074203c25d17df8f50f4413f336e6b2ac1 on 52b95a13ab6e96b03229c0c05c7370358ad51c9a: clean merge, no owned-file overlap except clean BUILD; normal hooks and all 1220 targets build/nogo, FAST18, all 13 tmux regression roots PASS including both bwrap tests. One pre-publication supplemental check per mayor clarification gm-wisp-fi0sgmb; later clean main drift is covered by required PR CI and actual merge-group CI, not another local re-gate. |
| 7 | One feature theme | PASS | Four source files change one test-harness tmux cleanup namespace boundary. Confirmed build bead IDs only; ancestry scope check required again before branch cut. |

## Required local lanes

FAST LANE FIRST: 18 uncached first-attempt targets PASS (11 policy/lint, six generated drift, standard OpenAPI input check); BUILD regeneration has zero drift. Every fast lane used its own fresh tree.

Required-CI accounting: retained exact path-filter/runner-policy evidence in ga-70xvj1-rekey.ci-accounting/ACCOUNTING_VERIFIED.json. No CI-config diff. Required bazel test (side-by-side) PR check is read on the final PR head before clearance. MPR must require that check AND the merge queue run on the actual merge group, per gm-wisp-nsjlvxh. These are the required full-combination checks at merge time; the supplemental local run is not labelled a full-suite run on current main.

- Unit: make check / bazel test //... (config ci + fork-cache).
- Acceptance: bazel test --config=acceptance //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests.
- Integration packages: bazel test --config=integration //test:integration_packages.
- Integration smoke: bazel test --config=integration-smoke //test/integration:integration_test (documented evidence-only lane).

test_cmd_scope: full-suite. All test attempts uncached, run=1/attempt=1, jobs=4. No package or test-name narrowing beyond the documented integration-smoke configuration. First unit/acceptance chain ended with the preserved attributed failure; a separate detached service executed only the remaining full integration lanes.

| Lane | Targets | Raw terminal PASS | FAIL | SKIP | Top-level PASS | FAIL | SKIP |
|---|---:|---:|---:|---:|---:|---:|---:|
| unit | 233 | 54731 | 0 | 210 | 29745 | 0 | 161 |
| acceptance | 7 | 409 | 1 | 13 | 115 | 1 | 10 |
| integration-packages | 26 | 36302 | 0 | 53 | 19007 | 0 | 43 |
| integration-smoke | 1 | 16 | 0 | 1 | 13 | 0 | 1 |

## Failure attribution

failure_attribution: TestProxiedSuspensionIsQuiescenceSuspendedCity -> ga-v3bt9i; designated fix ga-3xnq72.

- Clause 1: exact line 219 and complete function/file unchanged; no shared hunks in failing file.
- Clause 2: tracker created/opened in discovering run under gm-sf3238 escape with landed clause-3 mechanism proof and clear clauses 1/4 (complete controlling non-diff-owned failure protocol). Never claimed to predate the run.
- Clause 3(a): real acceptance_a import closure excludes cmd/gc. Acceptance’s external normal gc executable has unchanged production inputs and unchanged gc_lib/gc BUILD definitions. Only gc_test.srcs and _test.go helpers change; none compile into acceptance or normal gc.
- Clause 4: failing test package has no diff path overlap. No new target or census bump.

Raw symptom: seven bd calls after pair drain during a three-minute suspended-city window; not an initial drain deadline, and no load causality claimed. Original log/XML/BEP hashes retained; no failure rerun or erased.

## Skips and environment

Every skip row has an unchanged-site proof, source-reason review, computed binary import closure, and shared-hunk explanation where applicable. All 104 unit-deferred process rows have actual matching integration-package PASS. Both bwrap-owned namespace tests report PASS, never SKIP. Optional legacy executable, registry, platform/profile and existing placeholder skips are recorded individually and are not counted as coverage.

Pinned bd 1.3.1, Dolt 2.2.0, private ICU74 overlay, rootless Podman socket and enabled testcontainer sweep verified before suite. Ryuk disabled only with sweep and BEADS_ALLOW_UNREAPED_TESTCONTAINERS=1. Gas City isolation wrapper was invoked; it is a pass-through on this rig. Bazel fork-cache uses local sandbox execution; this is not RBE execution.

## Host load

Initial full chain:
```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1800 wait_timed_out=1 run_start_load=82.20 run_max_load=118.77 run_mean_load=82.92 run_readings=55 wait_first_load=51.33 wait_max_load=82.20 wait_mean_load=56.53 wait_readings=61 read_errors=0
```
Remaining integration continuation:
```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1800 wait_timed_out=1 run_start_load=23.93 run_max_load=33.62 run_mean_load=30.76 run_readings=31 wait_first_load=88.70 wait_max_load=90.17 wait_mean_load=45.59 wait_readings=61 read_errors=0
```

## P0 current-main supplemental check

Ruling: gm-wisp-nsjlvxh. Reviewed source remains c70d0a12d034f4a5884a35ad4a337ac50e9d4181. Supplemental base 52b95a13ab6e96b03229c0c05c7370358ad51c9a; merge a569a0074203c25d17df8f50f4413f336e6b2ac1; tree 4275732cbe3b35f17ef931f194b70c5cd27de28f.
Normal hooks/all-target build: 1220 targets PASS. FAST: 18 targets PASS. All 13 roots from the three tmux files, including both private mount namespace tests, PASS with no FAIL or SKIP; raw terminal counts {'PASS': 25, 'FAIL': 0, 'SKIP': 0}.
test_cmd_scope: focused supplemental freshness check. It does not replace the completed full-suite chain on f4196f0b538df37f306a8409c25efd8fdecda0c8. No full-suite PASS on the supplemental main base is claimed.

Drift since the full-suite base:
```text
M	.bazelversion
M	.github/actions/setup-bazel/action.yml
M	MODULE.bazel.lock
M	cmd/gc/BUILD.bazel
M	cmd/gc/bd_env.go
M	cmd/gc/class_store_cache_test.go
M	cmd/gc/cmd_convoy_dispatch.go
M	cmd/gc/cmd_convoy_dispatch_test.go
M	cmd/gc/control_dispatch_graph_store_test.go
M	cmd/gc/control_rig_graph_federation_test.go
A	cmd/gc/dispatch_bd_call_counter.go
A	cmd/gc/dispatch_bd_call_counter_test.go
M	cmd/gc/dispatch_control_ready.go
M	cmd/gc/dispatch_control_ready_freshness_test.go
M	cmd/gc/dispatch_control_ready_hold_label_test.go
M	cmd/gc/dispatch_control_ready_leak_test.go
M	cmd/gc/dispatch_control_ready_test.go
M	cmd/gc/dispatch_control_ready_wal_test.go
A	cmd/gc/dispatch_execution_emit.go
A	cmd/gc/dispatch_execution_emit_test.go
M	cmd/gc/dispatch_runtime.go
M	cmd/gc/dispatch_runtime_hold_label_test.go
M	cmd/gc/dispatch_runtime_quiet_retry_test.go
M	cmd/gc/graph_dispatch_mem_test.go
M	cmd/gc/provider_factory_census_test.go
M	cmd/gc/reconcile_stop_keys_test.go
A	engdocs/design/bazel-remote-repo-contents-cache.md
M	engdocs/design/index.md
M	internal/beads/BUILD.bazel
A	internal/beads/children_batch.go
A	internal/beads/children_batch_test.go
M	internal/beads/closeorder/BUILD.bazel
M	internal/beads/closeorder/closeorder.go
A	internal/beads/closeorder/closeorder_test.go
A	internal/beads/closeorder/testenv_import_test.go
A	internal/beads/doltlite_read_store_capabilities.go
M	internal/beads/exact_batch_get.go
M	internal/beads/membership.go
A	internal/beads/update_reporting.go
A	internal/beads/update_reporting_test.go
M	internal/dispatch/BUILD.bazel
A	internal/dispatch/call_budget_test.go
M	internal/dispatch/control.go
A	internal/dispatch/crash_injection_test.go
M	internal/dispatch/ralph.go
M	internal/dispatch/ralph_test.go
M	internal/dispatch/retry.go
M	internal/dispatch/retry_test.go
M	internal/dispatch/runtime.go
M	internal/dispatch/runtime_test.go
M	internal/molecule/cleanup.go
M	internal/molecule/cleanup_test.go
M	internal/session/BUILD.bazel
A	internal/session/fields.go
A	internal/session/fields_test.go
M	internal/worktree/testenv_import_test.go
M	scripts/bazel_multilane_test.go
M	scripts/residency-boundary-baseline.txt
M	test/integration/BUILD.bazel
A	test/integration/bdstore_batch_reads_test.go
M	test/integration/bdstore_test.go
```
Supplemental host load:
```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=60 wait_timed_out=1 run_start_load=74.59 run_max_load=NA run_mean_load=NA run_readings=0 wait_first_load=80.11 wait_max_load=80.11 wait_mean_load=76.75 wait_readings=3 read_errors=0
```
Supplemental evidence: /var/tmp/deploy-ga-70xvj1-narrow.083i2miv/NARROW_RECHECK_VERIFIED.json.

## Publication freshness ruling

Mayor clarification gm-wisp-fi0sgmb, read in full and confirmed in the live bead: finish the ONE supplemental check at 52b95a13ab6e96b03229c0c05c7370358ad51c9a, then publish the reviewed source immediately even if main changes again. Later clean drift is deferred to required PR side-by-side and actual merge-group CI; a GitHub conflict or required-check failure that the combination could cause requires notifying mayor before another supplemental check. No waiver and no current-main full-suite PASS claimed.

## Retained evidence

Evidence root: /var/tmp/deploy-ga-70xvj1-rekey.64vwghn0
Test log directory: /var/tmp/gc-heavy-gate/runs/ga-70xvj1-rekey.c3/logs
Canonical C6 proof: C6_VERIFIED.json; FAST proof: ga-70xvj1-rekey.fast/FAST_VERIFIED.json.
Full raw manifests/logs/XML/BEP: unit-artifacts.json, acceptance-artifacts.json, integration-packages-artifacts.json, integration-smoke-artifacts.json under the run directory.
Full verifier: SUITE_MANIFEST_VERIFIED.json; terminal-result-inventory.json; per-lane skip-causal-review.json; PROCESS_COMPANIONS_VERIFIED.json.
Failure proof: ACCEPTANCE_FAILURE_ATTRIBUTION.json under evidence root. Original first-chain failed RESULT retained alongside successful remaining-chain RESULT.

Publication checks: exact reviewed source ancestry, isolated branch target, current-main freshness, clean committed tree, PR own-head/ownership/engagement, final-head required CI read, and clearance status equality are checked before merge-request. The deployer does not merge.
