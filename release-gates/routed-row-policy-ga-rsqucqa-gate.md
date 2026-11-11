# Routed-row policy release gate — ga-rsqucqa

**Verdict:** **PASS**

Gate date: 2026-11-05T17:39:56.926453+00:00. Build ga-x8ux906; review ga-8g92rde.
Reviewed deploy source: `9c5cd3e8086d5240c1754db8b60c3c074c73266f`. Provenance branch `builder/ga-x8ux906` is not a push target.
Deploy mode: remote; push remote: fork (quad341/gascity); base ref: origin/main.
Gate base: `0883fb40ef124e3bc6b93ba823d70418ac67f376`. Gated merge: `2edbe706a548a0c1f688ac262f09296454b2cedd`; tree: `fc913615544b0ade6697869457f69e0c88c1bb5e`.
Evidence root: `/var/tmp/deploy-ga-rsqucqa-fresh.6WcSmn`. Pack evidence commit: `c83909fcd9d727341913b8ef4ba3b22a5611dd0a`.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Reviewer ga-8g92rde passed the exact resolved source. No review carryover or reviewer-added-test exception. |
| 2 | Acceptance criteria met | PASS | Six acceptance items verified below; future landing obligation carried without closing its tracker. |
| 3 | Full tests and policy | PASS | Fresh complete farm population plus required native floor; one unrelated clear-probe latency failure attributed under standing3a, never retried or waived. All owned tests actually passed. |
| 4 | No unresolved HIGH findings | PASS | Independent review has zero unresolved HIGH findings. Accepted minor scanner limits and explicitly descoped synthetic controls are recorded below. |
| 5 | Final branch clean | PASS | Reviewed source and gated scratch were clean. Only this gate record is carried to the isolated deploy branch; staged-path and final-clean checks are required before push. Eleven older untracked gate records are digest-preserved outside the branch, then restored to the role worktree. |
| 6 | Merge state/build/vet | PASS | Clean frozen-base merge, full-tree Bazel build/nogo and make vet succeeded with normal hooks active. |
| 7 | Single feature theme | PASS | One routed-matrix discovery correction; checker regression tests, BUILD registration and one subprocess-census row support that behavior. |

## Acceptance verification

1. Untouched pinned main reproduced the exact expected RED: `make check-routed-test-rows` rc2, both cmd_service_test.go and cmd_rig_list_hq_running_test.go falsely counted 1/6. The merged repair passes the actual guard in the fast policy and full make-check lanes. `ac-base-red.log` retains the baseline, not a candidate retry.
2. Five changed test parents and28 subtests actually PASS. Controls cover missing/empty/comments-only manifest, missing listed file, each dropped/renamed/duplicate row, all rows renamed, partial unlisted matrices1..5, complete unlisted matrix6, and a complete listed matrix. The manifest/nonempty denominator is unchanged.
3. Ordinary controller-down reason values no longer enroll either real-corpus file: discovery matches existing case-name declarations. Both files are byte-identical to the frozen main version. TestServiceRestartClientGrantGatedSupervisor (including no-supervisor-client) and TestDoServiceRestartRefusals (including controller-down) PASS by name in unit and integration-package logs. Additional synthetic unrelated-marker controls were explicitly descoped before review to OPEN ga-miihvqf; the approved follow-up remains separate. Accepted scanner limits: suffix identifiers ending in name and comment text can still match; unkeyed/map matrices need the manifest to be policed. These are reviewer-documented existing limitations.
4. Same two-commit test-first source: validator RED commit then minimal checker fix. Actual full `make -k check`, Bazel suites, format/nogo, hooks and BUILD/generated drift ran independently in this gate. No prior PASS substituted.
5. Build ga-x8ux906 is stamped gc.fixes_tracker=ga-lpaujfv. Tracker ga-lpaujfv is OPEN and must remain so until verified actual-main landing plus canonical guard PASS. Protected ga-xkdbsxx status, labels, metadata and dependencies were re-read identical to the pre-run snapshot; no rerun/release/clearance/publication of that held gate occurred. A branch or review/deploy close is not landing.
6. Scope remains the policy collision. No service/API production reason change, review of PR8656, or unrelated CI-coverage expansion. Existing follow-up ga-j30piqe is not a prerequisite.

## Merge and fast lane

merge_state: clean (origin/main@0883fb40ef124e3bc6b93ba823d70418ac67f376)
gate_base_kind: current-tip
merge_validation: clean merge result built/vetted here; merge authority validates the final merge against its then-current base. No rebase/reset of the reviewed source.

C6 evidence: `c6-resume-rbe`, terminal rc0, full `bazel build --config=remote-exec --noremote_local_fallback //...` and `make vet`/nogo. Normal pre-commit hooks completed on the exact expected scratch merge. Initial `c6` was cancelled while a setup-misconfigured local lint build was unfinished; the normal hook committed just before cancellation. Parent pair/tree/clean status were independently checked, and only unfinished explicit build/vet continued with corrected managed RBE flags. No test verdict was retried.

policy_lane: PASS — fresh-tree `git diff --check`, pinned golangci formatter, make lint/nogo, release-distribution/routed-row/split-topology/residency shell guards, and make check-hooks. `policy-output.log`.
drift_lane: PASS — fresh-tree `make bazel-sync` then `git diff --exit-code`; `drift-output.log`. Separate fresh-tree policy11/generated7/OpenAPI1 targets all PASS; `fast-verified.json` plus three BEPs/logs. Each lane used the frozen gate base and a committed-tree view, leaving the suite scratch untouched. Fast lane completed before runtime/suite launch.
ci_lane_run: n/a (no CI workflow/config change).
heavy_mode: none (`classify.txt`); no sharded-heavy exception.

## Criterion 3 execution

test_cmd_scope: full-suite
waiver_ref: none
test_exec_mode: remote-exec (all five actual BEPs; selection alone was not evidence).
fallback_reason: null; no local suite fallback and no mixed suite.

`gate-detached-run.sh` launched `gate-criterion3.sh --repo <scratch> --base <gate-base> --bead-json bead.json --owned owned.tsv --run-dir c3-run/evidence`; the supplied complete local fallback was never used.

Common actual BAZEL_FLAGS: `--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=<remote>/<phase>.bep.json`. NOGO_BUILD_FLAGS: `--keep_going --output_groups=nogo_fix --config=remote-exec`.

- Unit: `env -u MAKEFLAGS -u GNUMAKEFLAGS -u MFLAGS -u MAKELEVEL -u MAKEOVERRIDES -u MAKEFILES LANGUAGE=C make -k check` (includes full bazel test //...).
- Acceptance: `bazel test $BAZEL_FLAGS --config=acceptance //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests`.
- Integration packages: `bazel test $BAZEL_FLAGS --config=integration //test:integration_packages`.
- Full integration: `bazel test $BAZEL_FLAGS --config=integration --test_arg=-test.v --test_arg=-test.skip=<registry-regex> //test/integration:integration_test`.
- Isolated latency: `bazel test $BAZEL_FLAGS --config=integration --test_arg=-test.v --test_filter=<same-registry-regex> --test_sharding_strategy=disabled --run_under=<canonical-farm-probe> //test/integration:integration_test`.

The registry is the canonical gate-latency-tests.txt, read in full. Phase4 omitted exactly its two tests; phase5 ran exactly those. Phase-specific commands, windows and BEPs remain under `c3-run/evidence/remote`.

| Phase | Fresh remote result/verdict pairs | Verdict |
|---|---:|---|
| unit | 395 | PASS |
| acceptance | 25 | PASS |
| integration-packages | 86 | PASS |
| integration-full | 14 | PASS |
| integration-latency | 1 | FAIL, attributed below |

All521 results used remote strategy, zero remote/local cached flags, attempt1, valid phase-start windows, and terminal verdicts. farm_test_seconds: 11956.425. All1042 fetched log/XML artifacts were independently digest-rechecked (`artifact-integrity.json`).

test_counts: PASS146990 FAIL1 SKIP507 across537 retained logs,470 with Go results; top-level result lines summed per job/log, excluding subtests. These are not unique-test counts. Native floor contributes PASS21 FAIL0 SKIP14. Shell/Darwin checks have their separate exit statuses.
test_log_dir: `/var/tmp/deploy-ga-rsqucqa-fresh.6WcSmn/c3-run/evidence/remote/logs`; native originals: `/var/tmp/deploy-ga-rsqucqa-fresh.6WcSmn/c3-run/evidence/remote/floor-logs`. Logs retained; never deleted by this gate.
diff_tests_executed (actual full-suite unit output):

- TestCheckRoutedTestRowsAcceptsListedMatrix: PASS (unit phase, retained by-name log).
- TestCheckRoutedTestRowsPassesOnThisRepo: PASS (unit phase, retained by-name log).
- TestCheckRoutedTestRowsRejectsBrokenManifest: PASS (unit phase, retained by-name log).
- TestCheckRoutedTestRowsRejectsMissingRows: PASS (unit phase, retained by-name log).
- TestCheckRoutedTestRowsRejectsUnlistedMatrix: PASS (unit phase, retained by-name log).

Owned family totals: PASS33 FAIL0 SKIP0 (five parents +28 subtests), `owned-family-results.json`. `final-by-name.txt` verifies all five roots; `unit-preserved-by-name.txt` and retained integration logs verify the unchanged service assertions.

skip_inventory: farm493 entries (72 environment-absence,421 other) in `remote/skip-inventory.json`; every environment-absence selection was attempted by canonical floor package/parent selection. Remaining non-owned skips include explicit unit-lane/optional-fixture/legacy-provider/out-of-scope controls. Native14 top-level skips: Replay and SimCoverage opt-in inputs unset; seven herdr live-lane tests disabled without live opt-in; four tmux binding tests require a tmux server; br conformance requires absent br. Native39 all-level skip lines and source reasons retained in `native-skip-inventory.json` and floor logs. No owned test skipped; no FAIL is relabelled SKIP.
owned_topup: none required; all owned names PASS from the farm.
floor_topup: COMPLETE,16/16 commands rc0, `remote/floor-topup.json`. Canonical gate-floor-topup.py runs beadstest, all environment-absence selections, Darwin compile, push-gate-lock and local-concurrency self-tests. Each Go job used integration tags/count1/verbose, private TMPDIR, scrubbed GC/BD/BEADS/DOLT selectors and real pinned host tools. This is the documented ancillary native floor, not a stitched local suite. After attribution, only the unfinished floor ran; no farm/latency phase was retried. Systemd drift test actual PASS, restart6s < fixed15s.

## Retained failure and attribution

failure_attribution: TestGcSeatHookLatencyBudget -> OPEN ga-y39d8tx (predates this run, created2026-10-28), standing criterion3a clauses1,2,3(b),4.

Actual phase5: TestGcBdLatencyBudget PASS; seat-hook FAIL at site183, mail fastest-of7=1.065s > unchanged1s; prime610ms/drain471ms PASS. Clear probe: waited0/load1=31.00/load5=25.04/load15=11.63/cpus32/threshold50. This is a real measured failure, not a busy deferral or a claim that load caused it. No failed candidate retry, budget/statistic/probe change or waiver.

Clause1: own function/site unchanged, shared_hunks_in_file=0; `latency-ownership.txt`. Clause2: opened exact-condition tracker and appended verified sightings. Clause3(b): same clear-probe remote mail assertion failed at1.235s on unrelated own PR8314; compared source/base/merge all resolve, and its five changed provider-health files share no file with this four-file repair. Independent retained witness was re-read and hash-verified (SHA256 `6f5065c4d1312604db2696c6e5be50a8a1394d496eb82d34fc8e24aab581d74b`), copied to `latency-pr8314-witness.log`; complete proof `latency-cross-pr-proof.json`. Clause4: no test/integration package/path overlap. Added scripts subprocess census is acknowledged; no inconclusive-guard exemption is used. External gc includes a census reader for doctor, so absence of that package is not asserted. Condition fix ga-rxo15af is stamped and independently NOT-LANDED; build ga-x8ux906 is fix-carrying, though this is the first matching latency sighting on this deploy.

Raw execution.json remains unchanged: criterion3_valid=false/exit1 and latency_leg failed/complete=false. Overall criterion3 PASS is this standing3a assessment plus completed independent floor/CI, not an alteration of those raw values. `final-evidence-assessment.json` binds the complete assessment to the raw digest.

Raw execution compact summary:

```json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":1,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/deploy-ga-rsqucqa-fresh.6WcSmn/c3-run/evidence/remote","admission":{"class":"remote","floor_gib":25.0,"free_gib":67.19746780395508,"waited_seconds":0.001,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98","worker_env_origin_main":"sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"},"test_exec_bep":{"unit":{"results":395,"remote_executed":395,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":86,"remote_executed":86,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":14,"remote_executed":14,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":11956.425,"criterion3_valid":false}
```

Full raw execution pointer: `/var/tmp/deploy-ga-rsqucqa-fresh.6WcSmn/c3-run/evidence/execution.json`. Full latency_leg: `/var/tmp/deploy-ga-rsqucqa-fresh.6WcSmn/c3-run/evidence/remote/latency-leg.json`. rbe_state and admission are included above: own-code provenance, current worker-env pin,25GiB remote floor/cap2, no remote host-load wait. Rootless podman/pinned Dolt image and bd v1.3.4 verified before suite; ICU74 check OK with all soname digests in `icu-check.log`. The isolation wrapper honestly reported pass-through for the temporary tree; remote actions were hermetic, and each native command explicitly scrubbed store/session selectors. No overlay, unshare or library-path shim.

load_threshold:15; load_waited_seconds:1801; load_wait_timed_out:1; run_start_load:89.78; run_max_load:119.07; run_mean_load:109.74; run_readings:15 (native floor).
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=89.78 run_max_load=119.07 run_mean_load=109.74 run_readings=15 wait_first_load=100.80 wait_max_load=100.80 wait_mean_load=93.13 wait_readings=61 read_errors=0
Remote-chain load: waited0/timed_out0/start18.05/max105.92/mean68.95/readings55; separate runtime setup start22.90/waited0. Ordinary native wait timeout proceeds by policy, not by a threshold change. Runtime and CI job load summaries are retained separately.

## CI-required accounting

ci_required_closure: exact ci_required_triggers.py command in `ci-accounting.json`; merged workflow blob `e5b11c348505dab28dc223e874bc2c61cbbb33c5`; four LEAF jobs. `ci-closure.txt` records actual merged path filters and runs-on resolution.

| LEAF | Status | Evidence |
|---|---|---|
| changes | LOCAL-PASS | Exact closure tool rc0, all relevant filters no match. |
| runner-policy | LOCAL-PASS | Own EVENT_NAME=pull_request, PR_AUTHOR=quad341, FORCE_BLACKSMITH empty command on fresh committed tree; detached/load-gated/wrapped; rc0, runner-policy.log. |
| credential-provider-windows | NOT-TRIGGERED | Tool row: credential_provider filter no match. |
| pack-gate | NOT-TRIGGERED | Tool row: packs filter no match. |

windows_reach: none, helper rc0 (stderr lists closure dirs only).
deferred_to_pr_ci: none
not_covered: none
Aggregators check/ci-required roll up those jobs; no LEAF result assigned to them. CI runner-policy wait timed out1801s/start98.47; own command PASS; complete output `ci-complete-output.log`.

PASS authorizes PR-open only. The merge authority must require CI / required green on the exact PR head. Deploy-clearance publication is pinned to that head and precedes the verified merge-request. Deployer does not merge or watch PR CI.
