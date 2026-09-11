# Release Gate: Named-session routed-demand wake and push-guard read retry

- Deploy bead: `ga-tg4m6s`
- Reviewed source: `8137a6d6e73513336c12d3cb9815b185ff4a1773`
- Source commits:
  - `ff2621058af04ff57a109fe52cecc2ff07564da1` — wake asleep on-demand named singletons on routed demand
  - `8137a6d6e73513336c12d3cb9815b185ff4a1773` — bound and retry push-ownership-guard bead reads
- Review bead: `ga-lstvw3`
- Base evaluated: `origin/main@c967f1eebef64fe1ad4d9d287fd778fcd796f640`
- Overall verdict: **PASS**

### Maintainer fixups after the reviewed SHA

The gate below was evaluated at `8137a6d6e`. Maintainer review of the PR
surfaced integration gaps that were fixed on the branch afterward, so the
checklist evidence no longer describes the branch head verbatim — the
corrections are called out inline in criteria 2 and 3:

- `37a364c9d` — classify routed demand as work (`awakeSetToWakeEvals`) and keep
  its wake through non-interactive sleep suppression.
- This commit — gate `NamedSessionRoutedDemand` to canonical singleton backing
  pools, plus this gate refresh.

These are maintainer-side integration fixes to the same feature, not new
surfaces. They are **not** covered by the `ga-lstvw3` review verdict, which
closed against `8137a6d6e`.

## Gate checklist

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 1 | Review PASS present | **PASS** | Closed review bead `ga-lstvw3` records `REVIEW VERDICT: PASS` for exact commit `8137a6d6e73513336c12d3cb9815b185ff4a1773`, independently verifies both bundled fixes, and concludes: “Both fixes: PASS. No blocking findings.” |
| 2 | Acceptance criteria met | **PASS** | The asleep named-session alias holder now suppresses a redundant standby while `NamedSessionRoutedDemand` wakes that holder from raw pre-suppression routed demand. The signal is threaded through desired-state/reconciler/awake-set plumbing and remains absent from `mergeNamedSessionDemand`, preserving the wake-only, non-pool-sizing contract. **Corrected after `37a364c9d`:** the original wording also claimed the signal stays absent from `wakeDemandOverridesSleepSuppression`. It is now deliberately present there. Alias suppression zeroes the standby's `poolDesired`, so the pool count cannot carry the signal at that site and the holder would stay asleep under a configured non-interactive sleep policy — the exact wake this feature exists to perform. Explicit sleep intent still wins, so the non-sleep-suppressing intent is preserved for operator-requested sleep. **Scoped after this commit:** the signal is emitted only for canonical singleton backing pools, since a multi-instance pool serves routed demand with an ordinary standby and would otherwise both wake the holder and mint one. The push guard adds environment-overridable `POG_READ_ATTEMPTS` (default 3) to both `bd list` and `bd show` reads, preserves fail-closed behavior, and suggests retry before `--no-verify`. |
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

#### Added by the maintainer fixups

- `TestAwakeSetToWakeEvalsMapsRoutedDemandToWakeWork` — `"routed-demand"` maps to
  `WakeWork`, not the `WakeConfig` default fallthrough.
- `TestReconcilerWakeDemandOverridesSleepSuppressionForRoutedDemand` — the holder
  wakes under a non-interactive sleep policy when alias suppression has zeroed
  `poolDesired`, and explicit sleep intent still overrides.
- `TestBuildDesiredState_RoutedDemandWakesOnlyCanonicalSingletonNamedSessions` —
  a multi-instance backing pool does not emit the wake signal, while routed
  demand still reaches ordinary pool sizing; the singleton control still does.

Each was confirmed to **fail** with its production change reverted and the test
left in place, so all three pin real behavior rather than passing vacuously.

### Push ownership guard

- A transient failed read recovers and permits the push.
- Persistent read failure exhausts exactly three attempts and still blocks.
- Recovery followed by a real ownership change still blocks.
- Both guarded read sites use the bounded retry helper.
- Retry guidance precedes the last-resort `--no-verify` text.

The shell suite passed `26/26`, and its Go wrapper passed.
