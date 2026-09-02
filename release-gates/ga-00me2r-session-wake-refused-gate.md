# Release gate: durable pre-start wake-refusal diagnostics

- Deploy bead: `ga-00me2r`
- Build bead: `ga-fxvdit`
- Review bead: `ga-jxrm4o`
- Reviewed source: `64597522503b132fd895eb41b334d5c7f1588827`
- Source branch: `builder/ga-fxvdit` (provenance only)
- Deploy branch: `deploy/ga-00me2r-gate`
- Current base at final preflight: `origin/main@dee9c5e4848869f6d3e5e59b3a484bdd5885ccfb`
- Reviewed-source merge base: `bc1c3ccaf774675acb9cc8955093ab8221946daf`
- Deploy mode: `remote`; push remote: `origin`
- Gate date: 2026-09-01
- Overall verdict: **PASS**

| # | Criterion | Result | Evidence |
| --- | --- | --- | --- |
| 1 | Review PASS present | **PASS** | Closed review bead `ga-jxrm4o` records `verdict: pass` and `deploy_commit: 64597522503b132fd895eb41b334d5c7f1588827`, exactly matching this branch's reviewed source. |
| 2 | Acceptance criteria met | **PASS** | A refused explicit pre-start wake now emits the typed `session.wake_refused` event, writes a durable `wake_attempts` increment, throttles repeat emissions for the same wake request, clears the guard on a fresh explicit wake, and does not quarantine a held session. The four named regression tests and both API-contract tests pass. |
| 3 | Tests pass | **PASS with attributed raw failures** | The documented `LOCAL_TEST_JOBS=4 make test-local-full-parallel` union completed all 40 jobs: **34 PASS / 6 FAIL / 0 observed job-level SKIP**. Every raw failure is retained and attributed below to a predating, non-diff condition. All four diff-owned wake-refusal tests pass, the changed session assertions pass through the green fast/package coverage recorded by the reviewer, and the API/OpenAPI contract tests pass. |
| 3b | Policy and static lanes | **PASS with attributed lint baseline** | `make test-ci-policy`, `make vet`, merge-base-scoped `make fmt-check-changed`, and `git diff --check` pass. Fresh-cache `make lint-affected` conservatively widened to the full repository because a generated dashboard asset was deleted, then reported only the three known third-party `node_modules/flatted` findings tracked by `ga-bvixfw` and selector leak `ga-u8z8j6`; no candidate-owned file produced a finding. |
| 3c | CI configuration coverage | **N/A** | The candidate changes no workflow, Makefile target, runner policy, or CI script. `make test-ci-policy` nevertheless passes. |
| 4 | No unresolved high-severity findings | **PASS** | The exact-SHA review records no blocker, major, minor, security, style, or spec findings and corrects `uncovered_criteria` to `none`. |
| 5 | Final branch clean | **PASS** | The isolated branch was clean after all test, generation, preview, and static commands. Regeneration produced zero drift; this checklist is the sole deploy-only addition. |
| 6 | Branch diverges cleanly from main | **PASS** | After fetching current `origin/main`, `git merge-tree --write-tree origin/main 64597522503b132fd895eb41b334d5c7f1588827` exited 0 and produced tree `47600cfd3f77ce89d25171d412504a08ad3dd5a0`. The source is two main commits behind and one reviewed commit ahead, with no merge conflict. |
| 7 | Single feature theme | **PASS** | The production, event/API contract, lifecycle codec, generated schema/client/dashboard artifacts, and tests all support one theme: durable diagnostics for explicit wakes refused before session start. |

## Acceptance and focused evidence

`diff_tests_executed`:

- `TestEmitSessionWakeRefused_HeldSessionNotQuarantinedAtThreshold` — PASS
- `TestEmitSessionWakeRefused_SingleRefusalEmitsAndBumpsOnce` — PASS
- `TestEmitSessionWakeRefused_TenConsecutiveTicksGuardHolds` — PASS
- `TestEmitSessionWakeRefused_FreshWakeClearsGuardAndReemits` — PASS
- `TestOpenAPISpecInSync` — PASS
- `TestEveryKnownEventTypeHasRegisteredPayload` — PASS
- Modified `internal/session` codec, patch, and lifecycle assertions — PASS in the reviewer's green `make test-fast-parallel` run and the candidate's package/full-suite coverage

The new helper writes `wake_attempts` and the per-request
`wake_refused_event_at` marker through the session front door, while deliberately
avoiding wake-failure accrual and quarantine. A fresh explicit wake clears both
fields. The event has a registered typed payload and is included in
`events.KnownEventTypes`.

`waiver_ref`: none.

`skip_justification`: none required for diff-owned coverage; zero focused tests
skipped and the full union reported no job-level skip.

## Full-suite evidence

```text
test_cmd: LOCAL_TEST_JOBS=4 make test-local-full-parallel
test_cmd_scope: documented 40-job local full union
test_counts: 34 PASS jobs / 6 FAIL jobs / 0 observed job-level SKIP (40 total)
full_log: /var/tmp/ga-00me2r-full-suite.out
shard_logs: /var/tmp/gc-local-tests.sDP1cz
```

`failure_attribution`:

- `TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix` in
  `cmd-gc-process-3-of-6` -> `ga-esyijp` | clause 3(a), exact predating
  beads#4566 dirty-`issues` migration signature during fixture `bd init`.
- `TestBdFlagManifestCurrent` in `integration-packages-core-1-of-4` ->
  `ga-f0uceo` | clause 3(a), exact predating installed-`bd`/manifest skew;
  the candidate changes neither the installed binary nor `internal/bdflags`.
- `TestBdStoreMailWispInsert` in `integration-bdstore` -> `ga-vkhfnj`
  (canonical current condition tracker; original exact sighting `ga-sxtkmu`)
  | clause 3(a), test-owned Dolt SQL server failed to listen within its fixed
  readiness budget under the full parallel union. The candidate changes no
  Dolt startup, bdstore integration, or backend initialization path.
- `TestRetryManagedPooledWorkerRecoversClaimedAttemptAfterCrash` in
  `integration-review-formulas-recovery` -> `ga-esyijp` | clause 3(a), exact
  predating dirty-table migration signature across comments,
  compaction snapshots, events, and issue snapshots before recovery behavior.
- `TestE2E_SuspendResume_City` in `integration-rest-full-2-of-8` ->
  `ga-vkhfnj` | clause 3(a), predating `citysus.report` timeout under the full
  host-contention union; no changed wake-refusal path participates in the
  suspend/resume report contract.
- `TestHumaBinary_SessionMessageAsync` in `integration-rest-full-3-of-8` ->
  `ga-esyijp` | clause 3(a), fixture city initialization failed on the exact
  dirty-`issues` migration signature before the Huma scenario executed.

Every cited tracker predates this 2026-09-01 run. None of the six failing test
files is diff-owned. The three dirty-schema failures terminate during fixture
initialization, the bdstore failure is in a test-owned Dolt startup path, the
manifest failure is controlled by the host binary, and the suspend/resume
failure concerns a distinct report handoff. These mechanisms are structurally
disjoint from the candidate's explicit-wake refusal event and lifecycle marker.

## API, generated artifact, and documentation evidence

- `make dashboard-ci` — PASS: 268 frontend modules built; frontend, test, and
  end-to-end typechecks passed; dashboard Go tests passed; client generation
  completed; `git status` remained clean.
- `make check-docs` — PASS.
- The prescribed root `npm run preview` script is not defined. The actual
  workspace preview command,
  `npm --workspace gas-city-dashboard-frontend run preview -- --host 127.0.0.1 --port 41731`,
  served HTTP 200 with the expected application title and root element. It was
  stopped cleanly and the port had no remaining listener.
- `TestOpenAPISpecInSync` and
  `TestEveryKnownEventTypeHasRegisteredPayload` — PASS.
- The OpenAPI files, generated Go client, generated TypeScript types, and
  dashboard distribution were regenerated from their authoritative sources;
  no generated drift remained. No hand-authored frontend source changed.

## Independent policy and static lanes

```text
policy_lane: make test-ci-policy — PASS
vet_lane: make vet — PASS (go vet ./...)
format_lane: LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=bc1c3ccaf774675acb9cc8955093ab8221946daf make fmt-check-changed — PASS
diff_check: git diff --check bc1c3ccaf774675acb9cc8955093ab8221946daf...HEAD — PASS
hooks_path: /home/jaword/projects/gascity/.githooks
```

Fresh-cache affected lint command:

```text
GOLANGCI_LINT_CACHE=/var/tmp/ga-00me2r-golangci-cache \
  LINT_CHANGED_SCOPE=tracked \
  LINT_CHANGED_REF=bc1c3ccaf774675acb9cc8955093ab8221946daf \
  make lint-affected
```

The selector broadened to a full-repository scan because the diff includes a
deleted generated dashboard asset. Its only diagnostics were two `govet`
inline-constant findings and one `revive` package-comment finding in
`internal/api/dashboardspa/web/node_modules/flatted/golang/pkg/flatted/flatted.go`.
Tracker `ga-bvixfw`, opened 2026-08-19, documents these exact three findings on
clean main with a separate cache; `ga-u8z8j6`, opened 2026-08-25, documents the
full-fallback leak into dashboard `node_modules`. Both predate this candidate
and neither finding is in a first-party or diff-owned file.

## Disposition

All applicable release criteria pass, with raw suite and lint failures retained
and attributed to predating, structurally disjoint conditions. Commit this
record as the sole deploy-only change, push the isolated deploy branch, open a
pull request against `gastownhall/gascity`, and reference issue #5739. Publish
deploy clearance on the exact PR head, then route merge authority to the mayor.
The deployer does not merge.
