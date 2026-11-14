# Release gate: parallel close snapshot and edge reads

**Verdict:** **PASS**

- Deploy bead: ga-d4szt4v9; build: ga-svwt5ka; review: ga-6o6gcdm0.
- Reviewed commit: `dfdd3261b4b3458a3e0ddad56f3719d74f4c9959` (resolved with git).
- Evaluated: 2026-11-13T14:50:34+00:00; deploy mode: remote; push remote: fork.
- Gate snapshot: `0a87efd5f4d8efae215b20be92a5c214db0a1b4f`; evidence root: `/var/tmp/gc-gate-ga-d4szt4v9-_r6w2wu2`.
- `docs/PROJECT_MANIFEST.md` is absent. The seven pack criteria and current `TESTING.md` supply the contract.

| # | Criterion | Result and evidence |
|---|---|---|
| 1 | Review PASS present | PASS — reviewer verdict PASS names the exact reviewed commit; no carryover or reviewer-added-test substitution. |
| 2 | Acceptance criteria met | PASS — the new overlap test independently FAILs on the untouched base and PASSes in the full source suite; all 11 named close, bulk-close, guard and hook-announcement tests PASS by name. Guard cases pin one post-close edge read and none for other writes. Existing fallback retained per mayor ruling gcg-7763969; stricter suppression is separately scoped as ga-89bkpdfs, not claimed here. |
| 3 | Full test and CI coverage | PASS — five forced-fresh remote phases, required 16-step local floor, both owned tests PASS, documented skips, and all six CI leaf jobs accounted below. |
| 4 | No unresolved HIGH review findings | PASS — style, spec and security review have no unresolved HIGH findings; the acceptance wording conflict has the recorded ruling. |
| 5 | Final branch clean | PASS — tested source/scratch has no tracked or untracked changes; the gate record is the only deployment addition. Branch cleanliness is checked again before publication. |
| 6 | Merge state and build/vet | PASS — exact scratch merge built with `bazel build //...` and `make vet` (whole-tree nogo), rc=0; scratch tree equals git's merge-tree result. |
| 7 | Single feature theme | PASS — five cmd/gc files implement and test the same close-announcement read overlap; helper changes count edge reads without changing writes. |

merge_state: clean (origin/main@8d2fb3120c77121b81de3ad9b1957dcf81add368)
merge_validation: merge result built and vetted here; the merge authority validates the eventual merge

gate_base_kind: current-tip

gate_base: 8d2fb3120c77121b81de3ad9b1957dcf81add368

## Test evidence

`test_cmd`: `GC_GATE_REMOTE_ENABLED=1 GC_REMOTE_GATE_FLOOR_GIB=25 GC_REMOTE_GATE_CAP=2 ISOLATED_TEST_BEAD=ga-d4szt4v9 /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-criterion3.sh --repo /var/tmp/gc-gate-ga-d4szt4v9-_r6w2wu2/merge --base 8d2fb3120c77121b81de3ad9b1957dcf81add368 --bead-json /var/tmp/gc-gate-ga-d4szt4v9-_r6w2wu2/bead.json --owned /var/tmp/gc-gate-ga-d4szt4v9-_r6w2wu2/owned.tsv --run-dir /var/tmp/gc-gate-ga-d4szt4v9-_r6w2wu2/criterion3-run/evidence`, detached through `gate-detached-run.sh`, admission and `isolated-test-run.sh`.

The exact phase commands use the common BAZEL_FLAGS below plus `--build_event_json_file=<evidence>/remote/<phase>.bep.json`:

- unit: `env -u MAKEFLAGS -u GNUMAKEFLAGS -u MFLAGS -u MAKELEVEL -u MAKEOVERRIDES -u MAKEFILES LANGUAGE=C make -k check` (its test prerequisite is `bazel test //...`).
- acceptance: `bazel test $BAZEL_FLAGS --config=acceptance //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests`.
- integration-packages: `bazel test $BAZEL_FLAGS --config=integration //test:integration_packages`.
- integration-full: `bazel test $BAZEL_FLAGS --config=integration --test_arg=-test.v --test_arg=-test.skip=$latency_re //test/integration:integration_test`.
- integration-latency: `bazel test $BAZEL_FLAGS --config=integration --test_arg=-test.v --test_filter=$latency_re --test_sharding_strategy=disabled --run_under=$probe //test/integration:integration_test`.

`latency_re` is computed from `gate-latency-tests.txt`; `probe` is the worker-side `gate-farm-probe.sh` wrapper. Neither narrows the union of the integration legs. Every test invocation carries `--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1` and its phase-specific BEP path. Exact runner commands are retained in `execution.json` and each phase's stdout/make log.

- test_cmd_scope: full-suite
- test_counts: 154212 PASS, 0 FAIL, 604 SKIP (top-level result lines per retained job, including floor; repeated tests count per job).
- diff_tests_executed: `TestGcBdCloseConfirmsEdgesWhileItReadsTheRowBack` PASS; `TestGcBdWriteGuardSkipsTheReadWhereBdResolvesIDsExactly` PASS, in unit and integration-packages.
- test_log_dir: `/var/tmp/gc-gate-ga-d4szt4v9-_r6w2wu2/criterion3-run/evidence/remote/logs` (retained, not deleted).
- test_exec_mode: remote-exec
- test_exec_bep: `/var/tmp/gc-gate-ga-d4szt4v9-_r6w2wu2/criterion3-run/evidence/remote/<phase>.bep.json`; detailed predicate records `<phase>.evidence.json`.
- rbe_state: worker-env pins match (`sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72`); valid certificate; kill switch absent; definitions present.
- fallback_reason: none.
- admission: remote, floor 25.0 GiB, cap 2, waited 0.0s, free 67.58 GiB.
- farm_test_seconds: 13070.176.
- owned_topup: none required; both owned tests ran in the full phases.
- floor_topup: complete, 16/16 commands rc=0; beadstest, Darwin compile, runner self-tests and environment-absence SKIP packages covered. See `floor-topup.json` and `floor-logs/`.
- latency_leg: measured, attempt 1; both latency-budget tests PASS after clear farm probe (40s wait, load1=48.54 < threshold 50); neither ran in integration-full. See `latency-leg.json`.
- heavy_mode: none (classifier verified).
- ci_lane_run: n/a — no CI configuration changed.
- waiver_ref: none; failure_attribution: none (no source-suite failures).

| Phase | Results | Fresh remote | Cached | Retried | Phase predicate |
|---|---:|---:|---:|---:|---|
| unit | 411 | 411 | 0 | 0 | PASS |
| acceptance | 25 | 25 | 0 | 0 | PASS |
| integration-packages | 86 | 86 | 0 | 0 | PASS |
| integration-full | 14 | 14 | 0 | 0 | PASS |
| integration-latency | 1 | 1 | 0 | 0 | PASS |

Native floor load: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=9.48 run_max_load=12.62 run_mean_load=11.24 run_readings=6 wait_first_load=9.48 wait_max_load=9.48 wait_mean_load=9.48 wait_readings=1 read_errors=0`. CI runner-policy waited 1530s without timeout (start 14.83); native base comparison waited 0s (start 14.24, max 14.97, mean 14.66); Windows compile waited 0s (start 13.96, max 15.30, mean 15.14). Their full load records are in their retained output logs. ICU74 check: OK, recorded provenance; see `icu74.txt`.

## Skip justification and acceptance proof

skip_inventory: `execution.json` plus remote `skip-inventory.json` (590 events: 90 environment-absence, 500 other), and `native-floor-skips.json` (72 native events). These inventories count event rows and include subtests; the result-line count above has a different denominator.

The remaining skips are intentional lane exclusions, platform/tool or opt-in preconditions, unsupported provider interfaces, and fixture shapes that cannot represent a row. The full integration tier exercises process coverage excluded by the unit lane; the local floor repairs environment-absence coverage. No owned test skips. All 307 automatically mapped skip sites and five supplemental sites are unchanged; `all-skip-sites.txt` and `supplemental-skip-sites.txt` record the hunk checks. The CLI-only production change runs after close, without changing skip preconditions. Four import hunks and three doc-comment hunks do not change those conditions; the two changed test helpers have 14 call sites in 11 tests, all PASS in unit and integration (`shared-helper-callers-by-name.txt`).

Shared-package base comparisons from this run: unit 186/186, integration 65/65, native floor 36/36 reproduce the identical SKIP reasons on the untouched pinned base, with no new/changed/unjudged reason. See `unit-compare-skips.txt`, `integration-compare-skips.txt`, `native-compare-skips.txt` and their site proofs. Original logs are retained; derived Bazel comparison text removes the rules_go 0x16 prefix. `skip-map-supplement.json` resolves helper call sites and one inventory-parser context omission from its explicit raw log/source reason.

Independent negative control: the reviewed concurrency test on the untouched base fails in 10s with “the read-back ran alone: the other read had not started”; on the reviewed merge it PASSes in both full tiers. This expected base failure is separate from the source-suite counts. See `red-output.log`, `acceptance-by-name.txt`, `owned-explain.txt`.

## CI-required accounting

ci_required_closure: `ci_required_triggers.py <merge-scratch> 8d2fb3120c77121b81de3ad9b1957dcf81add368 dfdd3261b4b3458a3e0ddad56f3719d74f4c9959 fork`; workflow blob `fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612`; 6 LEAF jobs. `check` is an aggregator, with no separate result.

| LEAF job | Status | Evidence |
|---|---|---|
| runner-policy | LOCAL-PASS | Exact EVENT_NAME=pull_request, PR_AUTHOR=quad341, FORCE_BLACKSMITH empty, temporary GITHUB_OUTPUT invocation; rc=0, `accounting/runner-policy.log`. |
| changes | LOCAL-PASS | Closure tool's path-filter computation, rc=0, `accounting/closure.txt`. |
| credential-provider-windows | NOT-TRIGGERED | Tool row: credential_provider filter no match. |
| pack-gate | NOT-TRIGGERED | Tool row: packs filter no match. |
| windows-lane | LOCAL-PASS | Job's Windows allowlist build/test compile, nogo Windows build/test compile and whole-tree Darwin build/test compile, all rc=0; `accounting/windows-lane.sh`, `accounting/windows-lane.log`. Compile-only commands are not runtime test evidence. |
| windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | Native runner is Windows; this host is Linux. Its exact allowlist's Windows test import closure has 9 module package directories and no changed file or module input. `accounting/windows-native-reach.json`. |

windows_reach: none (the native Windows job's own closure, not a reused credential-provider-only check).

deferred_to_pr_ci: windows-lane-test (non-linux-runner).

not_covered: none.

This PASS authorizes PR opening. Merge requires CI / required green on the PR head and the deferred native Windows job successful; the merge authority is MPR.
