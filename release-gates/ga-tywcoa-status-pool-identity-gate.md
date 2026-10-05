# Release Gate: ga-tywcoa - status pool runtime identity

Deploy bead: `ga-tywcoa`
Source bead: `ga-7n2hhq`
Reviewed commit: `f87dae14af810a1c7764d0207ffaf0787dbb3640`
Base: `origin/main@c340e656f5b769be49ae335240ee54bd9866300f`
Merge tree: `4f3cce1733bd3102e91057cb00ce301a870dbb75`
Gate evaluated: 2026-09-16
Verdict: **PASS**

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | PR #6418 records `mpr-review head=f87dae14af810a1c7764d0207ffaf0787dbb3640 verdict=auto-merge`; Qwen, Claude, and Codex all reported `ok`. The review names two follow-up hardening ideas and explicitly says neither blocks this change. |
| 2 | Acceptance criteria met | PASS | Status now resolves the recorded runtime session name before probing liveness while retaining canonical-name precedence and failing closed on ambiguity. All five diff-owned tests passed twice in the full union: once in `unit-core` and once in `integration-packages-core-3-of-4`. |
| 3 | Tests pass | PASS | The documented 40-job full union completed 39 green jobs with 51,188 PASS / 1 attributed FAIL / 227 SKIP results, counting subtests. The sole raw failure is the independently reproduced concurrent-initializer schema race tracked by `ga-lejnse` and root-fix bead `ga-e2z1zb`; details are below. No test was rerun. |
| 4 | No high-severity review findings open | PASS | The ensemble review returned auto-merge with no blocking finding. The two hardening observations were explicitly non-blocking and outside this fix's incident contract. |
| 5 | Final branch is clean | PASS | `git status --short` was empty at the reviewed source before this gate record was created. Dashboard regeneration was clean with no tracked drift. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree f87dae14af810a1c7764d0207ffaf0787dbb3640 origin/main` returned clean tree `4f3cce1733bd3102e91057cb00ce301a870dbb75`. The target PR remained open, clean, and mergeable during preflight. |
| 7 | Single feature theme | PASS | One API status-resolution helper and its tests correct liveness reporting for recorded pool runtime names. No independent feature is bundled. |

## Criterion 3 evidence

The authoritative command was:

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
`/var/tmp/ga-tywcoa-full-logs`, with the coordinator log at
`/var/tmp/ga-tywcoa-full.log`.

`test_cmd_scope: full-suite`

`test_counts: 51,188 PASS, 1 attributed FAIL, 227 SKIP`

`diff_tests_executed:`

- `TestStatusReportsPoolSuffixedSessionAsRunning` PASS twice
- `TestStatusReportsPoolSuffixedSessionAsRunningWithRecordedAgentName` PASS twice
- `TestStatusMapsEachPoolInstanceToItsOwnSession` PASS twice
- `TestStatusReportsCanonicallyNamedSessionAsRunning` PASS twice
- `TestStatusKeepsUnlimitedPoolAgentVisible` PASS twice

The 227 skips are existing platform, provider, and opt-in integration guards;
none is diff-owned. Every test added by this change reported a real PASS in
both selecting lanes.

The sole raw failure was
`TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix` in
`cmd-gc-process-4-of-6`. Its temporary `hq` database was observed at schema
cursor v30 while another initializer was migrating it, then the shared-server
safety gate refused 36 pending migrations to v66. This is the exact random-
cursor condition already recorded for the same test on unrelated candidates.

`failure_attribution: TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix -> ga-e2z1zb | clause 3(b) CROSS-PR - exact test and refusal signature independently occurred on unrelated candidates`

Clause 1 passes because the failing test file is not in this diff. Clause 2
passes through the pre-existing condition tracker `ga-lejnse` and root-fix bead
`ga-e2z1zb`, both opened before this run. Clause 4 passes because the failure is
in `cmd/gc` while the candidate changes only `internal/api`. The sighting was
appended to both trackers. No rerun was used.

`waiver_ref: none`

`ci_lane_run: n/a (no CI configuration change)`

## Static, API, and policy evidence

All of the following passed at the reviewed source:

- `make build`
- `make test-ci-policy`
- `go vet ./...` in a clean serial run
- `LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=a41b91a032852bc6b6293aa63988d6bdc590c730 make lint-affected fmt-check-changed` with zero issues
- `make dashboard-ci`
- dashboard frontend preview served valid HTML at `127.0.0.1:4187`
- `make check-core-boundary check-native-dependency-surface check-residency-boundary check-gomod-replace check-eventexport-isolation`
- `make check-routed-test-rows check-split-topology-rows`
- `make check-release-dist-ignore`
- `make check-hooks`
- `git diff --check a41b91a032852bc6b6293aa63988d6bdc590c730...HEAD`
- `govulncheck -show verbose ./...` with `govulncheck@v1.7.0`: zero reachable or imported-package vulnerabilities; three module-only informational findings

One earlier `go vet` invocation was started concurrently with dashboard CI and
saw `npm ci` remove a `node_modules` directory while vet was traversing it. It
reported only that vanished-directory race. After dashboard CI completed, the
required clean serial vet run passed.

## Disposition

Commit this record on isolated branch `deploy/ga-tywcoa-gate`, push that branch,
open a Gas City PR, publish exact-head deploy clearance, and route the merge
request to mayor/mpr. The deployer does not merge.
