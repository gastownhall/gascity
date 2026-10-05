**Verdict:** **PASS**

# Release gate: herdr pane-busy retry

- Deploy bead: `ga-f0n58g`; build bead: `ga-iwanrj`; review bead: `ga-n71bby`.
- Reviewed source commit: `86e0bb642d1747b74a67b36239ac64b56ce56d90` (resolved as a commit).
- Full-suite candidate: merge scratch `79e3feda0031c0ab2db208f727697ac79e40a2c9`, combining the reviewed source with `origin/main` at `e9f7e957a39dbbfa17e58cf2cfa4d7f3e2c7b3b4`.
- Final base freshness check: `origin/main` advanced to `543f3bc789e59355c8ba6ea24595f3c5ffb5c0ba`; `git merge-tree --write-tree origin/main 86e0bb642d1747b74a67b36239ac64b56ce56d90` succeeds and `git diff --check` is clean. The new main commit changes beads/identity paths, with no herdr file overlap. The source commit remains the reviewed deploy source.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Reviewer bead `ga-n71bby` records `verdict: pass` for the resolved source commit. Builder red/green evidence is recorded on `ga-iwanrj`. |
| 2 | Acceptance criteria met | **PASS** | Both regression tests pass independently; six concurrent live streams each ran `TestProviderLiveClaudeKindPath` three times: 18 PASS, 0 FAIL, 0 SKIP, 0 `agent_pane_busy`. Existing retry limit and backoff remain intact. The four named error-code audit sites were inspected: `agent_name_taken.go:35` and `capabilities.go:56` receive typed zero-exit envelopes; the answer path at original `client.go:170` has its own strict parser for both shapes; the startup-turn path at original `client.go:548` receives typed zero-exit envelopes. They require no behavior change. |
| 3 | Tests pass | **PASS** | The documented full-scope command `make test-local-full-parallel`, run through `isolated-test-run.sh` with a rootless Podman socket, passed all 40 jobs: 40 PASS, 0 FAIL, 0 SKIP jobs. Named diff-owned tests: 2 PASS, 0 FAIL, 0 SKIP. Supplementary live acceptance: 18 PASS, 0 FAIL, 0 SKIP. Tier A acceptance passed. All eight WorkerCore profile commands passed. Pinned beads v1.3.0/Dolt v2.1.7 proxied-default, topology, and proxied-native jobs passed; see skip notes below. `go build ./...` and `go vet ./...` passed. Logs: `/var/tmp/ga-f0n58g-full-r2.rgG0l4/run.log`, `/var/tmp/ga-f0n58g-live/`, `/var/tmp/ga-f0n58g-worker/`, `/var/tmp/ga-f0n58g-beads-acceptance/`. |
| 4 | No HIGH review findings open | **PASS** | Reviewer security findings: none; no unresolved HIGH findings recorded. |
| 5 | Final branch clean | **PASS** | Role worktree was clean before the gate file was written; the isolated deploy branch will commit only this gate record above the reviewed source. |
| 6 | Clean divergence from main | **PASS** | Fresh `origin/main` `543f3bc789e59355c8ba6ea24595f3c5ffb5c0ba`; merge-tree exit 0, no conflicts; diff check exit 0. |
| 7 | Single feature theme | **PASS** | Two reviewed commits alter only herdr CLI error-code extraction, the pane-busy retry guard, and their focused tests under `internal/runtime/herdr/`. |

## Criterion 3 details

- `test_cmd_scope: full-suite`
- `test_cmd: isolated-test-run.sh -- bash -c 'make test-local-full-parallel'`
- `test_counts: 40/40 full-suite jobs PASS; 0 FAIL; 0 SKIP jobs`
- `diff_tests_executed: TestHerdrCodeAnyShapeRecoversBothErrorShapes PASS; TestStartRetriesAgentStartOnNonZeroExitPaneBusy PASS; 0 FAIL; 0 SKIP`
- `live_acceptance: 6 concurrent go-test streams x 3 iterations, 18 PASS, 0 FAIL, 0 SKIP, 0 pane-busy errors`
- `policy_lane: PASS — make test-ci-policy, check-gomod-replace, check-native-dependency-surface, check-eventexport-isolation, check-core-boundary, test-native-doltlite-beads, lint-affected, fmt-check-changed, check-docs; go vet ./... PASS. lint-affected initially read stale findings from a deleted scratch checkout; a fresh on-disk lint cache produced 0 issues.`
- `ci_lane_run: n/a — no CI configuration files changed`
- `skip_justification: CI's topology M5 legacy-GC row and both migration cases skip because no pre-journal gc binary is supplied by that CI job or this local equivalent; proxied-local M1, proxied-default, and both proxied-native top-level cases ran and passed. No diff-owned test skipped.`
- `failure_attribution: first full-suite attempt observed TestPhase2StartupOutcomeBounds/codex/tmux-cli/Ready timing failure -> pre-existing tracker ga-hvjhay; separate worker package cannot import internal/runtime/herdr, no path overlap, no added test target or census bump. The unlanded stamped fix ga-e4bhca permits the fix-carrying exception for this repeat. The fresh full-suite run recorded above passed that job. No failure is attributed in the final run.`
- `waiver_ref: none`
