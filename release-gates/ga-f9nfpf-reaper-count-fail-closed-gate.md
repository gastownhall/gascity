# Release Gate: ga-f9nfpf - reaper count failure fails closed

Deploy bead: `ga-f9nfpf`
Review bead: `ga-s7150s`
Build bead: `ga-ql4rt3`
Reviewed commit: `8a63f92ca3dd9b8fa8e428d5e14a0c110d8ef922`
Base: `origin/main@fae7c69647497d377c4ad89478545e296ac92f97`
Merge tree: `53a602e1d8ea46fae3d4e92277da14f366a78834`
Gate evaluated: 2026-09-16
Verdict: **PASS**

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | **PASS** | Review bead `ga-s7150s` passed exact source `8a63f92ca3dd9b8fa8e428d5e14a0c110d8ef922`. It independently reproduced the RED failure and GREEN behavior, found no blocking style or security issue, and classified this diff as the security fix for the fail-open deletion guard. |
| 2 | Acceptance criteria met | **PASS** | The type-scope guard snapshots the anomaly list around `get_sql_count`; when the count cannot be computed, it records a specific anomaly, sets `_PRUNE_SKIP=1`, and prevents the forced bulk prune. The shared count helper contract and the documented unresolved-`CITY_DB` behavior remain unchanged. The new T6 regression test passed in the focused gate run. |
| 3 | Tests pass | **PASS** | All 34 reaper shell cases passed, including the new diff-owned T6, followed by 77 top-level related Go tests with 0 failures or skips. The required 40-job full union completed 37 green jobs with 86,690 PASS / 3 attributed FAIL / 322 SKIP results, counting subtests. Every raw failure had the established concurrent-initializer random-cursor signature tracked by `ga-lejnse` and root-fix bead `ga-e2z1zb`; details are below. No test was rerun. |
| 4 | No high-severity review findings open | **PASS** | Review found no blocking issue. The only non-blocking observation was that other benign-zero `get_sql_count` consumers might merit a future audit; this change deliberately confines fail-closed behavior to the destructive type-scope gate. |
| 5 | Final branch is clean | **PASS** | `git status --short` was empty at the reviewed source before this gate record was created. Generated artifacts did not drift. |
| 6 | Branch diverges cleanly from main | **PASS** | After the final fetch, `git merge-tree --write-tree origin/main 8a63f92ca3dd9b8fa8e428d5e14a0c110d8ef922` exited 0 against `origin/main@fae7c69647497d377c4ad89478545e296ac92f97`, producing tree `53a602e1d8ea46fae3d4e92277da14f366a78834`. The mandatory ancestry guard passed for deploy `ga-f9nfpf`, review `ga-s7150s`, and build `ga-ql4rt3`; no unrelated or `.claude/**` change is present. |
| 7 | Single feature theme | **PASS** | Two TDD commits change only the shipped reaper script and its shell regression test for one safety theme: a failed destructive-scope count must fail closed. |

## Criterion 3 evidence

The focused command ran through the repository isolation wrapper with
`DOCKER_HOST` and `TESTCONTAINERS_RYUK_DISABLED` set. It executed all five
reaper shell suites, then:

```text
go test -run '^TestReaper' ./examples/gastown/... -v
go test ./internal/bootstrap/packs/core/... -v
```

The focused result was 34 shell PASS plus 77 top-level Go PASS, 0 FAIL, and
0 SKIP. The new diff-owned case was:

- `T6: type-scope count uncomputable (get_sql_count failed) -> prune skipped, anomaly recorded` — PASS

The authoritative full-union command was:

```text
DOCKER_HOST=unix:///run/user/1000/podman/podman.sock
TESTCONTAINERS_RYUK_DISABLED=true
GOFLAGS=-v
GO_TEST_TIMEOUT=30m
LOCAL_TEST_JOBS=4
CMD_GC_PROCESS_TOTAL=6
make test-local-full-parallel
```

It ran through the repository isolation wrapper. Logs are retained at
`/var/tmp/ga-f9nfpf-full-logs`, with the coordinator log at
`/var/tmp/ga-f9nfpf-full.log`.

`test_cmd_scope: full-suite plus all five non-CI reaper shell suites`

`test_counts: 86,690 PASS, 3 attributed FAIL, 322 SKIP`

The 322 skips are existing platform, provider, and opt-in integration guards;
none is diff-owned. The three raw failures were:

- `TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix` in
  `cmd-gc-process-4-of-6`: fresh `hq` observed at schema v48, then refused the
  18 pending migrations to v66.
- `TestCleanInstallTutorialPath` in `integration-rest-full-2-of-8`: fresh `hq`
  observed at schema v50, then refused the 16 pending migrations to v66.
- `TestHumaBinary_SessionMessageAsync` in
  `integration-rest-full-3-of-8`: fresh `hq` observed at schema v39, then
  refused the 27 pending migrations to v66.

Each printed the established `missing bd schema; re-initializing` followed by
the shared-server pending-migration refusal. This is the exact concurrent-
initializer random-cursor condition independently reproduced and root-caused
before this run in `ga-lejnse` and `ga-e2z1zb`. Both trackers were updated with
these sightings.

`failure_attribution: all three raw failures -> ga-e2z1zb | clause 3(b) CROSS-PR - exact random-cursor refusal signature independently occurred on unrelated candidates`

Clause 1 passes because none of the failing test files is in this diff. Clause
2 passes through the pre-existing condition tracker `ga-lejnse` and root-fix
bead `ga-e2z1zb`. Clause 4 passes because the failures are in city/bootstrap
initialization while this candidate changes only the reaper's Step 6 shell
guard and its isolated shell test. No rerun was used.

`waiver_ref: none`

`ci_lane_run: n/a (no CI configuration change)`

## Static and policy evidence

All of the following passed at the reviewed source:

- `bash -n` on both changed shell files
- `shellcheck` on both changed files, excluding only the two base-identical,
  diff-unrelated findings `SC1091` and `SC2034`
- `make build`
- `make test-ci-policy`
- `go vet ./...`
- `LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=2898f5467f807a469c3aa0358a78a23772f1f753 make lint-affected fmt-check-changed`
- `make check-core-boundary check-native-dependency-surface check-residency-boundary check-gomod-replace check-eventexport-isolation`
- `make check-routed-test-rows check-split-topology-rows`
- `make check-release-dist-ignore`
- `make check-hooks`
- `git diff --check 2898f5467f807a469c3aa0358a78a23772f1f753...HEAD`
- `govulncheck -show verbose ./...`: zero reachable or imported-package
  vulnerabilities; three module-only informational findings

Dashboard CI and preview are not applicable: this change does not touch the
API, OpenAPI schemas, dashboard, or generated dashboard types.

## Deployment safety note

The gc-management city override
`GC_REAPER_SESSION_PURGE_AGE="876000h"` remains required until this fix is
merged **and deployed**. This gate and PR do not authorize removing it.

## Disposition

Commit this record on isolated branch `deploy/ga-f9nfpf-gate`, push that branch,
open a Gas City PR, publish exact-head deploy clearance, and route the merge
request to mayor/mpr. The deployer does not merge.
