**Verdict:** **PASS**

# Release gate: pool work-dir isolation and deterministic MCP projection (ga-o8bi3t)

- Deploy mode: `remote`
- Base ref: `origin/main`
- Base tip evaluated: `8f3b31751b62b6642b132f8f9990f93aa337a9d5`
- Reviewed commit: `319c7d0cd70b2f43dd24327d69a9ffdc2008c464`
- Final gated head after bounded self-rebase: `63fa3c4c3769c314732cb982fb96725c5391e2b9`
- Push remote: `fork`
- Pre-flight: PASS — the reviewed commit resolves and the base repository reports no pull request carrying it.
- Review carryover: PASS — reviewed and rebased diffs have the identical stable patch ID `c8d9660b53b2426ad9873aa5edfadd7301b82e1d`.

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | **PASS** | Review bead `ga-khc9bt` records a round-two PASS at reviewed commit `319c7d0cd70b2f43dd24327d69a9ffdc2008c464`. Independent stable patch-ID comparison confirms the rebased head is content-identical. |
| 2 | Acceptance criteria met | **PASS** | Pool work-dir isolation remains intact, including explicit/unlimited pool rejection, per-instance templates, real gascity/tincan pool configurations, and dynamic-instance isolation. MCP projection now requests a session-specific comparison only for agents that actually carry a pool/session signal, preserving the shipping dir-less t3bridge mayor configuration. All diff-owned acceptance tests pass. |
| 3 | Full-scope tests pass | **PASS** | The isolated documented full suite completed 40 jobs: 39 PASS, one attributed raw FAIL, zero job SKIPs. All 19 diff-owned top-level tests and their subtests passed in a supplemental verbose isolated run. No waiver was used. |
| 3a | Pre-existing failures attributed | **PASS** | `TestPhase2StartupOutcomeBounds/kimi/tmux-cli/Failed` exceeded its fixed post-control timing bound under the 40-job run (2.932s > 2.02s). It is attributed to pre-existing tracker `ga-vkhfnj`: clause 1 passes because the test is not diff-owned; clause 2 passes because the tracker predates this run and covers the same full-suite startup-bound condition; clause 3(a) passes because `internal/worker/workertest` imports neither changed package; clause 4 passes because there is no failing-package/path overlap. The diff changes no census baseline or test target. The sighting was appended to the tracker. |
| 3b | Required policy/lint lane | **PASS** | Isolated `make test-ci-policy` passed. `go vet ./...`, formatting checks, `git diff --check`, and `make check-hooks` also passed. |
| 3c | Changed CI lane run | **PASS (n/a)** | No workflow, CI matrix, timeout, or required-check configuration is changed by this diff. |
| 4 | No high-severity review findings open | **PASS** | The round-two reviewer verdict is PASS with no blockers or unresolved high-severity findings. |
| 5 | Final branch is clean | **PASS** | `git status --short` was empty at gated head `63fa3c4c3769c314732cb982fb96725c5391e2b9` before writing this required gate record. |
| 6 | Branch diverges cleanly from main | **PASS** | The authorized bounded helper rebased and lease-pushed `319c7d0cd70b2f43dd24327d69a9ffdc2008c464` to `63fa3c4c3769c314732cb982fb96725c5391e2b9` with `rc=0`. Current `origin/main` is an ancestor, and `fork/builder/ga-61igzb` resolves to the exact final head. |
| 7 | Single feature theme | **PASS** | All six changed paths implement one coupled behavior: safe work-dir isolation for pool/dynamic instances and the MCP projection rule that consumes that isolation signal. |

## Test evidence

- `test_cmd`: `/home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- make test-local-full-parallel`
- `test_cmd_scope`: `full-suite`
- `test_counts`: 39 job PASS / 1 attributed raw job FAIL / 0 job SKIP
- Full-suite log: `/var/tmp/ga-o8bi3t-gate.iZT9XO/full-suite.log`
- Per-job logs: `/var/tmp/ga-o8bi3t-gate.iZT9XO/jobs`
- Policy log: `/var/tmp/ga-o8bi3t-gate.iZT9XO/policy-lane.log`
- Static-gate log: `/var/tmp/ga-o8bi3t-gate.iZT9XO/static-gates.log`
- Supplemental diff-owned test log: `/var/tmp/ga-o8bi3t-gate.iZT9XO/diff-owned-tests.log`
- `skip_justification`: none
- `waiver_ref`: none
- `ci_lane_run`: n/a — no CI-config change

`diff_tests_executed` (all PASS):

- `TestAgentMayHaveSessionSpecificMCPTargets` (five subtests PASS)
- `TestValidatePoolWorkDirIsolationRejectsUnsetWorkDirForPooledAgent`
- `TestValidatePoolWorkDirIsolationRejectsConstantWorkDirForPooledAgent`
- `TestValidatePoolWorkDirIsolationRejectsTemplateThatDoesNotVaryByInstance`
- `TestValidatePoolWorkDirIsolationAcceptsPerInstanceTemplate`
- `TestValidatePoolWorkDirIsolationRejectsMalformedTemplate`
- `TestValidatePoolWorkDirIsolationAcceptsExplicitSingleton`
- `TestValidatePoolWorkDirIsolationAcceptsZeroMaxActiveSessions`
- `TestValidatePoolWorkDirIsolationRejectsUnlimitedMaxActiveSessionsWithSharedWorkDir`
- `TestValidatePoolWorkDirIsolationAcceptsUnsetMaxActiveSessionsWithoutExplicitPoolSignal`
- `TestValidatePoolWorkDirIsolationRejectsExplicitUnlimitedNegativeOne`
- `TestValidatePoolWorkDirIsolationRejectsNamepoolAgentWithSharedWorkDir`
- `TestValidatePoolWorkDirIsolationAcceptsNamepoolAgentWithPerInstanceTemplate`
- `TestValidatePoolWorkDirIsolationAcceptsRealGascityAndTincanBuilderPools` (two subtests PASS)
- `TestValidatePoolWorkDirIsolationOnlyFlagsTheOffendingAgent`
- `TestValidatePoolWorkDirIsolationErrorDoesNotHardcodeARoleName`
- `TestPackageSourceDoesNotHardcodeARoleName`
- `TestResolveWorkDirPathStrictIsolatesDynamicInstancesOfDirlessWorkDirlessAgent`
- `TestResolveWorkDirPathStrictDoesNotAutoIsolateExplicitPoolAgents`

`failure_attribution`:

- `TestPhase2StartupOutcomeBounds/kimi/tmux-cli/Failed` → `ga-vkhfnj` | clause 3(a) mechanism: failing package tree cannot import either changed package; no path overlap or added test load.
