# Release gate: city-scope pool orphan-sweep self-probe (`ga-wlwk8p`)

Gate result: **PASS with attributed ambient failures**

- Evaluated: 2026-09-12 PDT
- Deploy mode: `remote`
- Base: `origin/main@003b78721dcf30e2354cb5ee0ae937b7f5efe9bf`
- Merge base: `391ad1eee6468b2fe1210b51020484d193ce3723`
- Reviewed deploy source: `9f4eb6fffe6cd62825bd7efd62cb26f0aafcef68`
- Source branch: `fix/ga-dei7xx-orphan-sweep-city-pool-selfprobe` (provenance only)
- Deploy branch: `deploy/ga-wlwk8p-gate`
- Source PR pre-flight: [#6320](https://github.com/gastownhall/gascity/pull/6320) is OPEN at the exact reviewed head SHA

## Checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Reviewer PASS present | **PASS** | The recovered deploy handoff records reviewed + PASSED at exact head `9f4eb6fffe6cd62825bd7efd62cb26f0aafcef68`. PR #6320's MPR synthesis records Qwen, Claude, and Codex all `ok`, verdict `auto-merge`, with no blocking finding. |
| 2 | Acceptance criteria met | **PASS** | The reviewed change reconstructs configured pool-seat session names using both structural separators, preserves city-scope `__` seats when a self-probe cannot resolve, and does not broadly exempt dead ephemeral `__` sessions. The three focused name-shape cases pass, and the three synchronized resource-census sources agree. |
| 3 | Tests pass | **PASS with attribution** | `make test-fast-parallel` completed 10/10 jobs PASS. The documented full local union completed 35/40 jobs PASS, 5/40 FAIL, with 0 explicit test SKIP. Four red jobs failed during fixture initialization because installed `bd` refused pending shared-server schema migrations before scenario logic ran; tracked by `ga-esyijp`. One `cmd/gc` shard reproduced the exact two-subtest `async starts did not finish` load failure already recorded on an unrelated PR; tracked by `ga-rh76sz`. Neither failure class is diff-owned. Focused candidate tests are green as recorded below. |
| 3b | Policy/lint lane | **PASS** | `make test-ci-policy`, merge-base-scoped `make lint-affected` (0 issues), `make fmt-check-changed`, `go vet ./...`, `make build`, `bash -n`, `shellcheck`, and `git diff --check` all pass. |
| 3c | CI-config lane run | **PASS / n/a** | No CI job, matrix, timeout, required-check list, workflow, or other CI configuration changed. Existing PR #6320 required checks are all green at the reviewed head. |
| 4 | No high-severity review findings open | **PASS** | MPR's ensemble review reports all three reviewers `ok` and an `auto-merge` verdict. Its only follow-up observation is explicitly non-blocking and concerns previously unprotected overlength/singleton session names outside this fix. |
| 5 | Final branch clean | **PASS** | The worktree was clean at detached reviewed SHA before this gate record was written; `git diff --check` passed. |
| 6 | Branch diverges cleanly from main | **PASS** | After a fresh fetch, `git merge-tree --write-tree --messages origin/main 9f4eb6fffe6cd62825bd7efd62cb26f0aafcef68` returned exit 0 with tree `a85e99d9a7ec5be097a9197763834d411b14f86a` and no conflict messages. The candidate is 10 commits behind and 1 commit ahead of the checked base. |
| 7 | Single feature theme | **PASS** | The sole feature commit changes orphan-sweep pool-seat classification, its regression test, and the required synchronized resource-census ledger entries. No independent behavior is bundled. |

## Test evidence

- `test_cmd`: `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true make test-local-full-parallel`
- `test_cmd_scope`: `full-suite` (the documented 40-job local union)
- `test_counts`: 35 PASS jobs, 5 FAIL jobs, 0 explicit test SKIP
- `full_logs`: `/var/tmp/ga-wlwk8p-gate-20260912-1800`
- `diff_tests_executed`:
  - `TestOrphanSweepTreatsPoolSessionNameSelfProbeAsUnverifiable/rig_scope_double_dash`: PASS
  - `TestOrphanSweepTreatsPoolSessionNameSelfProbeAsUnverifiable/city_scope_double_underscore`: PASS
  - `TestOrphanSweepTreatsPoolSessionNameSelfProbeAsUnverifiable/city_scope_numbered_slot`: PASS
  - parent `TestOrphanSweepTreatsPoolSessionNameSelfProbeAsUnverifiable`: PASS
- Focused resource-census package: 228 PASS, 0 FAIL, 0 SKIP; includes `TestRepositoryLedgerMatchesCensusAndDocumentation` PASS.
- `skip_justification`: none; no explicit test SKIP was observed
- `waiver_ref`: none; every red result satisfies the repository's non-diff-owned failure-attribution protocol
- `ci_lane_run`: n/a; no CI-config change

## Failure attribution

- `TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix` -> `ga-esyijp`
- `TestGCLiveContract_BeadsAndEvents` -> `ga-esyijp`
- `TestRetryManagedPooledWorkerRecoversClaimedAttemptAfterCrash` -> `ga-esyijp`
- `TestAdoptPRFormulaSoftFailsGeminiAfterTransientRetries` -> `ga-esyijp`
  - Clause 3(a), mechanism: all four fail in fixture initialization when installed `bd` refuses pending shared-server schema migrations to v66. The scenario never begins, and orphan-sweep classification cannot participate. The tracker predates this run; this sighting was appended.
  - Clause 4: no path overlap with the changed orphan-sweep script/test or census ledger paths.
- `TestSessionReconcilerTraceGH1654WorkRequestedStartCandidates/{named_session_post-kill,pool_respawn_after_drain}` -> `ga-rh76sz`
  - Clause 3(b), cross-PR: the tracker records the identical two-subtest `async starts did not finish` failure under shard-parallel load on 2026-09-11 for unrelated candidate `ga-2yq3p5`. The tracker predates this run; this sighting was appended.
  - Clause 4: no path overlap with `cmd/gc/session_reconciler_trace_integration_test.go` or its production path.

## Static and focused evidence

- `make build`: PASS
- `go vet ./...`: PASS
- `make test-ci-policy`: PASS
- `LINT_CHANGED_REF=391ad1eee6468b2fe1210b51020484d193ce3723 make lint-affected`: PASS, 0 issues
- `LINT_CHANGED_REF=391ad1eee6468b2fe1210b51020484d193ce3723 make fmt-check-changed`: PASS
- `bash -n internal/bootstrap/packs/core/assets/scripts/orphan-sweep.sh`: PASS
- `shellcheck internal/bootstrap/packs/core/assets/scripts/orphan-sweep.sh`: PASS
- focused orphan-sweep regression: 4 PASS, 0 FAIL, 0 SKIP
- focused resource-census package: 228 PASS, 0 FAIL, 0 SKIP
- `git diff --check 391ad1eee6468b2fe1210b51020484d193ce3723...HEAD`: PASS
- `.githooks` is configured as `core.hooksPath`; the deploy commit will run the active pre-commit hook.

An initial `lint-affected` invocation used the moving `origin/main` tip instead
of the candidate merge base. That made post-branch upstream additions appear as
candidate deletions, intentionally selected the entire repository, and exposed
three diagnostics inside ignored dashboard `node_modules`. Re-running with the
actual merge base selected the candidate diff correctly and completed with zero
issues; no repository file was changed to mask the local artifact.

## Disposition

Technical gate PASS. Cut `deploy/ga-wlwk8p-gate` at the exact reviewed SHA,
commit this gate record, publish the isolated branch, open a pull request to
`main`, attach exact-head deploy clearance, and route the merge request to the
mayor/MPR. The deployer does not merge the pull request.
