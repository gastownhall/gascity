# City-scope HQ redirect release gate

**Verdict:** **PASS**

Deploy bead: ga-rkzyd4. Source bead: ga-k1e9yp. Reviewed commit: `113f3489a870fe9f8ed10b5f5ca666419e362921`. Base: `origin/main` at `e9f7e957a39dbbfa17e58cf2cfa4d7f3e2c7b3b4`. The validated merge tree is `0b5f5ee8657ef27403429758d7ba55ebf010a592`.

| # | Result | Evidence |
|---|---|---|
| 1. Review PASS | PASS | The team-authored PR #6615 is open at the exact reviewed SHA. Its MPR review comment records Qwen, Claude, and Codex as okay with an auto-merge synthesis. |
| 2. Acceptance criteria | PASS | A city-scope worktree redirect to the city's HQ `.beads` resolves as city scope. The existing declared-rig match still runs first; foreign redirects still error. New tests cover `gc bd` create, list, help, and missing-ID show, plus direct and symlinked city-store redirects. The source bead records a live before/after CLI comparison against the city-root workaround. |
| 3. Tests pass | PASS | `test_cmd: make test` through `isolated-test-run.sh` in the materialized merge worktree, with `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock` and `TESTCONTAINERS_RYUK_DISABLED=true` passed into the test environment. `test_cmd_scope: full-suite`. `test_counts: 47,185 PASS, 0 FAIL, 201 SKIP`. `diff_tests_executed: TestResolveBdScopeTargetCityStoreRedirectUsesCityStore PASS; TestRigFromRedirectedBeadsDirTreatsCityStoreRedirectAsCityScope PASS`. `waiver_ref: none`. `skip_justification: the skips are existing opt-in real-process/Dolt, live herdr, host-specific SSH, and pinned-pack coverage in the documented fast-unit target; neither diff-owned test skipped.` |
| 3a. Failure attribution | PASS | No test failures required attribution. |
| 3b. Policy/lint lane | PASS | `policy_lane: make test-ci-policy PASS; make lint PASS (0 issues with a private lint cache)`. The first lint attempt read stale findings from a deleted sibling scratch worktree; its missing-file warnings make that attempt invalid as source evidence. |
| 3c. CI configuration | PASS | `ci_lane_run: n/a (no CI configuration in the diff)`. |
| 4. High-severity findings | PASS | No unresolved HIGH finding in the reviewed PR comment. |
| 5. Clean branch | PASS | The reviewed checkout and materialized merge worktree were clean before this gate record was written. |
| 6. Clean divergence from main | PASS | `git merge-tree --write-tree origin/main <reviewed SHA>` succeeded. The materialized merge tree built with `go build ./...` and passed `go vet ./...`; the full test and policy lanes above ran in that same worktree. |
| 7. Single feature theme | PASS | The diff changes only `cmd/gc` scope resolution and its tests for the city HQ redirect behavior. |
