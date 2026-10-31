# Managed Dolt foreign listener release gate

**Verdict:** **PASS**

- Deploy bead: ga-6aawc6g; build: ga-8vpq1k; review: ga-29mq3r.
- Exact reviewed source: `33434ad98ae896937fb8d5768f5d14258be7fe80`. The builder branch is provenance only.
- Pinned base: `1e6c1251db3bd7c2a1c8cab73310352dbbd446d0`; canonical normal-hook merge: `a3840975d8a1bae2e2af297c076e4dcf889a4400`; tree: `e8156a9a32bca2845414a9a6954718ec4d2414b8`.
- The final isolated deploy branch is cut mechanically from the reviewed source. This gate file is its only additional content; the canonical merge is a disposable test artifact.
- Source and canonical per-file patch IDs match for all eight changed paths. No branch-tip substitution, self-rebase, failure attribution or waiver.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Review ga-29mq3r is closed with literal PASS on the exact resolved source. Its build lineage matches ga-8vpq1k. Reviewer test results are inputs, not inherited gate evidence. |
| 2 | Acceptance criteria met | PASS | Foreign listeners survive shell start while a new scoped server starts on another port. Owned stale servers use the existing journal-safe stop helper. Both kernel bind errors and Dolt 2.3.3 port-in-use diagnostics retry. All seven diff-owned roots and twelve matcher children actually PASS in both complete unit and integration package lanes; database/name false positives stay negative. Generated configuration comments are edited at source and all regeneration checks pass. |
| 3 | Full CI-equivalent tests pass | PASS | Full whole-repository unit/nogo/format/generated/policy, acceptance, integration packages and the committed CI smoke target pass. Exact job and Go terminal counts, required CI leaf accounting and skip evidence are below. |
| 4 | No unresolved HIGH finding | PASS | Reviewer style/security/spec findings contain no HIGH or blocker. Explicitly non-gating follow-ups ga-9swd9p, ga-4qj1mx and ga-sdon3o remain separate work. |
| 5 | Final branch is clean | PASS | Assigned and canonical worktrees are clean before publication. Publication requires a normal-hook gate commit, an otherwise unchanged reviewed source tree, and an empty final git status before pushing or opening the PR. |
| 6 | Diverges cleanly from main | PASS | Exact source merged normally with the pinned base. Predicted/materialized trees match, normal hooks ran, and all 1232 whole-repository build/nogo targets passed. No conflict resolution or self-rebase. |
| 7 | Single feature theme | PASS | One managed Dolt startup behavior in Go and embedded shell, its tests and generated explanatory configuration docs. All three source commits cite the same build bead; the absolute deployer ancestry guard passes with no stack and no agent-config paths. |

test_cmd_scope: full-suite
waiver_ref: none
failure_attribution: none
ci_lane_run: n/a (no CI-config change)
heavy_mode: none (official computed classification)
test_log_dir: /var/tmp/gc-heavy-gate/runs/ga-6aawc6g.c3/logs
documented_gate: TESTING.md / Makefile / current CI. docs/PROJECT_MANIFEST.md is absent; no guessed language default is used.
policy_lane: PASS — required four shell guards, test-ci-policy, lint/nogo, format and all eleven policy targets.
drift_lane: PASS — make bazel-sync with empty tracked/untracked status and all six generated/lockfile targets, each in its own fresh view before the suite.
openapi_lane: PASS — standard make openapi-breaking-check with the normal copied pinned base input; real comparator and fixtures run. No exception.
ci_required_leaf_accounting: runner-policy and changes LOCAL-PASS; pack-gate LOCAL-PASS (exact bundled pin and both fresh live registry roots). credential-provider-windows DEFERRED-TO-PR-CI reason=non-linux-runner. Actual closure reaches none of its packages; no Windows vet requirement and no uncovered leaves.
runtime: actual Go1.26.9, rootless Podman socket/sweep and pinned Dolt2.2.0 image verified; test-only bd1.3.1, lsof and python3 available. Ryuk disabled with the verified sweep. The official isolation wrapper and one private ABI wrapper surround the suite. The ABI wrapper overlays ICU libraries only; a separate private-/tmp capability probe does not change that description. Every Bazel job uses linux-sandbox.
execution: Bazel9.3, CI/fork-cache, jobs4, uncached attempt1 for every job. No test-name or package filter except the committed CI integration-smoke config. Unit GC_FAST_UNIT=0 enables process tests across the whole //... scope, a superset of the default fast tier.
deployment_guard: GC_REAPER_SESSION_PURGE_AGE=876000h verified before building. This gate installs no gc binary.

| Lane | Actual full-scope command (common CI/fork-cache/uncached/jobs4/runtime flags retained) | Targets | Jobs | Go PASS | FAIL | SKIP |
|---|---|---:|---:|---:|---:|---:|
| unit | `make check BAZEL=bazel --batch; whole //..., GC_FAST_UNIT=0` | 238 | 322 | 55539 | 0 | 107 |
| acceptance | `bazel test --config=acceptance //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests` | 7 | 24 | 410 | 0 | 13 |
| integration-packages | `bazel test --config=integration //test:integration_packages` | 28 | 75 | 36858 | 0 | 53 |
| integration-smoke | `bazel test --config=integration-smoke //test/integration:integration_test` | 1 | 14 | 16 | 0 | 1 |

diff_tests_executed:

- cmd/gc TestGcBeadsBdStartLeavesForeignPortHolderAlone: actual PASS in both full unit and integration package lanes; all emitted children PASS.
- cmd/gc TestGcBeadsBdStartRestartsServerHoldingDeletedDataInodes: actual PASS in both full unit and integration package lanes; all emitted children PASS.
- cmd/gc TestGcBeadsBdStartRetriesAutoPortBindConflict: actual PASS in both full unit and integration package lanes; all emitted children PASS.
- cmd/gc TestGcBeadsBdStartRetriesAutoPortInUseText: actual PASS in both full unit and integration package lanes; all emitted children PASS.
- cmd/gc TestManagedDoltStartupPortInUse: actual PASS in both full unit and integration package lanes; all emitted children PASS.
- cmd/gc TestStartManagedDoltProcessWithOptions_AddressInUseBumpsPortWhenWaitTimesOut: actual PASS in both full unit and integration package lanes; all emitted children PASS.
- cmd/gc TestStartManagedDoltProcessWithOptions_PortInUseTextBumpsPortWhenWaitTimesOut: actual PASS in both full unit and integration package lanes; all emitted children PASS.

skip_justification: All 174 actual non-owned SKIP rows retain their reason, official UNCHANGED source-site result, byte-identical source file/shared-hunk proof and complete tag-specific test-binary import/input closure in each `/var/tmp/gc-heavy-gate/runs/ga-6aawc6g.c3/<phase>-skip-causal-review.json`. No observed skip site has shared hunks; changed helper callgraphs and the existing watchdog init are independently verified. Config code has an identical structural AST excluding comments/positions; generated reference docs are not root compile/embed inputs. Each reachable prerequisite is explicitly explained. Skips remain SKIP and supply no coverage. Whole unit enables process cases, so no process-row deferral is used.

Load measurement (actual complete chain, wait and run separately):

```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=120 wait_timed_out=0 run_start_load=14.19 run_max_load=18.60 run_mean_load=15.25 run_readings=48 wait_first_load=18.12 wait_max_load=18.12 wait_mean_load=16.24 wait_readings=5 read_errors=0
```

Complete evidence: `/var/tmp/gc-heavy-gate/runs/ga-6aawc6g.c3/SUITE_MANIFEST_VERIFIED.json`; `/var/tmp/gc-heavy-gate/runs/ga-6aawc6g.c3/terminal-result-inventory.json`; `/var/tmp/deploy-ga-6aawc6g.zgrjpojp/C6_VERIFIED.json`; `/var/tmp/gc-heavy-gate/runs/ga-6aawc6g.fast/FAST_VERIFIED.json`; `/var/tmp/gc-heavy-gate/runs/ga-6aawc6g.ci-accounting/ACCOUNTING_VERIFIED.json`.
Frozen commands and retained raw test logs/XML/BEP: `/var/tmp/gc-heavy-gate/runs/ga-6aawc6g.c3/evidence-tools` and `/var/tmp/gc-heavy-gate/runs/ga-6aawc6g.c3/bazel-artifacts`. Evidence/parser corrections retain originals under verification-instrumentation-retained: parallel Go NAME frames now associate the actual skip reason; a pre-existing init body is verified unchanged rather than incorrectly requiring its absence. Terminal results are unchanged and no tests were rerun.
Deploy clearance and the verified Mayor-to-MPR merge-request are published only after the guarded normal-hook push and exact PR head check. The deployer does not merge.
