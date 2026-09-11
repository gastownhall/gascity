# Release gate: suspend event targets the selected city

- Deploy bead: `ga-o2qu46`
- Source bead: `ga-41g9gr`
- Existing PR: https://github.com/gastownhall/gascity/pull/6244
- Gated source: `6025a9afbff09523be3ace0a876e8e0a0ae07dda`
- Base: `origin/main@a95c730a5c62dce2f32171b32df857aa3e5b57f6`
- Deploy mode: `remote`; existing internally authored PR adopted without changing its head

## Checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | The internal mpr full-ensemble review on the exact gated head recorded `verdict=auto-merge` with Qwen, Claude, and Codex all `ok`: [review comment](https://github.com/gastownhall/gascity/pull/6244#issuecomment-5622577363). PR author and comment author are both the internal `quad341` account; no external contributor engaged. |
| 2 | Acceptance criteria met | **PASS** | `doSuspendCity` now records through `openCityRecorderAt(cityPath, stderr)` after changing the selected city's suspension state. `TestSuspendRecordsEventInTargetCity` creates distinct ambient and target cities, asserts the ambient event log remains clean, and asserts `city.suspended` lands in the target log. The implementation also fixes the operator path where `gc suspend <other-city>` is invoked from inside an unrelated city. |
| 3 | Tests pass | **PASS with attribution** | The documented full union ran on the exact gated source: `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true GO_TEST_TIMEOUT=30m LOCAL_TEST_JOBS=4 GOFLAGS=-v make test-local-full-parallel`. `test_cmd_scope: full-suite`. Result: **37/40 jobs PASS**, 3 raw FAIL jobs; **46,343 PASS / 3 FAIL / 205 SKIP** top-level test executions. Each raw failure satisfies the four-clause attribution rule below. The diff-owned `TestSuspendRecordsEventInTargetCity` executed and passed twice, in `cmd-gc-process-1-of-6` and `integration-packages-cmd-gc-3-of-6`. `waiver_ref: none`. `ci_lane_run: n/a (no CI-config change)`. Logs: `/var/tmp/gc-deploy-ga-o2qu46-full.gc7Da3`. |
| 4 | No high-severity review findings open | **PASS** | The exact-head ensemble verdict is auto-merge and reports no blocking finding; unresolved HIGH count is 0. |
| 5 | Final branch is clean | **PASS** | The isolated evaluation branch `deploy/ga-o2qu46-gate` was clean at the gated source before this checklist was added. `gofmt -l` on both changed files, `git diff --check origin/main...HEAD`, `go build ./...`, and `go vet ./...` all passed. Hooks path is `.githooks`. |
| 6 | Branch diverges cleanly from main | **PASS** | After fetching current `origin/main`, `git merge-tree --write-tree origin/main 6025a9afbff09523be3ace0a876e8e0a0ae07dda` exited 0 and produced tree `ecc3f31103312fbec613906345df2c802fc8a932`. |
| 7 | Single feature theme | **PASS** | One commit changes only `cmd/gc/cmd_suspend.go` and its regression test: target-city routing for suspend/resume lifecycle events. |

## Test evidence integrity

- Environment was prepared before testing: rootless Podman 5.8.4 was live at `/run/user/1000/podman/podman.sock`, Ryuk was disabled, and cached image `dolthub/dolt-sql-server:1.32.4` matched `testcontainers-go/modules/dolt@v0.43.0`.
- `test_cmd_scope: full-suite`
- `test_counts: 46,343 PASS / 3 FAIL / 205 SKIP`
- `diff_tests_executed: TestSuspendRecordsEventInTargetCity PASS (two full-suite lanes)`
- `skip_justification: suite-controlled platform, privilege, installed-tool, and opt-in integration exclusions; none is diff-owned. The diff-owned test ran and passed twice.`
- `waiver_ref: none`
- `ci_lane_run: n/a (no CI-config change)`
- `policy_lane: make test-ci-policy — PASS`

## Failure attribution

- `TestBdFlagManifestCurrent -> ga-f0uceo | clause 3(a), mechanism`: the host's installed `bd` exposes `--brief-deps` and `--force` beyond the checked manifest. The candidate changes only `cmd/gc/cmd_suspend*`; `internal/bdflags` does not import `cmd/gc`, the failing test is not diff-owned, the tracker predates this run, and there is no path overlap. Sighting verified as tracker comment `865c14c9-9b69-5512-818f-78cca1547973`.
- `TestSweep_ReapsRealDoltDataDirAfterSIGKILL -> ga-cp7r41 | clause 3(a), mechanism`: the external `dolt init` setup process was killed after 30.12 seconds under the four-job load, before orphan-sweep behavior ran. `examples/gastown` does not import `cmd/gc`; the test is not diff-owned, the tracker predates this run, and no changed path overlaps it. Sighting verified as tracker comment `c99ab147-cdae-5e1c-adc1-285ecbc093d4`.
- `TestE2E_SuspendResume_City -> ga-dc9utn | clause 3(a), mechanism plus prior independent reproduction`: the 94.05-second missing `citysus.report` signature is the tracker's independently reproduced reconciler wake/retire defect, which reproduces alone and predates this run. This candidate changes only which `FileRecorder` receives the already-emitted event after suspension state is written; it does not touch the proven `build_desired_state` / session-reconciler root path, and `city.suspended` / `city.resumed` have no production event consumer. The test file is not diff-owned, its package path does not overlap the diff, and the separate fix on `ga-pmafyc` is not on `main`. Sighting verified as tracker comment `9ed43990-2622-50d5-93ba-e629824bdf4b`.

## Disposition

Gate **PASS**. PR #6244 remains at the exact gated head; this checklist is committed only on the isolated local gate-evidence branch so the reviewed PR head is not mutated. Deploy clearance must be posted to `gastownhall/gascity@6025a9afbff09523be3ace0a876e8e0a0ae07dda` before routing the merge request.
