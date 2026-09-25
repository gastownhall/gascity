# City-scope HQ redirect release gate

**Verdict:** **FAIL**

Correction: the first gate record incorrectly treated Gas City's `make test` fast-unit target as the full local sweep. Criterion 3 cannot pass until `make test-local-full-parallel` runs on the materialized merge tree. The earlier deploy clearance on commit `4ed2e354a94b19f6458dd577af991adbf3fa03ff` is withdrawn for merge purposes; PR #6623 is labeled `blocked-on-deploy-gate` pending a fresh gate.

Deploy bead: ga-rkzyd4. Source bead: ga-k1e9yp. Reviewed commit: `113f3489a870fe9f8ed10b5f5ca666419e362921`. Base: `origin/main` at `e9f7e957a39dbbfa17e58cf2cfa4d7f3e2c7b3b4`. The validated merge tree is `0b5f5ee8657ef27403429758d7ba55ebf010a592`.

| # | Result | Evidence |
|---|---|---|
| 1. Review PASS | PASS | The team-authored PR #6615 is open at the exact reviewed SHA. Its MPR review comment records Qwen, Claude, and Codex as okay with an auto-merge synthesis. |
| 2. Acceptance criteria | PASS | A city-scope worktree redirect to the city's HQ `.beads` resolves as city scope. The existing declared-rig match still runs first; foreign redirects still error. New tests cover `gc bd` create, list, help, and missing-ID show, plus direct and symlinked city-store redirects. The source bead records a live before/after CLI comparison against the city-root workaround. |
| 3. Tests pass | FAIL | The `make test` run through `isolated-test-run.sh` in the materialized merge worktree is diagnostic fast-unit evidence only: 47,185 PASS, 0 FAIL, 201 SKIP. `test_cmd_scope: focused (fast-unit tier; not full local suite)`. Both diff-owned tests passed in that run. `test_cmd: make test-local-full-parallel` is required and has not yet completed. `waiver_ref: none`. |
| 3a. Failure attribution | PASS | No test failures required attribution. |
| 3b. Policy/lint lane | PASS | `policy_lane: make test-ci-policy PASS; make lint PASS (0 issues with a private lint cache)`. The first lint attempt read stale findings from a deleted sibling scratch worktree; its missing-file warnings make that attempt invalid as source evidence. |
| 3c. CI configuration | PASS | `ci_lane_run: n/a (no CI configuration in the diff)`. |
| 4. High-severity findings | PASS | No unresolved HIGH finding in the reviewed PR comment. |
| 5. Clean branch | PASS | The reviewed checkout and materialized merge worktree were clean before this gate record was written. |
| 6. Clean divergence from main | PASS | `git merge-tree --write-tree origin/main <reviewed SHA>` succeeded. The materialized merge tree built with `go build ./...` and passed `go vet ./...`; the fast-unit and policy lanes above ran in that same worktree. |
| 7. Single feature theme | PASS | The diff changes only `cmd/gc` scope resolution and its tests for the city HQ redirect behavior. |
