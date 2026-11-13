# Push ownership ID release gate — ga-kk6dha5

**Verdict:** **PASS**

Evaluated 2026-10-29T12:38:39.664558+00:00. Deploy source `ca29ad2542ec5ba45b15ec708628af24c2cf85c0`; provenance `builder/ga-oomi8sp` is not a push target. Review ga-mc4zgc3 PASS, build ga-oomi8sp. Two source commits, both cited to the confirmed build bead, change only `scripts/push-ownership-guard.sh` and its shell regression script. No waiver or failure attribution.

merge_state: clean (origin/main@4a4921a60f684602f423a0ddcc882d54fdbf2ebe)
gate_base_kind: current-tip
gate_base: 4a4921a60f684602f423a0ddcc882d54fdbf2ebe
merge_head: 80ebb599dcde538105ad83b4433132b350b95c48
merge_tree: 208f039cac4f4ae616db2eb8300b4c01fd483597
merge_validation: normal hooks materialized the clean merged tree; whole-repo build and nogo/vet passed on that exact tree. Merge authority must validate the merge against its current base. The reviewed source was never rebased or rewritten.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Reviewer PASS | PASS | ga-mc4zgc3 records PASS on the resolved deploy source; no review carryover or reviewer-added commit. |
| 2 | Acceptance criteria | PASS | Whole IDs resolve for seven-character roots and dotted sub-beads, and six-character legacy IDs. Comment matches the widened expression. All three new tests pass by name; the old-guard mutation fails exactly the four seven-character observations. All scripts Bazel targets pass in the full unit phase. |
| 3 | Full CI-equivalent tests | PASS | Standard gate-criterion3.sh completed all five forced remote phases and required native floor; all owned results pass, fresh attempt 1, zero cache credit. Details below. |
| 4 | Unresolved high review findings | PASS | Zero; reviewer style, security and spec records contain no gating finding. |
| 5 | Clean branch | PASS | Merge scratch and deploy checkout checked clean; this record is the sole deploy-stage addition. Normal pre-commit hook runs when committing this record. |
| 6 | Merge state and build/vet | PASS | Clean merge above; 1395 whole-repo build targets and 1395 nogo/vet targets pass; hooks ownership checked. |
| 7 | One feature theme | PASS | Branch-derived bead ID resolution and its regression coverage; no unrelated source or internal configuration. |

## Acceptance evidence

- `test_bead_id_branch_resolves_seven_char_id`: PASS, both no-warning/matching-assignment and full-ID observations.
- `test_bead_id_branch_resolves_seven_char_subbead_id`: PASS, both observations.
- `test_bead_id_branch_resolves_six_char_legacy_id`: PASS, both observations.
- Full shell harness: 46 PASS, 0 FAIL, 0 SKIP. The hermetic original-guard mutation reports 42 PASS, 4 expected FAIL, 0 SKIP; only the two seven-character cases fail their two observations. The mutation is separate acceptance proof, never a failed suite retried into PASS. Guard restored and fresh view clean.
- Full-suite Go wrapper `TestPushOwnershipGuard`: PASS by name. Mechanical Go ownership inventory is empty (shell-only diff); the three added shell function bodies were inspected manually and all are called by run_all. Existing six-character and multi-level suffix regressions remain green.
- The expression consumes the complete alphanumeric run and repeated dotted numeric suffixes; hyphen and slash terminate the ID. Matching assignment produces no false warning. The adjacent comment names this behavior.

## Fast lane and test population

policy_lane: PASS — format check with pinned tool, shell guards and hooks ownership, 13 policy/docs/format/CI-boundary Bazel results. drift_lane: PASS — make bazel-sync, empty git diff/status, seven schema/spec/client/dashboard/lockfile drift results, and OpenAPI breaking check (one result). Each lane ran in its own fresh view through gate-base.sh on the pinned base; all 21 test results were fresh remote attempt 1 and all 42 raw artifacts were rehashed. No fast lane edited the suite scratch.

heavy_mode: none
ci_lane_run: n/a (no CI configuration change)
test_cmd_scope: full-suite
test_cmd: standard gate-criterion3.sh, with make check (bazel test //...), acceptance, integration-packages, full integration minus latency names, then the isolated clear-probe latency leg. All phases force --nocache_test_results and --noremote_local_fallback. The required native floor supplements the farm population.
test_exec_mode: remote-exec (derived from BEP)
fallback_reason: none
farm_test_seconds: 8943.623
test_counts: 133508 PASS, 0 FAIL, 373 SKIP (top-level Go result lines across retained logs; repeated jobs counted separately, subtests excluded)
diff_tests_executed: three shell tests PASS by name, parent TestPushOwnershipGuard PASS in the full suite
waiver_ref: none
failure_attribution: none

| Phase | Fresh remote result records | Result |
|---|---|---|
| acceptance | 24 | PASS |
| integration-full | 14 | PASS |
| integration-latency | 1 | PASS |
| integration-packages | 85 | PASS |
| unit | 375 | PASS |

All 499 records use remote strategy, neither cached flag, attempt 1, starts within the phase window and terminal passing verdicts. The 998 raw farm log/XML hashes were independently checked. Native floor complete: every recorded step exits zero, including beadstest, Darwin compile, runner self-tests and packages selected for environment-absence skips. No failed or partial native step was retried.

latency_leg: measured; attempt 1, clear farm probe, both budget tests PASS. No deferral.
owned_topup: []
floor_topup: 16 steps, complete=true; manifests and individual logs retained.
skip_inventory: farm 359 rows, native 39 rows. Every retained skip site checked UNCHANGED with zero shared hunks against the exact base/tree. Existing platform and mode exclusions are unchanged. Farm environment-absence skips receive the required native package top-up; remaining non-owned skips are inventoried with reasons. The shell-only resolver change touches no Go skip condition, helper, import, TestMain or build constraint.

admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 41.84998321533203, "waited_seconds": 5.003}
rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98", "worker_env_tree": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"}
Native load record (ordinary threshold and bounded soft wait; run/wait readings kept separately):

```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1470 wait_timed_out=0 run_start_load=14.53 run_max_load=31.94 run_mean_load=18.88 run_readings=8 wait_first_load=56.17 wait_max_load=78.86 wait_mean_load=48.07 wait_readings=50 read_errors=0
```

The independent shell proof also used local-heavy40GiB admission and the ordinary threshold15/1800s soft load wait. Its exact admission, load and mutation output are retained. Capacity waiting changed no admission cap, floor or timeout.

test_log_dir: /var/tmp/deploy-ga-kk6dha5-swb_elv1/c3-run/evidence/remote/logs
Native per-step logs: /var/tmp/deploy-ga-kk6dha5-swb_elv1/c3-run/evidence/remote/floor-logs
Evidence root: /var/tmp/deploy-ga-kk6dha5-swb_elv1; context.json, C6_VERIFIED.json, FAST_VERIFIED.json, independent-phases.json, execution.json, retained BEP/log/XML, load summaries, skip-audit.json/site proofs and supplementary-shell-results.json. Logs preserved after scratch cleanup.

The PR branch is mechanically derived as `deploy/ga-kk6dha5-gate`, freshly cut at the exact reviewed source with only this gate-record commit added. Clearance and merge-request must name its verified exact head. Merge authority is mpr; deployer does not merge.

## Test environment and supplementary load

The standard wrapper ran in PASS-THROUGH mode for Gas City; each check used its designated scratch or fresh view. The wrapper pinned bd v1.3.2; native floor commands scrubbed live store/session selectors. Rootless Podman was configured before the suite. Host ICU check, recorded before criterion 3:

```text
GATE_ICU74 lib=libicui18n.so.74 file="/usr/local/lib64/icu74-compat/libicui18n.so.74.2" sha256=d90d62f0d7b6c6fbc279ae5e433cc7004aa47cdb106a71a02350099f8df9ae51 recorded=yes
GATE_ICU74 lib=libicuuc.so.74 file="/usr/local/lib64/icu74-compat/libicuuc.so.74.2" sha256=33499dd97a5393926b32e0b535f5340f7dae14b35e59025b572e6bd9ad48a2ae recorded=yes
GATE_ICU74 lib=libicudata.so.74 file="/usr/local/lib64/icu74-compat/libicudata.so.74.2" sha256=c891c5fdb45e3cf756b728abe256be7af1bfc97f00ca2f2fac450c96c09bd4b7 recorded=yes
GATE_ICU74 verdict=OK provenance=recorded sonames="libicui18n.so.74 libicuuc.so.74 libicudata.so.74"
```

Remote host load (remote mode has no host-load wait):

```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=80.77 run_max_load=99.18 run_mean_load=55.37 run_readings=88 wait_first_load=80.77 wait_max_load=80.77 wait_mean_load=80.77 wait_readings=1 read_errors=0
```

Supplementary shell admission and load:

```text
{"class": "local-heavy", "floor_gib": 40.0, "free_gib": 42.93229293823242, "waited_seconds": 700.329, "cap": 3, "prewait_s": 1800.777}
GATE_ADMISSION prewait class=local-heavy result=timed_out waited_s=1800 readings=61
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=0 run_start_load=13.92 run_max_load=16.65 run_mean_load=16.65 run_readings=1 wait_first_load=86.85 wait_max_load=98.81 wait_mean_load=67.41 wait_readings=62 read_errors=0 prewait_s=1801
```
