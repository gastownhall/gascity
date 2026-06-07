# Release Gate: gc perf harness

Verdict: PASS

- Deploy bead: `ga-b8ux33`
- Source bead: `ga-s86cp`
- Review bead: `ga-l2v92h`
- Branch: `builder/ga-s86cp-perf-harness`
- Candidate HEAD before gate commit: `54c7df85695f856623f32771a3299e5eab75315c`
- PR: https://github.com/gastownhall/gascity/pull/3199
- Gate date: 2026-06-07
- Release criteria source: `docs/PROJECT_MANIFEST.md` is not present in this
  checkout, so this checklist uses the deployer release criteria from the
  active role prompt.

## Release Criteria

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 1 | Review PASS present | PASS | `bd show ga-l2v92h` contains `VERDICT: PASS` from `gascity/reviewer` for commit `54c7df85695f856623f32771a3299e5eab75315c`. |
| 2 | Acceptance criteria met | PASS | The source bead acceptance criteria are satisfied by `cmd/gc/cmd_perf.go`, `cmd/gc/cmd_perf_test.go`, and `cmd/gc/main.go`; see the acceptance evidence below. |
| 3 | Tests pass | PASS | Focused perf tests, binary smoke, `make test-fast-parallel`, and `go vet ./...` all passed on the candidate branch before this gate commit. |
| 4 | No high-severity review findings open | PASS | Review notes list no blockers and no high-severity findings. The only advisory is an intentional unused stderr parameter in testable helpers. |
| 5 | Final branch is clean | PASS | The candidate branch was clean before writing this gate file; after committing the gate file, `git status --short --branch` showed no uncommitted changes. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree` against `origin/main` completed without conflict markers before this gate commit. GitHub reports PR #3199 `mergeStateStatus=CLEAN`. |
| 7 | Single feature theme | PASS | The reviewed commit set is one commit touching `cmd/gc/cmd_perf.go`, `cmd/gc/cmd_perf_test.go`, and `cmd/gc/main.go`; all changes implement the hidden `gc perf` harness. |

## Acceptance Criteria Evidence

| # | Source-bead acceptance criterion | Evidence |
|---|----------------------------------|----------|
| 1 | Hidden `gc perf` command exists for repeated measurements. | `newPerfCmd` registers a hidden Cobra command with `Hidden: true`; `main.go` adds it to the root command; `TestPerfCmd_IsHidden` verifies the hidden invariant. |
| 2 | Fresh session-create scenario is available out of the box. | `newPerfSessionNewCmd` implements `gc perf session-new`; `setupPerfCity` scaffolds a temp city with file beads and a no-op agent; binary smoke completed `gc perf session-new --iter 1 --warmup 0`. |
| 3 | Arbitrary `gc` command args can be supplied to the harness. | `newPerfRunCmd` accepts `cobra.ArbitraryArgs` and `runPerfRun` forwards the supplied args to the current `gc` binary without a shell; binary smoke completed `gc perf run --iter 1 --warmup 0 -- version`. |
| 4 | Session create timing includes step breakdown data when available. | `parseLifecycleSteps` extracts lifecycle `phases=[...]` timing into the report's `Steps`; `TestParseLifecycleSteps_Present`, `TestParseLifecycleSteps_Absent`, and `TestPrintPerfReport_WithSteps` cover extraction and report shape. |
| 5 | Automated tests cover scenario setup and report output shape. | `TestSetupPerfCity_Layout`, `TestSetupPerfCity_Cleanup`, `TestPrintPerfReport_NoSteps`, `TestPrintPerfReport_WithSteps`, and `TestPrintPerfReport_JSONShape` cover scaffold and output behavior. |

## Verification Commands

| Command | Result |
|---------|--------|
| `go test ./cmd/gc -run 'TestPerf|Perf'` | PASS: `ok github.com/gastownhall/gascity/cmd/gc 0.221s` |
| `go build -o /tmp/gc-perf-ga-b8ux33 ./cmd/gc` | PASS |
| `/tmp/gc-perf-ga-b8ux33 perf run --iter 1 --warmup 0 -- version` | PASS: one measured iteration completed and printed min/mean/p50/p95/max stats. |
| `/tmp/gc-perf-ga-b8ux33 perf session-new --iter 1 --warmup 0` | PASS: one measured iteration completed and printed min/mean/p50/p95/max stats. |
| `make test-fast-parallel` | PASS: `All fast jobs passed`. |
| `go vet ./...` | PASS |
| `git merge-tree $(git merge-base origin/main HEAD) origin/main HEAD` | PASS: no conflict markers found. |
| `git config core.hooksPath` | PASS: `.githooks`. |
