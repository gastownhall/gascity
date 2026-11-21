# CLI JSON contract baseline release gate

**Verdict:** **PASS**

Evaluated 2026-11-08T08:05:30.555262+00:00. This gate authorizes PR creation; merge readiness is checked on the PR head by the merge authority.

- Deploy bead: ga-13szdk14; review: ga-5nmyrrqi; build: ga-ok4ppoq; public issue: #8453.
- DEPLOY_MODE: remote; BASE_REF: origin/main; PUSH_REMOTE: fork.
- reviewed_sha: `7026af692bfbe602a3e1cd1dc5dd098b414b983f`.
- gate_base_sha: `fdfec4c19dc4dcf3c80820f433f199cb0fdd3acc`; gate_base_kind: current-tip.
- merge_sha: `b7bc0211ecf9d399b8f12286231c6cd027949242`; merge_tree: `b7e752abb49fe3898ba3e999b3a9495665d648d8`.
- source_merge_base: `6c9663d4ca4c0c32196dcfb435d641fe387084d2`.
- merge_state: clean (origin/main@fdfec4c19dc4dcf3c80820f433f199cb0fdd3acc); conflicting files: none.
- merge_validation: a clean merge onto the pinned current tip was built and tested; mpr validates current-head CI and resolves any subsequent divergence at merge time. The deploy source stays at the exact reviewed SHA; no rebase or merge onto main was performed.
- Pre-flight: the reviewer-only head is local and returned HTTP 422 from upstream/fork commit-to-PR lookup; its published parent and source-branch PR queries returned no PR. No merged or closed target PR existed.
- Evidence root: `/var/tmp/gc-heavy-gate/runs/ga-13szdk14.20261108-pr1wkqqm`; the canonical scratch was `/var/tmp/gc-merge-13szdk14.0_lixt5h`.
- Repository fallback: docs/PROJECT_MANIFEST.md and work-packages/ are absent. Release criteria are the deployed release-gate-criteria protocol, TESTING.md/AGENTS.md and the amended AC1–AC8 in the build bead.

## Checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Live review bead records verdict pass for the exact reviewed head. Reviewer-added test carryover independently checked: see below. |
| 2 | Acceptance criteria met | PASS | AC1–AC8 mapped below to static checks and named results from this new full gate. PR-only clauses are prepared for publication and checked again before PR-open completion. |
| 3 | Tests pass | PASS | Fresh full-suite evidence: 149305 PASS / 0 FAIL / 512 SKIP top-level results across 550 retained logs; all 63 diff-owned roots PASS by name. Five remote phases and all 16 native-floor jobs complete. All four ci-required leaf jobs accounted. |
| 4 | No high-severity review findings open | PASS | Reviewer style/security findings are none, verdict pass; zero unresolved HIGH findings on this change. |
| 5 | Final branch clean | PASS | Source checkout and canonical merge scratch clean before writing this record. This record is the sole deploy-only commit; push-and-pr verifies the resulting branch is clean. |
| 6 | Merge state recorded; build/vet pass | PASS | Clean canonical merge has exact pinned base/source parents and merge-tree identity. Whole-repository bazel build //... with nogo/vet exits 0: 1482 target completions, zero failed targets. |
| 7 | Single feature theme | PASS | Nine commits, 28 files: one CLI schema-baseline feature, its generator, schemas, policy/instructions and tests. No independent feature, CI workflow, CODEOWNERS or complexity-baseline change. |

reviewer_tests_verified: `8d9940e66c78ae1d5d383df060c439b1b07799ed -> 7026af692bfbe602a3e1cd1dc5dd098b414b983f` files=internal/schemacontract/compare_test.go; reviewer-added-tests.sh check returned `reviewer-tests files=internal/schemacontract/compare_test.go`.

## Acceptance evidence

| AC | Result | Independent evidence |
|---|---|---|
| 1 | PASS | TestCheckBreakingMutants (removed/renamed/widened/nested/required cases), TestCheckConstChangedWithinVersionIsBreaking, TestCheckConstDroppedIsBreaking, TestCheckFramingChangeIsBreaking, TestCheckVariants, TestCheckVersionBumpRules, TestCheckUnversionedToConstBump and TestSchemaContractBaseline PASS. |
| 2 | PASS | TestCheckAdditiveFieldAsksForGenerator, TestMergeAppendsOnlyMissingRecordsAtSameVersion and TestMergeAppendsMissingVariant PASS. Additions instruct regeneration without a bump. |
| 3 | PASS | All 133 schema paths have baseline records; no missing/extra paths. TestEveryBuiltinSchemaIsExtractable, TestExtractAllWalksSchemaFilesOnly, TestCheckMissingRecordAsksForGenerator, TestCheckOrphanedRecordFails, TestBaselineIsCanonical and TestSchemaContractBaseline PASS. |
| 4 | PASS | Four changed result schemas declare version const 1; no additionalProperties change. TestFixedResultSchemasPinSchemaVersion covers the eleven commands and PASSes; TestFixedResultSchemasDeclareVersion PASSes. |
| 5 | PASS | Policy, owning/root AGENTS.md and PR template updated together. Prepared public body states the compatibility rule, Refs #8453, @julianknutsen, proposed baseline CODEOWNERS entry, honest security/semantic limitations, and excludes internal identities. CODEOWNERS untouched. Public body is verified after opening. |
| 6 | PASS | Zero-growth form (ii): strict base/merge exits 2/2 with the same 208 finding rows. complexity-diff exits 0, prints the pinned-base header and no threshold changes. Complexity baseline untouched. Public PR quotes both complete command outputs and identifies source and canonical validation merge bases. |
| 7 | PASS | TestExtractFailsLoudly rejects unknown keyword, remote ref, cycle, nested object/array union and reserved property names; all 133 current schemas extract. |
| 8 | PASS | Full mutant/generator coverage passes: nested removal, scalar-union narrowing/widening, integer/number subset and nullable unions, allOf, boolean schema, unversioned first entry/bump, backwards/newer/orphan/framing/ref-name cases, seed/append-only/new-version placeholders/idempotence/preservation/deterministic bytes. Builder's recorded red-first history is in the source commits; this gate independently ran the whole final suite. |

## Criterion 3: execution and integrity

- test_cmd_scope: full-suite.
- test_cmd: gate-criterion3.sh invoked through gate-base.sh run from the canonical merge scratch, detached as a systemd service and through isolated-test-run.sh. Remote unit phase runs make -k check; subsequent phases run the documented acceptance, integration-package, full-integration and separate latency commands.
- Remote flags in every phase: --config=remote-exec --config=ci --keep_going --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1, plus each phase's BEP path. Exact expanded commands retained in c3/evidence/execution.json and phase stdout.
- Unit: make -k check (Bazel //... plus shell guards); acceptance: bazel test --config=acceptance //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests; integration-packages: bazel test --config=integration //test:integration_packages.
- integration-full: bazel test --config=integration --test_arg=-test.v --test_arg=-test.skip=<names from gate-latency-tests.txt> //test/integration:integration_test. integration-latency: same target, only those names, unsharded with the farm probe. Both legs use the single shipped name list.
- test_counts: 149305 PASS, 0 FAIL, 512 SKIP (top-level result lines, counted per job; no subtest inflation).
- diff_tests_executed: 63/63 PASS, 0 FAIL, 0 SKIP, 0 MISSING; full list below.
- test_log_dir: `/var/tmp/gc-heavy-gate/runs/ga-13szdk14.20261108-pr1wkqqm/c3/evidence/remote/logs` (550 logs retained; no deletion).
- test_exec_mode: remote-exec derived from BEP; the required native floor is additive, not a stitched local replacement suite.
- test_exec_bep: 534/534 fresh remote first-attempt result events, each start within its phase window; zero remote/local cache credit, other strategy, retries, missing summaries or evidence errors. Independently re-read against original phase windows in REMOTE_REAUDIT_0758.json.
- rbe_state: valid rc marker, kill switch absent, certificate expiry 2027-01-09; tree/main worker environment pins match `sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98`.
- fallback_reason: none; no local restart.
- admission: remote class, floor 25 GiB, cap 2, observed free 87.390 GiB, waited 0.001 seconds.
- farm_test_seconds: 12527.664.
- heavy_mode: none (classifier: no listed heavy package owned/reached); heavy_units/assigned_total/executed_total/coverage_check: n/a.
- owned_topup: [] (all owned names already PASS in forced remote evidence).
- floor_topup: complete, 16/16 exit 0. Every environment-absence selection reran by package (parent selected for subtests), plus beadstest, Darwin compile, push-gate-lock and local-concurrency self-tests. Native floor has 119 named results including subtests; 39 native SKIP events are inventoried separately, 14 at top level, no failures.
- latency_leg: complete on attempt 1; probe clear after 20 s (load1 46.57, threshold 50); TestGcBdLatencyBudget and TestGcSeatHookLatencyBudget PASS. Neither ran in the main full-integration leg.
- waiver_ref: none. failure_attribution: none.

| Phase | Result events | Remote executions | Cached results | Retried results | Verdict |
|---|---:|---:|---:|---:|---|
| unit | 408 | 408 | 0 | 0 | PASS |
| acceptance | 25 | 25 | 0 | 0 | PASS |
| integration-packages | 86 | 86 | 0 | 0 | PASS |
| integration-full | 14 | 14 | 0 | 0 | PASS |
| integration-latency | 1 | 1 | 0 | 0 | PASS |

load_threshold: 15; load_waited_seconds: 1261; load_wait_timed_out: 0; run_start_load: 14.73; run_max_load: 19.12; run_mean_load: 18.34; run_readings: 9.

Native floor load evidence: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=1261 wait_timed_out=0 run_start_load=14.73 run_max_load=19.12 run_mean_load=18.34 run_readings=9 wait_first_load=49.73 wait_max_load=49.73 wait_mean_load=30.32 wait_readings=43 read_errors=0`. Remote admission bypasses the local-heavy wait by design; its initial runtime setup reading was 44.61, with zero wait. That is not the native floor's start load.

Environment: ICU74 provenance check OK; pinned test bd v1.3.4 / ref 7ef2fd05e verified, Podman socket and pinned image preflight passed. isolated-test-run.sh explicitly reports PASS-THROUGH / NOT ISOLATED for this repository: no cloned/scrubbed environment is claimed. Remote setup uses the official wrapper; native floor scrubs GC_/BD_/BEADS_/DOLT_ selectors and retains pinned test bd keys through the official runner.

skip_inventory: 498 remote top-level SKIPs, 290 unchanged source sites in 132 identical frozen-base/merge source blobs; 39 skip-bearing binaries have 117 positive import-closure checks over default/integration/acceptance_a. No changed shared test package is reachable. Native floor's 39 skip events (including subtests) cover 36 unchanged sites already in that source/reach audit; file blobs and --site UNCHANGED were rechecked. All reasons are retained, none empty. No diff-owned SKIP and no reachable shared/helper change requires a BASE-REF rerun. Native reasons: absent optional live-herdr/replay/simulation inputs, Postgres DSN, br binary, tmux parent session, and expected classified provider-start/fencing skips in the conformance test. These checks do not exercise the new generator or comparator. Audit evidence: REMOTE_SKIP_AUDIT_VERIFIED.json, NATIVE_SKIP_AUDIT_VERIFIED.json, native-skip-inventory.json and native-skip-sites-check.json under the evidence root.

policy_lane: PASS, before runtime setup/suite. run-pinned-lint.sh -- make fmt-check-changed; make lint with remote nogo; release-dist-ignore, routed-test-rows, split-topology-rows, residency-boundary and hooks guards; 14 policy/format/docsync/security/CI-policy targets PASS. Whole make -k check also passes in the canonical fresh unit phase.

drift_lane: PASS, each lane in its own fresh-tree-run.sh view through gate-base.sh run. make bazel-sync, git diff --exit-code and empty git status --porcelain --untracked-files=all; seven generated schema/spec/client/dashboard/lockfile drift targets PASS. OpenAPI compatibility check against the pinned base PASS (one target). Canonical scratch stayed clean and tree-identical. policy_attribution: none.

ci_lane_run: n/a (no CI-config change in this diff).

## Criterion 3e: required-job accounting

ci_required_closure: `python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-13szdk14.0_lixt5h fdfec4c19dc4dcf3c80820f433f199cb0fdd3acc 7026af692bfbe602a3e1cd1dc5dd098b414b983f fork`; workflow blob `e5b11c348505dab28dc223e874bc2c61cbbb33c5`; 4 LEAF jobs need a status. Merged workflow has 5 jobs in its transitive closure; check is an AGGREGATOR and takes no status. Runner expressions were followed to runner_policy.py: active jobs resolve to Linux Blacksmith labels; the Windows job is not triggered.

ci_required_accounting:

| Job | Status | Evidence |
|---|---|---|
| runner-policy | LOCAL-PASS | EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=<evidence>/runner-policy.out python3 .github/workflows/scripts/runner_policy.py; exit 0; log /var/tmp/gc-heavy-gate/runs/ga-13szdk14.20261108-pr1wkqqm/ci-required/runner-policy.log |
| changes | LOCAL-PASS | python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-13szdk14.0_lixt5h fdfec4c19dc4dcf3c80820f433f199cb0fdd3acc 7026af692bfbe602a3e1cd1dc5dd098b414b983f fork; exit 0; log /var/tmp/gc-heavy-gate/runs/ga-13szdk14.20261108-pr1wkqqm/ci-required/closure.txt |
| credential-provider-windows | NOT-TRIGGERED |   credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true' |
| pack-gate | NOT-TRIGGERED |   pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true' |

windows_reach: n/a (Windows job NOT-TRIGGERED). deferred_to_pr_ci: none. not_covered: none. This PASS authorizes opening the PR only. Merge-ready requires CI / required green on its current head and all triggered required Bazel lanes green; mpr owns that check.

## Diff-owned results

```text
  TestFixedResultSchemasPinSchemaVersion PASS (integration-packages-12697e2ea0d32c278115.log, unit-10cc7795c0ec199a5530.log)
  TestRunBackwardsVersionLeavesBaselineUnchanged PASS (unit-590fb8037d4d95edde2f.log)
  TestRunExtractorErrorWritesNothing PASS (unit-590fb8037d4d95edde2f.log)
  TestRunRequiresARoot PASS (unit-590fb8037d4d95edde2f.log)
  TestRunSecondRunIsByteIdentical PASS (unit-590fb8037d4d95edde2f.log)
  TestRunSeedsBaselineFromRootFlag PASS (unit-590fb8037d4d95edde2f.log)
  TestRunUsesBuildWorkspaceDirectory PASS (unit-590fb8037d4d95edde2f.log)
  TestBaselineIsCanonical PASS (unit-44b212787c3cc74968a2.log, unit-5ef9eae7728e05846b8e.log)
  TestCheckAdditiveFieldAsksForGenerator PASS (unit-44b212787c3cc74968a2.log)
  TestCheckAllOfMergeAndBooleanSchema PASS (unit-44b212787c3cc74968a2.log)
  TestCheckBreakingMutants PASS (unit-44b212787c3cc74968a2.log)
  TestCheckBreakingPersistsAgainstNewestEntryOnly PASS (unit-44b212787c3cc74968a2.log)
  TestCheckConstChangedWithinVersionIsBreaking PASS (unit-44b212787c3cc74968a2.log)
  TestCheckConstDroppedIsBreaking PASS (unit-44b212787c3cc74968a2.log)
  TestCheckFramingChangeIsBreaking PASS (unit-44b212787c3cc74968a2.log)
  TestCheckHelperDefRenameIsGreen PASS (unit-44b212787c3cc74968a2.log)
  TestCheckIdenticalIsClean PASS (unit-44b212787c3cc74968a2.log)
  TestCheckIfThenElseAreIgnored PASS (unit-44b212787c3cc74968a2.log)
  TestCheckIntegerIsSubsetOfNumber PASS (unit-44b212787c3cc74968a2.log)
  TestCheckIntegerSubsetOfNumberWithNullUnions PASS (unit-44b212787c3cc74968a2.log)
  TestCheckMissingRecordAsksForGenerator PASS (unit-44b212787c3cc74968a2.log)
  TestCheckNarrowingIsGreen PASS (unit-44b212787c3cc74968a2.log)
  TestCheckOrphanedRecordFails PASS (unit-44b212787c3cc74968a2.log)
  TestCheckProblemsAreSorted PASS (unit-44b212787c3cc74968a2.log)
  TestCheckScalarUnionWideningIsBreaking PASS (unit-44b212787c3cc74968a2.log)
  TestCheckUnversionedToConstBump PASS (unit-44b212787c3cc74968a2.log)
  TestCheckVariants PASS (unit-44b212787c3cc74968a2.log)
  TestCheckVersionBumpRules PASS (unit-44b212787c3cc74968a2.log)
  TestEveryBuiltinSchemaIsExtractable PASS (unit-44b212787c3cc74968a2.log)
  TestExtractAllNamesTheBadSchema PASS (unit-44b212787c3cc74968a2.log)
  TestExtractAllOfMergesPropertiesAndRequired PASS (unit-44b212787c3cc74968a2.log)
  TestExtractAllOfRequiredWithoutPropertyIsAny PASS (unit-44b212787c3cc74968a2.log)
  TestExtractAllWalksSchemaFilesOnly PASS (unit-44b212787c3cc74968a2.log)
  TestExtractArrayRoot PASS (unit-44b212787c3cc74968a2.log)
  TestExtractBooleanSchemaProperties PASS (unit-44b212787c3cc74968a2.log)
  TestExtractFailsLoudly PASS (unit-44b212787c3cc74968a2.log)
  TestExtractFramingFlags PASS (unit-44b212787c3cc74968a2.log)
  TestExtractFullDepthPaths PASS (unit-44b212787c3cc74968a2.log)
  TestExtractIgnoresDocumentedKeywords PASS (unit-44b212787c3cc74968a2.log)
  TestExtractInlineOneOfBranchesAreKeyedByIndex PASS (unit-44b212787c3cc74968a2.log)
  TestExtractInlinesLocalRefsWithoutRecordingDefNames PASS (unit-44b212787c3cc74968a2.log)
  TestExtractRootOneOfBecomesKeyedVariants PASS (unit-44b212787c3cc74968a2.log)
  TestExtractScalarUnionBecomesTypeUnion PASS (unit-44b212787c3cc74968a2.log)
  TestExtractUntypedRootYieldsAny PASS (unit-44b212787c3cc74968a2.log)
  TestExtractUnversionedSchemaHasEmptyVersion PASS (unit-44b212787c3cc74968a2.log)
  TestExtractVersionAndFlatObject PASS (unit-44b212787c3cc74968a2.log)
  TestFieldJSONShape PASS (unit-44b212787c3cc74968a2.log)
  TestFixedResultSchemasDeclareVersion PASS (unit-44b212787c3cc74968a2.log)
  TestMarshalEmptyBaseline PASS (unit-44b212787c3cc74968a2.log)
  TestMarshalIsDeterministicAndByteExact PASS (unit-44b212787c3cc74968a2.log)
  TestMergeAppendsMissingVariant PASS (unit-44b212787c3cc74968a2.log)
  TestMergeAppendsOnlyMissingRecordsAtSameVersion PASS (unit-44b212787c3cc74968a2.log)
  TestMergeDoesNotMutateItsInput PASS (unit-44b212787c3cc74968a2.log)
  TestMergeIsIdempotent PASS (unit-44b212787c3cc74968a2.log)
  TestMergeKeepsRecordsOfSchemasThatAreGone PASS (unit-44b212787c3cc74968a2.log)
  TestMergeNeverHidesABreakingChange PASS (unit-44b212787c3cc74968a2.log)
  TestMergeNewVersionAddsEntryWithEmptyPlaceholders PASS (unit-44b212787c3cc74968a2.log)
  TestMergeRefusesOlderVersion PASS (unit-44b212787c3cc74968a2.log)
  TestMergeSeedsSchemaWithNoRecord PASS (unit-44b212787c3cc74968a2.log)
  TestMergeUnversionedToConstBumpAddsEntry PASS (unit-44b212787c3cc74968a2.log)
  TestParseBaselineRejectsMalformedInput PASS (unit-44b212787c3cc74968a2.log)
  TestParseBaselineRoundTrip PASS (unit-44b212787c3cc74968a2.log)
  TestSchemaContractBaseline PASS (unit-44b212787c3cc74968a2.log)
```

## Complexity comparison

complexity_base: `fdfec4c19dc4dcf3c80820f433f199cb0fdd3acc`; command `make complexity-check 2>&1`: base exit 2, merge exit 2; 208 byte-identical new/regressed finding rows. Zero-growth form (ii) PASS. Complete outputs are retained as complexity-base.log and complexity-merge.log and quoted in the public PR. Pinned analyzer: gocyclo v0.6.0.

```text
COMPLEXITY_BASE_REF=fdfec4c19dc4dcf3c80820f433f199cb0fdd3acc make complexity-diff
complexity: diff against fdfec4c19dc4dcf3c80820f433f199cb0fdd3acc
complexity: no threshold changes
```
