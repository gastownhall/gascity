# Release gate: newest unread mail reminders (ga-ra0ou2)

**Verdict:** **FAIL**

- Deploy mode: remote; base ref: origin/main.
- Reviewed source: 40b49a83b9f95a19f3fad02fca3beab1fe2dafaa (resolved locally).
- Base inspected: da84dbfedca3134936775cbd8a9f40733286b384.
- Review: ga-kbkm8u, fresh PASS on this exact source; build: ga-vacioq.
- Preflight: commit-to-PR lookup returned no target PR; normal gate path.
- Merge-tree check exited 0, tree 4d358fe43d7f41ba0d8e71aa81ddeb8ac6668e1b.
- Ancestry scope helper passed with confirmed theme IDs ga-ra0ou2, ga-vacioq, ga-vtxkj5.

| Criterion | Result | Evidence |
|---|---|---|
| 6. Clean divergence from main | PASS | Git merge-tree against the pinned current base reports no conflicts. |
| 1. Review PASS | PASS | ga-kbkm8u records PASS, no carryover substitution. |
| 2. Acceptance criteria | PASS | Code selects highest priority then newest arrivals, keeps display/archive selection aligned, and withdraws queued mail reminders only when unread count is zero; the review records seven named regression checks PASS, including the conflict-preserved delivered-unobserved test. Independent full-suite certification remains blocked by criterion 3. |
| 3. Tests and build policy | FAIL | New untagged regression file cmd/gc/mail_inject_newest_window_test.go is absent from cmd/gc/BUILD.bazel. Pinned Gazelle emits its addition; .github/workflows/bazel-test.yml runs make bazel-sync followed by git diff --exit-code before building/testing. This candidate introduces BUILD drift. Full suite not started once this defect was established. |
| 4. No HIGH review findings | PASS | Reviewer reports no unresolved HIGH findings; one minor future store-routing consistency observation is nonblocking. |
| 5. Clean branch | PASS | Source worktree was clean; only this new gate record is committed on the isolated deploy branch. |
| 7. Single theme | PASS | Six changed files implement mail reminder freshness; no unrelated ancestry or stale prior gate record. |

## Test evidence

- policy_lane: go run github.com/bazelbuild/bazel-gazelle/cmd/gazelle@v0.53.0 -mode=diff cmd/gc — FAIL (exit 1).
- policy log: /var/tmp/gc-gate-ga-ra0ou2-gazelle.log.
- Local direct Gazelle invocation also emits external dependency label changes and a testing/synctest resolver diagnostic; those are not the attribution basis. The concrete diff-owned failure is the new regression source declaration Gazelle adds at BUILD.bazel's test srcs list.
- No Bazel binary was available locally; the entire Bazel lane was not executed or claimed.
- test_cmd: not run; test_cmd_scope: not evaluated; test_counts: 0 PASS, 0 FAIL, 0 SKIP (no tests launched).
- diff_tests_executed: none in this deploy attempt; reviewer evidence is input only.
- ci_lane_run: n/a (no CI configuration change).
- waiver_ref: none; failure_attribution: none (diff-owned BUILD drift).
- load metrics: n/a; criterion-3 test invocation was not reached.

## Handoff

Return ga-ra0ou2 to builder to regenerate and commit the Bazel BUILD/repo-tree metadata for the new regression source, preserving the reviewed mail behavior and red/green tests. Changed content then follows the normal review/deploy path. No push, PR, deploy-clearance status, or merge request from this FAIL run.
