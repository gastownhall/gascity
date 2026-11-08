# Release gate: Nightly aggregate summary

**Verdict:** **FAIL**

**Disposition:** HOLD on the same-store condition tracker `ga-v3bt9i`. The reviewed change is not routed back as an implementation rejection. No push, PR, workflow dispatch, deploy clearance, or merge-request was made.

- Deploy bead: `ga-4nw07a8p`; build lineage: `ga-wsfovk`, `ga-799w0wq`, `ga-clmemp.3`; review: `ga-4wbahtu1` PASS.
- Reviewed source: `bb9a9780aea8d94cd864b9de3d476adbd0b75288` (resolved in git; source branch is provenance only).
- Deploy mode: `remote`; base ref: `origin/main`.
- merge_state: clean (`origin/main@1786ca1ed5fd94d655423151eab9adb190edfa3a`).
- gate_base_kind: current-tip; gate_base: `1786ca1ed5fd94d655423151eab9adb190edfa3a`.
- Canonical merge: `1a8b053d3980629530dcbcd5b188ee1395aed3c9`; tree: `39683f6f3f50452798d661332ab4329d290be157`.
- merge_validation: merge result built and vetted here.
- Evidence directory: `/var/tmp/gc-heavy-gate/runs/ga-4nw07a8p.20261108-2c97qq2p`; canonical scratch: `/var/tmp/gc-merge-4nw07a8p.623hqkyn` (removed after evidence retention).
- Preflight: commit-to-PR API returned no associated PR. Interlock passed; round cap admitted round 2.
- Manifest: `docs/PROJECT_MANIFEST.md` is absent. Mandatory pack release criteria and repository release-gate conventions were read.

| # | Criterion | Verdict | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Fresh review `ga-4wbahtu1` explicitly passes the exact source above; no carryover is substituted. |
| 2 | Acceptance criteria met | FAIL | Structural/rebase obligations and 17 owned root/subtest cases pass. The required independent full suite and first real Nightly run remain incomplete after criterion 3 stops. |
| 3 | Tests pass | FAIL | Canonical full-scope runner exits 1 at acceptance; `criterion3_valid=false`. One unchanged suspended-city test fails; hold protocol applies. Integration, latency and native floor did not run. |
| 4 | No high-severity review findings open | PASS | Review has zero unresolved HIGH findings. |
| 5 | Final branch clean | PASS | Exact source and canonical scratch were clean; only this gate record is committed locally on the isolated evidence branch. |
| 6 | Merge state and build/vet | PASS | Clean canonical merge independently verified against both exact parents and `git merge-tree`; whole-repository Bazel build/nogo/vet passes, 1,456 target completions and zero failed targets. |
| 7 | Single feature theme | PASS | Nightly always-run aggregate, its tests, policy pin, census row, BUILD listing and historical gate record form one theme. |

## Completed checks

- `policy_lane`: PASS — pinned formatting/lint/nogo, required shell guards, hooks, and 14 Bazel policy targets, in a fresh view.
- `drift_lane`: PASS — `make bazel-sync`, empty tracked/untracked diff, and seven generated-file targets, in a separate fresh view.
- `openapi_lane`: PASS — pinned-base OpenAPI compatibility target, in a third fresh view.
- `fast_lane`: PASS before runtime setup or suite launch; canonical merge tree remains unchanged.
- Runtime: ICU74 provenance check OK; rootless Podman socket/pinned Dolt image verified; pinned test bd v1.3.4 with ref match. Wrapper reports PASS-THROUGH on this disposable worktree; no cloned environment is claimed.
- `heavy_mode`: none. Remote rollout was inherited enabled, not enabled by the deployer.
- `test_cmd`: canonical `gate-criterion3.sh --repo <scratch> --base <gate_base> --bead-json bead-current.json --owned owned.tsv --run-dir c3/evidence`, detached through `gate-detached-run.sh`, pinned with `gate-base.sh run`. Phase commands and flags are retained in `c3/evidence/execution.json`.
- `test_cmd_scope`: full-suite (command scope); **execution stopped at acceptance**, so full-suite coverage is not claimed.
- `test_exec_mode`: remote-exec; `fallback_reason`: none; no local restart and no stitching.
- Unit: 301 target summaries / 403 result events PASS. Acceptance: 7 passing / 1 failing target summaries, 24 passing / 1 failing result events. Every one of the 428 result events is remote, uncached, first attempt, within its phase window.
- `test_counts`: **88,539 PASS / 1 FAIL / 399 SKIP**, top-level result lines summed across 428 retained per-job logs. This is partial-suite evidence; repeated jobs count repeatedly and subtests are excluded.
- `test_log_dir`: `/var/tmp/gc-heavy-gate/runs/ga-4nw07a8p.20261108-2c97qq2p/c3/evidence/remote/logs`; read with `gate-test-evidence.py`, never deleted.
- `diff_tests_executed`: `TestNightlyHasOneAlwaysRunAggregateDependingOnEveryJob` PASS; `TestNightlyAggregateFailsForEveryNonSuccessDependency` PASS, including all 15 dependency/conclusion leaf cases. 17/17 named root/subtest cases PASS, zero FAIL/SKIP/MISSING.
- `skip_inventory`: retained per-job logs contain 399 non-owned SKIPs. Canonical native-floor inventory/top-ups were not reached; no skip waiver or complete coverage is asserted.
- `owned_topup`: not needed for the two owned roots, both present in full unit output. `floor_topup`: not run. `latency_leg`: not run. `waiver_ref`: none; Gas City has no waiver path.
- `ci_lane_run`: not run — `nightly.yml` has `workflow_dispatch` and no PR trigger, but pre-PASS dispatch was not reached. No old candidate waiver is inherited.
- `ci_required_closure`, `ci_required_accounting`, `windows_reach`, `deferred_to_pr_ci`, `not_covered`: not evaluated after criterion-3 fail-fast; no CI coverage claim or PR-open authorization.
- Static checks: aggregate needs exactly all five existing jobs, uses `always()`, timeout 5 minutes, passes each `needs.<job>.result` through environment, and fails closed on any non-success. Workflow is pure append; existing jobs are unchanged. The prior routed-row fix is an ancestor; TESTING.md is untouched.

## Failure and hold basis

`TestProxiedSuspensionIsQuiescenceSuspendedCity` fails at `test/acceptance/beads_proxied_suspension_quiescence_test.go:312`: after the initial pair drain, the 30-second suspended-city window records three calls (runtime-lease release update, show, dolt stop). The raw failure log is `c3/evidence/remote/logs/acceptance-d5cadbd9956b31f6437b.log`, SHA256 `bc70e9357be7dffc468f15088c1a5c5513a492065bb0c154d308a80171fc12c4`.

- Clause 1: not diff-owned; exact failure site UNCHANGED, function untouched, zero shared hunks in that file (`failure-ownership.txt`).
- Clause 2: open tracker `ga-v3bt9i` predates this run (2026-10-09); opened and existing comments read. New sighting was appended and read back. No duplicate tracker was filed.
- Clause 3: **INCONCLUSIVE**. No base-ref retry, cross-PR comparison or coverage proof was substituted. Combined acceptance/CLI import closure was computed under acceptance_a/gc_testhooks (190 module-directory rows); the conservative nearest-package rule puts changed root data in a reachable root package. No decisive mechanism exclusion is claimed (`failure-causal-reach.json`).
- Clause 4: failing package/path is unchanged; no same-package non-test edit.
- `inconclusive-guard`: reachable production inputs conservatively present; **added_test_load=yes** (new subprocess census row and workflow test file). Therefore `guard-load` requires escalation to a dependency hold, never attribution.
- Previous #7396 quiescence fix is already in the pinned base; stamped remedy `ga-3xnq72` is closed no-op without a new landed fix. The current deploy's `build_bead=ga-wsfovk` is not fix-carrying, so historical repeat does not supply an exemption.
- `failure_attribution`: none granted. No failure was rerun or erased. Root cause and host-load causation are not asserted.
- Hold class: `guard-or-clause-4`; missing: `guard-load`; same-store blocks edge: `ga-v3bt9i`. Evidence-regime commit: `9e56903a547d58b9731640b51da8d0268917d526` (resolved in the city repo).
- Clear condition: a concrete remedy must LAND, or fresh independent evidence must remove the recorded basis, then re-gate fresh. Repoint the tracker dependency to the new fix bead once one is filed. The bead stays open/unassigned with needs-deploy and its existing route, without a hold label or status=blocked.

## Load and retained execution evidence

`load_threshold=15`, `load_waited_seconds=0`, `load_wait_timed_out=false`, `run_start_load=56.71`, `run_max_load=75.92`, `run_mean_load=61.37`, `run_readings=37`; `wait_first_load=56.71`, `wait_max_load=56.71`, `wait_mean_load=56.71`, `wait_readings=1`. Remote admission bypasses the local load wait. These are the orchestration host's readings, not remote-executor load or a diagnosis of the failure. No native-floor load wait ran.

`admission`: remote, floor 25 GiB, cap 2, free 80.2748 GiB, admission wait 0.001 seconds. `farm_test_seconds`: 4786.386. `test_exec_bep` and `rbe_state` are retained in `c3/evidence/execution.json`, including matching worker-env pins, certificate expiry, and per-phase uncached/remote/attempt/window predicates. Durable C6, preparation and fast services completed; C3 ended failed exactly once. Every raw log/BEP remains under the evidence directory.
