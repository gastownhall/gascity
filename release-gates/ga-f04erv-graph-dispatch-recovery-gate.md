# Release gate: pooled review worker recovery (ga-f04erv)

**Verdict:** **PASS**

Reviewed source: `223b9163532c9ecbcb0c7e11ecadd488fc5a9bd0` (`ga-ne3k0e`)
Base: `origin/main` at `6f770bf87f92df771ffa2aa9e7a8eb369d0b4df8`
Tested merge tree: `3306747d561a0767fc407f97fc9fa46cf2b50b79`
Deploy mode: remote; no PR contained the reviewed commit at preflight.

| # | Verdict | Evidence |
|---|---|---|
| 1. Review PASS | PASS | Reviewer bead `ga-jf4ptl` records PASS for the resolved reviewed commit and no blocking findings. |
| 2. Acceptance criteria | PASS | The final diff changes only `test/agents/graph-dispatch.sh`: recovery queries the recorded claim identity (`BEADS_ACTOR`), and the startup trace includes that identity. The forced restart recovery test passed in the full suite. `bash -n` passed; `shellcheck -S warning` reported only pre-existing SC2034 on `claimed_here`, also present on base. PR label and backport note are checked separately when the PR opens. |
| 3. Tests | PASS | The documented full local suite ran on the clean materialized merge tree through `isolated-test-run.sh`: 40 jobs started, 38 passed, 2 failed on the attributed conditions below, 0 reported SKIP. The recovery shard passed (`TestRetryManagedPooledWorkerRecoversClaimedAttemptAfterCrash`, 266.940s). Build and vet passed. |
| 3a. Pre-existing failures | PASS | Both failed tests are outside the one-file shell diff and have open trackers that predate this run. Mechanism proof, tracker sightings, and the fix-carrying repeat exception are recorded below. |
| 3b. Policy and lint | PASS | `make test-ci-policy`, `go vet ./...`, and `LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main make lint-affected fmt-check-changed` passed. The changed-file checks found no Go build inputs or formatting targets. |
| 3c. CI configuration | PASS | No CI job, matrix, timeout, or required-check configuration changed; `ci_lane_run: n/a`. |
| 4. High review findings | PASS | Reviewer recorded zero unresolved high-severity findings. |
| 5. Clean branch | PASS | The tested merge worktree and the role worktree were clean before this gate record. The isolated deploy branch is checked clean after this gate record is committed. |
| 6. Clean divergence | PASS | `materialize_merge_tree` merged the reviewed SHA with the current `origin/main` without conflict. Remote `main` was rechecked at the same base SHA after the suite. |
| 7. Single feature theme | PASS | One change to the review-workflow test agent's claimed-work recovery, with a supporting trace field. No unrelated theme or internal documentation path is in the final merge-tree diff. |

## Test evidence

- `test_cmd`: `isolated-test-run.sh -- bash -c 'make test-local-full-parallel'`
- `test_cmd_scope`: `full-suite` (the documented 40-job local runner)
- `test_counts`: 38 PASS jobs, 2 attributed FAIL jobs, 0 reported SKIP jobs; actual runner exit 2
- `diff_tests_executed`: none (no test files added or modified; the only changed file is the shell test agent)
- `skip_justification`: none required
- `waiver_ref`: none
- `policy_lane`: `make test-ci-policy` PASS
- `ci_lane_run`: n/a (no CI configuration diff)
- Full log: `/var/tmp/ga-f04erv-full.log`; job logs: `/var/tmp/ga-f04erv-shards/`

`failure_attribution: TestLegacyCombinedSourceRecoversHotRollbackJournalInPrivateSnapshot -> ga-22dskp | clause 3(a) MECHANISM — the final diff is only test/agents/graph-dispatch.sh, which the internal/storebinding/sqlite Go package test cannot execute.` The test file is untouched (clause 1), the opened tracker predates this run and names this exact test (clause 2), and no path overlaps (clause 4). The failure was a missing crash-produced rollback journal at `legacy_hot_journal_linux_test.go:30`; the sighting was added to the tracker. Existing fix bead `ga-5skods` covers this fixture condition, now carries `gc.fixes_tracker=ga-22dskp`, and remains open and unlanded. Its builder admission was deferred by the daily build budget; mayor was notified. This deploy is fix-carrying via build bead `ga-ne3k0e` (`gc.fixes_tracker=ga-vkhfnj`), so the tracked-repeat exception applies.

`failure_attribution: TestGCLiveContract_BeadsAndEvents -> ga-lejnse | clause 3(a) MECHANISM — the final shell-agent diff cannot execute in the REST contract's temporary-city rig initialization path.` The test file is untouched (clause 1), the opened tracker predates this run and names this test and shared-server schema refusal (clause 2), and no path overlaps (clause 4). The test city hit a refusal to auto-apply six pending Dolt schema migrations (`v60 -> v66`); the sighting was added to the tracker. The condition's stamped fix beads `ga-3jssfa` and `ga-m1qxc8` are unlanded. The same fix-carrying repeat exception applies. No shared-server migration was attempted.

The PR must carry `needs-review-formulas` so its CI recovery matrix runs, and its body must call out the `release/v1.5.0` backport needed alongside #6591. The merge authority verifies CI before merge.
