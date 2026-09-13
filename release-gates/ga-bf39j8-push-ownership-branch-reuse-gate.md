# Release gate: push-ownership branch reuse (`ga-bf39j8`)

- Gate result: **PASS**
- Reviewed commit: `813d27f5cb9727a8b84a7589b7ee95c59abe3c29`
- Gated commit after bounded self-rebase: `323ae11493cfb19da07e820f1097e4e4a20f956f`
- Base ref: `origin/main@8c73625b974ce8d3d68d54ab42062e6247c47036`
- Deploy mode: remote
- Evaluation date: 2026-08-19
- Full-suite logs: `/var/tmp/gc-local-tests.U2ksqv`

`docs/PROJECT_MANIFEST.md` is not present at the evaluated ref. This gate uses
the canonical criteria in `mol-deployer-gate.formula.toml`, the current
deployer prompt, and
`engdocs/contributors/release-gate-criteria-conventions.md`.

## Checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Reviewer PASS present for deployed commit | **PASS** | The reviewer recorded an independent PASS for resolved commit `813d27f5cb9727a8b84a7589b7ee95c59abe3c29`. The bounded helper replayed that exact one-commit change onto current main as `323ae11493cfb19da07e820f1097e4e4a20f956f`; no content changes were added. |
| 2 | Acceptance criteria met | **PASS** | The guard now substitutes a declared live successor only after a fresh read confirms the branch-derived bead is inactive. The relationship must be explicit through `metadata.branch` or `metadata.build_bead`; unrelated concurrent work is rejected, an active predecessor stays authoritative, and read/parse failure remains fail-closed. The five added `branch_reused_*` cases exercise those obligations. |
| 3 | Tests pass | **PASS** | With rootless Podman enabled and the pinned testcontainers image `dolthub/dolt-sql-server:1.32.4` cached, the documented CI-equivalent command `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true GO_TEST_TIMEOUT=30m make test-local-full-parallel` completed **33 PASS / 7 FAIL / 0 SKIP jobs**. All seven failures are attributed under criterion 3a; none is diff-owned. The diff-owned shell suite completed **35 PASS / 0 FAIL / 0 SKIP**, including all five new cases, and the Go wrapper `TestPushOwnershipGuard` passed. A later pre-push fast run completed **9 PASS / 1 FAIL / 0 SKIP jobs**; its sole failure is also attributed under 3a. |
| 3a | Pre-existing failures attributable | **PASS** | The diff changes only `scripts/push-ownership-guard.sh` and `scripts/test-push-ownership-guard.sh`; it cannot compile into or execute any failing Go package, cannot change the installed `bd` flag surface, and cannot change the host tmux key table or Dolt fixture startup timing. This is a structural mechanism proof, stronger than a probabilistic base rerun, and there is no path overlap. Exact trackers: `TestBdFlagManifestCurrent` -> `ga-f0uceo`; `TestGetKeyBinding_CapturesDefaultBinding` -> `ga-afqddr`; `TestGetKeyBinding_CapturesDefaultBindingWithArgs` -> `ga-k3fxvj`; `TestAdoptPRFormulaRetriesTransientReviewerStep`, `TestAdoptPRFormulaSoftFailsGeminiAfterTransientRetries`, and `TestPersonalWorkFormulaCompileAndRun` with the beads#4566 dirty-table signature -> `ga-lpfjhc`; `TestDoltConfigWiringExternalHost` -> `ga-gajll3` (fix `e669302f4b7e046768bf81622cd90242a4cff289` verified not on current `origin/main`); pre-push `TestDisableAndPurgeRejectsPeerSuccessorReplacedDuringCleanProof` -> `ga-s759zk` / upstream issue #4653, which records the same load-sensitive race under parallel `make test`. |
| 3b | Policy/lint lane | **PASS** | `make test-ci-policy` passed: 5 runner-policy tests, 15 suite-coverage tests, the `scripts/cipolicy` package, and the focused static-scope suite. `shellcheck` on both changed scripts, `bash -n`, `zsh -n`, `LINT_CHANGED_REF=origin/main make lint-affected`, `LINT_CHANGED_REF=origin/main make fmt-check-changed`, and `go vet ./...` all passed. The affected lint/format selectors correctly reported no changed Go build inputs or Go files. |
| 4 | No unresolved HIGH review findings | **PASS** | The independent reviewer reported no defects; unresolved HIGH count is 0. |
| 5 | Final branch clean | **PASS** | `git status --porcelain` was empty before this checklist was written. This checklist is the only gate artifact added afterward. |
| 6 | Branch diverges cleanly from main | **PASS** | Initial staleness was resolved only through the mandated bounded helper: `813d27f5cb9727a8b84a7589b7ee95c59abe3c29` -> `323ae11493cfb19da07e820f1097e4e4a20f956f`. The helper completed its force-with-lease push, `origin/builder/ga-bf39j8` was independently verified at the after SHA, and `origin/main@8c73625b974ce8d3d68d54ab42062e6247c47036` is an ancestor of the gated commit. |
| 7 | Single feature theme | **PASS** | The one-commit, two-file change is confined to one subsystem: push-ownership guard branch-reuse resolution and its regression tests. |

## Test-integrity fields

- `test_cmd`: `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true GO_TEST_TIMEOUT=30m make test-local-full-parallel`
- `test_counts`: full union `33 PASS / 7 FAIL / 0 SKIP jobs`; pre-push fast attempt `9 PASS / 1 FAIL / 0 SKIP jobs`; all failures attributed above
- `diff_tests_executed`: all 35 cases in `scripts/test-push-ownership-guard.sh` PASS, including the five new `branch_reused_*` cases; Go wrapper `TestPushOwnershipGuard` PASS
- `skip_justification`: none; the runner reported no job-level or diff-owned skip
- `waiver_ref`: `ga-lpfjhc` carries the existing mayor standing authorization for the exact beads#4566 dirty-table signature; no diff-owned waiver was used
- `policy_lane`: `make test-ci-policy` PASS
- `failure_attribution`: `TestBdFlagManifestCurrent` -> `ga-f0uceo`; tmux default-binding tests -> `ga-afqddr` / `ga-k3fxvj`; beads#4566 review-formula fixture failures -> `ga-lpfjhc`; `TestDoltConfigWiringExternalHost` -> `ga-gajll3`; `TestDisableAndPurgeRejectsPeerSuccessorReplacedDuringCleanProof` -> `ga-s759zk` / upstream #4653; structural proof is the shell-only diff with zero path or execution overlap

## Disposition

All criteria pass. Proceed from the gated SHA to the isolated
`deploy/ga-bf39j8-gate` branch, push that branch only, open the pull request,
publish deploy clearance on the exact PR head, and route the merge-request to
the merge authority. The deployer does not merge.
