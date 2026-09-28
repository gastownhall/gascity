# Release gate: backend-independent hook route filtering

- Deploy bead: `ga-lmy6yj`
- Related implementation bead: `ga-4wwxl7`
- Review bead: `ga-rgf4nl`
- Reviewed source: `515702a982ccb11e8a14fc946c53b45e6fbb7a80`
- Source PR: [#5035](https://github.com/gastownhall/gascity/pull/5035)
- Source branch: `fix/ga-lmy6yj-hook-routed-to-filter` (provenance only)
- Planned deploy branch: `deploy/ga-lmy6yj-gate`
- Base checked at gate time: `origin/main@a85f857b3987bd18593cea2e9594a17a82b10df1`
- Gate result: **PASS with attributed pre-existing failures**

## Gate criteria

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Closed review bead `ga-rgf4nl` records `REVIEW VERDICT: PASS` for exact source `515702a982ccb11e8a14fc946c53b45e6fbb7a80`. It independently checked build, vet, the complete `cmd/gc` package, all five diff-owned tests, security, specification, and coverage, with no blocking finding. |
| 2 | Acceptance criteria met | **PASS** | The hook display path applies route visibility after work-query output, independent of the native-store/fallback backend. Assigned recovery work remains visible despite stale routes; unassigned work is kept only when unrouted or routed to an accepted identity. The shared route matcher handles slash and dot session encodings plus case differences, while deliberately leaving legacy bound-template migration to its explicit persisted-route migration. Pool-base and legacy workflow-control aliases are covered. No `bd` version change or schema-skew workaround was introduced. |
| 3 | Tests pass | **PASS with attributed failures** | The documented full-scope command, `make test-local-full-parallel`, completed 35/40 jobs PASS and 5/40 FAIL, with 0 observed test SKIP. The five failed jobs reduce to four open, pre-existing conditions attributed below; none is diff-owned. All six non-short `cmd/gc` process shards and all six integration `cmd/gc` shards passed. The exact-head GitHub `CI / required` aggregator passed, including 12/12 Linux `cmd/gc` process shards; 12/12 macOS `cmd/gc` process shards also passed. |
| 3b | Policy, build, and static checks | **PASS with attributed cache failure** | `make test-ci-policy`, changed-file formatting, `go build ./...`, and `go vet ./...` passed. The exact-head GitHub `Preflight / static checks` and `Mac / quality (lint, fmt, vet, docs)` jobs passed. Local `lint-affected` failed only by replaying diagnostics from deleted sibling worktrees, attributed below to `ga-039od0`; no diagnostic resolves to either changed path. |
| 3c | CI-config lane run | **PASS / n/a** | No workflow, matrix, timeout, required-check list, build-policy file, or CI configuration changed. |
| 4 | No unresolved HIGH review findings | **PASS** | Review bead `ga-rgf4nl` records no high-severity style, security, specification, or coverage finding. |
| 5 | Final branch clean | **PASS** | Before this record was added, detached source `HEAD` was clean and `git diff --check de50ed04441dd1549b0503e31d7e9fee7a45b48c..HEAD` passed. Cleanliness is rechecked after the gate commit. |
| 6 | Branch diverges cleanly from main | **PASS** | `git merge-tree --write-tree --messages origin/main HEAD` returned tree `9da8e675eb58e93bbdb148af97897deeca02ebc6` with no conflict messages. The reviewed source is 9 commits behind and 4 commits ahead of the checked base. Source PR #5035 remains `OPEN`, `MERGEABLE`, and exactly at the reviewed SHA. |
| 7 | Single feature theme | **PASS** | Four reviewed commits touch only `cmd/gc/cmd_hook_claim.go` and `cmd/gc/cmd_hook_visibility_test.go`. Both bead IDs belong to one hook-routing identity/visibility theme; removing either normalization coverage or the route filter would leave the same user-visible work-selection contract incomplete. |

## Source and ancestry evidence

The recorded source resolves to a commit and matches both the reviewed PR head
and the source checked by this gate:

```text
git rev-parse --verify --quiet '515702a982ccb11e8a14fc946c53b45e6fbb7a80^{commit}'
515702a982ccb11e8a14fc946c53b45e6fbb7a80
```

The reviewed range over merge base
`de50ed04441dd1549b0503e31d7e9fee7a45b48c` is:

```text
5d7266f3d2011b4842286ab14f0f7dc85c9b52d3 fix(hook): drop other agents' work from gc hook under the native-store fallback (ga-lmy6yj)
e3459b6572009b04d82d53ec6ae1aa205808d21f fix(hook): widen the routed-work identity set and exempt assigned rows (ga-lmy6yj)
ebe334c446d0efa354a5f842ec96175327899bdf fix(hook): normalize dot-encoded session identities in route filter (ga-4wwxl7)
515702a982ccb11e8a14fc946c53b45e6fbb7a80 fix(hook): stop collapsing legacy bound-template spelling in route matcher (ga-lmy6yj)
```

Net reviewed diff: 2 files, 141 insertions, 5 deletions. No `.claude/**`,
workflow, generated API, configuration, or unrelated package path is present.

## Test evidence

- `test_cmd`: `make test-local-full-parallel`
- `test_cmd_scope`: `full-suite`
- `test_counts`: 35 PASS jobs, 5 FAIL jobs, 0 observed test SKIP; 4 unique failing top-level tests/conditions
- `full_log`: `/var/tmp/ga-lmy6yj-full-1788166500/full-suite.out`
- `shard_logs`: `/var/tmp/ga-lmy6yj-full-1788166500`
- `diff_tests_executed`:
  - `TestHookRouteIdentitiesEqual`: PASS in `cmd-gc-process-2-of-6`
  - `TestHookRouteIdentitiesEqualDotAxisRegression`: PASS in `cmd-gc-process-4-of-6`
  - `TestHookCandidateVisible`: PASS in `cmd-gc-process-5-of-6`
  - `TestFilterForeignHookCandidatesPoolBaseRouteViaRoutedToIdentity`: PASS in `cmd-gc-process-1-of-6`
  - `TestFilterForeignHookCandidatesLegacyWorkflowControlAliasMatched`: PASS in `cmd-gc-process-2-of-6`
- `waiver_ref`: none
- `ci_lane_run`: n/a — no CI-config change
- `skip_justification`: local full-scope output reported no test-level skip. GitHub's skipped jobs are path-gated/non-applicable lanes; the required aggregator passed and the path-relevant Linux and macOS `cmd/gc` process lanes completed successfully.

### Full-suite failure attribution

| Failure | Tracker | Attribution |
|---|---|---|
| `TestRepositoryLedgerMatchesCensusAndDocumentation` in `unit-core` and `integration-packages-core-4-of-4` | `ga-cp3hwi.1` | Clauses 1/4: `internal/testpolicy/resourcecensus` and its ledgers are untouched. Clause 3(a), mechanism: this diff adds no fixed sleep or subprocess call, changes no census/baseline, and adds no suite target, so it cannot produce the reported bootstrap-policy count drift. Tracker predates this run; sighting appended. |
| `TestSessionEventsLive` in `integration-packages-core-3-of-4` | `ga-idsv6m` | Clauses 1/4: `internal/runtime/herdr` is untouched. Clause 3(a), mechanism: that package cannot import the `cmd/gc` main package, and the exact failure is the tracker's `getAgent evt-a: ok=false err=nil` race. Tracker predates this run; sighting appended. |
| `TestBdFlagManifestCurrent` in `integration-packages-core-1-of-4` | `ga-f0uceo` | Clauses 1/4: `internal/bdflags` is untouched. Clause 3(a), mechanism: the test compares the installed `bd` command's flags with a manifest; hook route matching cannot alter either input. Tracker predates this run; sighting appended. |
| `TestGCLiveContract_BeadsAndEvents` in `integration-rest-full-5-of-8` | `ga-esyijp` | Clauses 1/4: no integration, API, beads-init, or migration path changed. Clause 3(a), mechanism: the request failed while `bd init` rejected dirty-table schema migration, before hook routing was exercised. The condition tracker predates this run; sighting appended. |

`failure_attribution`: all four conditions have a landed mechanism proof, an
open tracker, and no changed-path overlap. The candidate adds neither declared
nor undeclared test load.

## Remote CI attribution

PR #5035 reports 74 PASS, 2 FAIL, and 22 SKIP checks at the exact reviewed
head. The required `CI / required` check is green. Both red checks are one
macOS root condition:

- `Mac / make test` and its `Mac regression summary` fail on
  `TestSocketPathFallsBackToHomeConfigWhenXDGUnset` and
  `TestSocketPathHonorsXDGConfigHomeOverHome`.
- `failure_attribution`: those tests map to open tracker `ga-wu2jmy`.
- Clauses 1/4: the failing file is `internal/runtime/herdr/client_test.go`,
  outside the two changed `cmd/gc` paths.
- Clause 3(d): the tracker records the identical failure on five independent
  `origin/main` macOS runs from 2026-08-28 through 2026-08-31.
- The gate appended this PR's exact job sighting to the tracker. All 12 macOS
  `cmd/gc` process shards and the macOS quality lane passed.

## Policy and static evidence

```text
make test-ci-policy                                                       PASS
LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main make fmt-check-changed PASS
go build ./...                                                            PASS
go vet ./...                                                              PASS
GitHub: Preflight / static checks                                         PASS
GitHub: Mac / quality (lint, fmt, vet, docs)                              PASS
LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main make lint-affected FAIL-ATTRIBUTED
```

The local lint failure emitted diagnostics under deleted, non-repository paths
such as `/var/tmp/gc-deploy-ga-d8g12r-eval` and warned that those files could
not be opened. It also replayed paths from other scratch and maintainer-review
worktrees. This is not current-tree static output.

`policy_attribution`: golangci-lint deleted-worktree diagnostic replay maps to
`ga-039od0`. Clauses 1/4 are clear because no emitted path is either changed
file. Clause 3(a) is direct: the reported files are outside this repository and
many no longer exist. The tracker was created during this discovering run under
the same-run exception after that mechanism proof landed; it is a non-routed
`gate-tracker`. The failed command was not rerun to manufacture a green result.

Policy logs are under `/var/tmp/ga-lmy6yj-policy-1788167300`.

## Release disposition

**Gate PASS.** Create `deploy/ga-lmy6yj-gate` from the reviewed source, allow
only the confirmed related ancestry ID `ga-4wwxl7`, commit this record, push the
isolated branch, open the deploy PR, publish deploy clearance on its exact head,
and route the merge request to mayor/mpr. The deployer does not merge.
