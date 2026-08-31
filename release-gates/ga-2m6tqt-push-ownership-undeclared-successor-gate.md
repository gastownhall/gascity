# Release gate: push-ownership undeclared successor (`ga-2m6tqt`)

- Gate result: **PASS**
- Reviewed commit: `3c77b6789bd9621bd019cdd07cc72093cf72cb98`
- Base ref: `origin/main@a85f857b3987bd18593cea2e9594a17a82b10df1`
- Deploy mode: remote
- Evaluation date: 2026-08-31
- Full-suite logs: `/var/tmp/gc-local-tests.dLtqwB`

`docs/PROJECT_MANIFEST.md` is not present at the evaluated ref. This gate uses
the canonical criteria in `mol-deployer-gate.formula.toml`, the current
deployer prompt, and
`engdocs/contributors/release-gate-criteria-conventions.md`.

## Checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Reviewer PASS present for deployed commit | **PASS** | The independent reviewer closed `ga-0ywrmy.1` with verdict PASS and recorded deploy commit `3c77b6789bd9621bd019cdd07cc72093cf72cb98`. The reviewed commit is the exact commit evaluated here. |
| 2 | Acceptance criteria met | **PASS** | The branch-reuse resolver now accepts an undeclared successor only after a fresh read confirms the branch-derived bead is inactive, the already-fetched in-progress set contains exactly one other bead, and that candidate passes the same live ownership predicate as the public guard. Ambiguity, unhealthy ownership, and read or parse failure remain fail-closed. The existing declared-successor, active-predecessor, deploy-gate, plain multi-match, retry, hook-wiring, and zsh behaviors remain covered. |
| 3 | Tests pass | **PASS** | With rootless Podman enabled, the documented CI-equivalent command `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true make test-local-full-parallel` completed **36 PASS / 4 FAIL / 0 SKIP jobs**. All four raw failures are attributed under criterion 3a; none is diff-owned. The diff-owned shell suite completed **40 PASS / 0 FAIL / 0 SKIP**, including the rewritten unrelated-candidate case and both new undeclared-successor cases. Its Go wrapper passed in `integration-packages-core-4-of-4`. The source PR's exact reviewed head also has green `CI / required`, CodeQL, static, acceptance, generated-artifact, dashboard, vulnerability, and critical-path evidence checks in GitHub Actions run `33361916791` and its companion workflows. |
| 3a | Pre-existing failures attributable | **PASS** | The diff changes only `scripts/push-ownership-guard.sh` and `scripts/test-push-ownership-guard.sh`. It cannot compile into or execute the failing `internal/runtime/herdr`, `internal/bdflags`, or `test/integration` code, and it changes no test census, Makefile, workflow, or runner policy. There is zero path overlap or test-load increase. Exact failures and open trackers: `TestSessionEventsLive` (`getAgent evt-a: ok=false`) -> `ga-vkhfnj`; `TestBdFlagManifestCurrent` (installed `bd` flags ahead of the checked manifest) -> `ga-f0uceo`; `TestE2E_SuspendResume_City` (timed out awaiting `citysus.report`) -> `ga-vkhfnj`; `TestHumaBinary_SessionMessageAsync` (beads#4566 dirty `events` table migration) -> `ga-esyijp`. Current-run occurrences were appended to those trackers. |
| 3b | Policy/lint lane | **PASS** | `make test-ci-policy` passed: 5 runner-policy tests, 15 suite-coverage tests, `scripts/cipolicy`, `scripts/prwatchdog`, and the focused static-scope suite. `shellcheck` on both changed scripts, `bash -n`, `zsh -n`, `go build ./...`, `go vet ./...`, `git diff --check`, `LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=2443a9d74166211ff8aef519810b039f23854edb make lint-affected`, and the matching `make fmt-check-changed` all passed. The affected selectors correctly reported no changed Go build inputs or Go files. |
| 4 | No unresolved HIGH review findings | **PASS** | The independent reviewer reported no style or security findings and no unresolved defects; unresolved HIGH count is 0. |
| 5 | Final branch clean | **PASS** | `git status --porcelain` was empty before this checklist was written. This checklist is the only gate artifact added afterward. |
| 6 | Branch diverges cleanly from main | **PASS** | After refreshing `origin/main`, `git merge-tree --write-tree origin/main 3c77b6789bd9621bd019cdd07cc72093cf72cb98` exited 0 and produced tree `3f92ba80935769e8cfb29d44f5fe728c0c95778e`. Source PR #5795 remains OPEN, MERGEABLE, and pinned to the reviewed SHA. No self-rebase was necessary. |
| 7 | Single feature theme | **PASS** | The one-commit, two-file change is confined to one subsystem: safe undeclared-successor resolution in the push-ownership guard and its regression tests. |

## Test-integrity fields

- `test_cmd`: `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true make test-local-full-parallel`
- `test_cmd_scope`: full-suite
- `test_counts`: full union `36 PASS / 4 FAIL / 0 SKIP jobs`; all four failures attributed above; focused shell suite `40 PASS / 0 FAIL / 0 SKIP`
- `diff_tests_executed`: all 40 cases in `scripts/test-push-ownership-guard.sh` PASS; specifically the rewritten unrelated-candidate rejection, new owned undeclared-single acceptance, and new ambiguous-multiple rejection cases PASS; Go wrapper `TestPushOwnershipGuard` PASS
- `skip_justification`: none; the local runner reported no job-level or diff-owned skips. Package lines saying `[no tests to run]` are not skipped jobs.
- `waiver_ref`: none; no diff-owned failure or waiver was used
- `ci_lane_run`: n/a (no CI configuration change)
- `policy_lane`: `make test-ci-policy` PASS
- `failure_attribution`: `TestSessionEventsLive` and `TestE2E_SuspendResume_City` -> `ga-vkhfnj`; `TestBdFlagManifestCurrent` -> `ga-f0uceo`; `TestHumaBinary_SessionMessageAsync` / beads#4566 -> `ga-esyijp`; structural proof is the shell-only diff with zero path, build, execution, or test-load overlap

## Disposition

All criteria pass. Proceed from the reviewed SHA to the isolated
`deploy/ga-2m6tqt-gate` branch, push that branch only, open the pull request,
publish deploy clearance on the exact PR head, and route the merge request to
the merge authority. The deployer does not merge.
