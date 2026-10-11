# Release gate: aggregate Container Scan image enforcement

- Deploy bead: `ga-t600qs`
- Build bead: `ga-hv6nw5`
- Review bead: `ga-tgrdp2`
- Reviewed commit: `f9e383074bd8e0c22505c74428baa87b4505704b`
- Reviewed-source merge base: `e3bee4cd15f279ad11abe9257014e413926b41ba`
- Base: `origin/main@c5fd5fb9683c3b85198e3675af874e090cd45584`
- Deploy mode: remote (`gastownhall/gascity`)
- Gate result: **PASS with attributed non-diff failures**

The pre-flight lookup found no pull request associated with the reviewed
commit, so the normal release gate applied. Criterion 6 was evaluated first
and required no bounded self-rebase.

## Checklist

| # | Criterion | Result | Evidence |
| --- | --- | --- | --- |
| 1 | Review PASS present | **PASS** | Review bead `ga-tgrdp2` is closed with reason `pass`, records `REVIEWER VERDICT: PASS`, and pins the exact reviewed commit. |
| 2 | Acceptance criteria met | **PASS** | The enforcement step now guards each `trivy image` call, scans all four configured images, logs a per-image `PASS:` or `FAIL:`, accumulates failures in `overall_rc`, and exits with that aggregate. The four image names and the `--severity HIGH,CRITICAL`, `--ignore-unfixed`, and `--ignorefile .trivyignore.yaml` flags remain unchanged. The diff does not touch `.trivyignore.yaml`, `scripts/container_tool_security_test.go`, image contents, dependency pins, or vulnerability remediation. |
| 3 | Tests pass | **PASS with attribution** | The documented full-scope command completed all 40 jobs: **38 PASS / 2 raw FAIL / 0 SKIP**. The logs contain 377 package-level `ok` results, two package failures, and no named SKIP. Both failures satisfy all four non-diff attribution clauses below. The diff-owned regression test passed by explicit name. |
| 3a | Pre-existing failures attributable | **PASS** | `TestBdFlagManifestCurrent` is tracked by `ga-f0uceo`; `TestE2E_SuspendResume_City` is tracked by `ga-dqd7gf`. Both trackers predate this run, cover the exact signatures, have independent base/cross-candidate evidence, and received verified sightings for this run. Neither failing path overlaps or can execute the candidate workflow/static-test diff. |
| 3b | Policy/lint lane | **PASS with attribution** | `make test-ci-policy`, `make fmt-check`, and `make vet` exited 0. Full-scope `make lint` reported only two `govet` and one `revive` diagnostic in ignored third-party `internal/api/dashboardspa/web/node_modules/flatted/...`; exact clean-main reproduction is tracked by predating bug `ga-bvixfw`, and this run's sighting was comment-verified. No candidate-owned file produced a diagnostic. |
| 3c | CI-config lane | **PASS with attributed job failure** | `ci_lane_run`: [run 33628277602, job 100241213629](https://github.com/gastownhall/gascity/actions/runs/33628277602/job/100241213629), exact reviewed SHA, completed (not timeout/partial). The modified step reached all four images, emitted four per-image `FAIL:` summaries, and exited 1 in aggregate. The failure condition is the pre-existing unwaived bundled-image vulnerability set tracked by `ga-ei3zwo`; scheduled main [run 33625891833](https://github.com/gastownhall/gascity/actions/runs/33625891833) failed the same condition 27 minutes earlier. The candidate changes control flow and a static test only, not image inputs or waiver policy. |
| 4 | No high-severity review findings open | **PASS** | The reviewer recorded no style, security, specification, or coverage findings; unresolved HIGH count is 0. |
| 5 | Final branch is clean | **PASS** | `git status --porcelain` was empty at the reviewed commit before this gate record was created; `git diff --check origin/main...HEAD` also passed. |
| 6 | Branch diverges cleanly from main | **PASS** | After fetching current main, `git merge-tree --write-tree origin/main f9e383074bd8e0c22505c74428baa87b4505704b` exited 0 and produced tree `c8b8cc56343c04fe607ba62ae311d60954442e77`. |
| 7 | Single feature theme | **PASS** | `assert_deploy_ancestry_scope` passed for `ga-t600qs` and confirmed source bead `ga-hv6nw5`. Both commits and both changed files implement or test one behavior: aggregate, per-image Container Scan enforcement. |

## Test evidence

Container preflight found the rootless Podman socket at
`/run/user/1000/podman/podman.sock`, set
`TESTCONTAINERS_RYUK_DISABLED=true`, and confirmed the repository-pinned
`dolthub/dolt-sql-server` and Testcontainers Ryuk images were cached.

- `test_cmd: DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true GO_TEST_TIMEOUT=30m make test-local-full-parallel`
- `test_cmd_scope: full-suite`
- `test_counts: 38 PASS jobs / 2 raw FAIL jobs / 0 SKIP jobs (40 total); 377 package ok / 2 package fail / 0 named skip`
- Raw logs: `/var/tmp/gc-local-tests.Gc7xvk`
- `diff_tests_executed: TestContainerScanImagePolicyDoesNotFailFast PASS`
  - The full suite's `unit-core` job passed `github.com/gastownhall/gascity/scripts`.
  - Fresh named confirmation: `go test -json -count=1 ./scripts -run '^TestContainerScanImagePolicyDoesNotFailFast$'` emitted a real `pass` event.
- `skip_justification: none — no local-suite job or named test skipped`
- `waiver_ref: none — all raw failures are non-diff-owned and satisfy criterion 3a attribution`
- `policy_lane: make test-ci-policy — PASS; make fmt-check — PASS; make vet — PASS; make lint — raw FAIL attributed to ga-bvixfw`

## Failure attribution

The candidate changes only:

- `.github/workflows/container-scan.yml`
- `scripts/container_scan_fail_fast_test.go`

Each local-suite failure is outside that diff, has a tracker predating this
run, has independent evidence that the condition is not caused by this
candidate, and has no path overlap:

- `failure_attribution: TestBdFlagManifestCurrent -> ga-f0uceo | clause 3(a), mechanism/cross-candidate proof — the host's installed bd exposes flags absent from internal/bdflags; workflow YAML and a scripts-package static test cannot alter either surface.`
  - Log: `/var/tmp/gc-local-tests.Gc7xvk/integration-packages-core-1-of-4.log`.
  - Sighting comment: `e89883ef-2782-5872-b240-f9f4e35a21bd`.
- `failure_attribution: TestE2E_SuspendResume_City -> ga-dqd7gf | clause 3(a), mechanism/base proof — the test timed out waiting for citysus.report under the 40-job load. The candidate's static test had already completed in unit-core, and workflow YAML cannot enter the integration binary.`
  - Log: `/var/tmp/gc-local-tests.Gc7xvk/integration-rest-full-2-of-8.log`.
  - Sighting comment: `9becac9d-997e-56cb-8faf-dedf030e9e69`.

Policy and CI-lane raw failures are likewise retained rather than hidden:

- `policy_attribution: node_modules/flatted govet/revive findings -> ga-bvixfw | exact clean-main reproduction predates this run; ignored third-party path has no overlap with the candidate.`
  - Sighting comment: `c0ae2f9d-0ee1-5203-8840-ef4811b6b3aa`.
- `ci_lane_attribution: Image vulnerabilities -> ga-ei3zwo | clause 3(d), base-ref run 33625891833 reproduced the unwaived bundled-image vulnerability condition before candidate run 33628277602; image contents and waiver policy are unchanged.`
  - Sighting comment: `5ed4dfb5-cac5-53be-a457-c9d169f392b8`.

`inconclusive-guard: n/a — an independent clause-3 proof landed for every raw failure.`

## Disposition

All applicable release criteria pass. Commit this checklist as the sole
deploy-only addition, push an isolated `deploy/ga-t600qs-gate` branch, open the
pull request, publish deploy clearance on its exact head, and route merge
authority to the mayor. The deployer does not merge.
