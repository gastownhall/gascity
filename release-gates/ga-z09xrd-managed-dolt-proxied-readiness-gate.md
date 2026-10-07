**Verdict:** **PASS**

# Release gate: managed Dolt proxied readiness (`ga-z09xrd`)

- Deploy bead: `ga-z09xrd`
- Build bead: `ga-jrapwl`
- Review bead: `ga-h8rm1g`
- Reviewed source: `4fdf07a1eb734c4e7e91a9dddc2d21d616e86c56`
- Base checked: `origin/main` at `af2b084ff0b143d5a642c5cdd27e1b4d516c8859`
- Synthetic merge checked: `2ed4607d171a1bab9c3e8ac13f2207311bc1aa2d`
- Deploy branch: `deploy/ga-z09xrd-gate`
- Evidence root: `/var/tmp/gc-deploy-ga-z09xrd-regate.cjUBt7/logs`

## Criteria

| # | Criterion | Verdict | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | `ga-h8rm1g` is closed with close reason `pass`. It records the reviewed commit `4fdf07a1eb734c4e7e91a9dddc2d21d616e86c56`, no style findings, no security findings, and `verdict: pass`. |
| 2 | Acceptance criteria met | PASS | The diff is limited to `test/integration/helpers_test.go`, matching the build bead's requested scope. Focused isolated acceptance run passed: `TestIsProviderOwnedProxiedDoltCity`, `TestWaitForManagedDoltCityReady_ProxiedModeProbesWithoutPortHint`, and `TestWaitForManagedDoltCityReady_ProxiedModeSurfacesProbeError`; log `diff-owned-acceptance.log`, rc 0. The five `ga-grepx8` review-formula readiness tests all passed in the full gate (`integration-review-formulas-basic-{1,2}`, `integration-review-formulas-retries-{1,2}`, and `integration-review-formulas-recovery`). |
| 3 | Tests pass | PASS with attributed non-diff-owned failures | Container setup was verified first (`podman-info.txt` from the first run; `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock`, `TESTCONTAINERS_RYUK_DISABLED=true`). Full-scope command was run through the isolation wrapper on the current-base synthetic merge: `make test-local-full-parallel`; log `test-local-full-parallel-rerun.log`. Raw result: 38/40 jobs green and 2 jobs failed. Both failures are the same attributed condition under criterion 3a below; no diff-owned test failed or skipped. |
| 3a | Pre-existing failures may be attributed | PASS | `failure_attribution: TestPoolSessionCreate_TerminalProviderErrorTearsDownBeforeRollback -> ga-z8yi2j \| clause 3: d — exact targeted run on current `origin/main` `af2b084ff0b143d5a642c5cdd27e1b4d516c8859` reproduced `row gc-2 status "open" after a confirmed teardown, want closed` (`base-current-pool-targeted.log`). The full current-base rerun repeated the same assertion in `cmd-gc-process-1-of-6` and `integration-packages-cmd-gc-1-of-6`; both sightings were recorded and read back on tracker `ga-z8yi2j`. Candidate diff is confined to `test/integration/helpers_test.go`, so there is no path overlap with the failing `cmd/gc` package. |
| 3b | Policy/lint lane | PASS | `make test-ci-policy` ran through the isolation wrapper and passed; log `test-ci-policy.log`, rc 0. |
| 3c | CI-config diff needs its own lane | PASS | Not applicable: candidate diff changes only `test/integration/helpers_test.go`; no CI job, matrix, timeout, or required-check file changed. |
| 4 | No high-severity review findings open | PASS | Review bead `ga-h8rm1g` records no security blockers/major/minor findings and no style findings; unresolved HIGH count is 0. |
| 5 | Final branch is clean | PASS | Deploy worktree was clean after cutting `deploy/ga-z09xrd-gate` at the reviewed SHA; the only added file is this gate checklist, to be committed as the release-gate record. |
| 6 | Branch diverges cleanly from main | PASS | `materialize_merge_tree` succeeded for PR head `36e5259abe719c4198e10d29d7fb934920a05d3f` over current `origin/main` `af2b084ff0b143d5a642c5cdd27e1b4d516c8859`; synthetic merge `2ed4607d171a1bab9c3e8ac13f2207311bc1aa2d`. `go build ./...` and `go vet ./...` both passed on that tree (`go-build.log`, `go-vet.log`). |
| 7 | Single feature theme | PASS | Single-bead deploy. Commit range touches one subsystem/theme: integration readiness probing for provider-owned proxied-server Dolt in `test/integration/helpers_test.go`. No independent feature or planning/documentation payload is included. |

## Test detail

- Full-suite command:
  `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- bash -lc 'make test-local-full-parallel'`
- Full-suite result: rc 2 before attribution; 38 jobs passed, 2 jobs failed.
- Failed raw jobs:
  - `cmd-gc-process-1-of-6`: `TestPoolSessionCreate_TerminalProviderErrorTearsDownBeforeRollback`, tracked by `ga-z8yi2j`.
  - `integration-packages-cmd-gc-1-of-6`: same `TestPoolSessionCreate_TerminalProviderErrorTearsDownBeforeRollback`, tracked by `ga-z8yi2j`.
- Focused diff-owned acceptance command:
  `go test -tags integration -count=1 -timeout 10m ./test/integration -run '^(TestIsProviderOwnedProxiedDoltCity|TestWaitForManagedDoltCityReady_ProxiedMode(ProbesWithoutPortHint|SurfacesProbeError))$' -v`
- Focused diff-owned acceptance result: rc 0; all three top-level tests PASS, with all table subtests PASS.
- Policy lane:
  `make test-ci-policy`, rc 0.

## Notes

- The mayor explicitly lifted the deploy stand-down for `ga-z09xrd` only in mail `gm-wisp-xsbi1i`.
- The raw full-suite failures were not ignored; they were attributed according to the non-diff-owned gate-failure protocol and recorded on tracker `ga-z8yi2j` before this gate was marked PASS.
- An earlier gate record on this branch used base `f9b8b2ebf02220d11e0e8e1b9d9faafa38715334`; after `origin/main` advanced, merge-request `gm-wisp-xudnxj` was held by verified correction mail `gm-wisp-6y08q6`, and this file was updated from the current-base rerun before the final handoff.
- `TestCleanInstallTutorialPath` did not fail in this gate run, so the mayor's `ga-vkhfnj` / `ga-5korc0` attribution path was not used.
- A maintainer follow-up commit, applied after this gate, widened the proxied-probe env strip in `waitForManagedDoltCityReady` to all four endpoint hints (`GC_DOLT_HOST`, `GC_DOLT_PORT`, `BEADS_DOLT_SERVER_HOST`, `BEADS_DOLT_SERVER_PORT`) and added `TestWaitForManagedDoltCityReady_ProxiedModeStripsInheritedEndpointHints`. The change is test-only and confined to `test/integration/helpers_test.go`; the focused diff-owned suite (now including the new test) was re-run and passed.
