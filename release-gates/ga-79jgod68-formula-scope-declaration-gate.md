# Formula-level scope declarations

**Verdict:** **PASS**

Reviewed source: `898e6f8e598f98114c1ea5e70663e59ebf835fc5`. Deploy `ga-79jgod68`; build `ga-tnirrt`; review `ga-anpk4y1j`. No review carryover or reviewer-added commit.
Tested merge: `275e10e1da75956e1014bd626ab3c4f505ef2b0f` (tree `52fe93bac6e42e726844fd386f25b41b7512773a`); immutable `refs/release-gates/ga-79jgod68/tested-merge`.
merge_state: clean (origin/main@1e7e600f2cc7474d5020a43cea7e9d5206716a95)
gate_base_kind: current-tip
merge_validation: MPR owns the merge and the required PR checks on its current head.

| # | Verdict | Evidence |
|---|---|---|
| 1 | PASS | Reviewer verdict pass binds to the resolved reviewed source; full review and build records retained. |
| 2 | PASS | Eleven owned tests cover stamping, inheritance, whole replacement by name, precedence, missing members and unchanged behavior without declarations. Independent disposable body-only and all-steps override probes pass, including the new terminal member. The new specification TOML parses and its fields match ScopeDecl. |
| 3 | PASS | Complete canonical suite, fast policy/drift lanes, all owned names, skip comparisons and six-leaf CI accounting below. |
| 4 | PASS | Zero unresolved HIGH findings. Two minor correctness cases and a missing committed body-override regression remain on ga-cpddjman, explicitly not claimed fixed. |
| 5 | PASS | Source and tested scratch clean. This record is the only deployer change; publication guards require clean status after its commit. |
| 6 | PASS | Exact clean merge built with Bazel and nogo: overallSuccess=true, exit SUCCESS. Scratch merge used hooks off; no rebase. Build strategy: 220 remote,19683 cache,2796 internal actions; build evidence gives no suite credit. |
| 7 | PASS | One formula compiler feature across five internal/formula files and the normative specification; no pack adoption bundled. |

## Tests and policy

test_cmd: gate-criterion3.sh with the pinned base, current bead JSON, tool-derived owned.tsv and this run directory. Exact argv and all phase flags are retained.
test_cmd_scope: full-suite
test_counts: 159299 PASS, 0 FAIL, 618 SKIP (top-level result lines across 571 retained logs).
test_exec_mode: remote-exec
test_exec_bep: 555/555 results remote, attempt1, valid phase windows, cached_remotely=0, cached_locally=0, other_strategy=0, retried=0.
farm_test_seconds: 13786.786
rbe_state: marker present, kill switch absent, certificate valid; tested-tree and base worker pin both sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72.
admission: remote; floor25GiB, cap2, free53.426GiB, waited0.001s.
fallback_reason: none
delta_gate: shadow/full, not-gate-pr; no narrowing applied.
owned_topup: none
floor_topup: all16 steps rc0, including environment-dependent native tests, Darwin compile and runner self-tests.
latency_leg: measured, attempt1; both budget tests PASS under clear worker probe (threshold50, max load1=37.50,6 readings).
heavy_mode: none
waiver_ref: none
failure_attribution: none
ci_lane_run: n/a (no CI-config change).

| Phase | Forced remote results | Scope |
|---|---:|---|
| unit | 427 | make -k check: all unit targets plus shell guards |
| acceptance | 25 | acceptance_test and acceptance_solo_tests |
| integration-packages | 87 | complete integration_packages suite |
| integration-full | 15 | full integration suite minus dedicated latency tests |
| integration-latency | 1 | dedicated clear-probe latency tests |

diff_tests_executed:

- TestCompileScopeDeclStampsBeforeGraphControls PASS
- TestResolveScopeDeclChildDeclarationReplacesTheParentsByName PASS
- TestResolveScopeDeclDoesNotLeakIntoAParentOrItsCache PASS
- TestResolveScopeDeclEarlierParentWinsForASharedName PASS
- TestResolveScopeDeclOnlyTheNamedScopeIsReplaced PASS
- TestResolveScopeDeclRejectsABadDeclaration PASS
- TestResolveScopeDeclRejectsATypoWithNoExtends PASS
- TestResolveScopeDeclRejectsMalformedNamesAndTeardown PASS
- TestResolveScopeDeclStampsMembersBodyAndTeardown PASS
- TestResolveScopeDeclSurvivesAnOverrideAcrossExtends PASS
- TestResolveScopeDeclWithoutADeclarationChangesNothing PASS

policy_lane: PASS — git diff --check at pinned base; make lint (full nogo); go_format_test; make check-release-dist-ignore check-routed-test-rows check-split-topology-rows check-residency-boundary.
policy_attribution: none
drift_lane: PASS — separate fresh view ran make bazel-sync followed by git diff --exit-code. Both fast lanes ran before runtime setup/suite and used gate-base.sh run.
test_log_dir: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-79jgod68/gate-pass-20261125-_1q6s_5u/criterion3/evidence/remote/logs

skip_inventory: 604 farm rows (514 other,90 environment-absence); local floor72 rows. Eleven remote baseline groups clear290 skip occurrences; one exact-command native base group clears36. Every compared row is pre-existing, with no new/changed-reason/diff-owned/no-reason row.
skip_justification: 318 ordinary source sites plus4 resolved runtime/dispatch sites are UNCHANGED;146 farm skip-source files are blob-identical to the pinned base. All45 native sites are UNCHANGED. Shared hunks are the3 test helpers in the new internal/formula/scope_decl_test.go; no HELPER-HUNKS. Test-binary closures computed for227 roots per tag set identify formula reach (16 unit,18 integration roots); every skipping reached group received a fresh same-command base comparison. Other skipped providers/packages do not import formula; reasons are unchanged optional PostgreSQL/br/herdr inputs, runtime capabilities, tmux context, fixture shape, platform and explicit lane gates. Full per-row site/reach/reason evidence retained.
COMPARE-NOTE: only the remote account differs for TestInfraScopedManagedDoltPIDIdentityUnderRealSystemd (base rbe-a00, head rbe-a09); official comparison accepts it. Raw logs are unchanged.

load_threshold: 15
load_waited_seconds: 1800 (native floor)
load_wait_timed_out: 1
run_start_load: 43.48
run_max_load: 67.93
run_mean_load: 56.77
run_readings: 12
Native floor wait max120.70/mean80.91,61 readings. Outer remote wrapper waited0s; host start29.73/max121.15/mean64.18,111 readings. Native baseline waited990s, no timeout, start14.97/max15.66/mean15.36,3 readings. CI compile waited1021s, no timeout, start14.97/max21.59/mean17.16,5 readings.
GATE_ICU74: OK, all three pinned ICU74 loader libraries verified; full lines retained.

## Required CI accounting

ci_required_closure: ci_required_triggers.py /var/tmp/gc-merge-validate.T4BUl1 1e7e600f2cc7474d5020a43cea7e9d5206716a95 898e6f8e598f98114c1ea5e70663e59ebf835fc5 fork; workflow blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612;6 LEAF jobs need a status.
ci_required_accounting:

- changes | LOCAL-PASS | /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-79jgod68/gate-pass-20261125-_1q6s_5u/ci-required/closure.stdout; command exit0
- runner-policy | LOCAL-PASS | /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-79jgod68/gate-pass-20261125-_1q6s_5u/ci-required/runner-policy.stdout; command exit0
- credential-provider-windows | NOT-TRIGGERED |   credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'
- pack-gate | NOT-TRIGGERED |   pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'
- windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-79jgod68/gate-pass-20261125-_1q6s_5u/ci-required/native-windows-reach.json
- windows-lane | LOCAL-PASS | /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-79jgod68/gate-pass-20261125-_1q6s_5u/ci-required/windows-lane-output.stdout; own four run steps, exit0: Windows build/test compilation, Windows nogo build/test compilation and all-package macOS build/test compilation

windows_reach: none — both native Windows allowlist closure tag sets and the official credential-provider closure exit0 with no changed path.
deferred_to_pr_ci: windows-lane-test (non-linux-runner)
not_covered: none
PR-open clearance only. Merge requires CI / required green on the current head and native Windows tests successful (or skipped only by their own path filter).

## Retained evidence

Evidence root: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-79jgod68/gate-pass-20261125-_1q6s_5u
SHA256-MANIFEST.json: 2307 files; SHA256 ec7426d9f960f2b4f5c7b4c2e5c2641b849f7ef5ae49908f0cad41d1b75e1f2f.
1132 farm/baseline raw CAS log/XML artifacts independently digest-verified. Focused AC raw log digest matches its BEP; its optional XML-copy collector failure is preserved, corrected from the same successful log/BEP with no test rerun and no suite credit. Paths inside archived JSON name the original run; the relative layout is preserved here.
Full execution.json retained: 6065 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-79jgod68/gate-pass-20261125-_1q6s_5u/criterion3/evidence/execution.json; SHA256 ca74b60c4bf2fb99e8c9212cb3ef6a46e331b9b97fea5f1f9423cfcce780b056.
Full skip site/reach proof retained: 19 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-79jgod68/gate-pass-20261125-_1q6s_5u/skip-proof-summary.json; SHA256 b8bb021e9b3632e7feabd90d3551742a50adc6579e1ee93cdef15dd9a2b4ff0f.
Full native skip comparison retained: 37 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-79jgod68/gate-pass-20261125-_1q6s_5u/native-skip-baselines/group-0-compare.txt; SHA256 20718ca174b4326e6dd20863f0f9837d3b040471accf063729bc34d9f9935a52.
Full CI commands retained: 19 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-79jgod68/gate-pass-20261125-_1q6s_5u/ci-required/windows-lane-commands.json; SHA256 b1674ced3d2305bfa90ce537b7cf6565f15239e3f8bf9b4035a2f1b1969df8a9.


## Publication-stage source snapshot

The normal pre-push hook at `ffd3e5820b8e41951978cf070e2338e173af8f0f` selected fork-cache/local because the bare reviewed source pins an older worker image. It finished with297 targets PASS and3 failed targets: the dated-waiver policy and the scripts/dispatch five-minute package budgets. These are publication-hook results, not a replacement for the fresh merged-tree criterion3 evidence above.

prepush_failure_attribution:

- TestDatedWaiversAgainstToday -> ga-5550q2dx, predating stale-source waiver tracker. Clauses1/4: test/site41 unchanged, no path overlap. Clause3(a): actual-head unit and integration test-binary closures contain no changed formula package; waiver inputs are untouched. Fresh merged gate target PASS.
- scripts_test shards3/6 and4/6 -> ga-vg3wto5, predating package-budget-under-load tracker. Clauses1/4: scripts and last-running test bodies unchanged, no path overlap. Clause3(a): both actual-head closures exclude formula; the diagnostic and worker-env fixture commands consume unchanged inputs. Aggregate300s ceiling fired with the last tests only1s/25s old. Fresh merged gate scripts shards PASS.
- dispatch_test -> ga-pxiuas5, closed with its repair LANDED. Clauses1/4: dispatch and the last-running test unchanged; no path overlap. Clause3(d): the independent build record ga-tnirrt reproduced300.4s without the diff at its original base `f518cf2538a6e54b839457d538577f48683396f9`, load68/76/80. That object and its exact merge-base relation were revalidated. Repair `67617358b143bbd00be000d615133fa1aea42da6` is absent from the bare source but present in this run's tested merge, where all five dispatch results PASS. The gate already tested the refreshed merge; the reviewed SHA was not rebased.

All trackers were opened and predate this run; each new sighting was appended and read back by comment ID. This is the first hit for this deploy, not a rerun of a previously attributed signature. The standing pre-push exception permits publishing the specifically attributed, code-identical head with ownership/ancestry/source/size guards retained. No criterion3 waiver, operator waiver_ref, or success claim for the failed hook is used. This publication record edits no application or test input.

Full publication attribution, original hook logs, source/reach/site proofs and independently recorded base proof: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-79jgod68/publication-20261125-rhd47u01; manifest SHA256 e5a4697abe6e56a51ca55a3458276c8a886173f7b52f4394ff7ea5721099537a. Canonical release evidence and its original manifest are unchanged.
