# Release gate: deterministic tmux startup-sweep test

- Deploy bead: `ga-y9dxsi`
- Review bead: `ga-6gwo3o`
- Build bead: `ga-gc305u`
- Reviewed commit: `894a599a36983fd30bd9b091e4f4276cf8bc80d3`
- Gate date: 2026-09-30
**Verdict:** **PASS**

`docs/PROJECT_MANIFEST.md` is absent. This gate uses the deployer release criteria, `TESTING.md`, and `engdocs/contributors/release-gate-criteria-conventions.md`.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Reviewer `ga-6gwo3o` passed exactly `894a599a36983fd30bd9b091e4f4276cf8bc80d3`; deploy bead records that SHA. No review carryover. |
| 2 | Acceptance criteria met | PASS | The Linux-only diff makes the fixture roots private to the boundary test, invokes a sibling startup sweep after creating them, and checks that the test's own sweep reaps the gone-root server, keeps the live-root server, and reports only the reaped server. `TestSweepStaleTmuxTestServers_ReapsRootGoneKeepsRootPresent` and `TestIsTmuxTestSocketRoot` passed in the full run. The production ownership rule is extracted unchanged; no production file changed. |
| 3 | Tests pass | PASS (two attributed unrelated failures) | The complete 40-job local CI-equivalent suite ran to completion: 38 jobs OK, 2 jobs failed, with 54,352 PASS, 2 FAIL and 236 SKIP test-result lines, including subtests. Both diff-owned tests ran and passed; none skipped or failed. The two failures meet all four pre-existing-failure attribution clauses and the fix-carrying repeat exception, documented below. The required policy/lint lane passed. No CI configuration was changed; heavy mode was `none`. |
| 4 | No high-severity review findings open | PASS | Independent review reports zero unresolved HIGH findings. The one-line test glue coverage observation is informational. |
| 5 | Final branch is clean | PASS | The role worktree and both disposable test worktrees were clean before creating this gate record. This record is committed as the isolated deploy branch tip. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main 894a599a36983fd30bd9b091e4f4276cf8bc80d3` exited 0: tree `630224c0ea6b2ce9c73807e4d0c0dd475eb538d8` at initial base `dfe4789f74afcaa0173b6d5b9756b7cbb6341fe2`; rechecked at latest base `13e4e752c02123e675a95deeeaf78329ed786410`, tree `9d9dbd596be2052236188b44af3f9a1ee5054ef4`. The source commit is not on main and has no associated PR. |
| 7 | Single feature theme | PASS | The reviewed diff changes only `cmd/gc/tmux_leak_guard_test.go` and `cmd/gc/tmux_leak_guard_check_test.go`, both Linux test files, for one tmux fixture race. |

## Criterion 3 evidence

- `test_cmd`: `make test-local-full-parallel LOCAL_TEST_JOBS=4`, through `load-gate-run.sh --threshold 15 --max-wait 1800` and `isolated-test-run.sh`. The isolation wrapper reported `PASS-THROUGH` for this disposable gascity checkout; the checkout was clean, and the run pinned `bd` v1.3.1-rc.2, Dolt 2.1.7, and the rootless Podman socket. The merged test commit was `3a036287dba63b43f6ad8ca8ad957262fcf5bbcb`, tree `630224c0ea6b2ce9c73807e4d0c0dd475eb538d8` (initial base plus the reviewed diff). It ran all 40 documented jobs, including all six `cmd-gc-process` shards and `TestTutorial01`; this covers the `cmd_gc_process` required CI path for `cmd/gc/**`. The full integration shards and unit jobs also ran. Full output: `/var/tmp/deploy-ga-y9dxsi.cxIgUN/full-suite.log` and `/var/tmp/deploy-ga-y9dxsi.cxIgUN/shards/`.
- `test_cmd_scope: full-suite`; `diff_tests_executed: TestIsTmuxTestSocketRoot PASS (8/8 subtests PASS), TestSweepStaleTmuxTestServers_ReapsRootGoneKeepsRootPresent PASS`; `waiver_ref: none`; `heavy_mode: none`.
- `test_counts`: 54,352 PASS, 2 FAIL, 236 SKIP lines (including subtests); 38/40 jobs OK. The skips are outside the two diff-owned tests. Examples are Darwin-only checks on Linux, optional live MCP/SSH integration, unavailable external tooling, and test-specific environment exclusions. The two failing jobs completed and emitted their full logs; the raw suite exit was 2.
- `failure_attribution: TestCmdGCRealBDTestsUseTestOwnedDoltContext -> ga-aik16g`, fix bead `ga-zq8iwb` unlanded (work outcome `blocked`, PR #6821 open). The sole failure was `testing.TempDir` cleanup racing an `eventsData/eventkit.lock` creation. Clause (i): failing `cmd/gc/pool_test.go` is untouched and is not one of the diff-owned tests. Clause (ii): open condition tracker `ga-aik16g` predates this run; this sighting was appended and read back. Clause (iii), mechanism proof: the diff changes only tmux test fixtures and cannot invoke the pool real-bd eventkit cleanup path. Clause (iv): no failing test-file path overlaps the two changed files. No new test target or resource census entry was added.
- `failure_attribution: TestCompactHistoryRealDoltAdoptedNoRemoteSquashesOnlyNewCommits -> ga-vkhfnj`, fix bead `ga-db2c94` open and unlanded (`gc.fixes_tracker=ga-vkhfnj`). The external `dolt commit -Am team commit 9` subprocess was killed at 54.41 seconds under suite load. Clause (i): the failing test in `examples/bd/dolt/compact_history_protection_real_dolt_test.go` is untouched. Clause (ii): open condition tracker `ga-vkhfnj` predates this run; this sighting was appended and read back. Clause (iii), mechanism proof: the two changed `cmd/gc` tmux test files cannot reach the examples/bd/dolt test helper or its external Dolt subprocess. Clause (iv): no path overlap. No new test target or resource census entry was added.
- `repeat_exception`: the current deploy's build bead `ga-gc305u` is stamped `gc.fixes_tracker=ga-ok9ick`. Both other conditions have specific unlanded fix beads, `ga-zq8iwb` and `ga-db2c94`; their tracker beads remain open. The two attributed failures therefore do not require a repeat-condition hold. No gascity waiver was used.
- `policy_lane: make test-ci-policy PASS`; merged-tree `go build ./...`, `go vet ./...`, pinned `golangci-lint` v2.12.0 (0 issues), format, native dependency, generated/spec and Bazel sync checks PASS. `make check-hooks` confirmed `.githooks`. Evidence: `/var/tmp/deploy-ga-y9dxsi.cxIgUN/quality.log`, `QUALITY_RESULT rc=0` at 2026-09-30T04:59:23Z.
- `load_threshold: 15`; `load_waited_seconds: 1770`; `load_wait_timed_out: 0`; `load_start: 17.83`; `load_max: 33.99`; `load_mean: 22.10`; 125 samples, 0 read errors. The suite ended 2026-09-30T05:53:00Z.

This gate records a PASS with two attributed failures. It does not claim a raw green full-suite exit.
