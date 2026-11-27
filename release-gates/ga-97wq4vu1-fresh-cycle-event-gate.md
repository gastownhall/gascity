# Fresh reassignment event release gate

**Verdict:** **PASS**

Bead: ga-97wq4vu1; build: ga-uci1jl; review: ga-h1hziox (closed PASS).
Evaluated 2026-11-23T18:48:10.371052+00:00. Deploy mode: remote; push remote: fork.
Reviewed source: `88ad1c75099e5104fa6c3b44a5832f4f87bfc92c` (resolved commit, reviewer test-only extension independently verified).
Gate base: `572f981c616854780f7e4a932da5398ea31986cc`. Merge head: `5c84e05f54bc9084bcfac51b221d950a40d69a83`.
Merge tree: `a930defbc2db3919e0f945af4a0501e2d9d6cebe`; independently equal to raw `git merge-tree` output, hooks disabled when materialized.
Evidence root: `/var/tmp/gc-deploy-ga-97wq4vu1.gyvfrlfo`. `docs/PROJECT_MANIFEST.md` is absent; supplied release protocol and current Makefile/TESTING.md define the gate.

| Criterion | Result | Evidence |
| --- | --- | --- |
| 1. Review complete | PASS | ga-h1hziox closed PASS, security/spec findings read, no unresolved HIGH. reviewer-added-tests.sh check resolves X/Y and accepts only cmd/gc/session_reconciler_bead_reassign_test.go. |
| 2. Acceptance criteria | PASS | Source wires the recorder and emits session.draining after successful handoff with actor, subject, session ID and both bead IDs. Five owned test roots and 11 parent/subtest names PASS. Real private-supervisor fresh reassignment appears in CLI event output; details below. |
| 3. Tests and required lanes | PASS | Complete canonical remote suite, native floor, policy/format/drift, owned-name and skip audits, required-CI closure below. No failed criterion-3 test, no waiver or attributed criterion-3 failure. |
| 4. Review findings | PASS | Review ga-h1hziox reports verdict pass; unresolved HIGH count 0. |
| 5. Clean branch | PASS | Source and raw merge scratch have empty git status. Only this gate record is added to the isolated deploy branch; clean delivery status is checked before publication. |
| 6. Merge/build state | PASS | Clean raw merge on pinned main; full-tree bazel build and make vet passed, c6-output.stdout/.stderr, terminal rc0. No self-rebase. |
| 7. One theme | PASS | Three source files implement and test the fresh reassignment event; all three source commits cite build ga-uci1jl. No independent feature or internal-document contamination. |

merge_state: clean (origin/main@572f981c616854780f7e4a932da5398ea31986cc)
gate_base_kind: current-tip
merge_validation: gated raw merge result; merge authority must validate current PR-head merge state and required/deferred CI before merge, per merge-time guarantee.

## Acceptance evidence

Private city `/var/tmp/gc-reassign-smoke.tb3eey23` ran the built merge binary, file-backed store and subprocess probe agent. Real API assignment changed `ci-10` to `ci-15`; `gc events --since 1h --json` exited 0 and reported `session.draining`, actor `gc`, subject `probe`, session `ci-5`, sequence 53, message `fresh-mode bead reassign: cycled session probe, bead ci-10 -> ci-15`. Evidence: `manual-accepted.json` and the private city's `commands.jsonl`; supervisor log was not used. The smoke reader initially expected one JSON value and exited 1; independent parsing of the actual successful CLI's JSONL established the event. Earlier setup failures occurred before reassignment and are retained. Private supervisor stop --wait exited 0; no process with its exact private GC_HOME remained.

Success-path-only scope is deliberate: a failed post-kill handoff emits no event, pinned by reviewer-added tests. A nil recorder still permits cycling. Existing session.draining reliability accounting applies. The separate v2 event wording is outside this change.

## Criterion 3 execution

fast_lane: PASS before runtime setup or suite launch, 2026-11-23T18:02:43.015373+00:00
policy_lane: canonical make fmt-check through pinned formatter 2.14.0; full-tree nogo lint/vet; make check-release-dist-ignore check-routed-test-rows check-split-topology-rows check-residency-boundary; all rc0 in isolated fresh views (format-output.*, policy-output.*, c6-output.*).
drift_lane: BAZEL_SYNC_FLAGS='--config=remote-exec --noremote_local_fallback' make bazel-sync then git diff --exit-code; rc0, no drift (drift-output.*). Nonfatal Gazelle warnings retained. No lane rewrote the suite scratch.
heavy_mode: none (computed HEAVY_MODE, shard_required=- watch=-).
CI-config changes: none; criterion 3c not applicable.

Canonical test_cmd: gate-detached-run.sh start --run-dir /var/tmp/gc-deploy-ga-97wq4vu1.gyvfrlfo/c3 --key a930defbc2db3919e0f945af4a0501e2d9d6cebe -- env GC_GATE_REMOTE_ENABLED=1 GC_REMOTE_GATE_FLOOR_GIB=25 GC_REMOTE_GATE_CAP=2 ISOLATED_TEST_BEAD=ga-97wq4vu1 GC_GATE_DELTA=shadow GC_GATE_LOAD_DEFER_MAX=3 gate-criterion3.sh --repo /var/tmp/gc-merge-ga-97wq4vu1.ydg5d7jn --base 572f981c616854780f7e4a932da5398ea31986cc --bead-json /var/tmp/gc-deploy-ga-97wq4vu1.gyvfrlfo/bead.json --owned /var/tmp/gc-deploy-ga-97wq4vu1.gyvfrlfo/owned.tsv --run-dir /var/tmp/gc-deploy-ga-97wq4vu1.gyvfrlfo/c3/evidence

The canonical wrapper ran five complete phases through isolation/load admission; test execution is remote-exec with no fallback. Every farm test result is uncached, remote, attempt/run 1 and inside its phase window. `test_exec_bep`: unit 426/426, acceptance 25/25, integration-packages 87/87, integration-full 15/15, latency 1/1 remote. Zero cached/local/other/retried results. All 1,108 retained test.log/test.xml artifacts independently match CAS URI hashes and sizes (`remote-independent-audit.json`).

Common BAZEL_FLAGS: --config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=<phase.bep.json>. Per-phase commands/flags, BEPs and logs retained under c3/evidence/remote. Native-floor commands and all 16 rc0 steps are retained in floor-topup.json/floor-logs; these are the canonical prescribed complement, not a substitute for the suite.

test_cmd_scope: full-suite
Delta mode shadow chose full, reason not-gate-pr, applied false.
criterion3_valid: true; exit_code: 0; GATE_RUN_EXIT rc=0 state=complete.

| Phase | Targets PASS | Fresh remote results | Top-level PASS lines | Top-level SKIP lines |
| --- | ---: | ---: | ---: | ---: |
| unit | 309 | 426 | 93815 | 443 |
| acceptance | 8 | 25 | 425 | 14 |
| integration-packages | 38 | 87 | 64525 | 133 |
| integration-full | 1 | 15 | 205 | 14 |
| integration-latency | 1 | 1 | 2 | 0 |

Native floor adds 24 top-level PASS and 14 SKIP lines. Combined test_counts: PASS 158996, FAIL 0, SKIP 618. Counts are top-level result lines summed per job, not unique tests; subtests excluded. Five owned roots / 11 unique parent/subtest names all PASS, zero owned SKIP/FAIL (`owned-final-audit.json`, `current-test-evidence.stdout`).

diff_tests_executed: TestCycleAliveSessionForFreshReassign_EarlyReturnsEmitNoEvent PASS
diff_tests_executed: TestCycleAliveSessionForFreshReassign_NilRecorderStillCycles PASS
diff_tests_executed: TestCycleAliveSessionForFreshReassign_WriteFailureAfterKillEmitsNoEvent PASS
diff_tests_executed: TestReconcileSessionBeads_FreshCycleEmitsEventNamingSessionAndBeads PASS
diff_tests_executed: TestReconcileSessionBeads_FreshCycleEmitsNoEvent_WhenNoKill PASS

skip_justification: 676 skip events including subtests, 319 source sites, zero unknown. All skip-source blobs match pinned base; --site proofs are unchanged. Shared imports/helper hunks in cmd/gc were explicitly accounted for by same-command base comparisons: unit 193/193, integration-packages 68/68, native-floor 36/36, all same reasons, zero new/reason-changed/diff-owned/no-reason/unjudged (297/297). Other packages cannot import cmd/gc main/test helpers; their skip files/sites are identical. Complete identities, reasons, source proofs and logs are in current-skip-audit.json, current-skip-sites.stdout, base-unit-results, base-integration-results and base-floor-results. Supplemental comparisons do not replace any full suite phase.
failure_attribution: none; waiver_ref: none.

load_observation: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=9.64 run_max_load=12.45 run_mean_load=9.04 run_readings=52 wait_first_load=9.64 wait_max_load=9.64 wait_mean_load=9.64 wait_readings=1 read_errors=0
RBE admission floor25 GiB/free49.212 GiB, cap2, waited0.001s. ICU74 check OK. Existing rootless Podman socket verified; test selectors and private pinned bd supplied by isolation, no fleet endpoint repointed. Latency farm probe waited30s for threshold50, clear load1=49.40 on 32 CPUs; both latency tests PASS on first attempt. C6 load record: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=14.03 run_max_load=65.96 run_mean_load=47.94 run_readings=24 wait_first_load=14.03 wait_max_load=14.03 wait_mean_load=14.03 wait_readings=1 read_errors=0. First C6 compile ran locally because scratch RBE RC was initially absent; subsequent vet ran remote. Build results are not counted as tests.

## Required CI closure (criterion 3e)

ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-ga-97wq4vu1.ydg5d7jn 572f981c616854780f7e4a932da5398ea31986cc 88ad1c75099e5104fa6c3b44a5832f4f87bfc92c fork (rc0). Workflow blob: fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612. Tool: 6 LEAF jobs need a status; check is an aggregator, no independent status. Raw tool output: required-ci/closure.stdout.

ci_required_accounting:

```text
runner-policy | LOCAL-PASS | EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=<retained file> python3 .github/workflows/scripts/runner_policy.py; rc0; required-ci/runner-policy.stdout
changes | LOCAL-PASS | ci_required_triggers.py invocation below, rc0; required-ci/closure.stdout
credential-provider-windows | NOT-TRIGGERED | tool row: needs.changes.outputs.credential_provider == 'true', no path match
pack-gate | NOT-TRIGGERED | tool row: needs.changes.outputs.packs == 'true', no path match
windows-lane | LOCAL-PASS | exact workflow run commands, all four steps rc0; required-ci/windows-lane-steps.json and windows-lane.stdout
windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | runner-policy selects blacksmith-4vcpu-windows-2025; Windows native execution must pass on the PR head
```

Both Linux jobs ran one at a time in fresh merge views behind load-gate-run.sh, isolated-test-run.sh and detached services. Exact job commands/env/rc/logs: required-ci/evidence.json and *-steps.json; Windows/macOS build and test-compile commands ran as written in CI. Logs retain per-job load summaries.
windows_reach: none (windows_reach.sh rc0); windows-lane-test's own GOOS=windows test-dependency closure independently computed for .github/windows-lane/test.txt, no changed inputs, rc0; windows-accounting/reach.json.
deferred_to_pr_ci: windows-lane-test (non-linux-runner)
not_covered: none

PASS authorizes PR opening. Merge requires CI / required green on the exact PR head and windows-lane-test success (or skip only when its own filter is false). A later head requires a fresh gate. The deployer never merges.

## Retained audit

final-audit.json, acceptance-source-walk.json, tree-proof.json, owned-final-audit.json, remote-independent-audit.json and all raw logs remain under the evidence root. Prior policy HOLD is superseded by this fresh, complete run after its stamped fix landed. No prior run's results are credited here.

## Publication pre-push attribution

The normal pre-push hook ran completely at `e0897d7b407acc49a854bd49667a2f62cf205a4d`: `bazel test //... --config=fork-cache --keep_going`, 297/298 targets PASS, only `TestDatedWaiversAgainstToday` FAIL, push rc1 and remote branch absent. It checked the frozen reviewed branch, whose historical provider ledger has t3bridge/exec expiry 2026-11-05 (fatal after 2026-11-19). The complete canonical current-base merge above passed this test by name; none of its results is replaced or retried.

prepush_failure_attribution: TestDatedWaiversAgainstToday -> ga-5550q2dx, clause 3a MECHANISM. Clause1: test/file and failure sites38/41 unchanged, no shared hunks. Clause2: existing tracker created2026-11-05, opened with all comments, explicitly covers this test/condition; sighting read back. Clause3a: computed test-binary module import closure excludes cmd/gc; datedWaivers consumes literal expiry ledgers, so event/reconciler changes cannot reach this failure. Clause4: no package/path overlap. First hit on this bead (prior HOLD was an unrelated routed-test-rows condition); no new tracker, expiry edit, source refresh, test retry or waiver grant.

The non-diff-owned-gate-failure protocol authorizes a specific-head pre-push exception after this attribution. Only this gate record is updated after the tested head; implementation remains exactly the reviewed source. Final publish head is recorded on the bead and verified remotely before PR opening. Normal pre-commit docsync still applies. Full proof, terminal hook output and retained per-job logs: prepush-attribution.json, prepush-waiverexpiry-sites.stdout, prepush-waiverexpiry-closure.stdout, push-ready-output.*, prepush-artifacts.json and prepush-testlogs under the evidence root.
