# Release Gate: Stranded Routed-Demand Detector (`ga-3q16a7`)

**Verdict:** **PASS**

- Deploy bead: `ga-3q16a7`
- Reviewed commit: `4a1d7cf5b8737866af70b49711e9d1bbb3f77df2`
- Comparison base: `origin/main@d807c8e0dd6d071a79e821be5da73097dbf4ff96`
- Merge base: `fa4da4beaf24d7441f28a050b39c825c3cca90c0`
- Deploy mode: `remote`; push remote: `fork`
- Release-criteria source: `docs/PROJECT_MANIFEST.md` is absent from both the reviewed commit and `origin/main`, so this record applies the active deployer criteria, `TESTING.md`, and the source bead's exit contract.

## Criteria

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Round-2 review bead `ga-5xzha5` records `verdict: pass` at the exact reviewed commit. Round 1 (`ga-cks4ct`) requested correction of four stale predicate descriptions; round 2 verified all four corrections and found no blocking uncovered criteria. |
| 2 | Acceptance criteria met | **PASS** | The candidate resolves routes with `agentutil.RoutedToIdentity`, treats missing/suspended/non-ephemeral targets as structurally unwakeable without consulting `min_active_sessions`, recognizes live pool-managed sessions, keeps event and Require-mode mutation sets on the same throttled subset, defaults the rollout gate to Off, registers a typed event payload and declared metadata keys, preserves first-sight/escalation/recovery markers, and carries generated schema/client/dashboard artifacts in sync. The nine detector tests, typed-event test, rollout-registry regression test, and typed-class census test all passed. The original PR `#4607` was already closed by the mayor and was not touched. |
| 3 | Tests pass | **PASS with attribution** | `test_cmd: isolated-test-run.sh -- bash -c 'make test-local-full-parallel'`; `test_cmd_scope: full-suite`; 40 jobs, 39 green and one raw failure; `test_counts: 51,408 PASS / 1 raw FAIL / 227 SKIP`; `waiver_ref: none`. The raw failure is attributable under 3a. No isolation `TRIPWIRE` fired. |
| 3a | Pre-existing failures attributable | **PASS** | `failure_attribution: TestGCLiveContract_BeadsAndEvents -> ga-lejnse | clause 3(a) mechanism — the temporary city's external bd initialization refused five pending shared-server migrations (v61 -> v66) before the controller or stranded-demand reconcile path could run`. The exact test/random-cursor condition predates this run on multiple unrelated deploys; its root cause/fix is `ga-e2z1zb`. Clause 1 passes because `test/integration/gc_live_contract_test.go` is not diff-owned. Clause 2 passes because `ga-lejnse` predates this run, covers this exact test/condition, and was opened; this sighting was appended and read back. Clause 4 passes because the candidate changes no file in `test/integration`. |
| 3b | Policy/lint lane | **PASS** | `policy_lane: make test-ci-policy — PASS`; `go vet ./...`, `go build ./...`, candidate `gofmt -l`, `git diff --check`, `make check-docs`, `make spec-ci`, `make dashboard-ci`, and `make check-hooks` all passed. The generated dashboard was served with Vite on `127.0.0.1:4179` and returned its HTML shell. |
| 3c | CI-config lane | **PASS — n/a** | `ci_lane_run: n/a (no CI job, workflow, matrix, timeout, or required-check list changed)`. |
| 4 | No high-severity review findings open | **PASS** | Unresolved HIGH findings: 0. Round 2 records `uncovered_criteria: none blocking`. The separate P3 stale payload-doc follow-up is `ga-kc7lrq`; it is explicitly outside this deploy. |
| 5 | Final branch clean | **PASS** | The exact reviewed commit was clean before gate creation. The gate record is committed on the isolated deploy branch and the branch is rechecked clean before push. |
| 6 | Branch diverges cleanly from main | **PASS** | After fetching `origin/main`, `git merge-tree --write-tree --messages origin/main 4a1d7cf5...` exited 0 and produced tree `a0d9cb1da369dd63bf2795c3884a2fbdc8e49d8a`; no bounded self-rebase was needed. |
| 7 | Single feature theme | **PASS** | All source, event, rollout, config, generated docs/OpenAPI/client, dashboard bundle, and tests implement one feature: detecting and reporting structurally stranded `gc.routed_to` demand. No independent behavior is bundled. |

## Test evidence

- `diff_tests_executed`: all nine top-level tests in `cmd/gc/stranded_routed_demand_test.go` PASS; `TestTypedClassCodecCensusRatchet` PASS; `TestRoutedDemandStrandedEventIsKnownAndTyped` PASS; `TestDemandStrandedRoutePolicySpecDoesNotRestateSupersededMinActiveSessionsPremise` PASS. The `cmd/gc` tests also passed again in the integration package shards.
- `skip_justification`: the 227 SKIPs are existing platform/capability/explicit opt-in cases (including Darwin service behavior, live-provider/Kubernetes tests, root-only permission behavior, and Dolt-persistence tests guarded on their required environment). None is diff-owned; every diff-owned test reported PASS by name.
- Full verbose job logs: `/var/tmp/ga-3q16a7-full-suite.JwFX5G/`.
- Failure log: `/var/tmp/ga-3q16a7-full-suite.JwFX5G/integration-rest-full-5-of-8.log`.

## Merge-authority note

This is an architecture-significant reimplementation of the detector rejected in PR `#4607`. Passing this release gate authorizes opening the replacement PR only. Merge remains MPR-exclusive and requires a fresh architect verdict/clear-hold cycle; this deploy must not be self-merged.
