# City-scope HQ redirect release gate

**Verdict:** **FAIL**

Correction: the first gate record incorrectly treated Gas City's `make test` fast-unit target as the full local sweep. The full `make test-local-full-parallel` run on the materialized merge tree was stopped after a decisive repeat tracked failure. Criterion 3 remains uncleared. The earlier deploy clearance on commit `4ed2e354a94b19f6458dd577af991adbf3fa03ff` is withdrawn for merge purposes; PR #6623 is labeled `blocked-on-deploy-gate` and its current head has no clearance.

Deploy bead: ga-rkzyd4. Source bead: ga-k1e9yp. Reviewed commit: `113f3489a870fe9f8ed10b5f5ca666419e362921`. Base: `origin/main` at `e9f7e957a39dbbfa17e58cf2cfa4d7f3e2c7b3b4`. The current materialized merge tree is `b1813903ef6494ebb444eab21b984b4e91ab536a`.

| # | Result | Evidence |
|---|---|---|
| 1. Review PASS | PASS | The team-authored PR #6615 is open at the exact reviewed SHA. Its MPR review comment records Qwen, Claude, and Codex as okay with an auto-merge synthesis. |
| 2. Acceptance criteria | PASS | A city-scope worktree redirect to the city's HQ `.beads` resolves as city scope. The existing declared-rig match still runs first; foreign redirects still error. New tests cover `gc bd` create, list, help, and missing-ID show, plus direct and symlinked city-store redirects. The source bead records a live before/after CLI comparison against the city-root workaround. |
| 3. Tests pass | FAIL | `test_cmd: make test-local-full-parallel LOCAL_TEST_JOBS=4` through `isolated-test-run.sh` in the current materialized merge worktree; `test_cmd_scope: full-suite`. Of 40 planned jobs, 22 PASS, 2 FAIL, 4 started without a result, and 12 were not started when the run was interrupted after the decisive repeat. This incomplete run is not PASS evidence. The two diff-owned tests were included in passing `cmd-gc-process` shards 2 and 4 and independently reported named PASS in the earlier fast-unit JSON log. `waiver_ref: none`. Full log: `/var/tmp/gc-gate-ga-rkzyd4-full.log`; shard logs: `/var/tmp/gc-local-tests.ZMNpKo`. |
| 3a. Failure attribution | FAIL (hold) | `TestInterruptWhileIdleDoesNotExit` timed out in Zcode adapter core shard 4. Existing open issue ga-vn6g6y predates this run and now records this Linux signature. The diff is confined to `cmd/gc`, unreachable from `internal/worker/adapters/zcode`, with no path overlap or new test target. `TestPhase2WorkerCoreRealTransportProof/claude/tmux-cli` timed out during startup in `cmd/gc` integration shard 5. This repeats the exact tracked WC-TRANSPORT-001 signature in ga-kgm5nr, consolidated under open tracker ga-vkhfnj. Source build bead ga-k1e9yp is not fix-carrying; the repeat rule requires a hold. Deploy bead ga-rkzyd4 depends on ga-vkhfnj. |
| 3b. Policy/lint lane | PASS on prior code-equivalent head | `policy_lane: make test-ci-policy PASS; make lint PASS (0 issues with a private lint cache)` on the preceding head, whose only subsequent changes are this gate record. The first lint attempt read stale findings from a deleted sibling scratch worktree; its missing-file warnings make that attempt invalid as source evidence. Re-run these lanes on the final head before any future PASS. |
| 3c. CI configuration | PASS | `ci_lane_run: n/a (no CI configuration in the diff)`. |
| 4. High-severity findings | PASS | No unresolved HIGH finding in the reviewed PR comment. |
| 5. Clean branch | PASS | The reviewed checkout and materialized merge worktree were clean before this gate record was written. |
| 6. Clean divergence from main | PASS | The PR head merged cleanly with `origin/main`. The current materialized merge tree built with `go build ./...` and passed `go vet ./...`. |
| 7. Single feature theme | PASS | The diff changes only `cmd/gc` scope resolution and its tests for the city HQ redirect behavior. |
