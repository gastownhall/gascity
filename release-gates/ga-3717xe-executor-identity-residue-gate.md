# Release gate: executor-identity residue doctor sweep

- Deploy bead: `ga-3717xe`
- Build bead: `ga-cm2o5t.1.2`
- Review bead: `ga-o0sl9f`
- Reviewed source: `4698739c7e496319ca16d8b33936464214a11a4e`
- Base checked: `origin/main@c6bb2aa30dd660c9dd9f955f9410e1e6fc202817`
- Deploy branch: `deploy/ga-3717xe-gate`
- Deploy mode: remote; push target: `fork`
- Evaluated: 2026-09-07
- Verdict: **PASS with three attributed, non-diff-owned test failures and one attributed lint-lane failure**

## Gate checklist

The target pre-flight ran before criterion 6. The reviewed SHA resolved to the
full commit above and is not carried by an existing pull request. Criterion 6
was checked first and passed against the freshly fetched base, so the remaining
criteria were evaluated.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review-of-record `ga-o0sl9f` is closed with `verdict: pass` on the exact reviewed source. No review carryover is involved. |
| 2 | Acceptance criteria met | **PASS** | The new `executor-identity-residue` doctor check enumerates the city plus active configured rigs, re-derives the current route for every bead, skips `in_progress` work, clears only `gc.session_name`, `gc.work_dir`, and legacy `work_dir` with partial-failure aggregation, retains `gc.routed_to`, reports through the established doctor result helpers, and is excluded from startup warmup. Tests cover stale cleanup, the in-progress guard, idempotence, canonical session-name equivalence, registration, preflight counts, and the ordered doctor-check catalog. No hardcoded role or point-in-time bead list was added. |
| 3 | Tests pass | **PASS with attributed raw failures** | The documented full-scope 40-job command completed **37 PASS / 3 raw FAIL / 0 SKIP jobs**, with zero observed `--- SKIP` test events. Every diff-owned test listed below ran in a green process shard and a green integration-package shard. The three raw failures are attributed under criterion 3a. `test_cmd_scope: full-suite`; `waiver_ref: none`. |
| 3a | Pre-existing failures attributed | **PASS** | `TestBdFlagManifestCurrent` is tracked by `ga-f0uceo`; `TestE2E_SuspendResume_City` by `ga-dqd7gf`; and the previously unseen missing-attempt-2 recovery condition is tracked by `ga-j88sfp`. The first two trackers predate the run. The third was created during this discovering run only after a decisive mechanism proof landed: the changed check can run only under `gc doctor`, is `WarmupEligible=false`, and its completed unit shards were no longer concurrent when the recovery lane started. No failing test path overlaps the diff. Sightings were written to the system of record and read back. |
| 3b | Policy and static lanes | **PASS with one attributed raw lint failure** | `make test-ci-policy`, `make check-gomod-replace`, `make check-native-dependency-surface`, `make check-eventexport-isolation`, `make check-core-boundary`, `make test-native-doltlite-beads`, `make fmt-check-changed`, `make check-docs`, `go build ./...`, `go vet ./...`, and `git diff --check origin/main...HEAD` all PASS. `make lint-affected` conservatively widened to the full repository for the stale reviewed head, then replayed diagnostics from a deleted `/var/tmp/gc-maintainer-fix...` checkout and ignored dashboard `node_modules`; exact predating tracker `ga-u8z8j6` covers the condition, and no candidate-owned path appears in the output. |
| 3c | CI-config lane | **PASS — n/a** | `ci_lane_run: n/a (no CI job, matrix, timeout, workflow, or required-check change)`. |
| 4 | No high-severity review findings open | **PASS** | Unresolved HIGH findings: 0. The review records no blocking correctness, security, style, or coverage finding. |
| 5 | Final branch is clean | **PASS** | `git status --porcelain=v1` was empty on the exact reviewed source after all test and policy checks and before this checklist was written. |
| 6 | Branch diverges cleanly from main | **PASS** | `git merge-tree --write-tree --messages origin/main 4698739c7e496319ca16d8b33936464214a11a4e` returned tree `78361b2b809166a2463e23770a6121cd34a82269` with exit 0 and no conflict messages against `origin/main@c6bb2aa30dd660c9dd9f955f9410e1e6fc202817`. The candidate was 8 commits behind and 2 ahead when checked. No self-rebase was needed. |
| 7 | Single feature theme | **PASS** | Two TDD commits and seven files implement one `gc doctor` theme: detect and clear stale executor-identity stamps while preserving live work and current routing. |

## Full-suite test evidence

`test_cmd_scope: full-suite`

```text
export gc_test_gitconfig=caller-export-seed
DOCKER_HOST=unix:///run/user/1000/podman/podman.sock \
TESTCONTAINERS_RYUK_DISABLED=true \
LOCAL_TEST_LOG_DIR=/var/tmp/ga-3717xe-gate.tnlchT/jobs \
LOCAL_TEST_JOBS=4 \
CMD_GC_PROCESS_TOTAL=6 \
GO_TEST_TIMEOUT=30m \
make test-local-full-parallel
```

The rootless Podman socket was reachable before the run. The repository-pinned
`docker.io/dolthub/dolt:2.1.7` image was pulled and resolved to digest
`sha256:22319531c51c2fb2ca3639ad284d0ff9a98b55c25c6ba4ebeefbf7769e663916`.
The caller export is an environment-only compatibility setup for this reviewed
head, which predates the landed `ga-cesmzs` / PR #6116 runner fix; the script
overwrites the seed with its real generated gitconfig path while retaining the
export attribute. The first setup-only invocation omitted that export, aborted
before any test ran, and is not counted as evidence.

- `test_counts: 37 PASS jobs, 3 attributed raw FAIL jobs, 0 SKIP jobs`
- `raw_test_failures: 3 attributed FAIL, none diff-owned`
- `skip_justification: n/a; zero explicit test skips were observed`
- `waiver_ref: none`
- Full-run log: `/var/tmp/ga-3717xe-gate.tnlchT/full-suite.log`
- Per-job logs: `/var/tmp/ga-3717xe-gate.tnlchT/jobs`

`diff_tests_executed` — all PASS in green `cmd-gc-process` and
`integration-packages-cmd-gc` shards:

- `TestExecutorIdentityResidueCheckFlagsAndFixesStaleStamp`
- `TestExecutorIdentityResidueCheckSkipsInProgressBead`
- `TestExecutorIdentityResidueCheckFixIsIdempotent`
- `TestExecutorIdentityResidueCheckAllowsCanonicalSessionNameEncoding`
- `TestBuildDoctorChecks_SkipsStoreChecksWhenStoreUnreachable`
- `TestBuildDoctorChecks_RegistersStoreChecksWhenStoreReachable`
- `TestBuildDoctorChecks_RigStoreNameSetPreflight`
- `TestBeadStorePreflightSkipCount`
- `TestBuildDoctorChecks_NameSetUnchanged`

## Raw failures and attribution

| Raw result | Test | Tracker / proof |
|---|---|---|
| **FAIL — ATTRIBUTED** | `TestBdFlagManifestCurrent` | Open tracker `ga-f0uceo`, created 2026-08-15. Clause 3(a), mechanism: this test compares the host-installed `bd` surface with `internal/bdflags`; the candidate changes neither. `internal/bdflags` cannot import the changed `cmd/gc` main package, and there is no path overlap. |
| **FAIL — ATTRIBUTED** | `TestRetryManagedPooledWorkerRecoversClaimedAttemptAfterCrash` | Tracker `ga-j88sfp`, created during this discovering run under the landed-proof exception. Clause 3(a), mechanism: the failure occurred in a formula-recovery workflow that never invokes `gc doctor`; the new check is excluded from startup warmup and therefore cannot execute on this path. The candidate adds no suite target or resource-census load, and its unit-test shards completed before this lane started. There is no path overlap. |
| **FAIL — ATTRIBUTED** | `TestE2E_SuspendResume_City` | Open tracker `ga-dqd7gf`, created 2026-09-02. Clause 3(a), mechanism: the exact tracked full-suite condition timed out waiting for `citysus.report`; the scenario invokes city suspend, session kill, and city resume, never `gc doctor`. The new check is excluded from startup warmup, and no test path overlaps. |

`failure_attribution`:

- `TestBdFlagManifestCurrent -> ga-f0uceo | clause 3(a) mechanism — installed-binary/source-manifest skew; candidate unreachable`
- `TestRetryManagedPooledWorkerRecoversClaimedAttemptAfterCrash -> ga-j88sfp | clause 3(a) mechanism — doctor-only, non-warmup change cannot execute in formula recovery`
- `TestE2E_SuspendResume_City -> ga-dqd7gf | clause 3(a) mechanism — tracked report timeout in suspend/resume path; doctor-only change cannot execute`

`inconclusive-guard: n/a — all three attributions have decisive mechanism
proof.` The diff changes no resource-census baseline and adds no suite target.

## Policy-lane attribution

`policy_lane: required policy/static commands PASS; make lint-affected raw FAIL attributed to ga-u8z8j6`

`LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main make lint-affected`
selected full-repository scope because the older candidate lacks a path deleted
on current main. Golangci then replayed 758 findings from a removed
`/var/tmp/gc-maintainer-fix.4547.S6Jy0t` checkout plus three findings under
ignored dashboard `node_modules`. This is the exact stale-head scope/cache leak
tracked by open `ga-u8z8j6`, created 2026-08-25. The candidate changes none of
those paths, no candidate-owned path appears in the output, and the independent
policy, build, vet, format, docs, and boundary checks all pass. The sighting was
appended and read back. Log:
`/var/tmp/ga-3717xe-policy.5r4QH8/lint-affected.log`.

## Pre-flight and ancestry evidence

- The recorded commit passed a hex-only guard and resolved to the full reviewed
  SHA above.
- `gh api repos/gastownhall/gascity/commits/4698739c7e496319ca16d8b33936464214a11a4e/pulls`
  returned no pull request, so no already-merged or closed-PR reconciliation
  applies.
- `assert_deploy_ancestry_scope origin/main 4698739c7e496319ca16d8b33936464214a11a4e ga-3717xe ga-cm2o5t.1.2`
  passed. The accepted sibling is the confirmed build bead cited by both TDD
  commits; no unrelated commit or `.claude/**` path rides in the range.
- No `PR-DESCRIPTION` block or `gc.pr_ping` metadata is present.

## Disposition

All seven criteria pass. Cut the isolated `deploy/ga-3717xe-gate` branch at the
reviewed SHA, commit this checklist, push it to the fork, open the PR, publish
deploy clearance on the exact gated head, and route the merge-request to the
mayor. The deployer does not merge.
