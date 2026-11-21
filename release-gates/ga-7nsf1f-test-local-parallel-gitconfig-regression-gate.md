# Release gate: exercise Git-config propagation through the sharded runner

- Deploy bead: `ga-7nsf1f`
- Build bead: `ga-9t7vpl`
- Review bead: `ga-mx6fsa`
- Reviewed source: `8f0df9f3504b94142bd721c82898464047fb56fa`
- Reviewed source branch: `builder/ga-9t7vpl` (provenance only)
- Base checked: `origin/main@4c4179eed55bd2815cdcddfb176ecd8605dbfc93`
- Planned deploy branch: `deploy/ga-7nsf1f-gate`
- Evaluated: 2026-09-09
- Verdict: **PASS with attributed raw test and lint failures**

## Gate checklist

The target pre-flight ran before criterion 6. The reviewed source resolved to
the full commit above and is not associated with an existing pull request. The
source and current base merge without conflict, so all seven deployer criteria
were evaluated.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Closed review bead `ga-mx6fsa` records `verdict: pass` in round 2 for the exact reviewed source. The reviewer independently verified that the production export predates this change, the net diff is test-only, and the regression test passes on the reviewed SHA. |
| 2 | Acceptance criteria met | **PASS** | The added `TestFanOutWorkerReceivesExportedGitConfigGlobal` executes the real `run_fan_out` body with a one-job synthetic harness and proves the child `bash -c` process receives a usable `GIT_CONFIG_GLOBAL`. The 40-job full runner dispatched every job without the target `gc_test_gitconfig: unbound variable` failure. The former real-push and `--no-verify` criteria were explicitly withdrawn by mayor after confirming the production fix had already shipped as `ga-cesmzs` / PR #6116. |
| 3 | Tests pass | **PASS with attributed raw failures** | `make test-local-full-parallel`, the documented full local runner, completed all 40 scheduled jobs: **38 PASS / 2 attributed raw FAIL / 0 job-level SKIP**. The diff-owned regression test passed in the unfiltered `unit-core` job and again in a direct named verification. Both raw failures are non-diff-owned and satisfy criterion 3a. `test_cmd_scope: full-suite`; `waiver_ref: none`. |
| 3a | Pre-existing failures attributed | **PASS** | `TestBdFlagManifestCurrent` is the installed-CLI manifest drift tracked by open bead `ga-f0uceo`. `TestProviderLiveClaudeKindPath` is live herdr pane contention tracked by open bead `ga-iepsvr`. Both trackers predate this run; current sightings were appended and read back. The candidate changes only `scripts/git_test_env_test.go`, so neither failing package can import or execute it, and neither failing path overlaps the diff. |
| 3b | Policy and static lanes | **PASS with attributed raw lint failure** | `make test-ci-policy`, module/native/event-export/open-core guards, native DoltLite tests, full formatting, `go vet`, and docs checks all passed. `make lint` failed only by replaying cached diagnostics from the already-deleted `/var/tmp/gc-maintainer-fix.4547.S6Jy0t` worktree; open tracker `ga-039od0` predates this run and records the same cross-worktree cache condition. No diagnostic path overlaps this diff. `policy_lane: 9 PASS / 1 attributed raw FAIL`. |
| 3c | CI-config lane | **PASS — n/a** | `ci_lane_run: n/a (no CI job, workflow, matrix, timeout, or required-check change)`. |
| 4 | No unresolved HIGH review findings | **PASS** | Unresolved HIGH findings: 0. The review-of-record reports no style, security, specification, or coverage blockers after the mayor corrected the original incident scope. |
| 5 | Final branch clean | **PASS** | Before this checklist was added, `git status --porcelain` produced no output, `git diff --check origin/main...8f0df9f3504b94142bd721c82898464047fb56fa` passed, and `core.hooksPath` was `.githooks`. The checklist is the only deployer-authored file and will be committed separately on the isolated deploy branch. |
| 6 | Branch diverges cleanly from main | **PASS** | After a final `git fetch origin main`, `git merge-tree --write-tree origin/main 8f0df9f3504b94142bd721c82898464047fb56fa` exited 0, produced tree `cc21f58bb8902ac2d59bc36584b79d657f33dc6f`, and reported no conflict. The source is 16 commits behind and 2 commits ahead of current `origin/main`. No self-rebase was needed. |
| 7 | Single feature theme | **PASS** | The RED/GREEN pair changes one test file in `scripts` for one purpose: prevent regression of hermetic Git-config propagation into sharded runner workers. No independent feature is bundled. |

## Full-suite test evidence

The rootless Podman socket was live before the run, and the cached Dolt tag
matched `deps.env` (`2.1.7`). The documented full local runner then exercised
the fast unit tree, non-short `cmd/gc` process shards, product-metrics profile,
and integration buckets:

```text
DOCKER_HOST=unix:///run/user/1000/podman/podman.sock \
TESTCONTAINERS_RYUK_DISABLED=true \
LOCAL_TEST_LOG_DIR=/var/tmp/ga-7nsf1f-full.ntHzMy \
  make test-local-full-parallel
```

- `test_cmd_scope: full-suite`
- `test_counts: 38 PASS jobs, 2 attributed raw FAIL jobs, 0 job-level SKIP`
- `raw_test_failures: TestBdFlagManifestCurrent; TestProviderLiveClaudeKindPath`
- `diff_tests_executed: TestFanOutWorkerReceivesExportedGitConfigGlobal PASS`
- `skip_justification: no job-level skips; the diff-owned test has no skip path and reported PASS`
- `waiver_ref: none`
- `ci_lane_run: n/a (no CI-config change)`
- `raw_logs: /var/tmp/ga-7nsf1f-full.ntHzMy` (40 per-job logs)

The direct named verification was supplemental evidence, not a narrowed
substitute for the full run:

```text
go test -count=1 -v ./scripts -run '^TestFanOutWorkerReceivesExportedGitConfigGlobal$'
=== RUN   TestFanOutWorkerReceivesExportedGitConfigGlobal
--- PASS: TestFanOutWorkerReceivesExportedGitConfigGlobal (0.05s)
PASS
```

## Raw failures and attribution

| Raw result | Test/check | Tracker and proof |
|---|---|---|
| **FAIL — ATTRIBUTED** | `TestBdFlagManifestCurrent` | Open tracker `ga-f0uceo`, created 2026-08-15. The failure is determined by the host-installed `bd` help surface and `internal/bdflags/bdflags.go`; this test-only `scripts` diff can alter neither. No path overlap. Current log: `/var/tmp/ga-7nsf1f-full.ntHzMy/integration-packages-core-1-of-4.log`. |
| **FAIL — ATTRIBUTED** | `TestProviderLiveClaudeKindPath` | Open tracker `ga-iepsvr`, created 2026-09-02, names the same live herdr pane-busy condition. The failing package cannot import package-local tests under `scripts`, and the error is the external pane state `w1:p1 is not an available shell`. No path overlap. Current log: `/var/tmp/ga-7nsf1f-full.ntHzMy/integration-packages-core-3-of-4.log`. |
| **FAIL — ATTRIBUTED** | Full-repository lint | Open tracker `ga-039od0`, created 2026-08-31, covers golangci-lint replaying diagnostics from deleted sibling worktrees. Every reported path was under deleted `/var/tmp/gc-maintainer-fix.4547.S6Jy0t`; none exists in or overlaps this candidate. Current log: `/var/tmp/ga-7nsf1f-policy.KGc0Ug/lint.log`. |

`inconclusive-guard: n/a — each attribution has a direct mechanism proof; no
failure is diff-owned, no failure path overlaps the candidate, and the diff
adds no test target, runner command, resource-census baseline, timeout, or
parallelism configuration.`

## Policy and static evidence

```text
make test-ci-policy                  PASS
make check-gomod-replace             PASS
make check-native-dependency-surface PASS
make check-eventexport-isolation     PASS
make check-core-boundary             PASS
make test-native-doltlite-beads      PASS
make lint                            FAIL — ATTRIBUTED to ga-039od0
make fmt-check                       PASS
make vet                             PASS
make check-docs                      PASS
git diff --check                     PASS
git config --get core.hooksPath      .githooks
```

Raw policy logs are in `/var/tmp/ga-7nsf1f-policy.KGc0Ug`. Sightings appended
to `ga-f0uceo`, `ga-iepsvr`, and `ga-039od0` were read back from the bead store
before this record was written.

## Source, pre-flight, and ancestry evidence

- The recorded source passed the hex-only guard and resolved exactly with
  `git rev-parse --verify --quiet`.
- `gh api repos/gastownhall/gascity/commits/8f0df9f3504b94142bd721c82898464047fb56fa/pulls`
  returned no pull request, so no merged or closed-PR reconciliation applies.
- The source range from merge base
  `042e965f0be01710bb6d45393052df80196c6dca` is the TDD pair
  `7196bb4a99` (RED) and `8f0df9f350` (GREEN), both citing build bead
  `ga-9t7vpl`.
- `assert_deploy_ancestry_scope origin/main 8f0df9f3504b94142bd721c82898464047fb56fa ga-7nsf1f ga-9t7vpl`
  passed. No unrelated commit or `.claude/**` path rides in the deploy range.

## Disposition

Gate PASS. Create `deploy/ga-7nsf1f-gate` from the exact reviewed source,
commit this checklist, push only the isolated branch, open a pull request
against `main`, publish `release-gate/deploy-clearance=success` on the exact
pull-request head, and route the merge request to mayor/mpr. The deployer does
not merge.
