# Release gate: suspended-to-asleep lifecycle re-projection

- Bead: `ga-we6bj3`
- Source build bead: `ga-7owgg2`
- Reviewed source: `3e68c47290c8a4ba75158f7adb2e99a6a1f592f7`
- Source branch (provenance only): `builder/ga-we6bj3`
- Base: `origin/main@270d53fadfa0254e3b18e6197564463a84b9e01c`
- Merge base: `ed146d8d9f2fdf142b4b23540ff0412fd2eec33c`
- Deploy mode: remote; push target would be `fork`
- Evaluated: 2026-09-10
- Overall: **FAIL**

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | The reviewer recorded a scoped-delta re-review PASS on 2026-09-02 for the exact resolved SHA `3e68c47290c8a4ba75158f7adb2e99a6a1f592f7`. No review carryover was used. |
| 2 | Acceptance criteria met | **PASS** | Inspected the seven-file lifecycle-only delta. Wake-blocker clearing receives `now`, clears suspension vocabulary, and stamps sleep vocabulary; session bead re-projection, `SleepPatch`, and `Manager.Suspend` clear the opposing vocabulary; all seven added or modified tests executed successfully in focused verbose runs. |
| 3 | Tests pass | **FAIL** | The documented full-suite command `make test-local-full-parallel` ran with rootless Podman configured and produced 37 passing jobs, 3 failing jobs, and 0 skipped jobs. Two failures are attributable to tracked pre-existing conditions, but `TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix` cannot be attributed because its `cmd/gc` package overlaps the candidate's `cmd/gc/session_beads.go`; there is no independently granted waiver. Details below. |
| 4 | No high-severity review findings open | **PASS** | Reviewer verdict is PASS with no changes requested and no unresolved HIGH finding. |
| 5 | Final branch is clean | **PASS** | `git status --short` was empty before this gate record was written. |
| 6 | Branch diverges cleanly from main | **PASS** | `git merge-tree --write-tree origin/main 3e68c47290c8a4ba75158f7adb2e99a6a1f592f7` exited 0 and produced merge tree `4c305916be2045eb0c00f809ce079201857b36c2`. No self-rebase was needed. |
| 7 | Single feature theme | **PASS** | Both commits and all seven changed files concern one session-lifecycle metadata invariant: coherent suspended/asleep vocabulary. |

## Criterion 3 evidence

- `test_cmd`: `make test-local-full-parallel`
- `test_cmd_scope`: `full-suite`
- Environment: `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock`, `TESTCONTAINERS_RYUK_DISABLED=true`; the cached Dolt image tag matched the repository pin.
- Result: 40 jobs; 37 PASS, 3 FAIL, 0 SKIP.
- Full log: `/var/tmp/ga-we6bj3-test-local-full-parallel.log`
- `diff_tests_executed`: all seven added or modified top-level tests PASS, 0 FAIL, 0 SKIP. Supplemental logs: `/var/tmp/ga-we6bj3-diff-tests-session.log` and `/var/tmp/ga-we6bj3-diff-tests-cmdgc.log`.
- `waiver_ref`: none.
- `ci_lane_run`: n/a — the diff does not change CI configuration.

Failure attribution:

1. `internal/bdflags.TestBdFlagManifestCurrent` — raw FAIL because the installed `bd` exposes flags newer than the candidate manifest. Existing pre-run tracker `ga-f0uceo` covers this condition and contains prior base/cross-run reproduction. The failing test and package are not diff-owned, cannot reach the changed session packages, and have no package/path overlap with the diff. **Attributed; not gate-blocking.**
2. `test/integration.TestGCLiveContract_BeadsAndEvents` — raw FAIL from the tracked beads #4566 dirty-schema migration condition. Existing pre-run tracker `ga-esyijp` covers the condition and prior identical sightings. The test is not diff-owned, the candidate adds no test load, and there is no package/path overlap with the diff. **Attributed; not gate-blocking.**
3. `cmd/gc.TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix` — raw FAIL from the same beads #4566 dirty-schema migration condition covered by pre-run tracker `ga-esyijp`; the test itself is not diff-owned. However, the candidate changes `cmd/gc/session_beads.go`, so the required no-package-overlap proof fails. The non-diff-owned failure protocol makes this a hard criterion-3 FAIL absent an independently granted waiver. **Not attributed; gate-blocking.**

Additional required lanes:

- Worker profiles: `claude/tmux-cli`, `codex/tmux-cli`, and `gemini/tmux-cli` all passed both core and phase-2 suites.
- Docker session suite: 33 PASS, 2 raw FAIL for missing setup markers. Neither failing script is touched by or reachable from the lifecycle delta; tracker `ga-0svyw1` records the reproduced condition. **Attributed.**
- Kubernetes suite: PASS with `TestK8sSessionConformance` skipped because the optional local `GC_SESSION_K8S_SCRIPT` fixture was not configured; the skipped test is not diff-owned.
- `policy_lane`: `make test-ci-policy` PASS; `make check-gomod-replace` PASS; `make check-native-dependency-surface` PASS; `make check-eventexport-isolation` PASS; `make check-core-boundary` PASS; `make test-native-doltlite-beads` PASS; `make fmt-check` PASS; `make vet` PASS; `make check-docs` PASS. `make lint` reported only the three pre-existing findings under ignored `internal/api/dashboardspa/web/node_modules/flatted`; tracker `ga-bvixfw` records the identical base condition, and the candidate has no path overlap. **Attributed.**

## Disposition

Criterion 3 is FAIL because one full-suite failure lacks the mandatory no-package-overlap proof and no mayor-issued waiver exists. Per the technical-failure path, do not push, open a PR, or publish deploy clearance. Return the bead to the builder for a fresh handoff after the blocker is resolved or an independent waiver is recorded.
