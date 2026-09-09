# Release gate: pack-cache fast validation

- Deploy bead: `ga-zaerju`
- Source bead: `ga-jwa1c5`
- Review context bead: `ga-nalx9q`
- Pull request: `https://github.com/gastownhall/gascity/pull/5387`
- Reviewed and gated commit: `14819b9e8da69e6fee157cc1719756000d1c2808`
- Base: `origin/main@ae5163dd3e0a7402484c477f282eaa95bb3e7377`
- Merge base: `e425e6691dc8ebf63cab16e5dfcc1f3bb6043296`
- Stable patch ID: `d8978e4796ab883a7daa36c7f76ad8a417282fc4`
- Deploy mode: remote (`gastownhall/gascity`)
- Gate result: **PASS**

This is the fresh gate run requested after the PR moved beyond the earlier
review-carryover head. The reviewer recorded a new exact-head PASS for
`14819b9e8da69e6fee157cc1719756000d1c2808`; no earlier review or test result is
used as a substitute for that exact-head verdict or this gate's independent
verification.

The target pre-flight ran before criterion 6. PR #5387 is open, internally
authored by `quad341`, and `MERGEABLE` / `CLEAN`. Discussion participants are
only `quad341` and recognized automation (`gascityinc-olivia`). At the final
freshness check the PR still had the exact reviewed head and GitHub reported
80 successful checks, 24 suite-controlled skips, and no failing, pending, or
cancelled check.

## Checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | The reviewer appended a fresh, exact-head PASS to `ga-zaerju` for `14819b9e8da69e6fee157cc1719756000d1c2808`. The review independently inspected the two commits after the formerly reviewed head, ran build/vet/tests, and found no security, specification, or coverage defect. This is a new review, not patch-ID carryover. |
| 2 | Acceptance criteria met | **PASS** | The ready path retains full synthetic-repository validation while reusing only positive verdicts whose tree fingerprint is unchanged. The fingerprint covers file path, size, mtime, permission bits, and every directory path; ordinary corruption, mode changes, and unexpected directories invalidate the memo and force validation. Negative verdicts are never memoized. The deliberately documented same-size/restored-mtime blind spot remains isolated to follow-up `ga-77cgcb`. |
| 3 | Tests pass | **PASS with attribution** | `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true LOCAL_TEST_JOBS=4 GO_TEST_TIMEOUT=30m make test-local-full-parallel` ran the documented full 40-job local CI union: 38 PASS / 2 raw FAIL / 0 skipped jobs. All six `cmd-gc-process` jobs, all six `integration-packages-cmd-gc` jobs, and `unit-core` passed. The two raw failures satisfy the four-clause attribution rule under 3a. An explicit exact-head run resolved all three diff-owned tests by name: 3 PASS / 0 FAIL / 0 SKIP. `test_cmd_scope: full-suite`; `waiver_ref: none`; `ci_lane_run: n/a (no CI-config change)`. Full logs: `/var/tmp/gc-local-tests.uz3ikB`; driver log: `/var/tmp/ga-zaerju-deploy-full.log`. |
| 3a | Pre-existing failures attributable | **PASS** | `TestBdFlagManifestCurrent` is attributed to predating tracker `ga-f0uceo`: the test reads installed-`bd` help against `internal/bdflags`, while this candidate changes neither that package nor the installed binary; there is no path or mechanism overlap. `TestE2E_SuspendResume_City` is attributed to predating tracker `ga-dc9utn` and fix bead `ga-pmafyc`: the tracker records the identical deterministic 93-second missing-`citysus.report` failure on exact current base `ae5163dd3e`, with the proven root cause in the session-reconciler suspend/wake path; the candidate touches none of that path or the failing test file. Both sightings were appended to their trackers. |
| 3b | Policy/lint lane | **PASS** | `make test-ci-policy`, merge-base-scoped `make fmt-check-changed`, `go vet ./...`, `go build ./...`, and `git diff --check` all exited 0 on the exact reviewed checkout. |
| 3c | CI-config lane | **PASS (n/a)** | The candidate changes no workflow, job, matrix, timeout, or required-check configuration. GitHub's actual exact-head CI is independently green: 80 SUCCESS / 24 suite-controlled SKIP / 0 other. |
| 4 | No high-severity review findings open | **PASS** | The exact-head reviewer found no style, security, specification, coverage, or unresolved HIGH issue. The comments/test-message cleanup closes the previously deferred cosmetic nit. |
| 5 | Final branch is clean | **PASS** | The isolated detached checkout at the exact reviewed SHA was clean after the full test and policy runs. The gate artifact is the deployer's only new repository file. |
| 6 | Branch diverges cleanly from main | **PASS** | Evaluated first after the open-PR pre-flight. `git merge-tree --write-tree origin/main 14819b9e8da69e6fee157cc1719756000d1c2808` exited 0 against `origin/main@ae5163dd3e0a7402484c477f282eaa95bb3e7377` and produced tree `1c8327766e4c771099aa16293a239f556b573023`. GitHub independently reports `MERGEABLE` / `CLEAN`; no bounded self-rebase was needed. |
| 7 | Single feature theme | **PASS** | The merge-base range changes exactly `cmd/gc/embed_builtin_packs.go`, `cmd/gc/synthetic_cache_verifier.go`, and `cmd/gc/synthetic_cache_verifier_test.go`, all for one synthetic-pack-cache validation optimization and its regressions. `assert_deploy_ancestry_scope` passes for confirmed related beads `ga-zaerju`, `ga-jwa1c5`, and `ga-nalx9q`; no `.claude/**` path or unrelated commit is present. |

## Test evidence

The rootless Podman socket returned `OK` before the run. The locally cached
images include the repository-pinned `docker.io/dolthub/dolt:2.1.7` at digest
`sha256:22319531c51c2fb2ca3639ad284d0ff9a98b55c25c6ba4ebeefbf7769e663916`
and `docker.io/dolthub/dolt-sql-server:2.2.0` at digest
`sha256:baf9449a54dc6572dc5d688386e6f45ecb9f76e5bc4869be429f450b90ffecb0`.
Ryuk remained disabled as required by the fleet test-container protocol.

- Full-suite command:
  `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true LOCAL_TEST_JOBS=4 GO_TEST_TIMEOUT=30m make test-local-full-parallel`
  - `test_cmd_scope: full-suite`
  - `test_counts: 38 PASS / 2 raw FAIL / 0 SKIP jobs`
  - The two raw FAIL results remain present in the logs and are attributed
    below; no rerun erased or replaced them.
- Diff-owned tests, resolved by explicit name on the same exact checkout:
  `go test -count=1 -v ./cmd/gc -run '^(TestWarmSyntheticCacheVerifierReusesPositiveVerdictsAcrossPasses|TestWarmSyntheticCacheVerifierNoticesAModeChange|TestWarmSyntheticCacheVerifierNoticesAnUnexpectedDirectory)$'`
  - `TestWarmSyntheticCacheVerifierReusesPositiveVerdictsAcrossPasses`: PASS
  - `TestWarmSyntheticCacheVerifierNoticesAModeChange`: PASS
  - `TestWarmSyntheticCacheVerifierNoticesAnUnexpectedDirectory`: PASS
  - `diff_tests_executed: 3 PASS / 0 FAIL / 0 SKIP`
  - `waiver_ref: none`
- Required supporting lanes:
  - `policy_lane: make test-ci-policy` — PASS
  - `LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=e425e6691dc8ebf63cab16e5dfcc1f3bb6043296 make fmt-check-changed` — PASS
  - `go vet ./...` — PASS
  - `go build ./...` — PASS
  - `git diff --check e425e6691dc8ebf63cab16e5dfcc1f3bb6043296..14819b9e8da69e6fee157cc1719756000d1c2808` — PASS

## Failure attribution

- `failure_attribution: TestBdFlagManifestCurrent -> ga-f0uceo | clause 1: not diff-owned; clause 2: tracker opened 2026-08-15 and covers the exact installed-bd manifest condition; clause 3(a): the candidate cannot change the installed bd binary or internal/bdflags manifest; clause 4: no path overlap.`
- `failure_attribution: TestE2E_SuspendResume_City -> ga-dc9utn (fix ga-pmafyc) | clause 1: not diff-owned; clause 2: exact-signature tracker predates this failure; clause 3(a)/(d): proven reconciler suspend/wake root cause and identical exact-base-alone failure at ae5163dd3e; clause 4: no failing-test or reconciler-path overlap.`
- `inconclusive-guard: n/a — both failures have affirmative clause-3 proofs.`
- `waiver_ref: none`

## Disposition

All seven criteria pass for PR #5387 at exact head
`14819b9e8da69e6fee157cc1719756000d1c2808`. This bead gates an existing open
PR, so the deployer must not open a duplicate PR or mutate its reviewed branch.
After one final head/mergeability/status check, publish
`release-gate/deploy-clearance=success` on that exact commit and send the
machine-actionable merge request to the mayor pinned to the same SHA.
