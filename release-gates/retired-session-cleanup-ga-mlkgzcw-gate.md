# Retired-session cleanup release gate

**Verdict:** **PASS**

Deploy bead: ga-mlkgzcw. Build: ga-mu5isj. Review: ga-kb96cis, round 2 PASS.
DEPLOY_MODE: remote. BASE_REF: origin/main. No runtime deployment requested or performed.

Reviewed source: `9dacdddfae1b31d5f90bd585aa54af0be563abdc`.
Implementation base: `d53530d63fabc668cd0025d51268bb546d7f83a7`.
Gated merge: `a1f5c16dcd83ccdd932bd5db878dfc2648e6fa71`; tree equals the pinned base plus the reviewed source, verified independently with git merge-tree.
gate_base: `6a12f4a43785b437530bb14f8a8848d0411d1255`.
gate_base_kind: `current-tip`.
merge_state: clean (origin/main@6a12f4a43785b437530bb14f8a8848d0411d1255; no conflicting files).
merge_validation: merge result built and vetted here; MPR owns merge-time reconciliation and the required checks.
merge_scratch: `/var/tmp/gc-merge-validate.ybarXm` (temporary; removed after evidence retention).

## Criteria

| # | Verdict | Evidence |
| --- | --- | --- |
| 1 Review PASS | PASS | ga-kb96cis records round 2 PASS at the reviewed source. Independently ran reviewer-added-tests.sh: reviewer_tests_verified cbae6a7d85cf86e7e00f3e6a3ab11e1a2dde6470 -> 9dacdddfae1b31d5f90bd585aa54af0be563abdc files=cmd/gc/session_beads_cleanup_bound_test.go. |
| 2 Acceptance | PASS | Code, test bodies, resolved RED/GREEN objects and the acceptance map below checked independently. Deferred PR clauses are prepared and must be read back from the actual PR before handoff. |
| 3 Tests | PASS | Full CI-equivalent four-phase run, every test result first-attempt uncached remote, plus required native floor. All 15 diff-owned tests verified by name; no waiver. |
| 4 Findings | PASS | Round 2 review has zero unresolved HIGH/major findings; accepted minor limitations are disclosed below and in the PR. |
| 5 Clean branch | PASS | Source/scratch clean before the run. The only deploy-branch addition is this gate record, committed through the normal pre-commit hook; clean status is verified before push and PR-open. |
| 6 Build and merge state | PASS | Clean merge against the pinned base; bazel build --config=remote-exec //cmd/gc:gc and make vet with remote nogo flags both rc0. Full nogo/vet covers 1357 targets. |
| 7 One feature theme | PASS | Seven source commits bound the same retired-session cleanup. Ancestry scope guard passed accepting the deploy/build/rework/validator/review IDs ga-mlkgzcw, ga-mu5isj, ga-5rdduni, ga-8ujo8ac, ga-kb96cis. No internal configuration contamination, stack or independent feature. |

## Fast lane

policy_lane: PASS — make lint (repository-wide nogo), check-release-dist-ignore, check-routed-test-rows, check-split-topology-rows and check-residency-boundary, each in a fresh view with the pinned gate base.
format_lane: PASS — run-pinned-lint.sh -- make fmt-check; pin/resolved 2.12.0, mismatch=false.
drift_lane: PASS — make bazel-sync BAZEL_SYNC_FLAGS=--config=remote-exec followed by git diff --exit-code, no drift; a separate fresh view ran this entire lane.
hooks: make check-hooks PASS; core.hooksPath=.githooks. Normal pre-commit ran for the scratch merge and runs again for this gate commit.
ci_lane_run: n/a (no CI configuration change).
heavy_mode: none (mandatory classifier).

policy_attribution: make complexity-check -> ga-q7p6is, the open stale-baseline tracker created before this run. The same-run untouched pinned gate base and candidate fail with identical 253-line normalized output (sha256 `9e65e43e80398af5c956dc41008f94db99ae01b1c88c9af867efcd00ab5a87ff`). make complexity-diff against that base PASSes with no threshold changes. Baseline, checker and census are untouched; no new test target. This is criterion 3b's static-policy mirror attribution, not a waiver. The sighting was appended to the tracker and read back.

The historical docs/PROJECT_MANIFEST.md is absent; current TESTING.md, Makefile, required CI workflows and the deployer criteria determine the command scope. CI jobs reproduced: unit/policy/generated artifacts, acceptance, integration-packages and full integration; the last has no smoke filter. Dashboard checks run through the unit Bazel gate; this diff changes no API, OpenAPI or dashboard path requiring manual preview.

## Fresh test evidence

test_cmd_scope: full-suite
test_exec_mode: remote-exec
test_cmd: gate-criterion3.sh with the documented four full phases:

```json
[
  "test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gate-ga-mlkgzcw-20261017/criterion3/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec",
  "test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gate-ga-mlkgzcw-20261017/criterion3/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec",
  "test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gate-ga-mlkgzcw-20261017/criterion3/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec",
  "test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gate-ga-mlkgzcw-20261017/criterion3/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec"
]
```

test_counts (retained result events from the remote phases and native floor; repeated phases/subtests are counted by the canonical parser):

```json
{
  "FAIL": 0,
  "PASS": 116935,
  "SKIP": 370
}
```

test_log_dir: `/var/tmp/gate-ga-mlkgzcw-20261017/criterion3/evidence/remote/logs`.
run_environment: disposable merge scratch through isolated-test-run.sh --rbe. The wrapper reported PASS-THROUGH (Gas City is outside its clone-isolation list), not ISOLATED. Farm phases execute hermetically through Bazel; the native-floor runner explicitly scrubs GC/BD/BEADS/DOLT selectors. The wrapper pins test bd v1.3.1 (c1c4b642a) without changing the fleet bd.
test_exec_bep:

- `unit`: BEP `/var/tmp/gate-ga-mlkgzcw-20261017/criterion3/evidence/remote/unit.bep.json`; predicate `/var/tmp/gate-ga-mlkgzcw-20261017/criterion3/evidence/remote/unit.evidence.json`: `{"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 371, "results": 371, "retried": 0, "window_ok": true}`
- `acceptance`: BEP `/var/tmp/gate-ga-mlkgzcw-20261017/criterion3/evidence/remote/acceptance.bep.json`; predicate `/var/tmp/gate-ga-mlkgzcw-20261017/criterion3/evidence/remote/acceptance.evidence.json`: `{"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 24, "results": 24, "retried": 0, "window_ok": true}`
- `integration-packages`: BEP `/var/tmp/gate-ga-mlkgzcw-20261017/criterion3/evidence/remote/integration-packages.bep.json`; predicate `/var/tmp/gate-ga-mlkgzcw-20261017/criterion3/evidence/remote/integration-packages.evidence.json`: `{"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 81, "results": 81, "retried": 0, "window_ok": true}`
- `integration-full`: BEP `/var/tmp/gate-ga-mlkgzcw-20261017/criterion3/evidence/remote/integration-full.bep.json`; predicate `/var/tmp/gate-ga-mlkgzcw-20261017/criterion3/evidence/remote/integration-full.evidence.json`: `{"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 14, "results": 14, "retried": 0, "window_ok": true}`

Each phase's retained predicate proves strategy=remote, neither cached flag, attempt 1, start inside its phase window and terminal verdict for every result. Test-result cache hits never count as PASS.
fallback_reason: none. owned_topup: `[]`.
farm_test_seconds: 8189.026.
waiver_ref: none (Gas City has no waiver path).
failure_attribution: tests none; only the static-policy condition attributed above.

rbe_state:

```json
{
  "cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT",
  "kill_switch_absent": true,
  "rc_marker_present": true,
  "worker_env_origin_main": "sha256:0c6c05c2196c246dba37e106a3d028e2f47f2e3d7eae0e41c2fe2bd9b2e4f6f0",
  "worker_env_tree": "sha256:0c6c05c2196c246dba37e106a3d028e2f47f2e3d7eae0e41c2fe2bd9b2e4f6f0"
}
```

admission:

```json
{
  "cap": 2,
  "class": "remote",
  "floor_gib": 25.0,
  "free_gib": 77.76848220825195,
  "waited_seconds": 0.001
}
```

Remote admission uses the 25 GiB floor/cap 2 with no host-load wait. Required native top-ups use the local soft load wait. Load records, independently extracted from the retained driver output:

```text
remote chain: run_start_load=15.54 run_max_load=17.94 run_mean_load=12.87 run_readings=33 wait_first_load=15.54 wait_max_load=15.54 wait_mean_load=15.54 wait_readings=1 read_errors=0 load_threshold=15 load_waited_seconds=0 load_wait_timed_out=0
native floor: run_start_load=10.87 run_max_load=14.36 run_mean_load=12.38 run_readings=6 wait_first_load=10.87 wait_max_load=10.87 wait_mean_load=10.87 wait_readings=1 read_errors=0 load_threshold=15 load_waited_seconds=0 load_wait_timed_out=0
```

floor_topup: complete, 16 steps; beadstest, Darwin compilation, runner self-tests and every environment-absence package/parent included. Step commands, rc and retained logs are in `/var/tmp/gate-ga-mlkgzcw-20261017/criterion3/evidence/execution.json`. No failed floor step or diff-owned SKIP is accepted.
skip_inventory: `/var/tmp/gate-ga-mlkgzcw-20261017/criterion3/evidence/execution.json`, `/var/tmp/gate-ga-mlkgzcw-20261017/skip-inventory.json` and `/var/tmp/gate-ga-mlkgzcw-20261017/native-skip-inventory.json`, with per-test reasons and log paths.

```json
{
  "environment-absence": 71,
  "other": 285
}
```

skip_justification: none of the 15 owned tests skipped. Remaining SKIPs are existing cadence splits (process coverage runs in integration-packages), environment/platform/opt-in conditions, unsupported provider capabilities, subprocess helper cases, and explicit pre-existing quarantine/placeholder rows. Their skip decisions do not depend on this change: it adds no env/feature flag, provider capability, platform condition, helper mode or skip statement. Every environment-absence package was topped up locally; 39 remaining non-owned native skip events are explicitly inventoried, including missing replay/simulator artifacts, postgres/br/herdr setup, provider-unavailable conformance fixtures and tmux bind-key cases outside a client. These leave no missing evidence for the cleanup change. The unchanged quarantine and placeholder reasons are retained rather than relabelled as executed passes.

diff_tests_executed (independently re-read with gate-test-evidence.py):

```text
GATE_TEST_EVIDENCE logs_dir=/var/tmp/gate-ga-mlkgzcw-20261017/criterion3/evidence/remote/logs logs=506 logs_with_results=440
test_counts: PASS=116935 FAIL=0 SKIP=370
  (top-level result lines, column 0, summed over the 506 logs; a test that several jobs run counts once per job; sub-tests are not counted)
logs_without_results: floor-13.log floor-14.log floor-15.log unit-02e0324f1ef05430d7e4.log unit-04c4f9e4335a6f1c2dfa.log unit-0787047af2c4dad6aebc.log unit-0eb92a6bd00590d91db6.log unit-0f39cb7e4c03399a72c4.log unit-1a34395a607c0208a005.log unit-1e67f201b300dc5f9e78.log unit-21372f682c4033d14a80.log unit-22d5121476fcaa40b586.log (+54 more)
diff_tests_executed:
  TestCancelStateAssignedToRetiredSessionBeadBoundsSlowExtmsgLeg PASS (integration-packages-509ef0a6cd81866833cc.log, unit-d9e667428f972d067bd3.log)
  TestCancelStateAssignedToRetiredSessionBeadBoundsSlowWaitsLeg PASS (integration-packages-c2ab4e7d4735e1bd66a7.log, unit-1ab526405ace21879f85.log)
  TestCancelStateAssignedToRetiredSessionBeadExtmsgLegCutOffLogsExactlyOneLine PASS (integration-packages-18745ddf7d62465654f5.log, unit-912ab643330b35bd0b6b.log)
  TestCancelStateAssignedToRetiredSessionBeadHealthyStoreIsNotCutOff PASS (integration-packages-251f1cf8cede4438b2d5.log, unit-fd986cf5bab8709b8d45.log)
  TestCancelStateAssignedToRetiredSessionBeadOrdinaryParticipantErrorIsNotCutOff PASS (integration-packages-75f7330b9b673df65e4f.log, unit-1cc8c8ab55e59a9e03a9.log)
  TestCancelStateAssignedToRetiredSessionBeadOrdinaryWaitErrorIsNotCutOff PASS (integration-packages-568812e4d525bdd1f974.log, unit-752a0bf25b15b465517a.log)
  TestCancelStateAssignedToRetiredSessionBeadSuccessfulWaitOvershootIsNotCutOff PASS (integration-packages-cb110414c557c0928e82.log, unit-abd97d2cb9b450225d57.log)
  TestCancelStateAssignedToRetiredSessionBeadWaitsLegCutOffLogsExactlyOneLine PASS (integration-packages-bc482d2ae768251ad47f.log, unit-f93a0ca96ccc61d0cae9.log)
  TestCancelWaitsAndCollectNudgeIDsReturnsAllNudgesAfterLookupLimit PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestCancelWaitsAndCollectNudgeIDsReturnsObservedNudgesOnCancelError PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestCancelWaitsBackgroundCtxKeepsOneCloseAllPerIDClass PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestCancelWaitsCancelInsideCloseAllLeavesLaterChunksOpen PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestCancelWaitsCancellableCtxChunksCloseAll PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestCancelWaitsPreCancelledCtxMakesNoStoreCalls PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestCancelWaits_CancelsLegacyWaitBeadsWithoutLegacyTypeQuery PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
GATE_TEST_EVIDENCE_RESULT ok: 15 diff-owned tests verified by name
```

## Acceptance map

| Build AC | Independent deploy-stage verification |
| --- | --- |
| 1 | RED and GREEN commit objects resolved from the review handoff. RED adds tests before production; recorded assertions and the reviewer's fresh red proofs are carried into the PR verbatim in substance. The healthy-store case is an intentional characterization guard. Fresh full-suite verification of every owned test is required below. |
| 2 | Read the CancelWaits signature, storebinding interface, manager/cmd_wait callers and waits_test call sites: ctx first; background callers preserve their behavior. The doc comment describes in-flight calls, returned nudge IDs and wrapped ctx errors. |
| 3 | Read the pre-lookup ctx guard and cancellable CloseAll chunk loop. The nil-Done branch calls CloseAll once per ID class. Four named session tests cover background, cancellable, pre-canceled and mid-close cancellation behavior. |
| 4 | Read the unchanged helper signature, 10-second documented const, two WithTimeout(Background) calls, immediate waits cancel and deferred extmsg cancel. No rollback/closeBead signature changed. |
| 5 | Read waitsCutOff before cancelWaits, with err AND own ctx done; extmsg cutoff classification is inside its error branch. The exact-one-line tests cover both legs, ordinary-error tests cover each existing error line, and the successful-overshoot test rejects a false cutoff. Capped-stamp code is unchanged. |
| 6 | Slow-waits test asserts some closed/some open, waits diagnostic and cleaned extmsg membership. Slow-extmsg test asserts extmsg diagnostic, remaining external state and all waits closed. Healthy-store test asserts complete cleanup with no cutoff. All use virtual time. |
| 7 | Helper comment names session-closed wake-state cleanup and the binding reaper; it explicitly states there is no participant/membership backstop. Those existing paths are present. |
| 8 | No baseline/script/census change. make complexity-check and make complexity-diff are recorded with any base-proven attribution; cancelWaitsAndCollectNudgeIDs gets the planned extraction. |
| 9 | Read both added test files: no time.Sleep or polling; cmd/gc scenarios run synctest against the real leg budget and assert ranges without naming the private session chunk const. internal/extmsg is untouched. Regression test results are checked in the fresh full-suite logs. |
| 10 | Source adds the unique SESSION-RUNTIME-021 requirements row. internal/session/AGENTS.md is untouched and that fact is stated in the PR, regardless of the separate convention rollout's landing state. Docsync is included in the full gate. |
| 11 | Production additions have comments describing current behavior. Build/nogo-vet and active-hook evidence are recorded; fresh-view format and BUILD sync plus the full test command provide the remaining checks. Test methods follow the surrounding package conventions. |
| 12 | PR draft says Refs #7170, names the resolved base and RED/GREEN objects, includes recorded before/after assertions, explicitly requests no deployment and states no comment/issue is being posted upstream. Diff contains no pack/city/hook/host configuration and no new config key. These deferred text clauses must be read back from the opened PR before reporting final PASS. |

Known accepted minor findings: participant/membership residue has no backstop; the existing v2 effect wiring passes nil stderr; the capped diagnostic can still write its existing stamp after the waits budget. No production or requirements fix is added from the deployer seat.

## Reviewable PR content and retained artifacts

PR body includes Refs #7170 (no closing keyword), resolved implementation base, recorded RED/GREEN and before/after assertion output, untouched internal/session/AGENTS.md, no pack/city/hook/host configuration or new config key, no deployment request and no upstream comment/issue. Those clauses are verified against the opened PR body before the deploy bead closes. On verified landing, mayor/MPR is asked to notify mayor and the architect; the parent design record is untouched.

Retained evidence directory: `/var/tmp/gate-ga-mlkgzcw-20261017` (protected from scratch reaping). Includes raw build/vet/format/policy/drift/complexity logs, pinned-base comparison proof, owned-test inventory/explanation, authenticated PR/head/body/status readbacks and handoff verification. Test logs are retained; only temporary checkouts and their own Bazel output bases are removed.
