# Release gate: pack runtime prompt-delivery opt-in (`ga-ggx43k`)

- Overall verdict: **PASS with attributed/waived raw test failures**
- Evaluated: 2026-09-01 PDT / 2026-09-01 UTC
- Deploy mode: `remote`; push remote: `origin`
- Reviewed deploy source: `c0e16a482397e8f94408463809d931ea229103b4`
- Source branch: `builder/ga-s5y62b.1` (provenance only)
- Base evaluated: `origin/main@c374d52479c5ccb112d2a804b36526f777b203da`
- Review bead: `ga-3juiyl`
- Build bead: `ga-s5y62b.1`

`docs/PROJECT_MANIFEST.md` is not present at the reviewed commit, so this gate
uses the seven release criteria embedded in `mol-deployer-gate` and the
deployer prompt. The pre-flight query found no pull request associated with
the reviewed commit; the normal deploy path applies.

## Criteria

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-3juiyl` is closed with reason `pass` on the exact reviewed commit. Its notes record no style, security, specification, or coverage finding. |
| 2 | Acceptance criteria met | **PASS** | Independent diff inspection confirms `[runtimes.<name>].prompt_delivery = "nudge-fallback"` opts a pack-declared runtime into oversized-prompt nudge delivery; unset remains fail-closed; invalid values fail pack/city composition; differing diamond re-declarations conflict; and the sibling `internal/runtime/exec/json.go` / `internal/config/resolve.go` scope is untouched. All 63 diff-owned cases pass by name. |
| 3 | Tests pass | **PASS with attributed/waived raw failures** | The documented complete matrix, `make test-local-full-parallel`, scheduled all 40 jobs and completed **37 PASS / 3 FAIL / 0 SKIP jobs**. Four non-diff-owned test failures are retained and adjudicated below; no candidate-owned failure remains. All six `cmd/gc` process shards required for this diff passed. |
| 3a | Non-diff-owned failures attributed | **PASS** | `TestBdFlagManifestCurrent` is attributed to `ga-f0uceo`; `TestE2E_SuspendResume_City` is attributed to consolidated tracker `ga-vkhfnj`; and the exact beads#4566 dirty-schema failures in `TestCleanInstallTutorialPath` and `TestGCLiveContract_BeadsAndEvents` remain **FAIL-WAIVED** under the mayor standing authorization on `ga-6bnc42`, with current sightings recorded on `ga-vkhfnj` and `ga-lpfjhc`. Full evidence is below. |
| 3b | Policy/lint lane | **PASS** | `make test-ci-policy`, fresh-cache affected-package lint, changed-file formatting, `go vet ./...`, and `git diff --check origin/main...HEAD` passed. The fresh-cache lint invocation reported `0 issues`. |
| 3c | CI-config lane run | **PASS / n/a** | No workflow, job matrix, timeout, required-check list, runner policy, or CI configuration changed. |
| 4 | No high-severity review findings open | **PASS** | The closed review bead records no findings and no blocker; no HIGH or request-changes finding is open for this reviewed source. |
| 5 | Final branch clean | **PASS** | The detached reviewed-source worktree was clean after the full matrix and static gates. This checklist is the deployer's only change and will be committed separately on the isolated deploy branch. |
| 6 | Branch diverges cleanly from main | **PASS** | `git merge-tree --write-tree origin/main c0e16a482397e8f94408463809d931ea229103b4` exited 0 and produced tree `83dd01b21c46cd892380e3e8873c4367e88fb57a`. The candidate was one commit behind and one ahead of the evaluated base; no bounded self-rebase was needed. |
| 7 | Single feature theme | **PASS** | One reviewed commit adds the pack-runtime prompt-delivery declaration, composition validation, runtime lookup plumbing, tests, specification text, and generated pack-schema projections. All nine files serve that one configuration feature. |

## Build and smoke evidence

- `make build`: **PASS**; built `bin/gc` from the reviewed commit.
- `./bin/gc version`: **PASS** (`dev`).
- `./bin/gc --help`: **PASS**.
- `make check-schema`: **PASS**; regenerated reference files remained clean.
- `make check-docs`: **PASS**.
- Build log: `/var/tmp/ga-ggx43k-build.out`.

## Criterion 3 evidence

Environment established before the full run:

- `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock`
- `TESTCONTAINERS_RYUK_DISABLED=true`
- `EXTRA_TEST_ENV` passed both values through the repository's scrubbed test wrapper
- rootless Podman 5.8.4 was reachable; the rig has no cached `dolt-tests-via-podman` cairn entry and the source contains no pinned `dolthub/dolt*` container tag

Test evidence:

- `test_cmd`: `EXTRA_TEST_ENV="DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true" make test-local-full-parallel`
- `test_cmd_scope`: `full-suite`
- `test_counts`: **37 PASS / 3 FAIL / 0 SKIP jobs**; the three red jobs contain four failing top-level test names
- `full_log`: `/var/tmp/ga-ggx43k-full-suite.out`
- `shard_logs`: `/var/tmp/gc-local-tests.DE18B8`
- `skip_justification`: none; no `--- SKIP:` result was observed in any shard log
- `ci_lane_run`: n/a; no CI-config change
- `waiver_ref`: mayor standing authorization on `ga-6bnc42`, limited to the two exact beads#4566 dirty-schema failures below

Diff-owned tests were re-run verbosely after the full matrix:

- `cmd/gc/prompt_delivery_test.go`: `TestPromptDelivery`, `TestPromptDeliveryOversized`, and `TestPromptDeliverySupportFor` — **43/43 top-level/subtest results PASS, 0 FAIL, 0 SKIP**.
- `internal/config/pack_runtimes_test.go`: all 12 changed top-level functions, including validation and conflicting `prompt_delivery` re-declaration — **20/20 top-level/subtest results PASS, 0 FAIL, 0 SKIP**.
- `diff_tests_executed`: **63/63 PASS, 0 FAIL, 0 SKIP**.
- Verbose log: `/var/tmp/ga-ggx43k-diff-tests.out`.

### Raw failure attribution

- `failure_attribution: TestBdFlagManifestCurrent -> ga-f0uceo | clause 3(a), mechanism — attributed`
  - `integration-packages-core-4-of-4` reported the tracker's exact installed-`bd` flag-manifest drift: `--cpu-profile`, `--database`, `--mem-profile`, `--no-color`, and command-specific flags are absent from `internal/bdflags`.
  - The candidate does not touch `internal/bdflags` or the installed CLI. `go list -deps ./internal/bdflags` includes neither changed Go package, and there is no path overlap.
  - Current sighting appended to the predating open tracker; raw log: `/var/tmp/gc-local-tests.DE18B8/integration-packages-core-4-of-4.log`.

- `failure_attribution: TestE2E_SuspendResume_City -> ga-vkhfnj | clause 3(a), mechanism — attributed`
  - `integration-rest-full-2-of-8` timed out after 93.73 seconds waiting for `citysus.report`, matching the consolidated whole-suite contention signature.
  - This fixture declares no pack runtime and its startup prompt is below the oversized guard. The candidate's new branch is limited to oversized prompts for pack-declared runtimes, so it is not exercised by this path; no `test/integration` file changed and no test-load/resource baseline changed.
  - Current sighting appended to the predating open tracker; raw log: `/var/tmp/gc-local-tests.DE18B8/integration-rest-full-2-of-8.log`.

- `failure_attribution: TestCleanInstallTutorialPath -> ga-vkhfnj / ga-lpfjhc | clause 3(a), mechanism — raw FAIL-WAIVED by ga-6bnc42`
  - Fixture `gc init` failed with the exact gastownhall/beads#4566 signature: pending schema migrations altered pre-existing dirty `comments`/`events` tables, followed by the missing `leases` table.
  - The failure occurs during Dolt store bootstrap before prompt delivery. The candidate does not change schema migration, store bootstrap, resource census, or test targets.
  - Current sighting appended to the consolidated open tracker and the standing-authorization record; raw log: `/var/tmp/gc-local-tests.DE18B8/integration-rest-full-2-of-8.log`.

- `failure_attribution: TestGCLiveContract_BeadsAndEvents -> ga-vkhfnj / ga-lpfjhc | clause 3(a), mechanism — raw FAIL-WAIVED by ga-6bnc42`
  - The rig-create fixture returned HTTP 500 because `bd init` hit the same beads#4566 pending-dirty-table signature for `issues`.
  - This is store initialization before the candidate's pack-runtime prompt-delivery behavior can run. No migration or store-bootstrap path changed.
  - Current sighting appended to the consolidated open tracker and the standing-authorization record; raw log: `/var/tmp/gc-local-tests.DE18B8/integration-rest-full-5-of-8.log`.

## Policy and static evidence

- `make test-ci-policy`: **PASS** (5 runner-policy tests, 15 CI-suite-coverage tests, `scripts/cipolicy`, `scripts/prwatchdog`, and the focused static-scope contracts).
- `GOLANGCI_LINT_CACHE=/var/tmp/ga-ggx43k-golangci-cache LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=3d317457dd7a7ff68f1c0333f7eb5fb399b2016c make lint-affected`: **PASS**, `0 issues`.
- `LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=3d317457dd7a7ff68f1c0333f7eb5fb399b2016c make fmt-check-changed`: **PASS**.
- `go vet ./...`: **PASS**.
- `git diff --check origin/main...HEAD`: **PASS**.
- Static logs: `/var/tmp/ga-ggx43k-static.out` and `/var/tmp/ga-ggx43k-lint-affected.out`.

## Disposition

Gate PASS. Cut `deploy/ga-ggx43k-gate` from the exact reviewed source, commit
this checklist there, push the isolated branch, and open a pull request. Merge
authority remains with mayor/mpr; the deployer does not merge.
