**Verdict:** **PASS**

# Release gate: managed Dolt proxied readiness (`ga-z09xrd`)

- Deploy bead: `ga-z09xrd`
- Build bead: `ga-jrapwl`
- Review bead: `ga-h8rm1g`
- Reviewed source: `4fdf07a1eb734c4e7e91a9dddc2d21d616e86c56`
- Base checked: `origin/main` at `f9b8b2ebf02220d11e0e8e1b9d9faafa38715334`
- Synthetic merge checked: `a70d0b9d58f0531f345965798c4e39ec0b5e686e`
- Deploy branch: `deploy/ga-z09xrd-gate`
- Evidence root: `/var/tmp/gc-deploy-ga-z09xrd.2LvxbL/logs`

## Criteria

| # | Criterion | Verdict | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | `ga-h8rm1g` is closed with close reason `pass`. It records the reviewed commit `4fdf07a1eb734c4e7e91a9dddc2d21d616e86c56`, no style findings, no security findings, and `verdict: pass`. |
| 2 | Acceptance criteria met | PASS | The diff is limited to `test/integration/helpers_test.go`, matching the build bead's requested scope. Focused isolated acceptance run passed: `TestIsProviderOwnedProxiedDoltCity`, `TestWaitForManagedDoltCityReady_ProxiedModeProbesWithoutPortHint`, and `TestWaitForManagedDoltCityReady_ProxiedModeSurfacesProbeError`; log `diff-owned-acceptance.log`, rc 0. The five `ga-grepx8` review-formula readiness tests all passed in the full gate (`integration-review-formulas-basic-{1,2}`, `integration-review-formulas-retries-{1,2}`, and `integration-review-formulas-recovery`). |
| 3 | Tests pass | PASS with attributed non-diff-owned failures | Container setup was verified first (`podman-info.txt`; `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock`, `TESTCONTAINERS_RYUK_DISABLED=true`). Full-scope command was run through the isolation wrapper: `make test-local-full-parallel`; log `test-local-full-parallel.log`. Raw result: 37/40 jobs green and 3 jobs failed. All failures are attributed under criterion 3a below; no diff-owned test failed or skipped. |
| 3a | Pre-existing failures may be attributed | PASS | `failure_attribution: TestPoolSessionCreate_TerminalProviderErrorTearsDownBeforeRollback -> ga-z8yi2j \| clause 3: d — exact targeted run on `origin/main` reproduced `row gc-2 status "open" after a confirmed teardown, want closed`; tracker filed during this discovering run under the gm-sf3238 escape after base reproduction landed. `failure_attribution: TestGCLiveContract_BeadsAndEvents -> ga-lejnse \| clause 3: d/same-package tracked condition — the tracker predates this run and names this test/signature; the failure stopped during temporary rig creation on the shared-Dolt schema refusal (database `rwdlno5n5ke635`, v58 -> v66) before candidate behavior. Same-package guard is clear: no census bump, no new test target, and no new test file in `test/integration`; the fix-carrying repeat rule applies because this deploy's build bead `ga-jrapwl` is stamped `gc.fixes_tracker=ga-grepx8`, while `ga-lejnse` has unlanded fix bead `ga-3jssfa` stamped `gc.fixes_tracker=ga-lejnse` and commit `95c851a0cb776c5f3d7c394248a5c157a4ec33d8` is not on `origin/main`. Sightings were recorded and read back on both trackers. |
| 3b | Policy/lint lane | PASS | `make test-ci-policy` ran through the isolation wrapper and passed; log `test-ci-policy.log`, rc 0. |
| 3c | CI-config diff needs its own lane | PASS | Not applicable: candidate diff changes only `test/integration/helpers_test.go`; no CI job, matrix, timeout, or required-check file changed. |
| 4 | No high-severity review findings open | PASS | Review bead `ga-h8rm1g` records no security blockers/major/minor findings and no style findings; unresolved HIGH count is 0. |
| 5 | Final branch is clean | PASS | Deploy worktree was clean after cutting `deploy/ga-z09xrd-gate` at the reviewed SHA; the only added file is this gate checklist, to be committed as the release-gate record. |
| 6 | Branch diverges cleanly from main | PASS | `materialize_merge_tree` succeeded for reviewed commit `4fdf07a1eb734c4e7e91a9dddc2d21d616e86c56` over `origin/main` `f9b8b2ebf02220d11e0e8e1b9d9faafa38715334`; synthetic merge `a70d0b9d58f0531f345965798c4e39ec0b5e686e`. `go build ./...` and `go vet ./...` both passed on that tree (`go-build.log`, `go-vet.log`). |
| 7 | Single feature theme | PASS | Single-bead deploy. Commit range touches one subsystem/theme: integration readiness probing for provider-owned proxied-server Dolt in `test/integration/helpers_test.go`. No independent feature or planning/documentation payload is included. |

## Test detail

- Full-suite command:
  `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- bash -lc 'make test-local-full-parallel'`
- Full-suite result: rc 2 before attribution; 37 jobs passed, 3 jobs failed.
- Failed raw jobs:
  - `cmd-gc-process-1-of-6`: `TestPoolSessionCreate_TerminalProviderErrorTearsDownBeforeRollback`, tracked by `ga-z8yi2j`.
  - `integration-packages-cmd-gc-1-of-6`: same `TestPoolSessionCreate_TerminalProviderErrorTearsDownBeforeRollback`, tracked by `ga-z8yi2j`.
  - `integration-rest-full-5-of-8`: `TestGCLiveContract_BeadsAndEvents`, tracked by `ga-lejnse`.
- Focused diff-owned acceptance command:
  `go test -tags integration -count=1 -timeout 10m ./test/integration -run '^(TestIsProviderOwnedProxiedDoltCity|TestWaitForManagedDoltCityReady_ProxiedMode(ProbesWithoutPortHint|SurfacesProbeError))$' -v`
- Focused diff-owned acceptance result: rc 0; all three top-level tests PASS, with all table subtests PASS.
- Policy lane:
  `make test-ci-policy`, rc 0.

## Notes

- The mayor explicitly lifted the deploy stand-down for `ga-z09xrd` only in mail `gm-wisp-xsbi1i`.
- The raw full-suite failures were not ignored; they were attributed according to the non-diff-owned gate-failure protocol and recorded on their trackers before this gate was marked PASS.
- `TestCleanInstallTutorialPath` did not fail in this gate run, so the mayor's `ga-vkhfnj` / `ga-5korc0` attribution path was not used.
