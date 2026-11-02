# Release Gate: quarantined workflow finalizer root settlement

Result: PASS
Date: 2026-09-07

## Scope

- Deploy bead: `ga-6nnqo2`
- Review bead: `ga-zje19p`
- Source beads: `ga-japz50`, `ga-li4qa4`
- Source branch: `builder/ga-japz50` (provenance only)
- Isolated deploy branch: `deploy/ga-6nnqo2-gate`
- Reviewed commit: `58e989a1b41f6ac74ea8d3d204415ab2ae0baecd`
- Base evaluated: `origin/main` at `f3610ae8e69a05a72c2f9639be8a5754907b99cb`
- Deploy mode: remote; push remote: `origin`
- Existing PR check: GitHub returned no pull requests for the reviewed commit.
- `docs/PROJECT_MANIFEST.md` is absent from both the candidate and current base, so this record applies the current deployer protocol's seven release criteria and the source beads' acceptance contract.

## Release Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead `ga-zje19p` is closed with `verdict: pass`, `tests_green: true`, no style or security findings, and reviewed commit `58e989a1b41f6ac74ea8d3d204415ab2ae0baecd`. |
| 2 | Acceptance criteria met | PASS | The candidate closes an open workflow root when its `workflow-finalize` control bead is quarantined; leaves non-finalizer and already-settled roots alone; and turns a root-close refusal into durable root metadata, a reconciliation bead, and typed `control.root_settle_failed` event. All eight diff-owned behavioral and typed-event tests passed in the independent deployer run. |
| 3 | Tests pass | PASS | The documented full local CI union completed 38/40 jobs successfully. Its two red jobs are independently attributable, pre-existing conditions outside this diff: installed-`bd` flag-manifest drift (`ga-f0uceo`) and the full-load city suspend/resume report timeout (`ga-dqd7gf`). The independent full-unit baseline recorded 42,974 PASS, 0 FAIL, and 189 expected fast-tier SKIPs; every diff-owned test explicitly reported PASS. The policy lane, build, vet, dashboard CI, and live dashboard preview also passed. |
| 4 | No high-severity review findings open | PASS | Reviewer notes report no style or security findings and no unresolved high-severity finding. The requested-change round was satisfied by the reviewed durable-visibility implementation. |
| 5 | Final branch is clean | PASS | `git status --short --branch` showed only detached-HEAD/branch identity, with no changes. `git diff --check origin/main...HEAD` and `gofmt -l` over all changed Go files produced no output. Dashboard regeneration left the candidate tree unchanged. |
| 6 | Branch diverges cleanly from main | PASS | After a final `git fetch origin main`, `git merge-tree --write-tree --messages origin/main 58e989a1b41f6ac74ea8d3d204415ab2ae0baecd` exited 0 and produced synthetic tree `053332180d56406e80faea5fdc09662ab37b2e64`. `assert_deploy_ancestry_scope` passed for `ga-japz50`, `ga-li4qa4`, `ga-zje19p`, and `ga-6nnqo2`. No rebase was needed. |
| 7 | Single feature theme | PASS | The five-commit, 37-file candidate is one feature theme: quarantined-finalizer root settlement, durable failure observability, typed event registration, and the generated API/dashboard projections of that event. |

## Test Evidence

- `test_cmd`: `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true make test-local-full-parallel`
- `test_cmd_scope`: `full-suite`
- `test_counts`: 38 jobs PASS, 2 jobs FAIL, 0 jobs SKIP, out of 40 jobs; both failures attributed below
- `tests_green`: true after permitted non-diff attribution
- `policy_lane`: `make test-ci-policy` — PASS
- `ci_lane_run`: n/a; the candidate does not change CI configuration
- `waiver_ref`: `mayor-2026-09-06-ga-6nnqo2-c3-clause4` — authorizes the prior exact-commit gate's same-package `TestWatcherSurvivesRotationWithoutGap` occurrence. That test did not fail in this fresh run; the waiver is cited as directed but is not needed for either current attribution.
- Full-union logs: `/var/tmp/gc-local-tests.8KQbgz`

The independent fast-unit baseline used:

```text
make test EXTRA_TEST_ENV='DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true'
```

Its JSON action log, `/var/tmp/gascity-test.jsonl.8Zou4H`, recorded 42,974 test PASS actions, 0 FAIL actions, and 189 SKIP actions. The skips are the documented `GC_FAST_UNIT=1` exclusions for process and live-infrastructure coverage; the full local union separately exercised the required process-backed and integration lanes.

`diff_tests_executed`:

- `TestQuarantineControlFailureBeadClosesWithDiagnostics`: PASS
- `TestQuarantineControlFailureBeadTruncatesReasonAtUTF8Boundary`: PASS
- `TestQuarantineControlFailureBeadSettlesRootWhenFinalizerQuarantined`: PASS
- `TestQuarantineControlFailureBeadDoesNotTouchRootForNonFinalizerControl`: PASS
- `TestQuarantineControlFailureBeadNeverDowngradesAlreadySettledRoot`: PASS
- `TestQuarantineControlFailureBeadToleratesRootCloseFailure`: PASS
- `TestControlRootSettleFailedIsAKnownEventTypeWithATypedPayload`: PASS
- `TestControlRootSettleFailedPayloadRoundTrips`: PASS

Additional passing gates:

- `make test-ci-policy`
- `go build ./...`
- `go vet ./...`
- `make dashboard-ci`
- Dashboard preview via `npm --workspace gas-city-dashboard-frontend run preview -- --host 127.0.0.1 --port 46174`; `curl` returned HTTP 200 and the generated app shell.

## Non-Diff Failure Attribution

| Failure | Tracker | Four-clause evidence |
|---------|---------|----------------------|
| `TestBdFlagManifestCurrent`: installed `bd` exposes flags absent from the checked-in manifest | `ga-f0uceo` | (1) PASS: `internal/bdflags/freshness_test.go` is not diff-owned. (2) PASS: open `gate-tracker` bead `ga-f0uceo` was created 2026-08-15 and names this exact test and condition. (3) PASS by mechanism: the test shells the installed `bd --help` and compares it with `internal/bdflags`; this candidate changes neither the installed binary nor `internal/bdflags`. (4) PASS: no path overlap. Sighting appended to the tracker. |
| `TestE2E_SuspendResume_City`: timed out waiting for `.gc-reports/citysus.report` under the 40-job union | `ga-dqd7gf` | (1) PASS: `test/integration/e2e_lifecycle_test.go` is not diff-owned. (2) PASS: open `gate-tracker` bead `ga-dqd7gf` was created 2026-09-02 and names this exact full-suite condition. (3) PASS by cross-run: the tracker records the identical report-file timeout on unrelated candidate `ga-2yq3p5`, whose diff was confined to `.trivyignore.yaml` and `scripts/container_tool_security_test.go`. (4) PASS: no path overlap. Sighting appended to the tracker. |

The raw failures remain in the full-union logs. No rerun replaced or erased them; the separate `make test` run supplies exact per-test accounting and the required fast baseline.

## Gate Decision

All seven release criteria pass. The two full-union failures satisfy the complete non-diff-owned attribution protocol, and all candidate-owned tests are green. Prepare the isolated deploy branch at the exact reviewed commit, commit this checklist, push it, and open the pull request.

## Commands Run

```text
git fetch origin main
gh api repos/gastownhall/gascity/commits/58e989a1b41f6ac74ea8d3d204415ab2ae0baecd/pulls
git merge-tree --write-tree --messages origin/main 58e989a1b41f6ac74ea8d3d204415ab2ae0baecd
assert_deploy_ancestry_scope origin/main 58e989a1b41f6ac74ea8d3d204415ab2ae0baecd ga-japz50 ga-li4qa4 ga-zje19p ga-6nnqo2
DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true make test-local-full-parallel
make test EXTRA_TEST_ENV='DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true'
make test-ci-policy EXTRA_TEST_ENV='DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true'
go build ./...
go vet ./...
git diff --check origin/main...HEAD
gofmt -l <changed Go files>
make dashboard-ci
npm --workspace gas-city-dashboard-frontend run preview -- --host 127.0.0.1 --port 46174
curl -fsS http://127.0.0.1:46174/
```
