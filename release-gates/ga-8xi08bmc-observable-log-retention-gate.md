# Observable test-log retention release gate

**Verdict:** **PASS**

Evaluated 2026-11-10T20:48:13.541989+00:00.
Deploy bead: ga-8xi08bmc; build: ga-rqq0vq; review: ga-1rjfuysx.
Reviewed source: `a6d246f3b91a0edc4371f9bd2645a2d96c2cc0d4`.
Frozen base: `origin/main@34485296e6bbef72f2940afea13ffcb66c5cfc80`.
Gated merge: `c8cde9cdba316cb2182b800dfa6f2d1799a08e46`; tree: `cd88422fa4d1b73812047c99102e3369f4802478`.
deploy_mode: remote; push_remote: fork; provenance: our reviewed build; stack: none.

| # | Result | Evidence |
|---|---|---|
| 1 | PASS | Reviewer records PASS on the exact resolved source. No review carryover or reviewer-added commit. |
| 2 | PASS | Acceptance 1–4 and T1–T18 verified from source and retained results: nine historical RED cases, all 14 owned roots and their subcases, nine original guards, cipolicy and resourcecensus pass. Validator tests are byte-identical between RED and GREEN; only the wrapper changes after RED. |
| 3 | PASS | Canonical full-scope gate completes with uncached remote results for all five phases, a complete native floor, by-name owned-test passes, justified unchanged skips and complete six-leaf CI accounting below. |
| 4 | PASS | Unresolved HIGH findings: 0. Reviewer highest severity: minor; two hardening findings retained below. |
| 5 | PASS | Isolated deploy branch starts at exactly the reviewed source with a clean index/worktree; gated merge scratch is also clean. This gate file is the sole subsequent commit change. Cleanliness is verified again before push. |
| 6 | PASS | Frozen-base synthetic merge is clean; complete Bazel build and nogo vet succeed. Exact parents/tree and successful terminal BEPs verified. |
| 7 | PASS | Two changes, one test-log lifecycle theme: scripts/go-test-observable and its contract tests. Both implementation commits cite the build bead; ancestry scope passes with no stack or denied path. |

merge_state: clean (origin/main@34485296e6bbef72f2940afea13ffcb66c5cfc80)
gate_base_kind: current-tip
gate_base_sha: 34485296e6bbef72f2940afea13ffcb66c5cfc80
merge_validation: this clean merged result was gated; later base movement and merge-time CI remain MPR responsibilities.

test_cmd: /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-criterion3.sh --repo /var/tmp/deploy-ga-8xi08bmc.xbtt2q5u/merge --base 34485296e6bbef72f2940afea13ffcb66c5cfc80 --bead-json /var/tmp/deploy-ga-8xi08bmc.xbtt2q5u/ga-8xi08bmc.json --owned /var/tmp/deploy-ga-8xi08bmc.xbtt2q5u/owned.tsv --run-dir /var/tmp/deploy-ga-8xi08bmc.xbtt2q5u/c3-run/evidence (rc=0).
test_cmd_scope: full-suite
Scope: bazel test //...; acceptance; complete integration packages; full integration split into ordinary and latency legs. No hand-selected subset substitutes for these phases.
test_counts: PASS=150487 FAIL=0 SKIP=515 (top-level result lines summed per job; repeated jobs count repeatedly).
test_log_dir: /var/tmp/deploy-ga-8xi08bmc.xbtt2q5u/c3-run/evidence/remote/logs
test_exec_mode: remote-exec
test_exec_bep: five raw phase BEPs in /var/tmp/deploy-ga-8xi08bmc.xbtt2q5u/c3-run/evidence/remote; independent predicates and hashes in remote-phases-audited.json.
rbe_state: {"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98","worker_env_origin_main":"sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"}
fallback_reason: none
admission: {"class":"remote","floor_gib":25.0,"free_gib":56.0713996887207,"waited_seconds":0.001,"cap":2}
farm_test_seconds: 12345.587
heavy_mode: none
owned_topup: none; all owned tests passed in the full unit phase.
floor_topup: complete; 16 successful steps: every remote environment-absence package, beadstest, Darwin compile and two runner self-tests. Native raw logs retained in remote/floor-logs; 39 subtest-inclusive native skips inventoried.
latency_leg: measured under a clear farm probe; both configured tests PASS on attempt 1; main integration leg excludes them.

diff_tests_executed:
- TestGoTestObservableConcurrentRunsKeepDistinctLogs: PASS (root and every subcase; unit raw logs).
- TestGoTestObservableDefaultLogOwnershipAndEvidence: PASS (root and every subcase; unit raw logs).
- TestGoTestObservableExplicitLogIsCallerOwned: PASS (root and every subcase; unit raw logs).
- TestGoTestObservableHousekeepingFailureKeepsProductResult: PASS (root and every subcase; unit raw logs).
- TestGoTestObservableInterruptedLogCanBePruned: PASS (root and every subcase; unit raw logs).
- TestGoTestObservableLeavesNoDetachedHousekeeping: PASS (root and every subcase; unit raw logs).
- TestGoTestObservableNeverTouchesOtherPaths: PASS (root and every subcase; unit raw logs).
- TestGoTestObservablePruneDoesNotChangeTiming: PASS (root and every subcase; unit raw logs).
- TestGoTestObservablePrunesOnlyExpiredOwnedRegularFiles: PASS (root and every subcase; unit raw logs).
- TestGoTestObservableRejectsUnownedDirectory: PASS (root and every subcase; unit raw logs).
- TestGoTestObservableReplacesStaleExplicitLog: PASS (root and every subcase; unit raw logs).
- TestGoTestObservableRetainsPassingAndFailingEvidence: PASS (root and every subcase; unit raw logs).
- TestGoTestObservableRetentionOutputGrammar: PASS (root and every subcase; unit raw logs).
- TestGoTestObservableRetentionSourceGuard: PASS (root and every subcase; unit raw logs).

policy_lane: PASS — make fmt-check-changed, lint/nogo, make check shell guards and check-hooks; 13 policy targets. Strict trusted storeclass baseline/growth predicates also PASS before the suite, with no approval label or allow-raise flag.
drift_lane: PASS — make bazel-sync, git diff --exit-code and empty full porcelain; seven generated-artifact checks and separate OpenAPI compatibility target. Each fast lane ran in a fresh view before runtime/suite; suite scratch remained clean.
ci_lane_run: n/a (no CI configuration change).
ICU74: recorded loader provenance OK; pinned test bd v1.3.4; remote worker environment matches main; no runtime provisioned on the operator host.
load_threshold: 15; load_waited_seconds: 1801; load_wait_timed_out: 1; run_start_load: 65.16; run_max_load: 60.48; run_mean_load: 50.04; run_readings: 8 (native floor, prescribed bounded soft wait; floor-topup.stdout).
Remote admission itself has no host-load wait. The outer remote chain separately reports waited_seconds=0, run_start_load=68.25, run_max_load=100.93, run_mean_load=71.35, run_readings=103; this is not a native test-load measurement.

skip_inventory: 501 remote and 39 native subtest-inclusive events; these inventories overlap the 515 top-level SKIP count and are not added to it. One configured non-Go unittest skip is recorded separately.
skip_justification: Every skip belongs to an unchanged test outside the changed scripts package; all exact source sites and files match the pinned base, and no changed shared test helper is in any skipped test binary import closure. Reasons cover selected lanes/platforms, optional or absent fixtures/tools, subprocess sentinels and explicitly pending tracked work. Native environment top-ups were executed and remaining skips inventoried. One interleaved subtest reason was recovered from its raw verbose context and unchanged t.Skip site; raw inventories were preserved. No baseline rerun is required by a changed helper or skip site. The one non-Go skip is a configured lockfile option absent from the bridge BUILD, with matching base source and inputs.
failure_attribution: none (zero failed tests); waiver_ref: none.

ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/deploy-ga-8xi08bmc.xbtt2q5u/merge 34485296e6bbef72f2940afea13ffcb66c5cfc80 a6d246f3b91a0edc4371f9bd2645a2d96c2cc0d4 fork; workflow blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs.
ci_required_accounting:
- changes | LOCAL-PASS | /var/tmp/deploy-ga-8xi08bmc.xbtt2q5u/ci-required/closure.stdout
- runner-policy | LOCAL-PASS | /var/tmp/deploy-ga-8xi08bmc.xbtt2q5u/ci-required/runner-policy.stdout
- credential-provider-windows | NOT-TRIGGERED |   credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'
- windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | /var/tmp/deploy-ga-8xi08bmc.xbtt2q5u/ci-required/windows-native-reach.json
- windows-lane | LOCAL-PASS | ['/home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh', '--from', '/var/tmp/deploy-ga-8xi08bmc.xbtt2q5u/merge', '--', '/home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh', '--threshold', '15', '--max-wait', '1800', '--', '/home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh', '--', 'python3', '/var/tmp/deploy-ga-8xi08bmc.xbtt2q5u/run-ci-job.py', 'windows-lane']; rc=0; stdout=/var/tmp/deploy-ga-8xi08bmc.xbtt2q5u/ci-required/windows-lane.stdout; stderr=/var/tmp/deploy-ga-8xi08bmc.xbtt2q5u/ci-required/windows-lane.stderr
- pack-gate | NOT-TRIGGERED |   pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'
windows_reach: none (credential-provider and native Windows allowlist closures independently computed; no changed input).
deferred_to_pr_ci: windows-lane-test (non-linux-runner)
not_covered: none
Gate PASS permits opening the PR. Merge only when CI / required and every triggered deferred job are green on its exact head.

Known minor hardening findings: a manually planted newline filename inside the reserved owned directory can split the prune list; an explicit-log run does not tighten an existing directory mode. Wrapper-generated names are sanitized, default runs establish/tighten 0700, and caller-selected logs must stay outside the reserved directory. Neither finding is HIGH.

Evidence root: /var/tmp/deploy-ga-8xi08bmc.xbtt2q5u. Raw logs are retained, not deleted. Independent audit verified 1,068 CAS artifact hashes and 534 normalized remote log copies. Acceptance and skip proofs embed names, source blobs and import closures; no tests were repeated to fix the prepared audit script.
Prepared-verifier corrections: unit-only scripts/T18 targets are verified in their actual unit universe, not required in integration-packages; source scope uses the pinned merge-base diff. One interleaved skip reason was recovered from raw log context and its unchanged source site, with original inventory preserved.
Full c6-verified.json: /var/tmp/deploy-ga-8xi08bmc.xbtt2q5u/c6-verified.json; SHA256 48f50922beb109d03c510ba3d04ede46679086a59971546a9f818a8bcfe0d2b7.
Full fast-verified.json: /var/tmp/deploy-ga-8xi08bmc.xbtt2q5u/fast-verified.json; SHA256 91ffb6dbded4fbed17b7a98b597ae07b68cd12643f7a510bc767eba7a59f7404.
Full storeclass-proof/verified.json: /var/tmp/deploy-ga-8xi08bmc.xbtt2q5u/storeclass-proof/verified.json; SHA256 42f9f07c40a7746ea64bc419ba5bd5e1267041d98fa99b1e0ced330ba2194c0e.
Full storeclass-order-note.json: /var/tmp/deploy-ga-8xi08bmc.xbtt2q5u/storeclass-order-note.json; SHA256 d712359215f1573c5f141f0eb7e452f56300152748bb96f816da91fcb29f33cd.
Full remote-phases-audited.json: /var/tmp/deploy-ga-8xi08bmc.xbtt2q5u/remote-phases-audited.json; SHA256 acb692c3c816d1f91c6daa458e73ce03cc0de03c8be653b7d54c77d76014f80c.
Full resume-c3-audit.json: /var/tmp/deploy-ga-8xi08bmc.xbtt2q5u/resume-c3-audit.json; SHA256 47f372145fe124253cbdcbf77240d8e270373600788295598350713147650663.
Full acceptance-scope-audit.json: /var/tmp/deploy-ga-8xi08bmc.xbtt2q5u/acceptance-scope-audit.json; SHA256 1766bf97310ba39fcd2cfdc32d1297b14cbf5731a4e29672ba44a134e2b425e6.
Full skip-proof-final.json: /var/tmp/deploy-ga-8xi08bmc.xbtt2q5u/skip-proof-final.json; SHA256 97a1facf347dd751a696252a25cce54967a03baaf7517049e39d957df15fc360.
Full ci-required/accounting-verified.json: /var/tmp/deploy-ga-8xi08bmc.xbtt2q5u/ci-required/accounting-verified.json; SHA256 f87b2a5b8c480fa727ccd374f81081de3ce932d1611a517550815f8ce1e1d519.
Full retention-safety-proof.json: /var/tmp/deploy-ga-8xi08bmc.xbtt2q5u/retention-safety-proof.json; SHA256 c5000f41dfd6b5669769cc56cfeeefba5f481f0df4a7b293b9148dd864442c09.
