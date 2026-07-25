# Release Gate: Named-session routed-demand wake and push-guard read retry

- Deploy bead: `ga-tg4m6s`
- Reviewed source: `8137a6d6e73513336c12d3cb9815b185ff4a1773`
- Source commits:
  - `ff2621058af04ff57a109fe52cecc2ff07564da1` — wake asleep on-demand named singletons on routed demand
  - `8137a6d6e73513336c12d3cb9815b185ff4a1773` — bound and retry push-ownership-guard bead reads
- Review bead: `ga-lstvw3`
- Base evaluated: `origin/main@c967f1eebef64fe1ad4d9d287fd778fcd796f640`
- Overall verdict: **PASS**

## Gate checklist

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 1 | Review PASS present | **PASS** | Closed review bead `ga-lstvw3` records `REVIEW VERDICT: PASS` for exact commit `8137a6d6e73513336c12d3cb9815b185ff4a1773`, independently verifies both bundled fixes, and concludes: “Both fixes: PASS. No blocking findings.” |
| 2 | Acceptance criteria met | **PASS** | The asleep named-session alias holder now suppresses a redundant standby while `NamedSessionRoutedDemand` wakes that holder from raw pre-suppression routed demand. The signal is threaded through desired-state/reconciler/awake-set plumbing and remains absent from `mergeNamedSessionDemand` and `wakeDemandOverridesSleepSuppression`, preserving the wake-only, non-pool-sizing, non-sleep-suppressing contract. The push guard adds environment-overridable `POG_READ_ATTEMPTS` (default 3) to both `bd list` and `bd show` reads, preserves fail-closed behavior, and suggests retry before `--no-verify`. |
| 3 | Tests pass | **PASS** | Exact-SHA checks passed on the first attempt: six focused routed-demand/alias/reconciler regressions; `scripts/test-push-ownership-guard.sh` (`pass=26 fail=0`), including transient recovery, exhaustion, and real ownership-change blocking; `go test ./scripts/... -count=1 -run TestPushOwnershipGuard`; shell syntax checks; `go build ./...`; `go vet ./...`; and serialized `make test-fast-parallel` with all eight jobs green. |
| 4 | No high-severity review findings open | **PASS** | `ga-lstvw3` reports no blocking findings after OWASP, test-coverage, design-contract, and retry-integrity review. Unresolved HIGH findings: 0. |
| 5 | Final branch is clean | **PASS** | `git status --porcelain=v1` was empty after all exact-SHA validation. `git diff --check origin/main...HEAD` produced no output. The configured hook path is active at `/home/jaword/projects/gascity/.githooks`; the gate commit runs the pre-commit hook. |
| 6 | Branch diverges cleanly from main | **PASS** | Evaluated first after fetching main. `git merge-tree --write-tree origin/main 8137a6d6e73513336c12d3cb9815b185ff4a1773` exited 0 and produced tree `3db85acd35f38148ef728a68dbcf178fd9f31899`; no content conflicts. The candidate is 15 commits behind / 2 ahead of current main, and no self-rebase or source-branch mutation was required. |
| 7 | Single feature theme | **PASS** | The commit set is exactly the explicitly reviewed reliability bundle: route unassigned demand to the existing named-session holder without a redundant standby, and keep the ownership guard reliable under transient Dolt read contention while delivering that change. There are no additional source-branch commits or unrelated product surfaces. |

## Acceptance evidence

### Named-session routed demand

- `TestCanonicalSingletonAliasHeldTemplates_AsleepNamedHolderStillHoldsAlias`
- `TestCanonicalSingletonAliasHeldTemplates_AsleepNamedHolderIdentityDiffersFromTemplate`
- `TestComputePoolDesiredStates_AsleepNamedHolderSuppressesRedundantStandby`
- `TestReconcileSessionBeads_OnDemandNamedSessionWakesFromPoolDemandWithoutNamedDemand`
- `TestReconcileSessionBeads_OnDemandNamedSessionWakesFromSingletonPoolDemandWithoutNamedDemand`
- `TestReconcileSessionBeads_AsleepNamedSingletonRegressionWakesInsteadOfStandby`

All six passed with `-count=1`.

### Push ownership guard

- A transient failed read recovers and permits the push.
- Persistent read failure exhausts exactly three attempts and still blocks.
- Recovery followed by a real ownership change still blocks.
- Both guarded read sites use the bounded retry helper.
- Retry guidance precedes the last-resort `--no-verify` text.

The shell suite passed `26/26`, and its Go wrapper passed.
