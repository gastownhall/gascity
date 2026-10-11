# Release gate: classify and migrate bare EvalSymlinks in the cmd/gc CLI cluster

- Deploy bead: `ga-65i89y`
- Build bead: `ga-iawy13.3`
- Review bead: `ga-xaed29`
- Reviewed commit: `294c27a69308d3bc18451aae222a279774dccbe0`
- Gate base: `origin/main` at `0223c3af63cf5cab296f9abed25bcced5eb91794`
- Evaluated: 2026-08-03
- Result: **PASS**

Criterion 6 was evaluated first, as required. The remaining criteria were then
evaluated in numeric order. `docs/PROJECT_MANIFEST.md` is absent from both the
reviewed commit and current `origin/main`; this checklist therefore applies the
deployer gate criteria and
`engdocs/contributors/release-gate-criteria-conventions.md` directly.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-xaed29` is closed with reason `pass`. Its round-2 notes record `verdict: PASS`, `review_round: 2`, and explicitly pin the reviewed commit to `294c27a69308d3bc18451aae222a279774dccbe0` — the reviewer flagged that `bd` metadata still carried round-1's stale SHA and used the git-verified branch-tip SHA instead. |
| 2 | Acceptance criteria met | **PASS** | Round-1 review found one `uncovered_criteria` gap: the `absPackRoot` (`cmd_registry.go:306`) and `repoRoot` (`cmd_registry.go:320`) normalization sites inside `buildRegistryPublishRequest` had no symlink-specific test. The round-2 diff (`bbf12c0199`..`294c27a693`, `cmd/gc/cmd_registry_test.go` `+27/-0`, test-only, no production code touched — confirmed via `git diff --stat`) adds `TestBuildRegistryPublishRequestResolvesSymlinkedPackRoot`; the reviewer independently read it against `buildRegistryPublishRequest` and confirmed it exercises both flagged sites in one scenario. All 8 `exit_contract` sites in the bead's own classification matrix are accounted for: 7 migrated to `pathutil`, 1 justified existence-only exception (`controller.go:626`, carries the `canonical-path-exception` comment as claimed). Round-1's `uncovered_criteria` finding is explicitly marked closed in the round-2 notes. |
| 3 | Tests pass | **PASS** | Required target `make test-cmd-gc-process-parallel` (`GC_FAST_UNIT=0`) was run against the reviewed commit `294c27a69308d3bc18451aae222a279774dccbe0` in isolated worktree `worktrees/ga-iawy13.3` (working tree clean, `HEAD` confirmed at the reviewed SHA). Result: all 6 shards + `productmetrics-testhook` reported `ok`/`pass`; `grep -E '^--- FAIL|^FAIL[[:space:]]'` across all 7 shard logs returned 0 matches; driver output `All cmd-gc-process jobs passed`, exit 0. This independently corroborates the reviewer's own round-2 evidence at the identical SHA (8243 tests across 6 shards `1374/1374/1374/1374/1374/1373` + 6 `productmetrics-testhook` tests, 0 failures, 0 skips). For the record: two earlier deploy-gate evaluation cycles on this same bead (see bead notes) hit exactly 3 failures at this identical, unchanged SHA — `TestBuildDesiredState_MinZeroDefaultScaleCheckRoutedWorkCreatesPoolSession`, `TestEvaluatePoolDefaultScaleCheckCountsRoutedReadyWork`, `TestEvaluatePoolDefaultScaleCheckIgnoresRoutedActiveUnassignedWork` — the same known ambient shared-Dolt-server signature root-caused at `ga-zxpfic` (closed) and previously precedented at gates `ga-pfdabs`/`ga-vn396k` against this exact 3-test signature. Two independent clean runs (the reviewer's and this gate's) and two independent failed runs all occurred at the same unchanged commit, which is itself direct evidence the failures are nondeterministic ambient-environment contention rather than anything introduced by this change. This gate's own run was unconditionally clean, so no merge-base differential was required to establish non-regression. The scoped environment fix remains tracked by open bead `ga-us7c35` (P1, unmerged). Logs: `/var/tmp/gc-ga-65i89y-gate/reviewed/*.log`. |
| 4 | No high-severity review findings open | **PASS** | Round-2 notes: `style_findings` clean (`gofmt -l` 0 files, `go vet ./...` exit 0 / 0 output); `security_findings` — no production code changed this round, round-1's OWASP walk and A01 fail-open-to-fail-closed analysis (`doctorPathWithinCity`) stands unchanged, no blockers; round-1's sole substantive finding (`uncovered_criteria`) explicitly closed. Notes conclude "No blockers remain." |
| 5 | Final branch is clean | **PASS** | `git status` in isolated worktree `worktrees/ga-iawy13.3` at `HEAD` `294c27a69308d3bc18451aae222a279774dccbe0`: "nothing to commit, working tree clean." |
| 6 | Branch diverges cleanly from main | **PASS** | After `git fetch origin main` (tip `0223c3af63cf5cab296f9abed25bcced5eb91794`), `git merge-tree --write-tree origin/main 294c27a69308d3bc18451aae222a279774dccbe0` exited 0 and produced tree `25087a7416ae5ea763c8ca08f14546c6e2928e24`; no content conflict, no self-rebase required. |
| 7 | Single feature theme | **PASS** | The 3-commit TDD sequence (red `7fed162ed4a3ff80b0cfc23f4ca79b2f6e71acf3`, green `bbf12c0199f60e8b0462dca088754f74e22a895e`, round-2 fix `294c27a69308d3bc18451aae222a279774dccbe0`) touches exactly 10 files, all under `cmd/gc/`: `cmd_import.go`, `cmd_pack_release.go`(+test), `cmd_registry.go`(+test), `cmd_supervisor_city.go`(+test), `controller.go`, `doctor_v2_checks.go`(+test) — all within the single declared theme of classifying and migrating bare `filepath.EvalSymlinks` calls to `pathutil` in the `cmd/gc` CLI cluster. |

## Gate decision

The reviewed change introduces no process-suite regression relative to its
merge-base (this run was unconditionally clean), satisfies the round-2
acceptance-criteria fix confirmed by direct reviewer read, and remains
conflict-free with current `origin/main`. It is eligible for an isolated
deploy branch and pull request.
