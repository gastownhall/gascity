# Release gate: explicit cross-rig sling refusal and remedy

- Deploy bead: `ga-pre4en`
- Build bead: `ga-pjqysm`
- Review bead: `ga-m58h98`
- Reviewed source: `daa6f558dde2a0dfcaa620a7f9ed4f7be70e2f03`
- Base evaluated: `origin/main@202c53604dbd8961f81a2d0031ff904478c5a758`
- Deploy mode: `remote`; push remote: `fork`
- Gate result: **PASS with attributed raw failures and mayor waiver**

`docs/PROJECT_MANIFEST.md` is absent from this repository and reviewed
revision. This checklist applies the seven criteria in the deployer contract,
the source bead's acceptance contract, and the repository release-gate
conventions.

## Criteria

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-m58h98` is closed with `verdict: pass`, `tests_green: true`, and `deploy_commit: daa6f558dde2a0dfcaa620a7f9ed4f7be70e2f03`. The SHA independently resolves to that exact commit. No review carryover is used. |
| 2 | Acceptance criteria met | **PASS** | `CrossRigError.Error` explicitly says the route is refused, says nothing was routed, and names three remedies: re-file in the target rig, select a city-scope target, or use `--force`. The CLI's local `checkCrossRig` delegates to `sling.CheckCrossRig`, so preview and enforcement use one comparison. All five diff-owned refusal/delegation tests passed by name. |
| 3 | Tests pass | **PASS with waiver** | The documented full local sweep completed all 40 jobs: **22 jobs PASS / 18 jobs with raw failures / 0 omitted**. Test-level totals were **41,902 PASS / 19 raw FAIL / 192 SKIP**. All five diff-owned tests ran and passed, with 0 diff-owned FAIL and 0 diff-owned SKIP. Eighteen raw failures are non-diff-owned, tracked, mechanism-attributed, and path-disjoint under criterion 3a. The remaining same-package full-load failure is covered by independently granted `waiver_ref: mayor-2026-09-10-ga-pre4en-c3`, scoped to this exact candidate and evidence. |
| 3a | Non-diff-owned failures attributed | **PASS** | Open trackers cover the installed-`bd` manifest (`ga-f0uceo`), expired provider catalog waivers (`ga-cojd80`), external herdr pane contention (`ga-iepsvr`), host tmux bindings (`ga-k3fxvj`), host-global Dolt routing (`ga-1037rg`), and legacy/dirty Dolt workspace initialization (`ga-esyijp`). Each mechanism is external to or executes before the changed cross-rig code, each test is unmodified, and no failing/mechanism path overlaps the four-file diff. The one package-overlap exception is separately waived via `ga-20zoji` plus the coverage proof below. Sightings were appended and read back from every tracker. |
| 3b | Policy/lint lane | **PASS** | `make test-ci-policy` passed. `go vet ./...` passed. `git diff --check origin/main...daa6f558...` passed. |
| 3c | CI-config lane run | **PASS / n/a** | The candidate changes no workflow, job, matrix, timeout, required-check list, Makefile, or runner policy. `ci_lane_run: n/a (no CI-config change)`. |
| 4 | No high-severity review findings open | **PASS** | Review `ga-m58h98` records no blocker, major, security finding, or unresolved HIGH issue. It specifically confirms that the shared comparison cannot allow genuinely cross-rig routes and introduces no new I/O, dependency, endpoint, or injection surface. |
| 5 | Final branch clean | **PASS** | The detached exact-candidate worktree was clean after the test artifacts were written outside it. This checklist is the deployer's only new file and will be committed on the isolated deploy branch. |
| 6 | Branch diverges cleanly from main | **PASS** | A fresh commit-to-PR lookup returned no PR. `git merge-tree --write-tree --messages origin/main daa6f558...` exited 0 against the current base and produced synthetic tree `59dc128946eaae64554206e81475fc71f206284a`, with clean auto-merges of the four diff-owned files. `assert_deploy_ancestry_scope` passed for the deploy, build, and review bead IDs, with no `.claude/**` path or unrelated commit. No self-rebase was required. |
| 7 | Single feature theme | **PASS** | The four changed files are confined to one behavior: making cross-rig sling refusal explicit, actionable, and consistent between CLI preview and enforcement. |

## Acceptance evidence

- The refusal string contains both `refusing cross-rig route` and `nothing was
  routed`.
- The remedy names re-filing in the target rig, choosing a city-scope target,
  and the existing explicit `--force` override.
- The CLI removes its duplicate case-sensitive comparison and delegates to the
  existing case-insensitive `sling.CheckCrossRig` enforcement predicate.
- The four-file diff contains 42 insertions and 14 deletions across only
  `cmd/gc/cmd_sling.go`, `cmd/gc/cmd_sling_test.go`,
  `internal/sling/sling.go`, and `internal/sling/sling_test.go`.

`diff_tests_executed`:

- `TestCheckCrossRigMessageStatesRefusalAndRemedy`: PASS
- `TestCheckCrossRigDifferentRig`: PASS
- `TestDoSlingCrossRigBlocks`: PASS
- `TestDoSlingBatchCrossRigBlocks`: PASS
- `TestDoSlingOnFormulaCrossRigBlocked`: PASS

## Test evidence

The container-backed environment was established before the independent run:

- rootless Podman 5.8.4 was reachable at
  `unix:///run/user/1000/podman/podman.sock`;
- `TESTCONTAINERS_RYUK_DISABLED=true` was set as required on this host;
- the candidate's pinned `dolthub/dolt-sql-server:1.32.4` image was cached.

```text
test_cmd: DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true GOFLAGS=-v make test-local-full-parallel
test_cmd_scope: full-suite
test_job_counts: 22 PASS / 18 raw FAIL / 0 omitted
test_counts: 41,902 PASS / 19 raw FAIL / 192 SKIP
diff_tests_executed: 5 PASS / 0 FAIL / 0 SKIP
skip_justification: all 192 skips are unchanged platform, optional-provider, permission, helper-process, or opt-in integration guards; no diff-owned test skipped
waiver_ref: mayor-2026-09-10-ga-pre4en-c3
ci_lane_run: n/a (no CI configuration change)
raw_logs: /var/tmp/ga-pre4en-artifacts.H6r4I5
shard_logs: /var/tmp/gc-local-tests.MJD8wY
```

The full command scheduled the repository's entire 40-job local CI union,
including all six non-short `cmd-gc-process` shards required for `cmd/gc/**`
and `internal/**` changes. No focused or package-subset command was substituted
for the gate run.

### Raw failures and attribution

| Raw result | Tracker | Attribution |
|---|---|---|
| `TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix` | `ga-20zoji`; waiver `mayor-2026-09-10-ga-pre4en-c3` | The test is not diff-owned but shares the changed `cmd/gc` package, so ordinary deployer attribution stops at clause 4. It passed alone in 17.425s and coverage showed 0.0% for every changed production function. The mayor independently granted the narrow exception for this exact full-load occurrence and candidate. |
| `TestBdFlagManifestCurrent` | `ga-f0uceo` | The unchanged test compares the externally installed `bd --help` surface with `internal/bdflags`. The candidate changes neither side and has no path overlap. |
| `TestCatalogMatchesProductionWiringAndDocumentation` (2 occurrences) | `ga-cojd80` | Nine `runtime.Provider` waiver entries expired on 2026-08-12. This is a date-driven catalog-policy condition outside the sling diff, with a landed mechanism and no path overlap. |
| `TestProviderLiveClaudeKindPath` | `ga-iepsvr` | The external herdr process returned `agent_pane_busy` for shared pane `w1:p1`; candidate code never executed and no runtime/herdr path changed. |
| `TestGetKeyBinding_CapturesDefaultBinding`, `TestGetKeyBinding_CapturesDefaultBindingWithArgs` | `ga-k3fxvj` | The unchanged tests observed host tmux default-binding state. The candidate changes no tmux/runtime path. |
| `TestDoltConfigWiringExternalHost` | `ga-1037rg` | The unchanged integration test reached a different project database through the known HOME/host-global Dolt server condition. The candidate changes no environment or Dolt configuration path. |
| Eleven formula, workflow, API, clean-install, live-contract, graph, and pooled-retry failures | `ga-esyijp` | Each fails during external `bd init` on the legacy/dirty-schema workspace condition, before cross-rig sling behavior can execute. The candidate changes no beads schema/bootstrap path. |

For every non-waived row: (i) the failing test is not diff-owned; (ii) the
named open tracker covers the root condition; (iii) an external,
date-deterministic, or pre-execution mechanism proves the candidate did not
cause it; and (iv) neither its test nor mechanism path overlaps the candidate.
The candidate modifies existing tests but adds no suite target, matrix entry,
or declared test-load census entry.

```text
failure_attribution: TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix -> ga-20zoji + mayor-2026-09-10-ga-pre4en-c3 | coverage proof and narrow mayor waiver
failure_attribution: TestBdFlagManifestCurrent -> ga-f0uceo | installed-bd manifest drift; mechanism proof; no path overlap
failure_attribution: TestCatalogMatchesProductionWiringAndDocumentation -> ga-cojd80 | expired provider waivers; mechanism proof; no path overlap
failure_attribution: TestProviderLiveClaudeKindPath -> ga-iepsvr | external pane contention before candidate code; mechanism proof; no path overlap
failure_attribution: default tmux binding tests -> ga-k3fxvj | host binding state; mechanism proof; no path overlap
failure_attribution: TestDoltConfigWiringExternalHost -> ga-1037rg | host-global Dolt routing; mechanism proof; no path overlap
failure_attribution: eleven legacy-workspace initialization failures -> ga-esyijp | external bd init refusal before candidate code; mechanism proof; no path overlap
inconclusive-guard: n/a — mechanisms or waiver landed; reachable_production_code not relied upon; added_test_load=no
```

### Mayor waiver scope

`mayor-2026-09-10-ga-pre4en-c3` covers only
`TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix` failing under
the preserved full-suite load for this reviewed diff. It covers no other
failure and no diff-owned test. The mayor's 18:05Z amendment allows the waiver
to carry through a clean rebase only when the stable patch ID is unchanged;
this gate required no rebase, so it remains on the original reviewed SHA.

## Policy and static evidence

- `policy_lane: make test-ci-policy — PASS`
- `static_lane: go vet ./... — PASS`
- `format_lane: git diff --check origin/main...daa6f558... — PASS`
- `pre_push_hook: make test-fast-parallel — all six cmd/gc shards and three
  auxiliary jobs PASS; unit-core raw FAIL only on ga-19onv3 and ga-cojd80`
- `ci_lane_run: n/a (no CI configuration change)`

The ordinary push invoked all ten fast-hook jobs. Unit-core reproduced two
non-diff conditions: external herdr reported `workspace_not_found` for `w1`
before candidate code could execute (predating tracker `ga-19onv3`, which
contains an `origin/main` reproduction), and the runtime-provider waiver dates
remained expired (`ga-cojd80`). All six `cmd/gc` shards passed. The hook log is
`/var/tmp/gc-local-tests.Gw7Blh`. Mayor was notified and the message was read
back as `gm-wisp-10ukb` before using the hook's documented
`git push --no-verify` bypass; the full gate and pre-commit hook were not
bypassed.

## Disposition

All seven release criteria pass under the explicit narrow waiver and complete
non-diff attribution. Cut `deploy/ga-pre4en-gate` from exact reviewed source
`daa6f558dde2a0dfcaa620a7f9ed4f7be70e2f03`, commit this checklist there,
push the isolated branch, and open the pull request. Merge authority remains
with mayor/mpr; the deployer does not merge.
