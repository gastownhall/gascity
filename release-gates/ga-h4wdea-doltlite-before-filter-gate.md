# Release gate: DoltLite before-filter timestamp handling

- Deploy bead: `ga-h4wdea`
- Review bead: `ga-pxi49s`
- Pull request: `https://github.com/gastownhall/gascity/pull/5357`
- Reviewed commit: `c569ec80a827e336994883d90c281d2226ed1a32`
- Base evaluated: `origin/main@9700d9a48fb35a2063e1fcb9ee49ab664df26de0`
- Deploy mode: `remote`

## Gate checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Review bead `ga-pxi49s` is closed with `verdict: pass` for the exact reviewed commit. No review carryover was used. |
| 2 | Acceptance criteria met | PASS | The SQL predicate now admits rows whose stored timestamp cannot be parsed by `julianday`, preserving the Go-side before-time filter as authoritative. SQL `LIMIT` is disabled while a before-filter is active so the Go-side filter, sort, and limit operate on the complete candidate set. The two diff-owned regression tests below exercise both behaviors. |
| 3 | Tests pass | PASS | The documented full local CI aggregate ran through the isolation wrapper. It produced tracked, non-diff-owned failures described below; all satisfy criterion 3a attribution. The separate required native DoltLite lane passed every test, including both diff-owned tests. |
| 3b | Policy/lint lane | PASS | `make test-ci-policy` passed all runner-policy, CI-suite-coverage, `scripts/cipolicy`, and static-scope checks. |
| 3c | CI-config lane | PASS | Not applicable: the diff changes no workflow, matrix, timeout, or required-check configuration. |
| 4 | No high-severity review findings open | PASS | Review notes record no style, security, or specification findings; unresolved HIGH findings: 0. |
| 5 | Final branch is clean | PASS | `git status --short` was empty after gate evaluation. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main origin/deploy/ga-h4wdea-gate` exited 0 against the base above. The PR reports `MERGEABLE`. The bounded self-rebase exception was not invoked because criterion 6 did not fail. |
| 7 | Single feature theme | PASS | The two-file commit set changes one subsystem and one behavior: DoltLite read-store before-time filtering and its regression coverage. |

## Criterion 3 evidence

### Full CI-equivalent aggregate

- `test_cmd`: `make test-local-full-parallel`
- `test_cmd_scope`: `full-suite`
- isolation: `isolated-test-run.sh -- make test-local-full-parallel`
- container environment: rootless Podman socket at `unix:///run/user/1000/podman/podman.sock`; `TESTCONTAINERS_RYUK_DISABLED=true`
- result: 40 jobs started; 33 job PASS, 7 job FAIL
- verbose markers: 79,147 PASS, 24 FAIL, 289 SKIP
- FAIL marker accounting: seven top-level failure occurrences plus 17 nested `TestBdFlagManifestCurrent` subtests; six distinct failing test names
- `skip_justification`: all 289 markers are declared suite exclusions for platform-only, opt-in/live-service, helper-process, build-tag, or fast-unit/process-lane handoff coverage. Process-backed coverage ran in the same full aggregate. No diff-owned test skipped.
- logs: `/var/tmp/ga-h4wdea-full-gate-20260915/*.log`

### Required native DoltLite lane

- `test_cmd`: `make test-native-doltlite-beads`
- status: PASS
- counts: 76 PASS, 0 FAIL, 0 SKIP
- `diff_tests_executed`:
  - `TestDoltliteReadStoreBeforeFiltersToleratesRawTimeBind`: PASS
  - `TestDoltliteReadStoreBeforeFilterLimitDoesNotPreemptGoSideFilter`: PASS
- `waiver_ref`: none; no diff-owned test failed or skipped
- log: `/var/tmp/ga-h4wdea-native-doltlite.log`

### Failure attribution

Every candidate failure is outside the two changed paths, and each tracker predates this run. All attributions use landed cross-PR recurrence or a directly isolated host/tool mechanism; no inconclusive attribution path was used.

| Failure | Tracker | Proof and path-overlap result |
|---|---|---|
| `TestCatalogMatchesProductionWiringAndDocumentation` (unit and integration core) | `ga-1s16pf` | Expired runtime-seam waiver owned by `ga-80po0c.3`, repeatedly reproduced across unrelated PRs. No overlap with `internal/testutil/providerledger` or runtime constructor wiring. |
| `TestBdFlagManifestCurrent` | `ga-f0uceo` | Installed `bd` exposes flags absent from the checked-in manifest, repeatedly reproduced across unrelated PRs. The candidate cannot change `internal/bdflags` or the installed binary. |
| `TestGetKeyBinding_CapturesDefaultBinding` and `TestGetKeyBinding_CapturesDefaultBindingWithArgs` | `ga-k3fxvj` | Host tmux 3.7b returns empty filtered default bindings, repeatedly reproduced across unrelated PRs. No overlap with `internal/runtime/tmux`. |
| `TestCleanInstallTutorialPath` | `ga-hrdd3h` | External `bd` circuit-breaker cleanup diagnostics contaminated the expected stdout, a repeatedly observed tool-output condition. No overlap with tutorial capture or circuit-breaker logging. |
| `TestHumaBinary_CityCreateAsync` | `ga-esyijp`; standing authorization `ga-6bnc42`, occurrence log `ga-lpfjhc` | Fixture initialization hit the beads#4566 `pending schema migrations alter pre-existing dirty tables: comments, issues` signature. The candidate only changes DoltLite `List` cutoff predicates and post-query limiting; it does not change schema migration, initialization, or store bootstrap. |

- `failure_attribution`: mappings above, with sightings appended to every tracker during this gate
- clause (i), not diff-owned: satisfied for every failure
- clause (ii), tracked before this run: satisfied for every failure
- clause (iii), independent proof: cross-PR recurrence and named host/tool mechanisms above
- clause (iv), no path overlap: satisfied for every failure
- `inconclusive-guard`: not used; every attribution has landed independent proof

## Decision

PASS. The raw full-suite failures remain recorded and attributed; the change's required native lane and both diff-owned tests pass without skips or waivers.
