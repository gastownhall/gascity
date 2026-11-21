# Release gate: prompt template render failures (ga-dlano3jn)

**Verdict:** **PASS**

Evaluated: 2026-11-15T04:53:13.954843+00:00
Bead: ga-dlano3jn; build: ga-5wwp1u; reviewer: ga-8a1lnp8l.
Reviewed source: `beda038a11b35fd255dc6372fb9221c64224ee3c`. Deploy mode: remote; push remote: fork.
Canonical merge: `a368d3e3bca9080afb971ef144e1e3bfb8a553f0`; tree: `3154a7eb9ae9e1fe12cdb9cf15cbd7e90fa37ec7`.
Evidence root: `/var/tmp/gc-gate-ga-dlano3jn-y5q_ree3`. Raw suite, per-job logs and BEPs are retained there.

| # | Criterion | Verdict | Evidence |
|---|---|---|---|
| 1 | Reviews pass on the reviewed source | PASS | ga-8a1lnp8l round 3 PASS names the exact source above; style/security/spec have no blocker. |
| 2 | Acceptance criteria met | PASS | Session resolution rejects parse/execute failures; typed read errors preserve unreadable-template fallback. Nine named tests pass; item 4 is formally descoped by PM, below. |
| 3 | Full-scope tests and required CI accounting | PASS | All five fresh remote phases, 16 native floor steps, both owned roots, policy/generated/OpenAPI lanes and every triggered Linux CI leaf pass. One Windows job is deferred to PR CI. |
| 4 | No unresolved high-severity finding | PASS | Zero HIGH/blocker/major findings in the exact-head review. Known nonblocking limits are recorded below. |
| 5 | Final branch is clean | PASS | Canonical merge worktree clean; isolated publication carries only the reviewed source plus this record. Publication checks cleanliness again after committing the record. |
| 6 | Merge state recorded; build and vet | PASS | Clean canonical merge at the pinned base; full Bazel build and nogo/vet exit 0, independently audited successful BEPs. |
| 7 | Single feature theme | PASS | Four source files in cmd/gc, +114/-3: prompt-render error discrimination, resolution and its tests/BUILD wiring. Ancestry scoped to this feature. |

## Merge and scope

merge_state: clean (origin/main@a1eb65702f57f3f994e9ea9fa1af007436d0901b)
gate_base_kind: current-tip
gate_base_sha: a1eb65702f57f3f994e9ea9fa1af007436d0901b
merge_validation: clean merged result built/vetted and tested here; MPR must pin the eventual merge to the published head and require current PR CI. A later conflict or head change requires its normal merge-time refusal/re-gate.
No rebase or source rewrite was performed. The deploy source remains exactly the recorded reviewed commit.
Ancestry check passed with ga-dlano3jn, build ga-5wwp1u and same-feature predecessor ga-jrtj3r; no stack or denied internal paths.
No docs/PROJECT_MANIFEST.md exists in this tree; criteria come from the active deployer release-gate protocol and the canonical TESTING.md/Makefile/merged CI workflows.

## Acceptance and review limits

- Err already exists on main for parse/execute failures. The new private read-error wrapper preserves errors.Is and distinguishes unreadable input using errors.As.
- renderPrompt keeps its string signature; resolveTemplate consumes renderPromptWithMeta and refuses render errors other than the deliberate unreadable-template fallback.
- Strict gc prime already fails parse/execute errors. Non-strict gc prime still emits raw body, exit 0 and stderr diagnostic.
- Done-when item 4 (replace non-strict raw output with the builtin prompt) is **DESCOPED, not delivered**: PM ga-3431hubq closed abandoned, ruling recorded on ga-5wwp1u. Mayor mail gcg-9098241 was independently peeked and accepts this owner descope.
- Static review indicates a broken template may drain an idle pool session under legacy reconciliation, while v2 keeps the session and refuses new starts. This difference has no new regression test and was not live-proven here.
- The parse-error table assertion matches generic prompt_template wording; the separate typed-error test pins parse-error versus read-error. Neither was a blocking reviewer finding.

All nine roots were found PASS by name in fresh unit logs (`acceptance-by-name-unit.txt`):

- TestResolveTemplatePromptRenderFailure — PASS
- TestRenderPromptWithMetaReadErrorIsTyped — PASS
- TestDoPrimeStrictTemplateParseErrorFails — PASS
- TestDoPrimeStrictTemplateExecuteErrorFails — PASS
- TestDoPrimeBrokenTemplateWithoutStrictFallsBackToRawText — PASS
- TestResolveSessionTemplate_MissingDeclaredPromptIsNotSilent — PASS
- TestRenderPromptWithMeta_ErrReportsFallbacks — PASS
- TestReconcileSessionBeads_UnresolvedNamedSessionTemplateIsNoConfigDrift — PASS
- TestRenderPrompt_DeclaredButMissingTemplateWarnsToStderr — PASS

## Criterion 3: command and execution

test_cmd_scope: full-suite
test_cmd: detached gate-criterion3.sh --repo <canonical merge> --base <pinned base> --bead-json bead.json --owned owned.tsv --run-dir criterion3-run/evidence. Exact argv: runtime-launch-result.json.
The official runner executes unit //..., acceptance, integration-packages, full integration excluding the two latency tests, then only the latency tests; every farm phase forces --nocache_test_results and --noremote_local_fallback.
test_counts: PASS=154603 FAIL=0 SKIP=606 (standard reporter, top-level lines summed across logs; repeated jobs count separately).
diff_tests_executed: TestRenderPromptWithMetaReadErrorIsTyped PASS; TestResolveTemplatePromptRenderFailure PASS, each in unit and integration-packages. No owned SKIP/FAIL/MISSING or owned top-up.
test_exec_mode: remote-exec
fallback_reason: none
heavy_mode: none (official classifier); no heavy composite required.
rbe_state: own-code provenance; inherited mayor-enabled opt-in=1; rc present; kill switch absent; certificate valid through 2027-01-09.
rbe_worker_env: sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72 (canonical tree and origin/main matched at launch).
admission: remote cap=2, floor=25.0 GiB, free=86.60 GiB, wait=125.012s.
farm_test_seconds: 12956.489
test_exec_bep:

| Phase | Terminal results | Remote | Cached | Retry | Window |
|---|---:|---:|---:|---:|---|
| unit | 415 | 415 | 0 | 0 | PASS |
| acceptance | 25 | 25 | 0 | 0 | PASS |
| integration-packages | 86 | 86 | 0 | 0 | PASS |
| integration-full | 14 | 14 | 0 | 0 | PASS |
| integration-latency | 1 | 1 | 0 | 0 | PASS |

Every result passed independently parsed BEP predicates: remote strategy, uncached, attempt 1, phase-window start and terminal verdict. 1082 retained artifact digests and 541 copied logs matched independently.
test_exec_bep proofs: farm-independent-audit.json; artifact-digest-audit.json.
latency_leg: measured, first attempt PASS for TestGcBdLatencyBudget and TestGcSeatHookLatencyBudget; remote farm probe cleared after 30s (load1=44.02, threshold50); no latency test ran in the full-integration phase.
floor_topup: complete, 16/16 steps exit0; every environment-absence SKIP selected by package/root, plus beadstest, Darwin compile and runner self-tests. Native logs: criterion3-run/evidence/remote/floor-logs.
native_load: threshold15 waited_seconds=0 wait_timed_out=0 run_start_load=10.28 run_max_load=16.66 run_mean_load=13.69. Actual run summary retained in remote/floor-topup.stdout.
owned_topup: none.
skip_inventory: skip-rows-final.json and skip-justifications-final.json; 664 result rows including subtests (72 native), 309 unique sites; all sites UNCHANGED, none owned, none unresolved.
skip_justification: every skip carries its printed reason and reach explanation. No changed test body contains a skip; --explain finds only the two owned roots and no SHARED-HUNKS or HELPER-HUNKS. All skip files are outside the candidate files, and --site confirms line_in_hunk=no. No base skip comparison is required.
Unchanged reasons cover OS-only paths, unavailable external tools/fixtures/capabilities, intentional disabled scenarios, helper subprocess branches and fast-process guards before their test bodies. The only new production behavior is cmd/gc error wrapping and resolution refusal; it changes no skip switch, fixture, runtime capability or shared test helper.
The prompt-adjacent crew-template skip is a direct fixture os.ReadFile before rendering. The two prime hook skips use unchanged early process-lane guards before prompt setup. Store-seeding skips use unrelated store fixture operations.
Standard top-level counts and the supplemental subtest/site inventory have different scopes; skipped rows are never credited as PASS.
failure_attribution: none (no failed product test). waiver_ref: none; gascity has no waiver path.

## Fast lane and required CI

policy_lane: pinned-base fresh-tree Bazel policy lane PASS, 13 targets; fast-lane.sh and fast-policy.bep.json.
drift_lane: separate fresh-tree generated-file lane PASS, 7 targets; OpenAPI sync PASS, 1 target. Canonical worktree unchanged.
ci_lane_run: n/a (no workflow/job/matrix/timeout/required-check change).
ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-gate-ga-dlano3jn-y5q_ree3/merge a1eb65702f57f3f994e9ea9fa1af007436d0901b beda038a11b35fd255dc6372fb9221c64224ee3c fork
ci_workflow_blob: fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612
ci_required_leaf_count: 6 LEAF jobs need a status; the rest are rolled up through their needs.
ci_required_accounting:

- runner-policy | LOCAL-PASS | exit 0; /var/tmp/gc-gate-ga-dlano3jn-y5q_ree3/ci-required/runner-policy.log; exact command in ci-required/accounting.json
- changes | LOCAL-PASS | exit 0; /var/tmp/gc-gate-ga-dlano3jn-y5q_ree3/ci-required/triggers.txt; exact command in ci-required/accounting.json
- credential-provider-windows | NOT-TRIGGERED |   credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'
- pack-gate | NOT-TRIGGERED |   pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'
- windows-lane | LOCAL-PASS | exit 0; /var/tmp/gc-gate-ga-dlano3jn-y5q_ree3/ci-required/windows-lane.log; exact command in ci-required/accounting.json
- windows-lane-test | DEFERRED-TO-PR-CI | reason=non-linux-runner; reach=none

Triggered Linux jobs ran one at a time, detached, in fresh views behind admission/load and isolation wrappers. windows-lane scrubs fleet GC_/BD_/BEADS_/DOLT_ variables before its literal workflow build/compile commands. Runner-policy uses the PR author quad341 and workflow environment.
windows_reach: credential-provider-windows none (not triggered); windows-lane-test none, computed with its own GOOS=windows native-test import closure and the merge-base PR diff.
deferred_to_pr_ci: windows-lane-test (non-linux-runner)
not_covered: none
The initial supplemental Windows reach collector used a two-tip comparison, stopped before any CI job and was corrected to the required merge-base diff. Original output retained as .two-tip-original; the new detached collector performed the complete job accounting. The suite was not rerun.
Gate PASS authorizes PR opening only. Merge requires CI / required green on the published head and the deferred windows-lane-test successful (or skipped only by its own false filter).

## Evidence integrity

Canonical source/base/tree, build/vet BEPs, all fast phases, actual acceptance roots, package-qualified owned tests and all skip sites were independently checked. The standard suite is complete and separate from supplemental CI checks; no farm/local stitching or cache-credited farm pass.

- c6-independent-audit.json: sha256 2884c61fe338b4332bb646bd5113a34389815cb05450ee401e146942987cc4bb
- fast-independent-audit.json: sha256 e742cfaa6261995ed582f710a847250ef4b32ec500d0069df6267e078d5285cd
- criterion3-run/evidence/execution.json: sha256 b1ff31860b2b056317e97b64d87a94a78ff660b5f4b0a300240ea9d5085dc7aa
- farm-independent-audit.json: sha256 5badb4e94f6a9631d2d40e783071c4672c800168856f5a809c8ba0fdf300bc2a
- artifact-digest-audit.json: sha256 cab4166ed0f15f3c54ab2622b6d78bb55031bf72d7b0550ac2425c9e03d30c40
- owned-pairs-final.json: sha256 7b5d2134b6bc4de3fcbdcfd40b4af9906e74b7ac16dd873bf35700a8f0efcff6
- skip-justification-audit.json: sha256 a448ec145f67dfefb4829a3be59681a50b53f529d7dc067513290b48dbb83bc1
- acceptance-final-audit.json: sha256 457a88757229dd8a7e08b12c81483fca95ed4276c5c5f75685ce3f5ac8c2fcdc
- ci-required/accounting.json: sha256 f46ee60302280df38518652c3b2534098347238c2f43b527b08fdc6851e82457

Required CI load observations:

- runner-policy: LOAD_GATE_SUMMARY threshold=15 waited_seconds=150 wait_timed_out=0 run_start_load=14.94 run_max_load=NA run_mean_load=NA run_readings=0 wait_first_load=16.00 wait_max_load=16.29 wait_mean_load=15.50 wait_readings=7 read_errors=0 prewait_s=150
- windows-lane: LOAD_GATE_SUMMARY threshold=15 waited_seconds=30 wait_timed_out=0 run_start_load=14.97 run_max_load=20.60 run_mean_load=18.94 run_readings=4 wait_first_load=15.03 wait_max_load=15.03 wait_mean_load=14.99 wait_readings=3 read_errors=0 prewait_s=30
