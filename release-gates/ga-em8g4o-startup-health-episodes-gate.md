# Release Gate: persistent startup-health episodes

- Deploy bead: `ga-em8g4o`
- Build bead: `ga-o04bfr.1.1`
- Review bead: `ga-xhf54z`
- Reviewed source: `bf50bd19e7d8fcf6c508c96834331c3755be4023`
- Merge base: `c9f32fbbfd10070f64de9fbcd3c1c8d3e2965005`
- Base evaluated: `origin/main@f91d19b476c23ba8732acede781b9a91fc462984`
- Deploy mode: remote
- Date: 2026-09-03
- Overall verdict: **FAIL**

The already-merged preflight found no base-repository pull request carrying the
reviewed source. Criterion 6 passed first, so no bounded self-rebase was needed.
`docs/PROJECT_MANIFEST.md` is absent at the evaluated source; the gate therefore
uses the deployer release criteria together with `TESTING.md` and the Makefile's
documented full-suite and policy targets.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | The deploy bead contains a fresh reviewer PASS explicitly pinned to `bf50bd19e7d8fcf6c508c96834331c3755be4023`. The SHA resolves to the evaluated commit; no review carryover is involved. |
| 2 | Acceptance criteria met | PASS | The typed episode record is keyed by session name, persists count/kind/timestamps/detail/disposition/quarantine, is excluded from Ready/session listings, is consulted before `Provider.Start`, mirrors active count/kind through the typed session store, and clears on confirmed recovery. Named and pool-expanded tests drive the real `syncSessionBeads` materialization path and prove a sixth start is blocked. |
| 3 | Tests pass | PASS WITH ATTRIBUTED FAILURES | `make test-local-full-parallel` ran the documented 40-job full suite: 37 PASS jobs, 3 raw FAIL jobs, 0 omitted jobs; top-level results were 46,973 PASS, 3 FAIL, 208 SKIP. All 28 distinct diff-owned tests reported PASS (53 PASS executions), with 0 diff-owned FAIL and 0 diff-owned SKIP. The three raw failures are attributed below to pre-existing trackers with mechanism and path evidence. `test_cmd_scope: full-suite`; `waiver_ref: none`. |
| 3b | Policy/lint lane | **FAIL** | `make test-ci-policy`, changed-file formatting, `go vet ./...`, and `go build ./...` passed. `LINT_CHANGED_REF=c9f32fbbfd10070f64de9fbcd3c1c8d3e2965005 LINT_CHANGED_SCOPE=tracked make lint-affected` failed: `runDesiredPendingCreateTicks`'s `ticks` parameter always receives `30`. Exact-base `make lint-full` reported 0 issues, while the candidate adds six more constant-30 callers, so this is not attributed as pre-existing. |
| 3c | CI-config lane | PASS | `ci_lane_run: n/a (no CI job, matrix, timeout, or required-check-list change in the triple-dot candidate diff)`. |
| 4 | No high-severity review findings open | PASS | Fresh review notes record no security finding, no architecture violation, and no unresolved HIGH finding. |
| 5 | Final branch is clean | PASS | `git status --short` was empty and `git diff --check origin/main...bf50bd19e7d8fcf6c508c96834331c3755be4023` was clean before this checklist was updated. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree --messages origin/main bf50bd19e7d8fcf6c508c96834331c3755be4023` exited 0 and produced tree `8d81e3abed5ecf3a8a9b3c36e52474277c19d7bf`. No bounded self-rebase was needed. |
| 7 | Single feature theme | PASS | The candidate's production and test changes form one startup-health persistence/quarantine feature across `cmd/gc`, `internal/session`, and the Beads Ready-exclusion predicate. The prior gate checklist is the only non-product artifact in the commit range. |

## Criterion 2: acceptance evidence

1. `internal/session/startup_health.go` defines a typed, session-name-keyed
   `StartupHealthEpisode` and Store load/save front door. Tests cover metadata
   projection, persistence, duplicate-preventing upsert, malformed values, and
   store errors.
2. `RecordStartupFailure` receives the existing threshold and quarantine
   duration from the reconciler. Timeout versus other failure kind is classified
   without parsing detail; the fifth failure applies the existing five-minute
   quarantine.
3. The reconciler consults the durable episode before a pending-create start,
   mirrors count/kind through `session.Store.ApplyPatch`, and prevents a sixth
   `Provider.Start`. The bookkeeping bead type remains excluded from Ready and
   session listing projections.
4. A durably committed successful start clears both the episode and the visible
   metadata mirror. Quarantine expiry permits the next start; clearing is not
   inferred from continuation metadata.
5. The new accounting remains a side channel to pending-create rollback. The
   existing wake-failure, rate-limit, terminal-error, and circuit-breaker paths
   are unchanged, and the full process/integration suite found no candidate-owned
   regression in them.
6. `TestNamedSessionStartupHealthEpisodeAccruesViaRealMaterialization` and
   `TestPoolSessionStartupHealthEpisodeAccruesViaRealMaterialization` drive the
   production `syncSessionBeads` path for configured named and pool-expanded
   identities. The broader transition/reconciler/store tests cover threshold,
   restart re-read, expiry, recovery, mirroring, and persistence errors.

## Criterion 3: full-suite evidence

Environment and command:

```text
DOCKER_HOST=unix:///run/user/1000/podman/podman.sock
TESTCONTAINERS_RYUK_DISABLED=true
LOCAL_TEST_LOG_DIR=/var/tmp/ga-em8g4o-gate-gm-wisp-ybyl/jobs
GOFLAGS=-v
make test-local-full-parallel
```

The rootless Podman socket was active before the run (`podman 5.8.4`, `crun`).
This candidate path has no pinned testcontainers image reference. Complete log:
`/var/tmp/ga-em8g4o-gate-gm-wisp-ybyl/full-suite.log`.

- `test_cmd_scope: full-suite`
- job counts: 37 PASS, 3 raw FAIL, 0 omitted
- top-level test results: 46,973 PASS, 3 FAIL, 208 SKIP
- `diff_tests_executed`: 28/28 distinct tests PASS, 0 FAIL, 0 SKIP (53 PASS
  executions across the unit and integration-tag matrices)
- `waiver_ref: none`
- `ci_lane_run: n/a (no CI-config change)`

Diff-owned results by file:

- `cmd/gc/startup_health_materialization_test.go`: 2/2 PASS — configured named
  and pool-expanded real-materialization paths.
- `cmd/gc/startup_health_reconcile_test.go`: 6/6 PASS — accrual across
  replacement beads, pre-expiry blocking/post-expiry retry, count/kind mirror,
  success clear, mirror clear, and load-error logging.
- `internal/session/startup_health_test.go`: 19/19 PASS — metadata projection,
  threshold/time/kind/detail transitions, reset/rearm, store round-trip,
  duplicate prevention, errors, read-only load, and listing exclusion.
- `internal/beads/beads_test.go`: `TestIsReadyExcludedType` PASS.
- `cmd/gc/session_reconcile_test.go`: the changed shared store fixture was
  exercised by the complete package matrices; it adds no test function and no
  test in that file failed or skipped.

`skip_justification`: all 208 SKIPs are non-diff-owned, explicit suite
conditions: subprocess helper sentinels, platform/privilege-specific tests,
live-provider opt-ins, Kubernetes/tmux/network-dependent checks, golden
regeneration, and pinned integration flags such as
`GC_INTEGRATION_BD_PERSISTENCE`. None of the 28 diff-owned tests skipped.

### Attributed raw test failures

Each tracker predated this run, was opened before citation, received this run's
sighting, and was read back afterward.

- `TestBdFlagManifestCurrent` -> `ga-f0uceo`. Mechanism proof: the test shells
  out to the independently installed host `bd --help` and compares that surface
  with `internal/bdflags/bdflags.go`. The candidate changes neither the test,
  the manifest, nor the installed binary; no candidate path overlaps
  `internal/bdflags`.
- `TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix` and
  `TestGCLiveContract_BeadsAndEvents` -> `ga-esyijp`. Both failed with the exact
  tracked beads#4566 condition: pending schema migrations encountered a dirty
  `issues` table during temporary Dolt/beads initialization. The first failed
  during city store initialization before startup-health reconciliation. The
  second successfully started its sessions, then failed later while adding a
  rig. The candidate changes neither schema migration/provider bootstrap nor
  either failing test path.

## Criterion 3b: policy and lint evidence

- `policy_lane: make test-ci-policy` — PASS.
- `LINT_CHANGED_REF=c9f32fbbfd10070f64de9fbcd3c1c8d3e2965005 LINT_CHANGED_SCOPE=tracked make fmt-check-changed` — PASS.
- `go vet ./...` — PASS.
- `go build ./...` — PASS.
- `LINT_CHANGED_REF=c9f32fbbfd10070f64de9fbcd3c1c8d3e2965005 LINT_CHANGED_SCOPE=tracked make lint-affected` — **FAIL**:
  `cmd/gc/session_pending_create_rollback_desired_test.go:25:73` reports
  `runDesiredPendingCreateTicks - ticks always receives 30 (unparam)`.
- Exact merge-base comparison in a disposable worktree:
  `make lint-full` at `c9f32fbbfd10070f64de9fbcd3c1c8d3e2965005` — PASS, 0 issues.

The helper source blob is identical at base and candidate
(`85370adb95f2301c0d0e7036d5709f52ff7bf74c`), but the candidate adds six
same-package calls, all passing the same constant `30`. The deterministic
candidate failure does not reproduce at the exact base, so it is not a valid
pre-existing-policy attribution. Tracker `ga-emldy6` records both the original
sighting and this decisive base comparison; this gate still fails criterion 3b.

## Decision

**Gate FAIL on criterion 3b.** Do not cut or push a deploy branch, open a pull
request, publish deploy clearance, or route a merge request. Return
`ga-em8g4o` to the builder to remove the candidate-introduced `unparam` finding,
then require a fresh reviewer PASS on the resulting commit before re-evaluating
the release gate.
