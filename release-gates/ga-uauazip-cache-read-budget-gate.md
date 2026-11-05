# Release gate: readiness cache closed-read budget

**Verdict:** **PASS**

Evaluated: 2026-10-28T17:26:13+00:00. Deploy bead ga-uauazip; build ga-4piv4zy;
review ga-x1ih2ie. Reviewed source: `8a5580b5c79e4f4fcb6afeea72cf744c2c1f9738`.
The deploy branch is cut at this exact resolved source; its only added commit
contains this record. The source/provenance builder branch is never a push target.

deploy_mode: remote
base_ref: origin/main
merge_state: clean (origin/main@abea2387cdad9ee8c3453bc8283fa198bb26f67d)
gate_base_kind: current-tip
gate_base_sha: abea2387cdad9ee8c3453bc8283fa198bb26f67d
merge_validation: canonical merge e4e3af10f4f2e5487185394c799bdb640f9bf8f5, tree 194fc05c01bb7e1587db59c049bcbdf57de98114; full build and nogo/vet passed. MPR verifies and merges the exact cleared PR head; later base movement is covered at merge time.

Preflight: the source resolves locally, and the builder explicitly recorded it as
unpushed. GitHub's commit lookup returned 422/no such remote commit; the exact
provenance-branch PR lookup returned an empty array. No existing/merged PR was
silently replaced. Two source commits cite ga-4piv4zy and tracker ga-7m0nf5a;
the scope guard accepted ga-uauazip and ga-4piv4zy. No stack.

| Criterion | Result | Evidence |
| --- | --- | --- |
| 1. Review PASS present | PASS | Closed review ga-x1ih2ie records PASS at the exact resolved source. No carryover/substituted branch tip or reviewer-added commit. |
| 2. Acceptance criteria met | PASS | Both concurrent read orders are accepted; exactly one list and one query remain mandatory. Both changed tests and all 13 subtests passed inside the full suite. Detailed acceptance checks below. |
| 3. Tests pass | PASS (attributed failure) | Four complete farm phases PASS; unchanged latency test's step-reminder API timeout attributed under 3a to ga-9l9g23m with mechanism proof. 498 fresh remote results, zero cache-credited results/retries/other strategies. Mandatory native floor: 16 commands, all exit 0. Both owned tests PASS, zero owned FAIL/SKIP. |
| 4. No high-severity review findings open | PASS | Zero unresolved HIGH, major or blocker findings; reviewer notes contain no change request. |
| 5. Final branch is clean | PASS | Source and canonical scratch clean before adding this record. Publication guard requires a gate-only commit whose parent is the reviewed source, then a clean worktree before push. |
| 6. Merge state and build/vet recorded | PASS | Exact canonical merged tree equals merge-tree output; all 1390 targets built with remote execution and full nogo/vet passed; normal merge hooks passed. |
| 7. Single feature theme | PASS | One test-file change, +51/-1, fixing the readiness-cache read-budget assertion. No independent feature, source deployment change, or internal operational documentation. |

## Acceptance evidence

The assertion in TestCachedReadyOverBdBackingHonorsAnEvictedBlockersWorkOutcome
now counts command kinds in the closed-inclusive reads instead of assuming
completion order. The existing no-closed-read prime, failed-veto hold, cached-ready
equality/no-live-read checks, blocked-state check and two-read budget remain.

TestClosedReadsAreOneListAndOneQueryInEitherOrder has eleven deterministic cases:
list/query and query/list both succeed; no reads, either missing tier, either
duplicate tier, either extra tier, an extra unrelated read, and substitution of
an unrelated read all fail the predicate. Two backing-mode subtests also pass.
There is no sleep, goroutine, new import, BUILD-file change, production change,
or skip. Recorded ledger reads containing `--all` have a command argument, so
the helper's command-index access is valid for every reachable input.

Recorded TDD commits resolve locally: RED `564f98e71c2e8e4bd1cbb872f617c92947c16c4b` contains the
order-sensitive helper/table; GREEN `8a5580b5c79e4f4fcb6afeea72cf744c2c1f9738` contains the count
predicate. RED was not rerun by the deployer; the independent GREEN evidence is
the fresh full suite below. Tracker ga-7m0nf5a remains open until the fix lands.

## Test command and provenance

test_cmd_scope: full-suite
test_exec_mode: remote-exec
test_cmd: gate-criterion3.sh --repo /var/tmp/gc-merge-validate.1WuoZFke --base abea2387cdad9ee8c3453bc8283fa198bb26f67d --bead-json /var/tmp/deploy-ga-uauazip.yE0HK6A0/bead-c3-live.json --owned /var/tmp/deploy-ga-uauazip.yE0HK6A0/owned.tsv --run-dir /var/tmp/deploy-ga-uauazip.yE0HK6A0/c3-run/evidence -- bash /var/tmp/deploy-ga-uauazip.yE0HK6A0/local-full.sh
test_log_dir: /var/tmp/deploy-ga-uauazip.yE0HK6A0/c3-run/evidence/remote/logs
test_counts: PASS=131317 FAIL=1 SKIP=373
test_exec_bep: {"unit":{"results":374,"remote_executed":374,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":24,"remote_executed":24,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":85,"remote_executed":85,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":14,"remote_executed":14,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}}
farm_test_seconds: 12231.251
admission: {"class":"remote","floor_gib":25.0,"free_gib":51.99460220336914,"waited_seconds":0.001,"cap":2}
rbe_state: {"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98","worker_env_origin_main":"sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"}
fallback_reason: none
owned_topup: []
heavy_mode: none
failure_attribution: TestGcSeatHookLatencyBudget -> ga-9l9g23m (clause3a mechanism; newly tracked discovering-run exception, clauses1/4 clear)
original_c3_raw_valid: false
original_c3_raw_exit: 1
waiver_ref: none
ci_lane_run: n/a (no CI job, matrix, timeout or required-check change)

The canonical runner executes `make check`/`bazel test --config=ci //...`, both
acceptance targets, integration-tagged packages, full integration with the
registered latency budgets excluded, then those budgets alone. Exact commands
and phase windows remain in each phase stdout/BEP. All five phases force
`--nocache_test_results --noremote_local_fallback`; independent BEP predicates
require strategy remote, neither cached flag, attempt 1, an in-window start and
a terminal verdict for every result. 996 current-phase retained
log/XML artifacts hash-match CAS. The native floor augments the required farm
population; no abandoned farm/local full suites were stitched.

CI's cmd/gc-process coverage is exercised by TestTutorial01, which literally
PASSed in the integration-packages full-suite logs with GC_FAST_UNIT=0 from
--config=integration. Exact retained log: `/var/tmp/deploy-ga-uauazip.yE0HK6A0/c3-run/evidence/remote/logs/integration-packages-75f7330b9b673df65e4f.log`.

| Phase | Targets | Fresh remote results | Farm seconds | Start–end (epoch ms) | Raw result |
| --- | ---: | ---: | ---: | --- | --- |
| unit | 288 | 374 | 3043.694 | 1793206182239–1793206307434 | PASS |
| acceptance | 7 | 24 | 1105.361 | 1793206308158–1793206401976 | PASS |
| integration-packages | 36 | 85 | 3541.012 | 1793206402447–1793206709512 | PASS |
| integration-full | 1 | 14 | 4401.376 | 1793206710362–1793207331968 | PASS |
| integration-latency | 1 | 1 | 139.808 | 1793207332664–1793207476801 | FAIL — attributed to ga-9l9g23m |

latency_leg: raw FAILED under a clear farm probe, complete=false, 1
attempt(s), retired worker time 0.0 s.
Verdicts: {"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"FAIL":1}}. TestGcBdLatencyBudget
PASSed. The seat-hook test hit a step-reminder API context deadline at line171,
before its final fastest-of7 timing assertions. Probe clear after30s at load1=44.20
on32 CPUs, threshold50. The failure was never retried. The main integration leg
ran neither registered budget test. Raw execution.json and latency-leg.json remain
unchanged; neither is represented as a green/complete raw run.

Attribution: ga-9l9g23m was created and read-verified for this distinct
API-route timeout. The exact failure site is UNCHANGED with zero shared hunks.
The integration-tagged test import closure (63 module directories) and the external
gc/supervisor source inputs were checked: the only changed path is a separate
internal/beads test file, with no changed non-test input that can execute in either
binary. The helper/table runs only in beads_test and adds no I/O, process, sleep,
parallelism, target or resource-census load. Clause4 is disjoint by package.

This tracker does not predate the run: the standing gm-sf3238 discovering-run
exception applies with landed mechanism proof and clauses1/4 clear. Open ga-y39d8tx
and closed FIX-IN-TREE ga-bqsrp4o were opened; they describe final timing overruns
after API-route checks succeeded, so their mail-local remedy is not asserted to
fix this earlier API timeout. Both are linked on the new record. No BASE_REF
runtime reproduction or root cause is claimed, and no waiver was issued. Evidence:
`/var/tmp/deploy-ga-uauazip.yE0HK6A0/attribution.json`, `latency-mechanism-proof.json`,
`latency-import-closure.txt` and `latency-not-owned-proof.txt`.

floor_topup: complete, 16 commands all exit 0. Exact commands,
selection reasons and outcomes: `/var/tmp/deploy-ga-uauazip.yE0HK6A0/c3-run/evidence/remote/floor-topup.json`; individual retained
logs: `/var/tmp/deploy-ga-uauazip.yE0HK6A0/c3-run/evidence/remote/floor-logs`. Includes beadstest, Darwin compile, push-gate-lock
and local-concurrency self-tests, and every environment-absence SKIP parent,
grouped by package. Native remaining SKIPs are inventoried without being
credited as coverage for changed tests.

The failed chain had not reached this floor. It was completed through the official
gate-rbe-run.floor_topup helper in a separate durable remainder service, on the same
clean canonical tree and retained farm population, with the same admission,
isolation/environment scrub and native load wait. This runs the missing mandatory
checks; it does not repeat the failed latency measurement or restart/stitch full
suites. Driver: `/var/tmp/deploy-ga-uauazip.yE0HK6A0/attributed-floor-run`.

load_threshold: 15
load_waited_seconds: 30
load_wait_timed_out: 0

```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=30 wait_timed_out=0 run_start_load=14.73 run_max_load=18.69 run_mean_load=16.26 run_readings=7 wait_first_load=15.70 wait_max_load=15.70 wait_mean_load=15.21 wait_readings=2 read_errors=0
```

Remote admission uses a 25 GiB floor/cap 2 without a host heavy slot/load wait.
Native floor checks retain the ordinary threshold-15 soft wait above. Readings
during the wait and during the run are reported separately.

skip_inventory: farm 359 rows/302
unique package/test pairs, classes {"other": 288, "environment-absence": 71}; native
39 rows including subtests, classes {"environment-absence": 29, "other": 10}.
Top-level result-line counts above count a test once per job, not unique tests;
inventory rows also include subtests. Full names/reasons/log paths:
`/var/tmp/deploy-ga-uauazip.yE0HK6A0/farm-skip-inventory.json` and `native-skip-inventory.json`.
Canonical `--explain`/`--site` checks report UNCHANGED and no shared hunk at all
282 farm and 36
native site candidates. The one shared hunk in this diff is a helper called only
by the two owned tests; its file has no skip. Remaining skips cover unchanged
fast-unit/process separation, optional environment/tool/legacy/live-provider
probes, platform/capability cases and existing disabled scenarios. No changed
skip condition, helper or fixture reaches them. Proofs: `/var/tmp/deploy-ga-uauazip.yE0HK6A0/skip-audit.json`.

diff_tests_executed:

```text
diff_tests_executed:
  TestCachedReadyOverBdBackingHonorsAnEvictedBlockersWorkOutcome PASS (integration-packages-ceb32a3c147a95796597.log, unit-3ee1ae2eaa26dfd63a88.log)
  TestClosedReadsAreOneListAndOneQueryInEitherOrder PASS (integration-packages-20cfdbabba8f52b16e0b.log, unit-363a36f2662d541bc75e.log)
GATE_TEST_EVIDENCE_RESULT ok: 2 diff-owned tests verified by name
```

Both owned parents and all 13 owned subtests literally PASS in the unit phase's
full-suite logs; the independent per-name map is `/var/tmp/deploy-ga-uauazip.yE0HK6A0/owned-subtests-unit-proof.json`.

## Build, fast lane and environment

Criterion 6: `bazel build --config=remote-exec --noremote_local_fallback --keep_going //...`
and `make vet NOGO_BUILD_FLAGS='--config=remote-exec --noremote_local_fallback --keep_going --output_groups=nogo_fix'`.
Normal merge hooks used generated role RBE definitions through supported Make
Bazel knobs before the scratch installed its own sanctioned definitions. No hook
bypass or local execution was credited as remote evidence. Pinned lint record:
2.14.0 resolved 2.14.0, mismatch false.

policy_lane: PASS — pinned changed-format check, shell policy/hook guards,
11 policy/format/docsync Bazel targets, in their own fresh view with frozen base.
drift_lane: PASS — make bazel-sync, git diff --exit-code, empty tracked/untracked
status and seven generated-artifact targets; separate fresh view. OpenAPI
compatibility target PASS in a third fresh view. All 19 expected BEP summaries
verified. Commands: `/var/tmp/deploy-ga-uauazip.yE0HK6A0/fast-lane.sh`; verdicts: `/var/tmp/deploy-ga-uauazip.yE0HK6A0/fast-proof.json`.
The runtime suite started only after every fast lane passed.

`docs/PROJECT_MANIFEST.md` is absent; TESTING.md, Makefile, CI and the pack
release criteria supply the checks. No API/spec/dashboard change requires a
manual dashboard preview. The isolation wrapper accurately reported
PASS-THROUGH for these scratch worktrees. Farm tests are hermetic; canonical
native commands scrub live store/session selectors. ICU 74 loader/hashes OK,
bd pinned to v1.3.2/9b65b9e19, rootless Podman socket ping OK with paired Ryuk
cleanup settings; containerhost uses its documented emulated host. No library
path override, bwrap overlay, alternate test runner or narrowed-suite claim.

Evidence root: `/var/tmp/deploy-ga-uauazip.yE0HK6A0`. C3 driver: `/var/tmp/deploy-ga-uauazip.yE0HK6A0/c3-run`.
Pack evidence revision: `3a5734e69eea6311280d005af4a28d677f7b53ac`.
