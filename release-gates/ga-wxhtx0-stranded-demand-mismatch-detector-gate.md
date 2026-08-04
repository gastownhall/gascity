# Release Gate: Stranded-demand wake/idle-kill treadmill detector

Gate result: **PASS**

- Deploy bead: `ga-wxhtx0`
- Source bead: `ga-4tu2z7`
- Review bead: `ga-g8qh81`
- Reviewed commit: `ed5eba7b9db4e93880c3aae25bce06097c73b068`
- Current base: `origin/main` at `4873ef3d59da36afa8b7e6c009f8ebc0551af713`

`docs/PROJECT_MANIFEST.md` is absent from both the reviewed commit and current
`origin/main`. This gate therefore applies the active deployer release criteria
and the repository gates in `AGENTS.md` and `TESTING.md`.

## Criterion 6: branch diverges cleanly from main

Evaluated first and rechecked immediately before writing this checklist.

- `git merge-base origin/main HEAD`:
  `b62945290d2ce1fca754390a40e91bdc2763430c`
- `git rev-list --left-right --count origin/main...HEAD`: `1 1`
- `git merge-tree --write-tree origin/main HEAD`: exit 0, tree
  `03da7fccc3ddfefabbf028b0b8b6df5f39a51c85`
- `git diff --check origin/main...HEAD`: exit 0

Result: **PASS**. The reviewed commit is behind current main but has no merge
conflict, so the bounded self-rebase exception was not needed.

## Release criteria

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | `ga-g8qh81` is closed with a PASS close reason and `REVIEWER VERDICT: PASS` for the exact reviewed SHA. |
| 2 | Acceptance criteria met | PASS | The reviewed code adds a mutex-protected, in-memory per-template detector; observes successful idle-timeout kills; fails closed when assigned-work lookup fails; publishes at the configurable tracker threshold (default 3) and doubling intervals; resets when the episode breaks; registers a typed `session.demand_mismatch` payload; and performs no autonomous response. See the acceptance evidence below. |
| 3 | Tests pass | PASS | The final `make test-fast-parallel` rerun passed all 8 jobs. All change-owning tests and required surface gates also pass. An earlier run exposed two unrelated baseline defects: the provider-factory ambient-city failure reproduced on current main and is owned by `ga-y4se3w`; a controller reload timeout is owned by `ga-jin89y`. The candidate changes none of the failing tests or their controller/provider implementation files. |
| 4 | No high-severity review findings open | PASS | The reviewer recorded no blocking findings and no HIGH or CRITICAL finding. Security, spec compliance, and coverage reviews are PASS. |
| 5 | Final branch is clean | PASS | Before writing this gate file, `git status --porcelain=v1` returned no paths at the reviewed detached HEAD. Cleanliness is rechecked after committing the gate artifact. |
| 6 | Branch diverges cleanly from main | PASS | Evaluated first; see the dedicated evidence above. |
| 7 | Single feature theme | PASS | One feature commit changes the reconciler’s stranded-demand detection/event surface and its direct tests/generated API artifacts. No independent behavior is bundled. |

## Acceptance evidence

| Requirement | Result | Evidence |
|---|---|---|
| Detect repeated wake-to-idle-kill cycles per template | PASS | `demandMismatchTracker` keeps independent episode state keyed by the qualified template; the reconciler records an observation only after a successful idle-timeout stop. |
| Count only no-claim cycles while demand remains positive | PASS | The reconciler queries reachable assigned work and treats lookup errors as `hasWork=true`; open work or non-positive demand resets the episode. A demand drop starts a fresh episode. |
| Default threshold 3 without making the value load-bearing | PASS | `newDemandMismatchTracker(threshold)` accepts an injected threshold and maps non-positive values to `defaultDemandMismatchThreshold = 3`. |
| Bound event volume | PASS | Publications occur at threshold multiples `3, 6, 12, 24, ...`; unit tests cover threshold crossing, doubling backoff, and all reset paths. |
| Typed event and payload registration | PASS | `events.SessionDemandMismatch` is in `KnownEventTypes`; `SessionDemandMismatchPayload` is registered with `events.RegisterPayload`; OpenAPI artifacts and the generated Go client are synchronized. |
| Structural payload only | PASS | Payload fields are template, cycle count, demand, and episode first-seen time. The event envelope timestamp records the publication/last observation time. No bead title, body, or secret is included. |
| Detection and publication only | PASS | The new path calls `rec.Record` and does not restart, suspend, mute, reroute, or otherwise alter the lifecycle decision. |
| Best-effort, non-persistent state | PASS | Episode state is an in-memory map owned by the reconciler’s idle tracker and is not written to the bead store or filesystem. |

## Test evidence

- `go test ./cmd/gc -run '^(TestDemandMismatchTracker_.*|TestReconcileSessionBeads_IdleTimeout(FeedsDemandMismatchTracker|DemandMismatchSeesOpenWork|DemandMismatchPublishesEvent))$' -race -count=1 -v`: PASS, all 11 new tests.
- `go test ./internal/events/... ./internal/api/... -run '^(TestEveryKnownEventTypeHasRegisteredPayload|TestOpenAPISpecInSync)$' -count=1 -v`: PASS.
- `go vet ./...`: PASS.
- `go build ./...`: PASS.
- `make test-fast-parallel`: PASS, all 8 jobs on the final rerun.
- `make dashboard-check`: PASS, including the dashboard build, TypeScript checks, e2e typecheck, and dashboard Go packages.
- `make check-docs`: PASS.
- Dashboard smoke: the built frontend was served with the current workspace’s
  `npm run --workspace gas-city-dashboard-frontend preview -- --host 127.0.0.1 --port 41739`;
  `curl --fail http://127.0.0.1:41739/` returned the rendered HTML, then the
  preview was stopped and the port was verified closed.
- Changed Go files are `gofmt`-clean; `git diff --check` reports no whitespace errors.

## Earlier-run baseline diagnostics

- `TestErrorReturningSessionProviderFactoriesPreserveSuccessBehavior/default`
  fails on both the candidate and current `origin/main` because the ambient
  enclosing city resolves `*auto.Provider` instead of the injected
  `*runtime.Fake`. Existing bug: `ga-y4se3w`.
- `TestControllerReloadCommandReloadsConfigImmediately` timed out once in
  `unit-cmd-gc-2-of-6` after 3.32 seconds. The candidate does not modify
  `cmd/gc/controller.go`, `cmd/gc/controller_test.go`, or
  `cmd/gc/city_runtime.go`; 10 isolated runs on current main and the final
  all-shards candidate rerun passed. Follow-up reliability bug: `ga-jin89y`.
- A deliberately broad diagnostic race selector also exposed an existing
  unsynchronized test buffer in
  `TestControllerReloadsNamedSessionModeAndAppliesIdleTimeout`. The identical
  race reproduces on current main; the exact 11 new tests remain race-clean.
  Existing bug: `ga-9cndyo`.
