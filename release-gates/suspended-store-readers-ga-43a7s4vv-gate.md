**Verdict:** **PASS**

## Suspended-store reader retirement

Bead: ga-43a7s4vv; build: ga-02lhinfz; PR: https://github.com/gastownhall/gascity/pull/9217.
This fresh record supersedes release-gates/suspended-store-readers-ga-7r9u8hlj-gate.md for the updated head.

Deploy source: 2e865bb2243d7cd9376891e18f176d737cd53c82
Reviewed fix: 24666b8b20a032238975efd065c6930453986e80 (review ga-6iz7rcj5 PASS).
MPR-reviewed head: 9b13cf0c1356ba594dc03a03979818868198d80f.
Source composition approved by mayor gcg-11005953: exact reviewed 14-line test fix above the MPR head, with both histories preserved. Component patch IDs match; no re-review required by that ruling.
Validation merge: 7e23da0756117a05cb5860df64c4ccaa749eb784; tree: 7fc3869b8b2f4d113cb7f75e05eaf06ade6bcefd.
gate_base: 5f7cd988a6be249c9d95546a4b81cd7f53614786
gate_base_kind: current-tip
merge_state: clean (origin/main@5f7cd988a6be249c9d95546a4b81cd7f53614786)
merge_validation: canonical merge result built and vetted here; Gas City also uses merge-group CI at merge time.

| # | Result | Evidence |
|---|---|---|
| 1 | PASS | Reviewer PASS at the reviewed fix; MPR head retained; component patch/tree integrity independently verified; approved composition pin. |
| 2 | PASS | Seven owned roots pass, including held cache readers and event backlog; original suspended-city and suspended-rig acceptance pass by name with configured backstop speedup 6. Deterministic autoclose completion is awaited before retire ticks. Complexity has zero growth; canonical carrier remains ga-02lhinfz. |
| 3 | PASS | All five full-scope phases, native floor, policy/drift and required-CI accounting below complete. Existing skips accounted by unchanged guards, base controls and helper reach. |
| 4 | PASS | Zero unresolved HIGH findings in the reviewer record; approved MPR changes retained. |
| 5 | PASS | Source and validation trees clean; publication guard requires the final gate-only commit and a clean working tree. |
| 6 | PASS | Whole-tree Bazel build and vet/nogo each exit 0 on the canonical merge. Both BEPs report overallSuccess. Normal pre-commit hooks active. |
| 7 | PASS | One theme: waiting for background store readers before city retirement, with deterministic regression synchronization. |

test_cmd: gate-criterion3.sh on the canonical merged tree; unit make -k check, acceptance, integration-packages, full integration minus the separate latency lane, then that latency lane.
test_cmd_scope: full-suite
test_exec_mode: remote-exec
test_counts: 155116 PASS / 0 FAIL / 613 SKIP (top-level per-job records in 557 unified logs, including the native floor; repeated roots count per job).
test_log_dir: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-43a7s4vv/gate-pass-20261117T033917Z/criterion3-run/evidence/remote/logs
diff_tests_executed: all 7 roots below PASS; reported subtests also PASS.
waiver_ref: none
failure_attribution: none
heavy_mode: none (verified classifier)
policy_lane: pinned format/lint/nogo and policy guards PASS
drift_lane: BUILD sync with empty tracked/untracked diff, generated specs/schema/client/dashboard/lockfiles/docs checks PASS
ci_lane_run: n/a (no CI-config change)
known_red_precheck: proceed fix-carrying (explicit --rig correction retained; original service lacked GC_RIG).
complexity: base and merge each have 251 diagnostic rows; no worse row and no new/regressed/deleted diff row; baseline JSON unchanged.

| Phase | Test results | Evidence |
|---|---:|---|
| unit | 415 | remote, fresh, attempt 1, PASS |
| acceptance | 25 | remote, fresh, attempt 1, PASS |
| integration-packages | 86 | remote, fresh, attempt 1, PASS |
| integration-full | 14 | remote, fresh, attempt 1, PASS |
| integration-latency | 1 | remote, fresh, attempt 1, PASS |

All 541 remote results are terminal, attempt 1, strategy remote, neither cached flag, and inside the phase window. Latency measured after a clear farm probe; both budget tests PASS. No failed test was retried.
Native floor: all 16 steps exit 0, including environment-absence top-ups, beadstest, Darwin compile and runner self-tests. Fresh pinned-base controls: four remote phases and all 16 native steps exit 0.

- TestBeadsQuiescenceDoesNotWaitAfterNonBeadEventsAreConsumed: PASS
- TestBeadsQuiescenceDoesNotWaitWithoutAnEventWatcher: PASS
- TestBeadsQuiescenceRetiresOnlyAfterAnInFlightCachePrimeFinishes: PASS
- TestBeadsQuiescenceRetiresOnlyAfterAnInFlightCacheReconcileFinishes: PASS
- TestBeadsQuiescenceRetiresOnlyAfterTheBeadEventWatcherFinishesEarlierEvents: PASS
- TestBeadsQuiescenceWaitsWhenTheEventLogHeadCannotBeRead: PASS
- TestMergeOracleFieldCoverage: PASS

skip_justification: 671 test/subtest skip events at 325 unchanged sites; no owned skip. Raw reasons were independently resolved; no environment class was missed. Thirty ambiguous conformance locations resolve to the unchanged fencedStart guard. All comparisons requiring changed shared-helper reach pass literally. The sole shared hunk is the new cmd/gc fixture/helpers; each is named and scoped in the retained helper audit, with no TestMain/init or parallel test.
Two extra literal comparisons report reason-changed because generated worker/TempDir path names differ: the nudgepoller hard-link guard observes EXDEV on both trees; workspacesvc observes distinct-case paths not naming one directory on both trees. Both test bodies/guards are unchanged, neither binary compiles the changed cmd/gc test helpers, and the workspacesvc prefix uses unchanged NewMemStore rather than CachingStore. Raw messages and comparator gaps are retained; no comparator PASS, log normalization or waiver is claimed.

ci_required_closure: 6 LEAF jobs need a status; the rest are rolled up through their needs.; workflow blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612. Derived in the canonical merged tree for fork publication.

| CI leaf | Status | Exit |
|---|---|---:|
| runner-policy | LOCAL-PASS | 0 |
| changes | LOCAL-PASS | 0 |
| credential-provider-windows | DEFERRED-TO-PR-CI (non-linux-runner) | n/a |
| pack-gate | LOCAL-PASS | 0 |
| windows-lane | LOCAL-PASS | 0 |
| windows-lane-test | DEFERRED-TO-PR-CI (non-linux-runner) | n/a |

windows_reach: credential-provider-windows=none; windows-lane-test=none.
deferred_to_pr_ci: credential-provider-windows (non-linux-runner), windows-lane-test (non-linux-runner)
not_covered: none
The Windows runtime jobs must succeed on the pushed head before merge, along with CI / required and the repository's required merge checks. Gate PASS authorizes publication, not merge.

load_threshold / wait / observed run metrics:
primary overall: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=19.25 run_max_load=19.33 run_mean_load=11.45 run_readings=44 wait_first_load=19.25 wait_max_load=19.25 wait_mean_load=19.25 wait_readings=1 read_errors=0
primary native floor: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=10.82 run_max_load=11.06 run_mean_load=10.80 run_readings=6 wait_first_load=10.82 wait_max_load=10.82 wait_mean_load=10.82 wait_readings=1 read_errors=0
base native floor: LOAD_GATE_SUMMARY threshold=15 waited_seconds=780 wait_timed_out=0 run_start_load=14.89 run_max_load=17.58 run_mean_load=16.19 run_readings=7 wait_first_load=38.27 wait_max_load=51.28 wait_mean_load=28.90 wait_readings=28 read_errors=0 prewait_s=780
Remote admission uses the remote-class policy; the native floor/control and local CI jobs retain their local soft wait. Base-native admission prewait was 780 seconds; detailed job telemetry is retained.

Review scope: per-rig-only retirement and cache priming before owner installation retain their documented limitations. Additional store-work seam tests remain tracked separately as ga-azn28djo; no acceptance criterion was silently waived. The exact interleaving of the original historical failure is not asserted.

Evidence bundle: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-43a7s4vv/gate-pass-20261117T033917Z
Full records (arrays and raw logs stay outside the PR):
- final audit: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-43a7s4vv/gate-pass-20261117T033917Z/final-evidence-audit.json; SHA256 9fbd85abf5f5c079f50a4d3a387c8ab0064a2b97acb13e57ab5c3d6ba34e531e
- phase/BEP/artifact audit: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-43a7s4vv/gate-pass-20261117T033917Z/remote-phases-independent-audit.json; SHA256 0a6082b7599645bc986bea6ea4fcce36a14e8603f998d7efd359d0514f7079be
- complete skip sites: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-43a7s4vv/gate-pass-20261117T033917Z/complete-skip-audit-plan.json; SHA256 abd91373e439588b2fbee030914f6d8639c97950d5f1ae51520b0ee9aa63b065
- shared/helper reach: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-43a7s4vv/gate-pass-20261117T033917Z/shared-helper-audit.json; SHA256 29e134e7cac3dc222596c29865d2aadc132cd6850a2f21623624c96108c96209
- raw filesystem gap proofs: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-43a7s4vv/gate-pass-20261117T033917Z/filesystem-skip-gap-proofs.json; SHA256 b5a1a0b4d0b4af2c4cfb8c46bf4623c0583b6335d3d72895dbe319c5ec7bbe3d
- CI commands/results: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-43a7s4vv/gate-pass-20261117T033917Z/ci-required/accounting.json; SHA256 38ed07a3fa662a6dcb806faeb5af0aed5ab6c3b684a88bd54e69b2d9c5c202c7
- component patch integrity: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-43a7s4vv/gate-pass-20261117T033917Z/composition-patch-integrity.json; SHA256 d6e1929690285d75fe2c37fc068c7371af7d7528566788489c923704475f9f63
