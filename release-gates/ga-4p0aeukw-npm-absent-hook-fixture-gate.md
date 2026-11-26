**Verdict:** **PASS**

# npm-absent pre-commit fixture release gate

bead: ga-4p0aeukw; build: ga-2uxmyqq0; review: ga-c6888v5p.
reviewed_commit: ff5b1e741f13268f511784451d97643d9865510f
gate_base: 0ea6a79188d7315e9fd7183a7934b0ad7be5ba61
tested_merge: 058a673ec2188e3c669d69004e53ce97be0323dc
tested_tree: e7e54713ab396582e649e6707dd7ec5ed3aaf676
deploy_mode: remote; push_remote: fork; deploy_branch: deploy/ga-4p0aeukw-gate.
evidence_root: /var/tmp/gc-ga-4p0aeukw-gate.a2l8ip4_; relative evidence paths below refer to this retained directory.
criteria_source: release-gate-criteria.md.tmpl and TESTING.md (docs/PROJECT_MANIFEST.md absent).

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS | PASS | Freshly read ga-c6888v5p verdict PASS names the resolved reviewed source; no carryover or reviewer-added commit. |
| 2 | Acceptance | PASS | Restricted PATH includes real cat and still excludes npm. Four restricted-PATH cases each pass 10 fresh runs with the pipeline writer delayed to force the old SIGPIPE race: 40 PASS, 0 FAIL, 0 SKIP. The deterministic new helper test and npm-absent failure contract pass. acceptance-proof.json and forced-writer.diff; focused evidence only. |
| 3 | Full suite and required CI | PASS | Fresh complete five-phase canonical Bazel chain, native floor, skip proof, policy/drift and six CI accounts below. |
| 4 | No unresolved HIGH finding | PASS | Reviewer reports no blocker, major or minor; no unresolved HIGH finding. |
| 5 | Clean branch | PASS | Source and tested merge clean; final deliverable adds only this gate record. Publication verifies cleanliness after its commit. |
| 6 | Merge state and build/vet | PASS | Pinned clean merged tree; bazel build --config=remote-exec --noremote_local_fallback //... includes nogo; rc0 and BEP finished.overallSuccess=true. c6-proof.json. |
| 7 | One feature theme | PASS | One scripts test-support file (+26/-2), three commits all citing build ga-2uxmyqq0. Ancestry scope guard PASS for deploy/build ids; no stack or denied path. ancestry-proof.json. |

merge_state: clean (origin/main@0ea6a79188d7)
gate_base_kind: current-tip
merge_validation: this record pins the tested merge above; current mergeability and merged-result CI belong to merge authority.

test_cmd: gate-criterion3.sh --repo /var/tmp/gc-merge-validate.PYvwH3 --base 0ea6a79188d7315e9fd7183a7934b0ad7be5ba61 --bead-json /var/tmp/gc-ga-4p0aeukw-gate.a2l8ip4_/bead-suite.json --owned /var/tmp/gc-ga-4p0aeukw-gate.a2l8ip4_/owned.tsv --run-dir /var/tmp/gc-ga-4p0aeukw-gate.a2l8ip4_/criterion3/evidence; detached through the isolation wrapper, remote enabled, cap2/floor25GiB.
test_cmd_scope: full-suite
test_counts: 159275 PASS, 0 FAIL, 618 SKIP (column-zero result lines across 571 logs; subtests excluded).
diff_tests_executed: TestPreCommitFailsClosedWhenGoBlockStagesSpecAsSideEffectAndNpmAbsent PASS (unit-ee115c844954da53d34d.log); TestRestrictedPathWithoutNpmProvidesToolsTheDrainStubUses PASS (unit-271bc77ad6715453dfe1.log).
test_log_dir: /var/tmp/gc-ga-4p0aeukw-gate.a2l8ip4_/criterion3/evidence/remote/logs
failure_attribution: none; waiver_ref: none (gascity has no waiver path).
test_exec_mode: remote-exec; fallback_reason: none; criterion3_valid: true; farm_test_seconds: 16345.758.
test_exec_bep: unit427, acceptance25, integration-packages87, integration-full15, integration-latency1 = 555 fresh remote results; each attempt1, start inside phase window, terminal verdict, zero remote/local cache credit or other strategy. Per-phase BEP and predicate JSON retained.
rbe_state: generated rc validated; kill switch absent; certificate valid through 2027-01-09; worker environment hashes match pinned main. criterion3/evidence/rbe-state.json.
admission: remote cap2, floor25GiB, free 53.115GiB, wait 0.002s.
delta_gate: shadow/full, reason not-gate-pr; no narrowing.
heavy_mode: none; coverage_check: n/a (ordinary complete chain).
owned_topup: none needed; floor_topup: 16/16 steps rc0, including beadstest, Darwin compile, runner self-tests and every environment-absence SKIP package.
latency_leg: both current manifest names PASS under a clear probe on attempt1; waited40s, 8 readings, max_load1=43.65, threshold50, over_threshold0. latency-leg.json retained.
GATE_ICU74 verdict=OK; exact soname/hash lines retained in icu74.txt.
load_evidence: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=27.84 run_max_load=34.63 run_mean_load=17.57 run_readings=53 wait_first_load=27.84 wait_max_load=27.84 wait_mean_load=27.84 wait_readings=1 read_errors=0

skip_inventory: 604 farm and 72 native rows including subtests; separate population from the top-level tally.
skip_justification: all 320 sites UNCHANGED; all 146 source files blob-identical to the pinned base. Shared hunk is restrictedPathWithoutNpm in scripts only; unit/integration import closures (227 roots each) show only scripts imports it, and no scripts test skipped. No changed test, helper in reach, production input or dependency graph can affect these skip reasons, so no base comparison required. Native floor re-exercised environment-absence packages; remaining skips are explicit platform/capability, opt-in fixture and subprocess-helper cases. skip-audit-summary.json, all-skip-site-proofs.json, shared-hunk-reach.json and raw inventories retain each reason.

policy_lane: make lint (full nogo, remote), bazel test //:go_format_test, git diff --check, make check-release-dist-ignore check-routed-test-rows check-split-topology-rows check-residency-boundary; all rc0. fast.stdout.txt and fast.stderr.txt; fresh view and gate-base pin before suite.
drift_lane: make bazel-sync then git diff --exit-code; rc0, separate fresh view before suite.
ci_lane_run: n/a (no CI configuration changed).
ci_required_closure: ci_required_triggers.py <merged-tree> 0ea6a79188d7315e9fd7183a7934b0ad7be5ba61 ff5b1e741f13268f511784451d97643d9865510f fork; workflow blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs, suite_mode filtered, packs/credential/shared filters no match. ci-required/closure.stdout.
ci_required_accounting:
changes | LOCAL-PASS | /var/tmp/gc-ga-4p0aeukw-gate.a2l8ip4_/ci-required/closure.stdout
runner-policy | LOCAL-PASS | /var/tmp/gc-ga-4p0aeukw-gate.a2l8ip4_/ci-required/runner-policy.stdout
credential-provider-windows | NOT-TRIGGERED |   credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'
pack-gate | NOT-TRIGGERED |   pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'
windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | /var/tmp/gc-ga-4p0aeukw-gate.a2l8ip4_/ci-required/native-windows-reach.json
windows-lane | LOCAL-PASS | /var/tmp/gc-ga-4p0aeukw-gate.a2l8ip4_/ci-required/windows-lane-output.stdout
windows_reach: windows-lane-test own allowlist, GOOS=windows GOARCH=amd64 go list -mod=readonly -test -deps under unit/integration tags: no changed reachable file. ci-required/native-windows-reach.json. credential-provider-windows is NOT-TRIGGERED.
deferred_to_pr_ci: windows-lane-test (non-linux-runner)
not_covered: none
ci_commands: runner-policy ran with EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= and recorded GITHUB_OUTPUT, rc0. changes is the closure-tool run, rc0. windows-lane ran exact YAML run steps sequentially in a fresh view behind load/isolation wrappers: Windows allowlist build/test-compile, nogo analyzer build/test-compile, full macOS build/test-compile; rc0. ci-required/windows-lane.sh and windows-lane-output.stdout/.stderr retain commands, FRESH_TREE_RECORD and LOAD_GATE_SUMMARY.
evidence_disclosures: wrapper reported PASS-THROUGH, not environment isolation, for temporary gascity paths; farm actions use Bazel's pinned remote environment and native jobs use the canonical floor. Initial C6 postprocessor expected the wrong BEP key; actual successful build validated from retained finished object without rerun. Four selected forced-race tests were initially listed as owned; original selection retained and actual two tool-owned names verified separately. No failed test was retried or attributed.
