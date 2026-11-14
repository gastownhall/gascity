# Release gate: listener fixture port holds

**Verdict:** **PASS**

Bead: ga-5tm4y3hs. Build: ga-os7kifr. Review: ga-9o5wnprt. Evaluated 2026-11-14T13:42:36.870101+00:00.
Deploy mode: remote; base branch: main; push remote: fork (quad341/gascity).
`docs/PROJECT_MANIFEST.md` is absent; the supplied deployer release criteria,
root AGENTS.md, TESTING.md, Makefile and current required-CI workflow define this gate.

| # | Criterion | Verdict | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | ga-9o5wnprt records PASS on the resolved reviewed source below. Reviewer-added tests independently verified as test-only additions. |
| 2 | Acceptance criteria met | PASS | Source inspection plus the independent acceptance checks below; all owned tests PASS by name in the complete suite. |
| 3 | Tests pass | PASS | Fast policy/generated checks, all five fresh remote phases, native floor, skip audit and six-job required-CI accounting complete. Windows native tests deferred explicitly below. |
| 4 | No blocking findings | PASS | Review and source inspection found no high/security or blocking style findings. No production behavior or dependency changes. |
| 5 | Clean worktree | PASS | Reviewed source and canonical merge were clean before evaluation; canonical tree/head reverified after tests. Only this gate record is added for publication; commit/push guards require a clean tree. |
| 6 | Merge state and build/vet | PASS | Canonical merge is clean at the pinned base; full `bazel build //...` and whole-tree `make vet`/nogo passed on that merge. |
| 7 | Single feature theme and scope | PASS | Twelve files implement one listener-fixture lifetime/diagnostic change, with test declarations, testing guidance and census reductions. Ancestry scope is checked before publication. No `.claude/**` contamination. |

## Source and merge identity

- reviewed source: `dabfb4ad82395239430488c3a13b6e1386f1bda4` (resolved with git, never a provenance branch tip)
- pinned base: `13bb101b8fb3bdcae9f7135ad344665af9e7bbd9` (`origin/main` as observed at criterion 6)
- canonical merge commit: `d07a827237e0bf9b04fa016d4a109a1c1409d3a8`
- canonical merge tree: `70c8ad63ea7b61d9583a2bd84c8f4c85ba9b7917`
- `merge_state: clean`; `merge_conflicts: none`; `gate_base_kind: current-tip`
- Already-merged preflight found no PR for this target. No rebase/reset of reviewed work.
- Evidence root: `/var/tmp/gc-gate-ga-5tm4y3hs-fresh-a55cm572`; `context.json` pins all identities. Later movement of main does not alter this record. Merge-time resolution/validation belongs to MPR.

## Acceptance verification

`reviewer-added-tests.sh check` independently returned
`reviewer-tests files=cmd/gc/loopback_port_hold_guard_linux_test.go`.
The checked parent/head are recorded in `acceptance-audit.json` and
`reviewer-added-check.txt` (parent is the builder pin, head the source above).

| Acceptance | Verification |
|---|---|
| AC1 | Shared pickers retain ports above the existing threshold, retain the IPv6 companion when available, and preserve the low-port selector. |
| AC2 | CLOEXEC, reuse boundary, bounded retry, retained rejected candidates, narrow IPv6 absence and wrapped errors are covered by owned tests. |
| AC3 | Raw bind-zero exclusion, same-number IPv6 collision, descriptors and cleanup release all PASS in the full suite. Known post-release probe race ga-786qh71w is disclosed, with no failed run excused. |
| AC4 / AC6 | Five child-launch paths use file-backed stderr and one error formatter after child wait. The instrumented scratch error sample includes readiness EOF, exit status 1 and Python OSError errno 98 Address already in use. Source inspection covers capture ordering; no race-detector claim. |
| AC5 | Python open-file child holds IPv4 and available IPv6 before reporting readiness. |
| AC7 | Matched nine-scenario first-attempt remote stress: 128 actions × 100 iterations per test, 115,200 iterations per tree; baseline six failed iterations, reviewed merge zero; no skips. Both include INNERPY rebind and real Dolt. |
| AC8 | Full build/nogo, formatting, policy, generated/BUILD drift and runtime pass; testing rule and resource census changes inspected. PR body includes both historical and fresh counts, failure text and all six residuals. |

Focused stress is acceptance evidence, separate from the full-suite verdict.
`ac7-result.json`, `ac7-before/evidence` and `ac7-after/evidence` retain BEPs,
raw logs/XML, hashes, commands and action coverage. Earlier local seven-test
comparison was 8 failing results to 0; prior reviewer remote after runs were 0.
The earlier held/incomplete suite is retained under `prior_run` in context.json
and contributes no test credit to this fresh gate.

PR disclosure retains real-Dolt reuse/absent-tool skip, INNERPY rebind without
retry, the existing Python ProcessState.Exited check, the separate Go integration
helper, both close-and-pass-number callers (ga-3pv9o04q), macOS bind-and-close,
and ga-786qh71w. Production listener PID lookup is unchanged.

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
sync targets. OpenAPI breaking check passed. No production complexity or dependency surface changed.
Exact commands: `c6-build.sh`, `fast-lane.sh`, `fast.sh`; successful BEPs and
hashes: `independent-source-skip-audit.json`. No policy or suite failure is attributed.

`test_cmd`: detached `gate-criterion3.sh --repo /var/tmp/gc-gate-ga-5tm4y3hs-fresh-a55cm572/merge --base 13bb101b8fb3bdcae9f7135ad344665af9e7bbd9 --bead-json /var/tmp/gc-gate-ga-5tm4y3hs-fresh-a55cm572/bead.json --owned /var/tmp/gc-gate-ga-5tm4y3hs-fresh-a55cm572/owned.tsv --run-dir /var/tmp/gc-gate-ga-5tm4y3hs-fresh-a55cm572/criterion3-run/evidence`.
`test_cmd_scope: full-suite`; `heavy_mode: none` (verified by classifier).
Remote legs cover `bazel test //...`, acceptance, integration packages and full
integration; latency tests run separately under a clear farm probe. Every leg
forces `--nocache_test_results --noremote_local_fallback`; no cache-credited
result, retry or local fallback contributes to this PASS. `test_exec_mode: remote-exec`;
`fallback_reason: none`; `farm_test_seconds: 12030.855`.
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
`test_counts`: PASS=154502, FAIL=0, SKIP=606
(column-zero Go result markers, including flattened subtests; not unique tests).
`test_log_dir: /var/tmp/gc-gate-ga-5tm4y3hs-fresh-a55cm572/criterion3-run/evidence/remote/logs`;
`runtime-by-name.log` was read using `gate-test-evidence.py`; logs are retained.
`waiver_ref: none`; `failure_attribution: none`.

```text
diff_tests_executed:
  TestHeldLoopbackPortDescriptors PASS (integration-packages-12697e2ea0d32c278115.log, unit-5a9df316125cd0aeff2e.log)
  TestHeldLoopbackPortReleaseFreesTheNumber PASS (integration-packages-9326181a736eb30b71ca.log, unit-10cc7795c0ec199a5530.log)
  TestHeldLoopbackPortSemantics PASS (integration-packages-e5330b8c981927473156.log, unit-088cc38d26d094729229.log)
  TestHoldLoopbackPortFDsGivesUpAtTheTryBound PASS (integration-packages-e4245ddf6a37030111bc.log, unit-23e7d9fc763496dd8095.log)
  TestHoldLoopbackPortFDsHoldsEveryFamilyTheHostHas PASS (integration-packages-514989cc035c0e9a8ff5.log, unit-493754ea2d5b4d8858c8.log)
  TestHoldLoopbackPortFDsKeepsRejectedCandidatesOpenUntilThePickEnds PASS (integration-packages-821a75e4a503601af052.log, unit-2bde55fab377f50b4739.log)
  TestHoldLoopbackPortFDsWrapsACandidateError PASS (integration-packages-bf8ed8ba46004086b3b9.log, unit-4d20b3d28497f844aa39.log)
  TestHoldLoopbackPortFDsWrapsAV6BindErrorAndReleasesTheCandidate PASS (integration-packages-074fc6c34a1fb2577127.log, unit-2ef13e1b6898c5544764.log)
  TestHoldLoopbackPortSkipsCandidateWhoseV6TwinIsTaken PASS (integration-packages-aa6cf5c138f0b61b866a.log, unit-cd6930bb0d0da8646500.log)
  TestListenerChildFailureCarriesReadinessExitAndStderr PASS (integration-packages-bc482d2ae768251ad47f.log, unit-fd986cf5bab8709b8d45.log)
  TestListenerChildFailureKeepsAnEmptyStderrVisible PASS (integration-packages-18745ddf7d62465654f5.log, unit-752a0bf25b15b465517a.log)
  TestListenerFixturesReportChildStderrAndExitStatus PASS (integration-packages-cb110414c557c0928e82.log, unit-d9e667428f972d067bd3.log)
  TestLoopbackV6UnavailableMatchesOnlyAHostWithoutLoopbackV6 PASS (integration-packages-75af79934639abaae69c.log, unit-64014bc0bb3833bc198c.log)
  TestPortHelpersHoldTheNumberUntilCleanup PASS (integration-packages-b72a67a2f9444af705e3.log, unit-f93a0ca96ccc61d0cae9.log)
  TestReservedLoopbackPortIsNeverReturnedByRawBindZero PASS (integration-packages-f7fba3a97bc02cffff0b.log, unit-912ab643330b35bd0b6b.log)
  TestStartOpenFileAndTCPListenerHoldsLoopbackV6Twin PASS (integration-packages-56f04454dd9158a9ee8a.log, unit-f679cd2551fcc64525aa.log)
GATE_TEST_EVIDENCE_RESULT ok: 16 diff-owned tests verified by name
```

`admission`: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 69.24256134033203, "waited_seconds": 0.001}.
`rbe_state`: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72", "worker_env_tree": "sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"}.
Remote admission used cap 2 / 25 GiB, no local-heavy slot/load wait.
`owned_topup`: [] (all owned tests were present).
`floor_topup: complete`, all 16 steps exit 0: beadstest, required native
environment-absence package top-ups, Darwin compile and runner self-tests.
Native steps use the isolation wrapper, local admission and bounded load wait.
Their exact commands/results/logs are in `remote/floor-topup.json`.

```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=7.50 run_max_load=39.63 run_mean_load=24.20 run_readings=38 wait_first_load=7.50 wait_max_load=7.50 wait_mean_load=7.50 wait_readings=1 read_errors=0
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
unit and integration tags. Shared helper hunks are answered by these measured base comparisons.
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
