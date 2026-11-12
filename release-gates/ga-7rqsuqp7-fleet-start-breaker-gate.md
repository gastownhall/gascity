# Fleet start breaker release gate

**Verdict:** **PASS**

- bead: ga-7rqsuqp7; build: ga-riekfj; review: ga-dmmf9oe3.
- recorded_at: 2026-11-11T06:45:53.306463+00:00.
- reviewed_commit: `99c06918b5240805073abf8aa0893ca0f90e7040`; deploy source is this immutable reviewed commit.
- merge_state: clean (`origin/main@607e3570e3fa2409cb6d216c36a243b35a99d6fb`); conflicts: none.
- gate_base_kind: current-tip; gate_base_sha: `607e3570e3fa2409cb6d216c36a243b35a99d6fb`.
- merge_head: `bdd487e29a7ea7ce5f06f719beb993c5db3d7038`; gated_tree: `a5b0529c9734b6e82793e8ba4ab64d4ea454ea9a`.
- merge_validation: fresh synthetic merge built and vetted; mpr validates the merge-time head and CI. No deployer rebase or merge.
- retained_evidence: `/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-7rqsuqp7/pass-20261111T064553Z`; original logs remain at `/var/tmp/deploy-ga-7rqsuqp7-fresh-nnj7lnl1`.
- policy_source: `docs/PROJECT_MANIFEST.md` is absent; canonical commands come from `TESTING.md`, `Makefile`, `.github/workflows/ci.yml` and the release-gate protocol.

| # | Criterion | Result and evidence |
|---|---|---|
| 1 | Review PASS | PASS; live ga-dmmf9oe3 verdict binds the resolved reviewed commit; no carryover or reviewer-added test exception. |
| 2 | Acceptance met | PASS; trigger/classification, probe fairness and stable confirmation, both start paths, wake accounting, pending-create lease bounds, operator exemption, visibility/reset, default/kill switch, cache staleness and published boundaries verified; `acceptance-precheck.json`. PR disclosures are preserved in the prepared body and must be read back after opening. |
| 3 | Tests and required lanes | PASS; full suite and native floor, 69 package-bound owned roots, required job accounting and skip proofs below. |
| 4 | Review findings | PASS; zero unresolved HIGH findings in the live PASS review. |
| 5 | Clean branch | PASS; reviewed and validation trees clean; the isolated deploy adds only this gate record. Publication must verify clean status after committing it. |
| 6 | Build/vet and merge state | PASS; clean synthetic merge; remote `bazel build //...` and nogo vet, 1481 successful completions each; `c6-verified.json` and raw C6 BEPs. |
| 7 | Single theme | PASS; fleet start breaker and its required config, visibility, cache observation, generated references and regression tests. Accepted lineage: ga-riekfj, ga-biw7e2r7, ga-zfl8sbo, ga-3ck2pnh, ga-7rqsuqp7. No stack. |

## Suite evidence

- test_cmd: `/home/jaword/projects/gc-management/packs/actual/all/scripts/gate-criterion3.sh --repo /var/tmp/deploy-ga-7rqsuqp7-fresh-nnj7lnl1/merge --base 607e3570e3fa2409cb6d216c36a243b35a99d6fb --bead-json /var/tmp/deploy-ga-7rqsuqp7-fresh-nnj7lnl1/ga-7rqsuqp7.json --owned /var/tmp/deploy-ga-7rqsuqp7-fresh-nnj7lnl1/owned.tsv --run-dir /var/tmp/deploy-ga-7rqsuqp7-fresh-nnj7lnl1/c3-run/evidence`; full unit/acceptance/integration-package/full-integration/latency chain.
- test_cmd_scope: full-suite; heavy_mode: none.
- test_counts: 150948 PASS, 0 FAIL, 515 SKIP (top-level Go result lines summed over 550 canonical logs; repeated jobs count separately; subtests excluded).
- diff_tests_executed: 69/69 changed package/test roots PASS; 67 distinct names, zero owned SKIP/FAIL/MISSING. `owned.tsv`, `owned-explain.txt`, `owned-package-verified.json`, per-package canonical reader outputs.
- waiver_ref: none; failure_attribution: none in this fresh run. Prior incomplete run is not evidence for this verdict.
- test_exec_mode: remote-exec; fallback_reason: None; rbe_state: certificate valid, kill switch absent, tree/main worker-environment hashes match (`execution.json`).
- test_exec_bep: 534 first-attempt uncached remote results, 353 terminal PASSED target summaries; every start inside its phase window, no cache flags/local strategy/retries. Raw predicates recomputed; 1068 CAS artifact hashes and 534 normalized logs verified.
- admission: remote, floor 25.0 GiB, cap 2, waited 0.001 seconds; farm_test_seconds: 13092.177.
- latency_leg: both declared latency tests PASS under clear farm probe; ordinary integration excluded them. `remote-early-audit.json` and `integration-latency.*`.
- owned_topup: none; floor_topup: all 16 prescribed steps exit 0, including beadstest, Darwin compile, runner self-tests and every remote environment-absence package. Native results supplement the remote suite under the prescribed methodology.
- test_log_dir: `/var/tmp/deploy-ga-7rqsuqp7-fresh-nnj7lnl1/c3-run/evidence/remote/logs`; per-job native logs retained in `floor-logs`.
- ci_lane_run: n/a (no CI-config change).

| Phase | Fresh remote action results | PASSED target summaries | Result |
|---|---:|---:|---|
| unit | 408 | 306 | PASS |
| acceptance | 25 | 8 | PASS |
| integration-packages | 86 | 37 | PASS |
| integration-full | 14 | 1 | PASS |
| integration-latency | 1 | 1 | PASS |

## Fast prerequisites and skips

- policy_lane: PASS; pinned formatting, full nogo, release/routed/split/residency/hook guards and 13 policy/docsync targets.
- drift_lane: PASS; `make bazel-sync`, empty tracked/untracked diff and seven schema/OpenAPI/client/dashboard/lockfile drift targets; separate OpenAPI compatibility target PASS. Each lane used a fresh tree before the suite.
- strict_storeclass: PASS; trusted lint, baseline and zero-growth predicates; `storeclass-proof/verified.json`.
- skip_inventory: 501 remote and 39 native skip events (includes subtests; distinct from the top-level tally). All non-owned, reasoned and at unchanged source sites. All reachable shared/helper hunks named in the closure proof and compared to the frozen base with identical skip reasons; remaining closures cannot reach those hunks. `skip-proof-automated.json`, `skip-manual-reviewed.json`, `skip-comparisons/`.
- skip_justification: existing platform/privilege/provider capability exclusions, unit/process lane splits, opt-in live-service/fixture/manual-measurement gates and intentional skip-contract fixtures. Native environment top-ups ran first; none skips a changed test. One additional non-Go bridge lockfile subtest is configured inapplicable (`rules_ts_typescript=false`); source/data unchanged and dashboard counterpart runs all three. `non-go-skip-reviewed.json`.

## Required CI jobs

- ci_required_closure: `python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/deploy-ga-7rqsuqp7-fresh-nnj7lnl1/merge 607e3570e3fa2409cb6d216c36a243b35a99d6fb 99c06918b5240805073abf8aa0893ca0f90e7040 fork`; workflow blob `fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612`; 6 LEAF jobs. Full command/output: `ci-required/closure.command.json` and `closure.stdout`.
- windows_reach: none for both Windows runtime jobs; their own closures computed separately. The Linux-host Windows and macOS compile commands passed.
- deferred_to_pr_ci: windows-lane-test (non-linux-runner), credential-provider-windows (non-linux-runner).
- not_covered: none.

| Job | ci_required_accounting | Evidence |
|---|---|---|
| changes | LOCAL-PASS | ci-required/closure.stdout |
| runner-policy | LOCAL-PASS | ci-required/runner-policy.stdout |
| windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | ci-required/windows-native-reach.json |
| credential-provider-windows | DEFERRED-TO-PR-CI reason=non-linux-runner | ci-required/credential-windows-reach.stdout |
| windows-lane | LOCAL-PASS | run-ci-job.py windows-lane via the recorded fresh-tree/load/isolation chain; exit=0; ci-required/windows-lane.command.json; ci-required/windows-lane.stdout |
| pack-gate | LOCAL-PASS | run-ci-job.py pack-gate via the recorded fresh-tree/load/isolation chain; exit=0; ci-required/pack-gate.command.json; ci-required/pack-gate.stdout |

The job commands, environments and working directories match the frozen workflow, run sequentially behind fresh-tree/load/isolation wrappers. `ci-required/accounting-verified.json` binds them to this tree. Gate PASS authorizes PR opening; merge requires `CI / required` and every deferred job green on the exact PR head.

## Host load and deployment boundary

Local legs used threshold 15 and the bounded 1800-second soft wait. Criterion-3 native floor: load_threshold=15, load_waited_seconds=1800, load_wait_timed_out=1, run_start_load=15.95, run_max_load=15.18, run_mean_load=14.17, run_readings=6. Exact observed load summaries (including timeouts) are retained below; the outer remote controller reports aggregate load without waiting, and remote admission requires no host-heavy wait.

- `c3-run/evidence/remote/floor-topup.stdout`: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=1800 wait_timed_out=1 run_start_load=15.95 run_max_load=15.18 run_mean_load=14.17 run_readings=6 wait_first_load=79.39 wait_max_load=82.61 wait_mean_load=57.66 wait_readings=61 read_errors=0`
- `ci-required/pack-gate.stderr`: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=1290 wait_timed_out=0 run_start_load=13.98 run_max_load=14.55 run_mean_load=14.41 run_readings=3 wait_first_load=17.46 wait_max_load=59.64 wait_mean_load=31.16 wait_readings=44 read_errors=0`
- `ci-required/windows-lane.stderr`: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=13.33 run_max_load=17.86 run_mean_load=16.82 run_readings=3 wait_first_load=13.33 wait_max_load=13.33 wait_mean_load=13.33 wait_readings=1 read_errors=0`
- `skip-base-evidence/native.stderr`: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=9.57 run_max_load=10.00 run_mean_load=9.77 run_readings=2 wait_first_load=9.57 wait_max_load=9.57 wait_mean_load=9.57 wait_readings=1 read_errors=0`
- `skip-base-evidence/owned-skip-acceptance.stderr`: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=13.21 run_max_load=16.20 run_mean_load=15.48 run_readings=5 wait_first_load=13.21 wait_max_load=13.21 wait_mean_load=13.21 wait_readings=1 read_errors=0`
- `skip-base-evidence/owned-skip-integration-full.stderr`: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=10.19 run_max_load=9.65 run_mean_load=9.65 run_readings=1 wait_first_load=10.19 wait_max_load=10.19 wait_mean_load=10.19 wait_readings=1 read_errors=0`
- `skip-base-evidence/owned-skip-integration-packages.stderr`: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=16.02 run_max_load=15.54 run_mean_load=12.96 run_readings=7 wait_first_load=16.02 wait_max_load=16.02 wait_mean_load=16.02 wait_readings=1 read_errors=0`
- `skip-base-evidence/owned-skip-unit.stderr`: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=12.16 run_max_load=13.40 run_mean_load=13.23 run_readings=2 wait_first_load=12.16 wait_max_load=12.16 wait_mean_load=12.16 wait_readings=1 read_errors=0`
- `storeclass-proof/wrapper.stderr`: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=61.52 run_max_load=60.15 run_mean_load=55.58 run_readings=4 wait_first_load=35.14 wait_max_load=71.97 wait_mean_load=44.80 wait_readings=61 read_errors=0`
- `c3-run/units/gate.w1.stderr.log`: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=44.34 run_max_load=102.77 run_mean_load=56.49 run_readings=112 wait_first_load=44.34 wait_max_load=44.34 wait_mean_load=44.34 wait_readings=1 read_errors=0`

C13: disposable gate builds only; no installation, live binary replacement, supervisor restart, live-city smoke, or comment on issue #7170 / PR #6164. The breaker starts closed after restart; restart seeding is not built. v2 not covered; tracked on ga-3yqaa8.
