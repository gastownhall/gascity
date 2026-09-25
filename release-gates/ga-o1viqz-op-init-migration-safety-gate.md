# Release gate: ga-o1viqz — op_init migration safety

**Verdict:** **FAIL**

- Reviewed source: `92fdaf38f0d411449fa5ebbae6f5a391e994a502` (final PASS on review bead `ga-15jj5o`; the older SHA in this deploy bead's description is stale).
- Base at the full-suite start: `origin/main@543f3bc789e59355c8ba6ea24595f3c5ffb5c0ba`.
- Base after that run: `origin/main@4140118a13a91cdaccb6a56febe39e91571dce5c`.
- Bounded self-rebase: locally replayed `builder/ga-3jssfa` from `92fdaf38f0d411449fa5ebbae6f5a391e994a502` to `d9ac228ebab2e4756e9c1b16c308516ed6bde0b5`; function returned **13** because its guarded push did not complete. The fork branch was absent on readback. No deploy branch or PR was opened.

| # | Criterion | Verdict | Evidence |
|---|---|---|---|
| 1 | Review PASS present | SKIPPED | Fail-fast after criterion 6; review bead `ga-15jj5o` passed the pre-rebase source, but the local rebased head has no fresh gate. |
| 2 | Acceptance criteria met | SKIPPED | Fail-fast after criterion 6; the prior-base snapshot passed `TestGCLiveContract_BeadsAndEvents` and `TestGraphWorkflowFailureRunsCleanup`, but does not certify the rebased head. |
| 3 | Tests pass | SKIPPED | Fail-fast after criterion 6. The required full suite ran on the prior-base merge snapshot only. During the bounded rebase's guarded push, the rebased head failed `TestRepositoryLedgerMatchesCensusAndDocumentation` in the pre-push `make test-fast-parallel` hook: `TESTING.md` resource ledger stale. Log: `/var/tmp/gc-local-tests.ddQoeR/unit-core.log`. This is a direct failure of changed ledger/census content and needs builder repair and fresh review. No waiver. |
| 4 | No high-severity review findings open | SKIPPED | Fail-fast after criterion 6. |
| 5 | Final branch clean | SKIPPED | Fail-fast after criterion 6; local working tree was clean when the bounded rebase ran. |
| 6 | Branch diverges cleanly from main | **FAIL** | `git merge-tree --write-tree origin/main 92fdaf38f0d411449fa5ebbae6f5a391e994a502` reported a `TESTING.md` content conflict against base `4140118a13a91cdaccb6a56febe39e91571dce5c`. `attempt_bounded_self_rebase builder/ga-3jssfa main` replayed locally but returned `13` after its guarded push failed; protocol requires builder handoff on any nonzero/non-20 result. |
| 7 | Single feature theme | SKIPPED | Fail-fast after criterion 6; source lineage concerns one bd-init migration-safety theme. |

## Prior-base test evidence (informational; not a PASS on the final head)

`test_cmd: isolated-test-run.sh -- bash -c 'make test-local-full-parallel'`; `test_cmd_scope: full-suite`; 40 jobs completed: 39 PASS, 1 FAIL, 0 job SKIP. Top-level test lines: 53,332 PASS, 1 FAIL, 232 SKIP (platform/opt-in skips; none of the 13 diff-owned tests skipped). `TestGCLiveContract_BeadsAndEvents` and `TestGraphWorkflowFailureRunsCleanup` passed. The one failure, `TestStartDrift_SystemdManaged_RestartsToNewBuildID`, is attributed to tracker `ga-ltjdum`: untouched test, independent base-ref reproduction of the same missing systemd unit, no package overlap; fix `ga-2qe684` is stamped for that tracker and unlanded. This fix-carrying deploy qualifies for the repeat-sighting exception. Aggregate log: `/var/tmp/ga-o1viqz-full-r3.log`; shards: `/var/tmp/ga-o1viqz-full-shards-r3`.

`load_threshold=15`; `load_waited_seconds=0`; `load_wait_timed_out=0`; `load_start=10.92`; `load_max=46.46`; `load_mean=23.80`.

`go build ./...`, `go vet ./...`, `make test-ci-policy`, `LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main make lint-affected`, and `make fmt-check-changed` passed on the prior-base snapshot. No CI configuration file changed. A pinned beads topology acceptance attempt on that old snapshot was stopped after main advanced; it is not counted as final-head evidence.

**Next action:** builder reconciles the resource ledger on the rebased branch, reruns the required checks, and sends the corrected head through review. Do not open a PR from either unreviewed or unpushed head.
