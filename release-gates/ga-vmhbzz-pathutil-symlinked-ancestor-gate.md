# Release gate: resolve expected path in symlinked-ancestor test

Deploy bead: `ga-vmhbzz`

Source bead: `ga-nwa90w`

Originally reviewed commit: `267ea0f34ad610cc72c840848d060a146a5a3abf`

Verified review-carryover commit: `03c735e350b057be0c198d43d9f2eb746e57f82c`

Base: `origin/main@fc2f6d069ca5997ebfdea6a71d872322e5ddcc51`

Overall verdict: **PASS**

## Release criteria source

`docs/PROJECT_MANIFEST.md` is not present in this checkout. This checklist uses
the active deployer release criteria and the full-suite policy in `TESTING.md`.

## Criteria

| # | Criterion | Result | Evidence |
| --- | --- | --- | --- |
| 1 | Review PASS present | PASS | The internally authored PR #6258 carries an mpr marker for the originally reviewed head with verdict `auto-merge`; Qwen, Claude, and Codex each reported `ok`. The builder made a message-only replacement commit citing `ga-nwa90w`. Independent recomputation produced the same stable patch-id, `4f953c65179b56c49eede1036758fc5837bd8cd2`, for `267ea0f34ad610cc72c840848d060a146a5a3abf` and `03c735e350b057be0c198d43d9f2eb746e57f82c`, each against merge base `93e5d31b40dcf3bb31f3331a507b7bb50a4c2ed5`. `review_carryover_verified` is recorded on the bead. |
| 2 | Acceptance criteria met | PASS | The sole change resolves `realParent` before constructing the expected missing-leaf path, matching `ResolveNearestExistingAncestor` semantics and the sibling test. The modified regression test passed twice in the full-suite run. |
| 3 | Tests pass | PASS with attributed failures | The documented full-scope command completed with **49,869 PASS / 5 raw FAIL / 210 SKIP** test executions. All six `cmd-gc` process shards, all `cmd-gc` integration shards, all core-package shards, and all tmux shards passed. The modified `TestResolveNearestExistingAncestorSymlinkedAncestor` ran and passed twice. The five raw failures satisfy the four-clause attribution rule and are recorded below. The 210 skips are existing suite-controlled opt-in, platform, runtime, fixture, privilege, or live-infrastructure exclusions; none is diff-owned. `test_cmd_scope: full-suite`; `diff_tests_executed: TestResolveNearestExistingAncestorSymlinkedAncestor PASS x2`; `waiver_ref: none`; `policy_lane: make test-ci-policy — PASS`; `ci_lane_run: n/a (no CI-config change)`. |
| 4 | No high-severity review findings open | PASS | The mpr ensemble reported all three reviewers `ok`, chose `auto-merge`, and opened no high-severity finding. |
| 5 | Final branch is clean | PASS | Before writing this checklist, `git status --porcelain` was empty. `make fmt-check-changed`, `git diff --check`, `go build ./...`, `go vet ./...`, `make lint-new`, and `make lint-changed` all passed; `git config core.hooksPath` returned `.githooks`. |
| 6 | Branch diverges cleanly from main | PASS | Evaluated first after the open-PR preflight and repeated after the full suite on freshly fetched `origin/main`. `git merge-tree --write-tree origin/main 03c735e350b057be0c198d43d9f2eb746e57f82c` exited 0 and produced tree `d6dd1a8a920430d30846e8d3dd4fb0cef3087058`; no self-rebase was needed. |
| 7 | Single feature theme / ancestry scope | PASS | The commit changes one test in `internal/pathutil` and no production code. Its message cites accepted source bead `ga-nwa90w`; the mandatory ancestry-scope guard is run again immediately before cutting the isolated deploy branch. |

## Full-suite command and result

```text
DOCKER_HOST=unix:///run/user/1000/podman/podman.sock \
TESTCONTAINERS_RYUK_DISABLED=true \
EXTRA_TEST_ENV='DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true' \
GOFLAGS=-v LOCAL_TEST_JOBS=4 CMD_GC_PROCESS_TOTAL=6 GO_TEST_TIMEOUT=30m \
LOCAL_TEST_LOG_DIR=/var/tmp/ga-vmhbzz-full.A3Jfwo \
make test-local-full-parallel
```

The rootless Podman socket was live before the run. The candidate adds no
container-backed test, but configuring the runtime prevents a false-green skip
in any existing container-backed lane.

## Failure attribution for criterion 3

The candidate changes only `internal/pathutil/pathutil_test.go`. It changes no
production code, Beads provider/bootstrap code, reconciler code, CI target, or
test-load census; it has no path overlap with any failing integration test.

- `TestAdoptPRFormulaRetriesTransientReviewerStep`,
  `TestRetryManagedPooledWorkerRecoversClaimedAttemptAfterCrash`,
  `TestGraphWorkflowSuccessPath`, and `TestGraphWorkflowFailureRunsCleanup`
  → `ga-esyijp`; clause 3(a), mechanism: each fixture failed during city
  initialization because the host `bd` refused pending schema migrations on a
  shared Dolt server (observed counts 34, 21, 7, and 13 respectively), before
  the scenario under test began. The tracker predates this run and covers this
  exact shared-server migration condition. Candidate code cannot participate in
  Beads schema discovery or fixture initialization. No path overlap; no added
  test load.
- `TestE2E_SuspendResume_City` → `ga-dc9utn`; clause 3(a), mechanism: the run
  reproduced the tracker's deterministic roughly 94-second timeout with a
  missing `citysus` report. The tracker predates this run. A package-local
  pathutil test expectation cannot participate in city reconciler report
  generation. No path overlap; no added test load.

Both trackers were already open before the run, were updated with this exact
sighting, and were read back after the update. Full logs are under
`/var/tmp/ga-vmhbzz-full.A3Jfwo`.

## Additional static and policy evidence

- `make test-ci-policy` — PASS
- `go build ./...` — PASS
- `go vet ./...` — PASS
- `make fmt-check-changed LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=93e5d31b40dcf3bb31f3331a507b7bb50a4c2ed5` — PASS
- `git diff --check 93e5d31b40dcf3bb31f3331a507b7bb50a4c2ed5 HEAD` — PASS
- `make lint-new LINT_BASE=93e5d31b40dcf3bb31f3331a507b7bb50a4c2ed5` — PASS, 0 issues
- `make lint-changed LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=93e5d31b40dcf3bb31f3331a507b7bb50a4c2ed5` — PASS, 0 issues

## Target preflight

Original PR #6258 remains open at the originally reviewed head. It is our own
internally authored PR and has no external contributor engagement. The isolated
deploy PR produced by this gate supersedes it because the review-carryover diff
is patch-identical; the merge authority will close #6258 after the deploy PR
lands.
