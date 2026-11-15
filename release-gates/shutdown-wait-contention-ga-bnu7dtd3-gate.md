# Release gate: shutdown wait under contention

**Verdict:** **PASS**

Bead: ga-bnu7dtd3. Build: ga-5wuikntp. Review: ga-46rr72p5. Evaluated 2026-11-14T13:42:36.951814+00:00.
Deploy mode: remote; base branch: main; push remote: fork (quad341/gascity).
`docs/PROJECT_MANIFEST.md` is absent; the supplied deployer release criteria,
root AGENTS.md, TESTING.md, Makefile and current required-CI workflow define this gate.

| # | Criterion | Verdict | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | ga-46rr72p5 records PASS on the resolved reviewed source below. Source matches the reviewer pin; no carryover or reviewer-added commit. |
| 2 | Acceptance criteria met | PASS | Source inspection plus the independent acceptance checks below; all owned tests PASS by name in the complete suite. |
| 3 | Tests pass | PASS | Fast policy/generated checks, all five fresh remote phases, native floor, skip audit and six-job required-CI accounting complete. Windows native tests deferred explicitly below. |
| 4 | No blocking findings | PASS | Review and source inspection found no high/security or blocking style findings. No production behavior or dependency changes. |
| 5 | Clean worktree | PASS | Reviewed source and canonical merge were clean before evaluation; canonical tree/head reverified after tests. Only this gate record is added for publication; commit/push guards require a clean tree. |
| 6 | Merge state and build/vet | PASS | Canonical merge is clean at the pinned base; full `bazel build //...` and whole-tree `make vet`/nogo passed on that merge. |
| 7 | Single feature theme and scope | PASS | One test function in cmd/gc/cmd_supervisor_test.go changes its timing seam. Ancestry scope is checked before publication. No `.claude/**` contamination. |

## Source and merge identity

- reviewed source: `34f7a4f88ef5d117f6b96e3736f78954a12ab2b3` (resolved with git, never a provenance branch tip)
- pinned base: `420b7bb65fff819301423a6e8631478254bbfae8` (`origin/main` as observed at criterion 6)
- canonical merge commit: `08211610dbffa4cba3fc358bc9980a34b78b82bc`
- canonical merge tree: `c277ba7d943790cf643b1a8636ad5082c54dc834`
- `merge_state: clean`; `merge_conflicts: none`; `gate_base_kind: current-tip`
- Already-merged preflight found no PR for this target. No rebase/reset of reviewed work.
- Evidence root: `/var/tmp/gc-gate-ga-bnu7dtd3-06qsn0xv`; `context.json` pins all identities. Later movement of main does not alter this record. Merge-time resolution/validation belongs to MPR.

## Acceptance verification

Only TestStopManagedCityPreservingSessionsWaitsForRuntimeShutdownOnTimeout
changes: the shutdown timeout rises from 20 ms to 400 ms and the fake runtime
uses a deterministic 600 ms SIGTERM-exit delay. Existing timeout-error,
stopped-state, preserve-session and ordering assertions remain. There is no
production timeout, skip, retry or weaker assertion. Review reports normal
runtime increasing from about 0.22 s to 2.5 s; the added delay is intentional.

Independent acceptance at this canonical merge, first-attempt remote actions:
restoring only the 20 ms constant while retaining the delayed child failed
5/5 times at the stopped-state assertion; unmodified reviewed code passed
50/50 times, no skips. `acceptance-result.json` and `acceptance-{red,green}/`
retain commands, mutation diff, BEPs and raw logs/XML. These are supplemental
acceptance runs; full-suite evidence follows. No host CPU load was generated.
Reviewer mutation evidence detects missing shutdown-wait behavior in both
versions; the disclosed surviving single-bound mutants make no stronger claim.
The requested zero-growth check passed via `COMPLEXITY_BASE_REF` on the pinned
base in a separate fresh view; the test file adds no production complexity.

## Full-scope test evidence

Evaluation order was preflight → criterion 6 → review/acceptance → fast lane →
full suite → CI accounting. The fast lane used separate fresh views with
`gate-base.sh run`, never modifying the canonical merge. Full `make check`
coverage is supplied by its four shell guards plus `bazel test //...`; the
acceptance and both integration tiers also ran. C6 used `bazel build //...`
and `make vet NOGO_BUILD_FLAGS=...--output_groups=nogo_fix`. Hooks are active
(`make check-hooks`); normal commit/push hooks remain enabled.

`policy_lane: PASS`: pinned changed-format check, full C6 nogo, release-dist,
routed-row, split-topology, residency-boundary and hooks guards; 13 Bazel
format/docsync/policy targets passed. `generated_drift: PASS`: `make bazel-sync`
produced no change, followed by seven spec/schema/client/dashboard/lockfile
sync targets. OpenAPI breaking check passed. The zero-growth complexity lane passed.
Exact commands: `c6-build.sh`, `fast-lane.sh`, `fast.sh`; successful BEPs and
hashes: `independent-source-skip-audit.json`. No policy or suite failure is attributed.

`test_cmd`: detached `gate-criterion3.sh --repo /var/tmp/gc-gate-ga-bnu7dtd3-06qsn0xv/merge --base 420b7bb65fff819301423a6e8631478254bbfae8 --bead-json /var/tmp/gc-gate-ga-bnu7dtd3-06qsn0xv/bead.json --owned /var/tmp/gc-gate-ga-bnu7dtd3-06qsn0xv/owned.tsv --run-dir /var/tmp/gc-gate-ga-bnu7dtd3-06qsn0xv/criterion3-run/evidence`.
`test_cmd_scope: full-suite`; `heavy_mode: none` (verified by classifier).
Remote legs cover `bazel test //...`, acceptance, integration packages and full
integration; latency tests run separately under a clear farm probe. Every leg
forces `--nocache_test_results --noremote_local_fallback`; no cache-credited
result, retry or local fallback contributes to this PASS. `test_exec_mode: remote-exec`;
`fallback_reason: none`; `farm_test_seconds: 13690.016`.
The unit/acceptance/integration command flags, exact phase windows, copied
per-action logs/XML and digests remain in `criterion3-run/evidence/remote/`.

| Phase | Fresh remote results | Cache/local/other/retry | Window valid |
|---|---:|---|---|
| unit | 415 | 0 / 0 / 0 / 0 | yes |
| acceptance | 25 | 0 / 0 / 0 / 0 | yes |
| integration-packages | 86 | 0 / 0 / 0 / 0 | yes |
| integration-full | 14 | 0 / 0 / 0 / 0 | yes |
| integration-latency | 1 | 0 / 0 / 0 / 0 | yes |

`test_exec_bep`: all 541 results satisfy remote execution, attempt 1, neither
cached flag, start inside their phase window and terminal verdict.
`test_counts`: PASS=154446, FAIL=0, SKIP=606
(column-zero Go result markers, including flattened subtests; not unique tests).
`test_log_dir: /var/tmp/gc-gate-ga-bnu7dtd3-06qsn0xv/criterion3-run/evidence/remote/logs`;
`runtime-by-name.log` was read using `gate-test-evidence.py`; logs are retained.
`waiver_ref: none`; `failure_attribution: none`.

```text
diff_tests_executed:
  TestStopManagedCityPreservingSessionsWaitsForRuntimeShutdownOnTimeout PASS (integration-packages-821a75e4a503601af052.log, unit-1ab526405ace21879f85.log)
GATE_TEST_EVIDENCE_RESULT ok: 1 diff-owned tests verified by name
```

`admission`: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 66.24853897094727, "waited_seconds": 0.0}.
`rbe_state`: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72", "worker_env_tree": "sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"}.
Remote admission used cap 2 / 25 GiB, no local-heavy slot/load wait.
`owned_topup`: [] (all owned tests were present).
`floor_topup: complete`, all 16 steps exit 0: beadstest, required native
environment-absence package top-ups, Darwin compile and runner self-tests.
Native steps use the isolation wrapper, local admission and bounded load wait.
Their exact commands/results/logs are in `remote/floor-topup.json`.

```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=14.96 run_max_load=15.91 run_mean_load=11.68 run_readings=42 wait_first_load=14.96 wait_max_load=14.96 wait_mean_load=14.96 wait_readings=1 read_errors=0
```

These are native-floor observations; remote test load is separately recorded
by the farm probe. `latency_leg: measured`, first attempt,
clear probe, both named latency tests PASS; raw proof in `remote/latency-leg.json`.
`skip_inventory`: `remote/skip-inventory.json` plus `skip-rows-final.json` and
`skip-final-audit.json`; remote reason classes {"other": 502, "environment-absence": 90}.
All 664 skip markers (remote plus native subtest markers) have literal reasons;
309 sites are UNCHANGED, zero reasonless/unresolved/touched sites. The affected
cmd/gc profiles reproduce all 257 remote and 36 native skips on the pinned base
with identical reasons; outside test-binary import closures exclude cmd/gc for
unit and integration tags. All changed hunks are inside the one owned test; no shared helper/init hunks.
The native floor already tops up every environment-absence class. Remaining
platform, fixture, capability and opt-in skips are inventoried; no diff-owned
SKIP or FAIL, and no failure is waived. Source/tree and 1082 artifact digests
were independently checked after collection.

## Required CI accounting (criterion 3e)

`ci_required_closure`: workflow `.github/workflows/ci.yml` blob
`fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612`, all six transitive LEAF jobs,
computed from canonical workflow + reviewed changed paths by
`ci_required_triggers.py` (command and output in `ci-required/accounting.json`
and `ci-required/closure.txt`).

| Job | Status | Evidence |
|---|---|---|
| changes | LOCAL-PASS | Actual closure/path-filter evaluation. |
| runner-policy | LOCAL-PASS | Actual workflow command, detached fresh view, exit 0. |
| windows-lane | LOCAL-PASS | All four actual Windows/Darwin compile commands, detached fresh view, exit 0. |
| credential-provider-windows | NOT-TRIGGERED | Changed-path/import reach evaluates empty. |
| pack-gate | NOT-TRIGGERED | Pack path filter false. |
| windows-lane-test | DEFERRED-TO-PR-CI | Non-Linux runner; own Windows credential/genshell binary import closures have empty diff reach. |

`deferred_to_pr_ci: windows-lane-test (non-linux-runner)`.
`not_covered: none`. Native Windows CI must be green on the PR head before
merge, along with the current `CI / required` aggregate. Gate clearance
approves this evaluated source and record; it does not authorize a merge or
replace required CI. MPR owns the merge and checks the exact cleared head.
