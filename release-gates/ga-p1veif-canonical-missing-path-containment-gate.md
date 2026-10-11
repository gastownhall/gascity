# Release gate: canonical missing-path containment

- Deploy bead: `ga-p1veif`
- Build bead: `ga-iawy13.1`
- Review bead: `ga-bcaxo1`
- Originally reviewed source: `6220b5be65c7d1a26a81cd465398db8b06b3e6b6`
- Reviewed source at this gate: `4641a732c172594cceaaf8e3ba781edd94ecb209`
- Stable patch ID: `093eb4658691a438e8489192efbc7062c1d556ff`
- Base evaluated: `origin/main@202c53604dbd8961f81a2d0031ff904478c5a758`
- Deploy mode: `remote`; push remote: `fork`
- Gate result: **PASS with attributed raw test failures**

`docs/PROJECT_MANIFEST.md` is absent from this revision. This checklist applies
the seven criteria in the deployer contract, the implementation bead's exit
contract, and `engdocs/contributors/release-gate-criteria-conventions.md`.

## Criteria

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-bcaxo1` is closed with `verdict: pass`, `tests_green: true`, and no security, style, or specification blocker. The builder rebased the reviewed two-commit change from `6220b5be...` to `4641a732...`; independent deployer recomputation produced the same stable patch ID (`093eb465...`) for both diffs. `review_carryover_verified` is recorded on the deploy bead, so the latter is the authoritative deploy source. |
| 2 | Acceptance criteria met | **PASS** | `internal/pathutil.ResolveNearestExistingAncestor` is exported and documented, remains stdlib-only, resolves existing paths and missing leaves through the nearest existing ancestor, follows symlinked ancestors, and returns non-ENOENT resolution errors. `NormalizePathForCompare` consumes it, while `cmd/gc` delegates `realPathForContainment` to the same helper. The lexical and symlink-aware rig containment passes remain intact. All four new helper tests and the pre-existing symlink-containment acceptance tests passed in the full suite. |
| 3 | Tests pass | **PASS with attributed raw failures** | The documented full local CI union scheduled all 40 jobs: **24 jobs PASS / 16 jobs with raw failures / 0 omitted**. Its result lines contain **41,174 PASS / 21 raw FAIL / 184 SKIP** top-level test events. All four diff-owned tests passed in both unit and integration-package lanes, with 0 diff-owned FAIL and 0 diff-owned SKIP. Every raw failure maps to one of six non-diff-owned root conditions satisfying criterion 3a below. The required policy lane and vet passed. |
| 3a | Non-diff-owned failures attributed | **PASS** | Six verified open trackers cover all 21 raw failure occurrences: installed-`bd` manifest drift (`ga-f0uceo`), expired runtime-provider catalog waivers (`ga-cojd80`), external `herdr` rejecting `--no-focus` (`ga-fm5mmm`), host tmux default-binding drift (`ga-k3fxvj`), integration HOME/host-global Dolt routing (`ga-1037rg`), and legacy Dolt server workspaces failing during `bd init` (`ga-esyijp`). Each mechanism executes outside or before the candidate's path helper, each failing test is unmodified, and none of the failing/mechanism paths overlaps the three-file diff. Sightings were appended and read back from every tracker. |
| 3b | Policy/lint lane | **PASS** | `make test-ci-policy` passed. `go vet ./...` passed. `git diff --check origin/main...4641a732...` passed. |
| 3c | CI-config lane run | **PASS / n/a** | The candidate changes no workflow, CI job, matrix, timeout, required-check list, Makefile, or runner policy. `ci_lane_run: n/a (no CI-config change)`. |
| 4 | No high-severity review findings open | **PASS** | Review `ga-bcaxo1` records no unresolved HIGH finding. Its security analysis confirms that the refactor preserves the symlink-aware defense-in-depth boundary and changes no authentication, injection, dependency, or wire surface. |
| 5 | Final branch clean | **PASS** | The exact-candidate scratch worktree remained clean after the full suite, policy lane, vet, and diff check. The gate record is the deployer's only new file and will be committed on the isolated deploy branch. |
| 6 | Branch diverges cleanly from main | **PASS** | After the final `git fetch origin main`, `git merge-tree --write-tree --messages origin/main 4641a732...` exited 0 and produced synthetic tree `41aea50e854b0bcd80b8016dea038ec131ff4e2c`, with only a clean auto-merge of `cmd/gc/api_state.go`. `assert_deploy_ancestry_scope` passed for the deploy, build, and review bead IDs, with no `.claude/**` path or unrelated commit. No bounded self-rebase was required. |
| 7 | Single feature theme | **PASS** | Two TDD commits change three files in one subsystem theme: centralizing missing-path/symlink resolution in leaf package `internal/pathutil` and making the rig-containment adapter consume it. There is no independent feature or unrelated ancestry. |

## Acceptance evidence

- `internal/pathutil/pathutil.go` exports the canonical helper with a doc
  comment and only standard-library imports.
- `cmd/gc/api_state.go` removes its duplicate ancestor walk and delegates to
  the helper without removing either containment check.
- The four required diff-owned tests passed by exact name:
  - `TestResolveNearestExistingAncestorExistingPath`
  - `TestResolveNearestExistingAncestorMissingLeaf`
  - `TestResolveNearestExistingAncestorSymlinkedAncestor`
  - `TestResolveNearestExistingAncestorErrorsOnSymlinkLoop`
- The existing acceptance tests for paths below a symlinked city and paths
  that genuinely escape the city also passed in the full process-backed lane.
- `git diff --stat origin/main...4641a732...` reports 100 insertions and 30
  deletions across only `cmd/gc/api_state.go`, `internal/pathutil/pathutil.go`,
  and `internal/pathutil/pathutil_test.go`.

## Test evidence

The container-backed environment was established before the run:

- rootless Podman 5.8.4 was reachable at
  `unix:///run/user/1000/podman/podman.sock`;
- `TESTCONTAINERS_RYUK_DISABLED=true` was set, as required on this host;
- the cached Dolt SQL Server 1.32.4 image was present;
- the candidate does not introduce or modify a pinned container-image tag.

```text
test_cmd: DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true GOFLAGS=-v make test-local-full-parallel
test_cmd_scope: full-suite
test_counts: 24 jobs PASS, 16 jobs raw FAIL, 0 omitted; 41,174 test PASS, 21 raw FAIL, 184 SKIP
diff_tests_executed: 4 unique diff-owned tests PASS in both unit and integration-package lanes; 0 FAIL; 0 SKIP
skip_justification: all 184 skips are unchanged platform, permission, optional-provider, live-infrastructure, helper-process, or opt-in integration guards; no diff-owned test skipped
waiver_ref: none
ci_lane_run: n/a (no CI configuration change)
full_log: /var/tmp/ga-p1veif-artifacts.R2dPtR/full-suite.log
shard_logs: /var/tmp/gc-local-tests.jUsJTK
policy_log: /var/tmp/ga-p1veif-artifacts.R2dPtR/policy.log
vet_log: /var/tmp/ga-p1veif-artifacts.R2dPtR/vet.log
```

The 40-job union includes all six `cmd-gc-process` shards with
`GC_FAST_UNIT=0`, the product-metrics hook, unit/core packages, all integration
package shards, and the full/rest integration shards. It therefore exercises
the CI-required process lane for both `cmd/gc/**` and `internal/**`; a focused
or package-subset command was not substituted.

### Raw failure attribution

| Raw results | Tracker | Four-clause evidence |
|---|---|---|
| `TestBdFlagManifestCurrent` | `ga-f0uceo` | The unchanged test compares an externally installed `bd --help` surface with `internal/bdflags`. This candidate changes neither side and has no path overlap. The predating open gate tracker records the exact condition. |
| `TestCatalogMatchesProductionWiringAndDocumentation` (2 occurrences) | `ga-cojd80` | The unchanged catalog test reports nine expired `runtime.Provider` waiver entries. That date-driven policy condition is outside the path helper and `cmd/gc` adapter diff. An open root-condition tracker was created and verified during this discovering run under the landed-mechanism exception. |
| `TestHerdrConformance` (2 occurrences), `TestProviderLive` (2 occurrences) | `ga-fm5mmm` | The external installed `herdr` binary rejects `--no-focus` before candidate code executes. The candidate changes no runtime/provider or herdr path. An open root-condition tracker was created and verified during this discovering run under the landed-mechanism exception. |
| `TestGetKeyBindingDefault`, `TestGetKeyBindingWithArgs` | `ga-k3fxvj` | The unchanged tests observed the host's empty/default tmux binding behavior. The candidate changes no tmux/runtime code and has no path overlap; the open tracker predates this run. |
| `TestDoltConfigWiringExternalHost` | `ga-1037rg` | The unchanged integration test reached a different project database through the known HOME/host-global Dolt server condition. The candidate changes no test environment, Dolt configuration, or routing path; the open tracker predates this run. |
| Eleven integration failures spanning adopt-PR compile/retry/soft-fail, clean-install tutorial, GC live, graph success/failure, Huma city/session endpoints, personal-work formula, and pooled retry | `ga-esyijp` | Each failure occurs during external `bd init` with `legacy Dolt server workspace detected`, before the changed helper or containment adapter can execute. The candidate changes no beads schema/bootstrap path. The open gate tracker predates this run. |

For every row: (i) the failing test is not diff-owned; (ii) the named tracker
is open and covers that root condition; (iii) the external/pre-execution
mechanism proves the candidate did not cause it; and (iv) neither the failing
test nor its root-condition path overlaps the candidate diff. The candidate
adds four ordinary unit tests but no new suite target, matrix entry, or declared
test-load census entry.

```text
failure_attribution: TestBdFlagManifestCurrent -> ga-f0uceo | installed-bd manifest drift; mechanism proof; no path overlap
failure_attribution: TestCatalogMatchesProductionWiringAndDocumentation -> ga-cojd80 | expired provider waivers; mechanism proof; no path overlap
failure_attribution: TestHerdrConformance, TestProviderLive -> ga-fm5mmm | external herdr rejects option before candidate code; mechanism proof; no path overlap
failure_attribution: TestGetKeyBindingDefault, TestGetKeyBindingWithArgs -> ga-k3fxvj | host tmux default binding drift; mechanism proof; no path overlap
failure_attribution: TestDoltConfigWiringExternalHost -> ga-1037rg | host-global Dolt routing; mechanism proof; no path overlap
failure_attribution: eleven legacy-workspace integration failures -> ga-esyijp | external bd init refuses legacy workspace before candidate code; mechanism proof; no path overlap
inconclusive-guard: n/a — mechanism proofs landed; reachable_production_code not relied upon; added_test_load=no
```

## Policy and static evidence

- `policy_lane: make test-ci-policy — PASS`
- `static_lane: go vet ./... — PASS`
- `format_lane: git diff --check origin/main...4641a732... — PASS`
- `pre_push_hook: make test-fast-parallel — all six cmd/gc shards and three
  auxiliary jobs PASS; unit-core raw FAIL only on ga-fm5mmm and ga-cojd80`
- `ci_lane_run: n/a (no CI configuration change)`

The ordinary push invoked the repository's pre-push hook and ran all ten fast
jobs. Its only red job reproduced the same two non-diff root conditions already
captured by the independent 40-job gate: the external `herdr` binary rejects
`--no-focus` before candidate code, and date-expired runtime-provider catalog
waivers fail policy. The hook log is `/var/tmp/gc-local-tests.IoevU7`. Mayor
was notified and the message was read back as `gm-wisp-6zqlh` before using the
hook's documented `git push --no-verify` bypass; the full gate and pre-commit
hook were not bypassed.

## Pre-flight and disposition

GitHub's commit-to-PR lookup returned no pull request for the reviewed source,
and the reviewed source is not already on `origin/main`. The final base refresh
did not change `origin/main`, and the synthetic merge remains conflict-free.

All seven release criteria pass. Cut `deploy/ga-p1veif-gate` from exact source
`4641a732c172594cceaaf8e3ba781edd94ecb209`, commit this checklist there, push
the isolated branch, and open the pull request. Merge authority remains with
mayor/mpr; the deployer does not merge.
