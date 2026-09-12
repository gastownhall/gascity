# Release gate: Dashboard SPA Playwright install timeouts

- Deploy bead: `ga-29lvns`
- Review bead: `ga-luwbo3`
- Reviewed commit: `dbffcbe02523f13b3a3bdcf7fe31bc0bd7ecedc0`
- Current base: `origin/main@1be466f69a4c74b7721a36e5978244556396c2d5`
- Isolated branch: `deploy/ga-29lvns-gate`
- Gate state: **HOLD — first real Dashboard SPA CI run required**

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | `ga-luwbo3` records `verdict: pass` for the exact reviewed commit `dbffcbe02523f13b3a3bdcf7fe31bc0bd7ecedc0`; no review carryover was used. |
| 2 | Acceptance criteria met | PASS | The Playwright install step writes apt HTTP timeout `15` before its three-attempt loop, bounds each `npm run test:e2e:install:ci` invocation with `timeout 240`, retains the 12-minute outer step timeout, and preserves backoff/final failure. The diff-owned policy test and an induced-stall control-flow simulation pass. |
| 3 | Tests pass | HOLD | Local full-suite, policy, build, vet, actionlint, and diff-owned evidence pass after attribution. Because this diff changes the Dashboard SPA CI job, criterion 3c requires that job's first real PR execution; no such run exists before a PR is opened. `ci_lane_run: not-yet-run`. |
| 4 | No unresolved HIGH review findings | PASS | Reviewer reported no blockers, majors, or minors; unresolved HIGH count is 0. |
| 5 | Final branch clean | PASS | The reviewed tree is clean; `git diff --check` and changed-file formatting/lint pass; `.githooks` is configured as `core.hooksPath`. |
| 6 | Branch diverges cleanly from main | PASS | After a fresh fetch, `git merge-tree --write-tree origin/main dbffcbe02523f13b3a3bdcf7fe31bc0bd7ecedc0` exited 0 and produced tree `908d61961c0ee8af186dbaefa4092da14ded224b`; no self-rebase was needed. |
| 7 | Single feature theme | PASS | The three-file diff is one CI-hardening theme: workflow time bounds plus the policy implementation and regression test that enforce them. |

## Criterion 2: acceptance evidence

- Exact workflow literals and ordering are enforced by `TestPlaywrightChromiumInstallHardensAgainstHungAptMirror`: apt `Acquire::http::Timeout "15"` is configured before the retry loop, each attempt uses `timeout 240 npm run test:e2e:install:ci`, and the step retains `timeout-minutes: 12`.
- The existing three-attempt loop, linear 10/20-second backoff, success exit, and final failure exit are unchanged.
- Induced-stall simulation used a one-second stand-in timeout around a ten-second sleeper and observed attempts 1, 2, and 3 before final failure (`simulation_result=PASS attempts=3 elapsed_seconds=3`). This verifies that a timed-out command advances through the retry loop; the policy test separately locks the production timeout values.
- `actionlint .github/workflows/ci.yml`: PASS.
- `make test-ci-policy`: PASS.

## Criterion 3: local test evidence

`test_cmd: make test-local-full-parallel`

`test_cmd_scope: full-suite`

Environment: rootless Podman 5.8.4 via `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock`; `TESTCONTAINERS_RYUK_DISABLED=true`. The repository has no Go testcontainers reference or pinned testcontainer image, and this diff adds no container-backed test.

- Raw result: all 40 job logs completed; 29 job-level PASS and 11 job-level FAIL.
- Top-level counts across preserved verbose logs: 46,467 PASS, 12 FAIL, 210 SKIP.
- `diff_tests_executed`: `TestPlaywrightChromiumInstallHardensAgainstHungAptMirror` PASS in `unit-core` and PASS again in `integration-packages-core-4-of-4`; 0 diff-owned FAIL and 0 diff-owned SKIP.
- Skip justification: the 210 skips are suite-declared platform, optional-provider, live-infrastructure, helper-process, or opt-in persistence exclusions. None is diff-owned.
- Build/static evidence: `go build ./...` PASS; `go vet ./...` PASS; `make fmt-check-changed` PASS; `make lint-changed` PASS for `./scripts/cipolicy`; `git diff --check` PASS; `actionlint .github/workflows/ci.yml` PASS.
- `policy_lane: make test-ci-policy — PASS`. This is the policy target invoked by `.github/workflows/ci.yml`; the repository has no `ci-pr-policy` target.
- `waiver_ref: none` — failures below are attributed under the non-diff-owned failure protocol, not waived.

### Failure attribution

Every tracker predates this run, each current sighting was appended and read back, no failing test file overlaps the diff, and `go list -deps -test` confirms the failing `cmd/gc`, `internal/runtime/acp`, `internal/runtime/tmux`, and integration packages do not import `scripts/cipolicy`.

- `TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix`, `TestAdoptPRFormulaCompileAndRun`, `TestPersonalWorkFormulaCompileAndRun`, `TestAdoptPRFormulaRetriesTransientReviewerStep`, `TestAdoptPRFormulaSoftFailsGeminiAfterTransientRetries`, `TestRetryManagedPooledWorkerRecoversClaimedAttemptAfterCrash`, `TestHumaBinary_CityCreateAsync`, and `TestGCLiveContract_BeadsAndEvents` -> `ga-esyijp`. Proof: each failed during fixture bootstrap on the exact shared-server pending-schema-migration refusal before scenario behavior ran. The workflow/policy diff cannot affect Dolt schema migration or `bd init`.
- `TestE2E_SuspendResume_City` -> `ga-dc9utn`. Proof: exact 93.83-second missing `citysus.report` signature; the tracker documents the independently proven reconciler suspend/wake defect and its fix bead. This diff cannot reach that path.
- `TestIsAgentRunning` -> `ga-vzckwr`. Proof: exact tracked transient setup-shell signature (`current cmd: zsh`, `setup cmd: sh`) in both matching-shell subtests. The open fix bead predates this run; the diff does not touch or reach tmux runtime code.
- `TestStartSeedsDurableActivity` and `TestDoltStateWaitReadyCmdReturnsReady` -> `ga-vkhfnj`. Proof: both exhausted fixed readiness deadlines under the same 40-job shared-host contention run (5-second ACP initialize and one-second fake-Dolt readiness respectively). Their packages cannot import the only changed Go package and their test paths do not overlap the diff.

The diff adds one ordinary policy test function, not a new suite target and not a resource-census bump. All attributions use affirmative mechanism or import-coverage proof, so the inconclusive added-load guard does not apply.

### CI-config lane evidence

`ci_lane_run: not-yet-run`

The diff modifies `.github/workflows/ci.yml`, specifically the real Dashboard SPA job. Policy/meta-tests can prove the YAML and enforced literals, but cannot prove the changed install command completes on the hosted runner. An isolated draft PR will be opened only to obtain that first real execution. Until the Dashboard SPA job finishes successfully, this gate remains HOLD: no deploy-clearance status, no ready-for-review transition, no bead close, and no merge-request.
