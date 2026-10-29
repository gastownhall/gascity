# Human routed readiness release gate

**Verdict:** **PASS**

Bead: `ga-t34icc`. Recorded 2026-10-08T16:45:46.156961+00:00.

Unassigned routed beads labeled `human` are withheld from worker and control readiness and pool demand. Assigned recovery remains human-transparent. The descriptor, native bd flags, legacy jq path and Go filter carry the same rule.

## Source and gate tree

- Reviewed source: `310016ecb46e180f5e5ef102e99e9b5cdf68dcc0`; review `ga-e3goy8` PASS; build `ga-wqeyvk`.
- Pinned main: `e39d98ccfa23364258af6e26abe1f673d6c06bfd`.
- Normal-hook canonical merge: `90377d63ed91cac30687c77dd3e9111e242e5a0e`; tree `7060c3f494719f8c21863e458aed02913c39a397`, exactly equal to the predicted conflict-free merge.
- Tests ran in `/var/tmp/gc-merge-ga-t34icc-rekey-fresh.iA2Vq6`; build/vet and fast mutations ran in independent fresh views.
- Isolated publication branch: `deploy/ga-t34icc-gate`, cut exactly at reviewed source. This gate record is its only addition.
- Main advanced during the pinned gate to `864dac9ca69a79c6c8336ad825e020bd5361c26a`; current text merge remains clean with tree `bccdd30dad90eb6c9e40ce6a14e7daa87d2794df`. Runtime evidence certifies the pinned base above, never the newer tree. CI / required on the PR current merge remains mandatory.
- Already-merged preflight: source is not an ancestor of pinned main; all three source patches are absent by git cherry; associated target query found no current deploy. No self-rebase. Old PR #4782 was neither read nor acted on; mayor disposition ga-skrmh5 authorizes a new isolated PR and reserves old-thread handling to mayor.

## Criteria

| # | Result | Evidence |
|---|---|---|
| 1 | PASS | Resolved reviewed source matches closed review ga-e3goy8 PASS and build ga-wqeyvk. TDD red/green and comment-only correction remain pinned; no branch-tip substitution. |
| 2 | PASS | All 24 owned roots execute by name: clean/human controls for routed and legacy-ephemeral worker queries, control readiness, Go filters, demand counts and assigned recovery. Five shared consumers and all 36 snapshots PASS; snapshot byte proof changes only 84 human flags and 18 jq clauses. |
| 3 | PASS | Complete uncached first-attempt unit, acceptance including solo fixtures, integration packages and repository-defined integration smoke. Required CI leaves are accounted below; Windows runtime is explicitly deferred to PR CI. Owned tests have no SKIP or FAIL. |
| 4 | PASS | No HIGH or blocking finding. Optional hold-label prose and EqualFold-comment clarifications remain non-blocking. Hand-typed Human variant may undercount demand; lowercase human is the supported bd label. Assigned-human semantics are preserved, not broadened. |
| 5 | PASS | Canonical and publication trees clean before this record. Whole-repository generated/BUILD drift and untracked-inclusive checks PASS. Final committed checkout is checked before push. |
| 6 | PASS | Normal-hook merge matches predicted tree. Independent fresh-view go build ./... and go vet ./... exit0. |
| 7 | PASS | One feature through existing readiness descriptor: three production Go files, five test files and 36 goldens. All source commits cite ga-wqeyvk. No internal agent config paths or unrelated theme. |

## Full-suite evidence

`test_cmd_scope: full-suite`; `heavy_mode: none`; `waiver_ref: none`; `failure_attribution: none`; `policy_attribution: none`; `ci_lane_run: n/a (no CI-config diff)`.

Common Bazel flags: `--config=ci --config=fork-cache --nocache_test_results --jobs=4 --remote_download_outputs=all --rewind_lost_inputs`, with recorded runtime test env and per-phase BEP.

```text
make check BAZEL="bazel --batch" BAZEL_FLAGS="<common flags + runtime env + BEP>"
bazel --batch test <flags> --config=acceptance --keep_going //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests
bazel --batch test <flags> --config=integration --keep_going //test:integration_packages
bazel --batch test <flags> --config=integration-smoke --keep_going //test/integration:integration_test
```

| Lane | Targets | PASS | FAIL | SKIP |
|---|---|---|---|---|
| unit | 232 | 53223 | 0 | 214 |
| acceptance | 7 | 410 | 0 | 13 |
| integration-packages | 26 | 35002 | 0 | 53 |
| integration-smoke | 1 | 16 | 0 | 1 |

Counts include subtests and repeated CI coverage. Totals: `{"all": {"FAIL": 0, "PASS": 88651, "SKIP": 281}, "top_level": {"FAIL": 0, "PASS": 47770, "SKIP": 218}}`; 383 Bazel test executions. Every configured test target has a retained first-attempt uncached result, log/XML, BEP and verified hashes.

Diff-owned named results:

- `cmd/gc/TestFilterReadyByAssigneeDoesNotExcludeHumanLabel`: PASS in unit and integration.
- `cmd/gc/TestFilterReadyByRouteExcludesHumanLabel`: PASS in unit and integration.
- `cmd/gc/TestWorkflowServeControlReadyQueryBD105IncludesEphemeral`: PASS in unit and integration.
- `cmd/gc/TestWorkflowServeControlReadyQueryExcludesHumanLabeledBeads`: PASS in unit and integration.
- `cmd/gc/TestWorkflowServeControlReadyQueryIgnoresInProgressAssigned`: PASS in unit and integration.
- `cmd/gc/TestWorkflowServeControlReadyQueryIncludesCanonicalRoutedControlWork`: PASS in unit and integration.
- `cmd/gc/TestWorkflowServeControlReadyQueryIncludesMetadataRoutedWorkAfterAssignedPending`: PASS in unit and integration.
- `cmd/gc/TestWorkflowServeControlReadyQueryPreservesQueryPriorityWhenMerging`: PASS in unit and integration.
- `cmd/gc/TestWorkflowServeControlReadyQueryQuotesMetadataFallbackTarget`: PASS in unit and integration.
- `cmd/gc/TestWorkflowServeControlReadyQuerySkipsInstantiatingBeads`: PASS in unit and integration.
- `cmd/gc/TestWorkflowServeControlReadyQueryUsesControlTiers`: PASS in unit and integration.
- `cmd/gc/TestWorkflowServeControlReadyQueryUsesLegacyRouteForNamedSessions`: PASS in unit and integration.
- `internal/config/TestAssigneeScopedProbesStayHumanTransparent`: PASS in unit (config is not an integration_packages member).
- `internal/config/TestEffectiveWorkQueryBD105CompatibilityOptIn`: PASS in unit (config is not an integration_packages member).
- `internal/config/TestEffectiveWorkQueryDefault`: PASS in unit (config is not an integration_packages member).
- `internal/config/TestEffectiveWorkQueryExcludesEpics`: PASS in unit (config is not an integration_packages member).
- `internal/config/TestEffectiveWorkQueryExcludesEpicsControlDispatcher`: PASS in unit (config is not an integration_packages member).
- `internal/config/TestEffectiveWorkQueryExcludesHumanLabeledRoutedWork`: PASS in unit (config is not an integration_packages member).
- `internal/config/TestEffectiveWorkQueryLegacyEphemeralExcludesHumanLabeledRoutedWork`: PASS in unit (config is not an integration_packages member).
- `internal/config/TestEffectiveWorkQueryRoutedQueueRidesReaderPriorityOrder`: PASS in unit (config is not an integration_packages member).
- `internal/config/TestEffectiveWorkQueryRoutedQueueUsesNativeCanonicalSortAcrossReadyTiers`: PASS in unit (config is not an integration_packages member).
- `internal/config/TestEffectiveWorkQueryRoutedTierServesCanonicalPriorityOrder`: PASS in unit (config is not an integration_packages member).
- `internal/config/TestPoolDemandServeRulesExcludeHumanLabelWithoutMakingItADispatchHold`: PASS in unit (config is not an integration_packages member).
- `internal/config/TestRouteScopedPoolDemandShellsExcludeHumanLabel`: PASS in unit (config is not an integration_packages member).

All surviving roots in five changed test files are accounted in changed-test-consumers-pass.json; unchanged skipped neighbors retain individual skip proof, not a false PASS. Shared consumers TestWorkQueryGolden, TestDemandCountsExactlyTheClaimableRows, TestSlotSuffixCollapseIsPersistedForClaimableFormsOnly, TestGoPredicateAndGeneratedQueryAgreeRowByRow and TestTierThreeServeRulesMatchTheGeneratedQuery PASS. Four cmd/gc shared consumers also PASS in integration. Tutorial01 including 08-agent-pools PASSes with GC_FAST_UNIT=0. TestSchemaFreshness and actual OpenAPI base/comparator fixtures PASS.

## Skip integrity

Each of 281 SKIP rows has its printed reason, actual source guard, per-file --explain, --site UNCHANGED result, shared-hunk accounting and causal reach review in `/var/tmp/gc-heavy-gate/runs/ga-t34icc-rekey-fresh.c3/full-skip-site-proof.json` (SHA-256 `b7abdde7e2958888d2ed3ed8689c23728a78de8090590e6205527b4985c47e1f`). All individual sites/reasons were read. No skipped site is in an owned body.
The production diff adds a pure human-label constant to routed unassigned rules and renders it through existing bd flags, jq filters and the Go twin. Assigned selectors and dispatch-hold constants remain intact. Test-binary import reach is acknowledged rather than used to claim absence. No environment, fixture, provider lifecycle, binary lookup, include parsing, opt-in, helper entry, platform or privilege input read by the skip guards changes. Changed-file skipped neighbors are independently unowned, with no shared hunk. The shared agreementRows addition is one deterministic scalar row; the other shared hunk adds stdlib slices. No TestMain/init/global setup changes; all five consumers and 36 snapshots PASS.

Every unit-process omission has same-name/package integration PASS (104 rows). Parent PASS never stands for skipped child execution. Required live pack tests PASS separately; opt-in rows in other invocations remain SKIP. Legacy-bd and missing-runfile skips are not claimed as executed.

- unchanged dependency feature guard: 2 result rows.
- unchanged explicit external fixture or opt-in guard: 27 result rows.
- unchanged explicit historical placeholder or disabled characterization: 20 result rows.
- unchanged external executable, runfile or capability precondition: 40 result rows.
- unchanged host permission or procfs capability guard: 4 result rows.
- unchanged narrowly tracked conformance row: 4 result rows.
- unchanged platform guard: 30 result rows.
- unchanged provider conformance fixture: 20 result rows.
- unchanged retired characterization condition: 1 result rows.
- unchanged skip-ledger example fixture: 1 result rows.
- unchanged subprocess helper entry guard: 9 result rows.
- unchanged unsupported fixture capability: 17 result rows.
- unchanged upstream fixture connection guard: 2 result rows.
- unit process coverage executed in integration: 104 result rows.

## Required CI job accounting

Actual merged ci.yml blob `7c1953ee32ea7c17a01fe1c19ecf133642a7ff4c` has four LEAF dependencies under ruling ga-1zgega. Shared/config paths trigger full suite, Windows and pack lanes.

| Leaf | Accounting | Evidence |
|---|---|---|
| runner-policy | LOCAL-PASS | Actual policy command with EVENT_NAME=pull_request, PR_AUTHOR=quad341 and empty FORCE_BLACKSMITH. |
| changes | LOCAL-PASS | Actual required-triggers tool matches shared/config paths and computes all four leaves. |
| credential-provider-windows | DEFERRED-TO-PR-CI | Non-Linux runtime. Actual Windows production import reach is none, so no local cross-vet is required; no Windows runtime PASS is claimed. |
| pack-gate | LOCAL-PASS | Network probe, exact bundled-pin --check and both live catalog canaries PASS with zero SKIP. Release 0.1.10, commit33d3a430a67d. |

`windows_reach: none`; `deferred_to_pr_ci: credential-provider-windows (non-linux-runner)`; `not_covered: none`.
Exact commands, env and results: `/var/tmp/gc-heavy-gate/runs/ga-t34icc-rekey-fresh.ci-accounting/ACCOUNTING_VERIFIED.json` (SHA-256 `d46f164c1a5254df77cb5b2de4d9efc1ca5649a9fad45339dfba2903280291d4`).
Gate PASS permits publication and handoff. MPR must wait for CI / required and the deferred Windows check to be green on the exact gated PR head.

## Fast lane and runtime

All 18 policy/lint/generated/OpenAPI targets PASSed before the suite. Whole-repository nogo, shell/hook policies, CI-policy commands, generated and BUILD synchronization are retained at `/var/tmp/gc-heavy-gate/runs/ga-t34icc-rekey-fresh.fast-rest/COMPLETE_FAST_VERIFIED.json` (SHA-256 `e1c3e79746e3fe029f37d043c608bb962b818c13d27b68e71d8e019f1a1db6ca`). Mutation lanes used separate fresh views under gate-base.sh; canonical suite checkout remained clean.
OpenAPI form: workflow-equivalent per ga-3rtlng; standard form not run while ga-twhw87 is open. E1-E6 retain tracker/rule identity, the real pinned-base file outside /tmp, exact git bytes, comparator and all six fixtures PASS. Exception sunsets on tracker close or rule change.
Podman socket, pinned Dolt2.2.0 image and enabled container sweep were verified before C3. Ryuk-disabled pairs with BEADS_ALLOW_UNREAPED_TESTCONTAINERS. Test bd v1.3.1 and private ICU supplied. Isolation wrapper honestly reports PASS-THROUGH / NOT ISOLATED; fresh worktrees and sanitized Bazel/test-owned fixtures supply isolation.

`LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=9.48 run_max_load=11.80 run_mean_load=9.78 run_readings=72 wait_first_load=9.48 wait_max_load=9.48 wait_mean_load=9.48 wait_readings=1 read_errors=0`

Official detached systemd runs survive session recycle; raw per-job logs/XML/BEP and command/env/manifests are retained, never deleted.
`test_log_dir: /var/tmp/gc-heavy-gate/runs/ga-t34icc-rekey-fresh.c3/logs`. Complete proof `/var/tmp/gc-heavy-gate/runs/ga-t34icc-rekey-fresh.c3/FULL_SUITE_VERIFIED.json` (SHA-256 `b16f16315f7f41f969c579f45a1d21f4fca4c0031ce3e61eee7f8afd082e7e4b`).
Snapshot byte proof `/var/tmp/ga-t34icc-rekey-fresh-source-validation.json` (SHA-256 `88fb8ee0f569a0c5336db8f732d69bce8f92fc10dc3f5a2d5a1197a416acde68`).

## Publication

New isolated first-party PR from fork. No merge or live-city binary installation. Mayor owns superseding/credit/re-review interaction on old PR #4782; deployer sends the new PR URL after verification and does not act on that thread. Conditional issue-link instruction remains for mayor disposition, not an authorization to fabricate an issue.
