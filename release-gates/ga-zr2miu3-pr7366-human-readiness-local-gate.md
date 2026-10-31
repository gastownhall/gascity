# PR #7366: exact-human routed readiness — local revision gate

**Verdict:** **PASS**

Evaluated 2026-10-10T20:56:28.889956+00:00. This PASS authorizes mayor's publication of this LOCAL revision to the SAME existing PR, under ga-1zgega's required-job accounting ruling. It does not authorize merge or clear an unpublished SHA. The Windows job remains deferred to PR CI. All GitHub publication, contributor interaction, and merge authority stay with mayor/MPR.

## Exact inputs and history

- Gate bead: ga-zr2miu3; build ga-5gwr2e; independent review ga-qtsbwo: PASS, zero actionable findings.
- Reviewed commit: `aeea1ac6d8953c168ad4ecd3c94b609171bb821a`; reviewed base `7ad7d59c02c3def5038e34bedb57b484090f2a70`; rework `6cb68431cbddd6626b130a61979ef148d0479b56`.
- Tested local source head: `cfd23ff9baf4827b47ee30b10c651af88eaa110a`; source tree `bc7dc341433ffb7e2a09264912d8658b0657c7ed`.
- Pinned gate base: `09bfe82776252fb80f9421f17f73180101227ab3` (fresh origin/main at gate start). `gate-base.sh run` preserved it throughout every lane.
- Isolated local branch: `deploy/ga-zr2miu3-gate`. This gate record is the sole subsequent evidence-only commit; final publication head/tree are recorded in retained `final-coordinates.json` and the bead.
- Existing PR: https://github.com/gastownhall/gascity/pull/7366; published owner/branch `quad341:deploy/ga-t34icc-gate`; full publication lease `3cdd8406ecbee337496a3cbc2b3ea1864c9aa19d` (read-only reverified OPEN).
- Latest observed main: `d20818a52a7a76c26ec328843017f0e4390c5141` at 2026-10-10T20:50:20.751641+00:00; read-only merge-tree probe exit 0, prospective tree `2c10a06cddc093a9eaf5d78dddf2443afcf66064`. That prospective newer-base tree was not substituted for the tested input.
- Durable raw evidence: `/var/tmp/ga-zr2miu3-evidence.iUgUfY`; `test_log_dir: /var/tmp/ga-zr2miu3-evidence.iUgUfY/test-logs`. Per-action raw logs, XML, browser artifacts, BEP, SHA256 inventories, scripts, exit files and setup failures are retained. No CI action-key parity claimed.

All cross-agent commit values were resolved against the local object store. Merge-only catch-up preserves both reviewed and original published ancestry. The sole conflict was `internal/beadmeta/hold_labels.go`: the result equals current-main bytes plus the reviewed `HumanLabel` comment/constant. No narrow rework path conflicted. Two rework files remain byte-identical; three incorporated unrelated main changes while their reviewed function/section bodies remain identical (`reviewed-rework-body-preservation.json`). Full final-tree tests below cover the automatic merges.

## Seven criteria

| # | Criterion | Result and evidence |
|---|---|---|
| 1 | Review PASS present | PASS — independent reviewer ga-qtsbwo certifies the recorded reviewed SHA; authorized mechanical main catch-up is proven in `conflict-resolution-proof.json`, `rework-path-preservation.json`, and `reviewed-rework-body-preservation.json`. No rebase or review-carryover substitution. |
| 2 | Acceptance criteria met | PASS — all 27 required names and 34 existing guard names PASS in this full suite. Exact `human` exclusion agrees with ordinary worker-pool demand; `Human`/`HUMAN` remain served and counted; case-insensitive holds and assigned-human recovery remain intact. Canonical/native/legacy/federated goldens, priority assertion, #7404 freshness and #6059 held continuation pass. Source-built `gc ready --help` exits 0 and matches generated docs. |
| 3 | Tests and required jobs accounted | PASS — four complete Linux Bazel lanes, policy/format/drift fast lanes, and the pack job's own commands pass. Every required CI leaf has exactly one status below. Windows has the expressly allowed non-Linux deferral, with empty changed import-closure intersection. No test failure attributed and no waiver used. |
| 4 | No high-severity review findings open | PASS — independent local review reports zero actionable findings; no HIGH finding outstanding in the reviewed work. |
| 5 | Final branch clean | PASS — source branch and immutable merge scratch were clean; commit hook was active and passed normally. The gate-only commit must leave a clean tree and preserve the source projection, verified in `final-coordinates.json`. |
| 6 | Clean divergence from main | PASS — pinned main is an ancestor of source head, canonical merge validation has the identical tested tree, and the read-only newer-main probe exits 0. No force, rebase, reset or publication. |
| 7 | Single feature theme | PASS — 48 original changed paths cover one routed-readiness/shared ordinary-demand label behavior, its query goldens/help/tests, and its earlier gate record. Scope guard exit 0 with explicitly verified original build/review lineage ga-wqeyvk, ga-t34icc, ga-5gwr2e and this gate. No `.claude`, CI-config, dependency, role/provider or unrelated feature delta. |

## Criterion 3: execution and counts

`test_cmd_scope: full-suite`; `waiver_ref: none`; `failure_attribution: none`; `heavy_mode: none` (computed by `heavy-composite-gate.sh classify`, not hand-selected).

Current `.github/workflows/bazel.yml`, TESTING.md and Makefile define the full PR lanes. The old `docs/PROJECT_MANIFEST.md` reference does not exist at this tree. All tests used the immutable canonical merge tree `bc7dc341433ffb7e2a09264912d8658b0657c7ed`. Every suite invocation was detached via `gate-detached-run.sh`, load-gated at threshold 15 with timeout DEFER, and passed through `gate-base.sh run` and `isolated-test-run.sh`. Gas City's wrapper is PASS-THROUGH; actual isolation comes from owned random worktrees, sanitized detached environment/Makefile and Bazel sandboxes. Native test bd is pinned v1.3.1, full ref checked. Private ICU74 overlays only the command namespace; host libraries, caches and role configuration were unchanged.

Commands (each exit 0), with common Bazel flags `--config=fork-cache --config=ci --local_resources=cpu=4 --local_test_jobs=2 --disk_cache=/var/tmp/ga-zr2miu3-evidence.iUgUfY/task-cas --config=fresh --test_env=GO_TEST_WRAP_TESTV=1` plus one per-lane BEP path:

1. `make check` (`BAZEL_FLAGS` above): shell guards plus whole-repository `bazel test //... --keep_going`, including nogo, format, generated artifacts, docs and dashboard.
2. `make test-acceptance` (`BAZEL_FLAGS` above): `--config=acceptance //test/acceptance:acceptance_test`.
3. `bazel test --config=integration //test:integration_packages ... --keep_going`, including the actual `GC_FAST_UNIT=0` process lane.
4. `bazel test --config=integration-smoke //test/integration:integration_test ... --keep_going`. This committed CI configuration's filter is unchanged; no locally curated test filter or package subset replaced a required lane.

Counts are terminal result events per job, not a unique test census. The canonical evidence reader counts column-zero events. Go test2json also emits subtests at column zero; those counts therefore include subtests. Actual root/subtest decomposition uses `/` in test names. Non-Go policy targets are accounted by actual Bazel target/action status, not fabricated Go PASS lines.

| Lane | Passing targets / fresh test actions | Root PASS/FAIL/SKIP | Subtest PASS/FAIL/SKIP | Column-zero PASS/FAIL/SKIP |
|---|---:|---:|---:|---:|
| unit | 238 / 322 | 30119/0/167 | 25393/0/51 | 55512/0/218 |
| acceptance | 1 / 18 | 110/0/10 | 273/0/0 | 383/0/10 |
| integration-packages | 28 / 75 | 19330/0/43 | 17656/0/10 | 36986/0/53 |
| integration-smoke | 1 / 14 | 13/0/1 | 3/0/0 | 16/0/1 |

Actual roots total: 49572 PASS / 0 FAIL / 221 SKIP. Subtests: 43325 PASS / 0 FAIL / 61 SKIP. All 429 Bazel test actions PASS with no locally cached test result. Build action cache hits are separate from fresh test execution. `TestTutorial01` and all 33 scenario subtests PASS, including `08-agent-pools` (`tutorial-process-evidence.log`).

`diff_tests_executed: 24 mechanically owned roots + 3 edited agreement-corpus consumers = 27 PASS, 0 FAIL, 0 SKIP`. Full inventory: `diff-owned.tsv`, `diff-owned-explain.txt`, `required-test-names.tsv`; by-name evidence:

```text
  TestDemandCountsExactlyTheClaimableRows PASS (integration-packages-446e226d8933d0ae.log, unit-f93a0ca96ccc61d0.log)
  TestFilterReadyByAssigneeDoesNotExcludeHumanLabel PASS (integration-packages-56b343b7f13925db.log, unit-1cc8c8ab55e59a9e.log)
  TestFilterReadyByRouteExcludesHumanLabel PASS (integration-packages-f8708a951b283d00.log, unit-752a0bf25b15b465.log)
  TestGoPredicateAndGeneratedQueryAgreeRowByRow PASS (integration-packages-23eba9471eef8eb1.log, unit-f679cd2551fcc645.log)
  TestTierThreeServeRulesMatchTheGeneratedQuery PASS (integration-packages-b6892e79da2e2cbb.log, unit-cd6930bb0d0da864.log)
  TestWorkflowServeControlReadyQueryBD105IncludesEphemeral PASS (integration-packages-774cd9eaba9dbeaf.log, unit-423c6a0da1986b90.log)
  TestWorkflowServeControlReadyQueryExcludesHumanLabeledBeads PASS (integration-packages-9fc9e57bbf991872.log, unit-f93a0ca96ccc61d0.log)
  TestWorkflowServeControlReadyQueryIgnoresInProgressAssigned PASS (integration-packages-a2cfe0bb23eaf377.log, unit-fd986cf5bab8709b.log)
  TestWorkflowServeControlReadyQueryIncludesCanonicalRoutedControlWork PASS (integration-packages-f8708a951b283d00.log, unit-1cc8c8ab55e59a9e.log)
  TestWorkflowServeControlReadyQueryIncludesMetadataRoutedWorkAfterAssignedPending PASS (integration-packages-1fc06f346db9ea2f.log, unit-752a0bf25b15b465.log)
  TestWorkflowServeControlReadyQueryPreservesQueryPriorityWhenMerging PASS (integration-packages-446e226d8933d0ae.log, unit-912ab643330b35bd.log)
  TestWorkflowServeControlReadyQueryQuotesMetadataFallbackTarget PASS (integration-packages-78792c429704b040.log, unit-2bde55fab377f50b.log)
  TestWorkflowServeControlReadyQuerySkipsInstantiatingBeads PASS (integration-packages-56b343b7f13925db.log, unit-abd97d2cb9b45022.log)
  TestWorkflowServeControlReadyQueryUsesControlTiers PASS (integration-packages-8778cbda8bf74e5e.log, unit-2ef13e1b6898c554.log)
  TestWorkflowServeControlReadyQueryUsesLegacyRouteForNamedSessions PASS (integration-packages-16020a3dcad02447.log, unit-4d20b3d28497f844.log)
  TestAssigneeScopedProbesStayHumanTransparent PASS (unit-4bbf1267e0c4ccc5.log)
  TestEffectiveWorkQueryBD105CompatibilityOptIn PASS (unit-4bbf1267e0c4ccc5.log)
  TestEffectiveWorkQueryDefault PASS (unit-4bbf1267e0c4ccc5.log)
  TestEffectiveWorkQueryExcludesEpics PASS (unit-4bbf1267e0c4ccc5.log)
  TestEffectiveWorkQueryExcludesEpicsControlDispatcher PASS (unit-4bbf1267e0c4ccc5.log)
  TestEffectiveWorkQueryExcludesHumanLabeledRoutedWork PASS (unit-4bbf1267e0c4ccc5.log)
  TestEffectiveWorkQueryLegacyEphemeralExcludesHumanLabeledRoutedWork PASS (unit-4bbf1267e0c4ccc5.log)
  TestEffectiveWorkQueryRoutedQueueRidesReaderPriorityOrder PASS (unit-4bbf1267e0c4ccc5.log)
  TestEffectiveWorkQueryRoutedQueueUsesNativeCanonicalSortAcrossReadyTiers PASS (unit-4bbf1267e0c4ccc5.log)
  TestEffectiveWorkQueryRoutedTierServesCanonicalPriorityOrder PASS (unit-4bbf1267e0c4ccc5.log)
  TestPoolDemandServeRulesExcludeHumanLabelWithoutMakingItADispatchHold PASS (unit-4bbf1267e0c4ccc5.log)
  TestRouteScopedPoolDemandShellsExcludeHumanLabel PASS (unit-4bbf1267e0c4ccc5.log)
```

Supplemental 34 acceptance guards PASS in `acceptance-guard-results.txt`. Two historical names had been renamed by an already-landed main commit; `supplemental-name-mapping.json` records its resolved SHA and proves the mapping, and only the current names are claimed executed. No test rerun was used to hide the initial name-inventory miss.

`skip_justification`: 282 SKIP events, 223 distinct sites, all mechanically UNCHANGED with zero SHARED-HUNKS in every skip-site file. `skip-causal-review.json` records each package/test/site/reason and why this label/query delta cannot determine the guard; `all-skip-site-proof.txt` and `all-skip-file-proof.json` retain the code proof. Same-package/same-full-name alternate PASS logs account for process tests skipped in the unit lane. Other guards concern optional tools, OS/permission fixtures, subprocess helpers, explicit characterization or opt-in scenarios. The smoke BdStore conformance guard is an unconditional unchanged legacy skip before any runtime setup, not proof that this run installed bd1.0.4. Live registry opt-in cases additionally run in their required pack job below. No SKIP is presented as PASS; no diff-owned test skipped.

Fast lanes ran before the suite in separate fresh views: `make build`, `make lint` (nogo/vet and shell policy), pinned `make fmt-check` (2.12.0), `make bazel-sync` then `git diff --exit-code` (empty drift), and `scripts/check-generated-docs-drift.sh`; all exit 0. `policy_lane: PASS`; `drift_lane: PASS`. `ci_lane_run: not applicable — no CI-config delta`; no workflow was dispatched.

## Required CI leaf accounting (ga-1zgega)

`ci_required_closure: python3 /var/tmp/ga-1zgega-gate-tools/ci_required_triggers.py /var/tmp/gc-merge-validate.mmixbf 09bfe82776252fb80f9421f17f73180101227ab3 cfd23ff9baf4827b47ee30b10c651af88eaa110a fork`; CI blob `b72b2dde9cd2d9077189ddd898d56f8a9dccbbf1`; **4 LEAF jobs**. Hash-matching ruling tools classified the merged tree; full/shared diff. Aggregator `check` takes no independent status. Workflow step list at this blob is authoritative.

| Required leaf | Exactly one status | Command/result/log |
|---|---|---|
| runner-policy | LOCAL-PASS | `EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= ... python3 .github/workflows/scripts/runner_policy.py`; exit 0; `runner-policy-run/units/gate.w1.stdout.log`, `runner-policy.output`, `runner-policy.summary`. |
| changes | LOCAL-PASS | Closure/classifier command above exit 0; `ci-required-closure.txt`: shared MATCH, suite_mode full. |
| credential-provider-windows | DEFERRED-TO-PR-CI reason=non-linux-runner | `windows_reach.sh /var/tmp/gc-merge-validate.mmixbf 09bfe82776252fb80f9421f17f73180101227ab3 cfd23ff9baf4827b47ee30b10c651af88eaa110a` exit 0; `windows-reach.txt` empty; import closures credentialprovider/pathutil/testenv/testutil, no changed-file intersection. No Windows test PASS claimed. |
| pack-gate | LOCAL-PASS | `scripts/update-bundled-gastown-pack --check` and `GC_TEST_GASCITY_PACKS_REGISTRY=main make test-pack-registry-live`, exit 0 each; `pack-pins.stdout/.stderr/.exit`, `pack-live.stdout/.stderr/.exit`. Task-private Go cache, verbose real test results; first invocation retained. |

`windows_reach: none`; `deferred_to_pr_ci: credential-provider-windows (non-linux-runner)`; `not_covered: none` within the current required closure. The advisory upstream-bd radar and mail job are outside that closure. Ga-1zgega permits this PASS for publication only; CI / required, every required Bazel check and the deferred Windows job must succeed on the actual published head before mayor/MPR considers merge.

## Load evidence

| Lane | load_threshold | load_waited_seconds | load_wait_timed_out | run_start_load | run_max_load | run_mean_load | run_readings |
|---|---:|---:|---:|---:|---:|---:|---:|
| build-vet-policy | 15 | 0 | 0 | 13.04 | 21.21 | 18.07 | 18 |
| format | 15 | 180 | 0 | 14.32 | 13.63 | 13.41 | 2 |
| bazel-drift | 15 | 0 | 0 | 13.05 | NA | NA | 0 |
| generated-docs | 15 | 0 | 0 | 13.60 | NA | NA | 0 |
| unit | 15 | 0 | 0 | 12.58 | 23.26 | 17.37 | 23 |
| acceptance | 15 | 0 | 0 | 11.39 | 13.17 | 12.21 | 13 |
| integration-packages | 15 | 0 | 0 | 10.60 | 16.82 | 13.25 | 27 |
| integration-smoke | 15 | 0 | 0 | 9.57 | 10.48 | 10.45 | 3 |
| pack-pins | 15 | 0 | 0 | 10.13 | NA | NA | 0 |
| pack-live | 15 | 0 | 0 | 10.13 | 11.40 | 11.27 | 3 |

Full `wait_*` readings and raw summaries are retained per stderr. Initial private-hook wait DEFERRED at threshold15/300s with no execution (first38.34/max38.34/mean27.01,11 readings); successful hook waited90s, started14.70. Earlier active-hook attempt's nogo passed but docs loader failed before tests because host ICU74 was absent (opened existing tracker ga-sdon3o). The private pinned-library namespace repaired execution; subsequent active normal hook/docs/dashboard preview passed. Initial fast-lane launcher quoting error was NOT_RUN. The unit suite itself passed on its first run; a nested-artifact collector failure after it was fixed offline, preserving the first traceback and all original results. All setup failures are retained, none is a test PASS or a waiver. The first gate-record commit launcher returned65 before the hook: gate-base correctly refused the real merge branch because that wrapper is for canonical merge scratches. The final evidence-only commit uses the ordinary active commit hook, in its private ICU namespace; no suite verdict was involved in that launcher refusal.

## Publication handback

Mayor owns SAME-PR publication against the full lease, preserves the published ancestor, and verifies the resulting remote head. Deployer may clear only that newly verified published SHA under a subsequent explicit handback; historical ga-t34icc clearance covers only the old lease. No branch push, new PR, comment/review, CI rerun, status publication or merge was performed here. The updated proposed PR body is local evidence for mayor, not a contributor reply.

Build ga-5gwr2e remains OPEN until mayor confirms MAIN LANDING. Deferred ga-gw831h (control wake-demand) and ga-ir0y7p (exact-human continuation preassignment) remain OPEN with their same-store blocks edges on that build. Broader #6026/ga-n03flr and assigned-human recovery ga-5k0ehr remain separate. Preserve Closes #7367 and sjarmak credit when mayor updates the PR description.
