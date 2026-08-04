# Release Gate: routed-demand dashboard client

Status: PASS

Deploy bead: ga-qy82ie
Source review bead: ga-66ghjv
Predecessor deploy bead: ga-fkls32
Reviewed commit: 0ce50c1ca71ad821c1dbb09e88ad743eee99593d
Source branch: builder/ga-z7evh4.1-stranded-routed-demand-throttle
Deploy branch: deploy/ga-qy82ie-gate

`docs/PROJECT_MANIFEST.md` is not present in this worktree, so this gate uses
the deployer role's release criteria table plus the repo testing policy in
`TESTING.md`.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `bd show ga-66ghjv` records `REVIEW PASS (gascity/reviewer)` for commit `0ce50c1ca71ad821c1dbb09e88ad743eee99593d`; predecessor/source review `bd show ga-z7evh4.1` also records `REVIEWER VERDICT: PASS` for the underlying routed-demand stack. |
| 2 | Acceptance criteria met | PASS | The deploy fix regenerates `RoutedDemandStrandedPayload.escalated` and `first_seen` in the dashboard TypeScript client and rebuilt SPA dist. The source stack remains the single-theme stranded routed-demand throttle/dedup candidate: `git diff --name-status origin/main...HEAD` lists 50 files, all core routed-demand implementation/wiring or mechanically coupled generated outputs; `git diff --name-only origin/main...HEAD \| rg -i 'docsync\|scaffold'` returned no matches. |
| 3 | Tests pass | PASS | `go build ./cmd/gc`; `go vet ./...`; `make spec-ci`; `make dashboard-ci`; `make dashboard-smoke`; `go run ./cmd/genschema`; `make check-docs`; focused routed-demand tests in `./cmd/gc` and `./internal/api` all passed. `make test-fast-parallel` failed only `cmd/gc` shard 5 at `TestErrorReturningSessionProviderFactoriesPreserveSuccessBehavior/default` with the known live-city native-store schema warning; the same focused test passed in clean `/var/tmp` worktrees for both this reviewed SHA and `origin/main`, confirming the fast-suite failure is ambient/pre-existing and not introduced by this diff. |
| 4 | No high-severity review findings open | PASS | Reviewer notes on `ga-66ghjv` report no security concerns for the generated client fix; source review notes on `ga-z7evh4.1` report a fresh security walk with no blocking findings. No unresolved HIGH findings are recorded in the deploy/review bead notes. |
| 5 | Final branch is clean | PASS | Before adding this gate file, `git status --short --branch` at `0ce50c1ca71ad821c1dbb09e88ad743eee99593d` reported `## HEAD (no branch)` with no file changes. The final deploy branch is clean after committing this gate file. |
| 6 | Branch diverges cleanly from main | PASS | Evaluated first: `git merge-tree --write-tree origin/main 0ce50c1ca71ad821c1dbb09e88ad743eee99593d` exited 0 and produced merged tree `701308fb5049b408204a53b1e25311ac168179aa`. |
| 7 | Single feature theme | PASS | The merge-base PR diff is one routed-demand feature: stranded `gc.routed_to` demand detection, throttle/escalation metadata, rollout/config/event payload wiring, and generated API/dashboard/docs artifacts. The final commit `0ce50c1ca` is generated-client-only on top of the already reviewed routed-demand stack. |

## Test Evidence

- `go build ./cmd/gc`: PASS.
- `go vet ./...`: PASS.
- `make spec-ci`: PASS; no OpenAPI/generated Go client drift.
- `make dashboard-ci`: PASS; generated TypeScript client and embedded `dashboardspa/dist` stayed current.
- `make dashboard-smoke`: PASS; Vite preview eventually served `/` with HTTP 200.
- `go run ./cmd/genschema`: PASS; generated config/schema docs stayed clean.
- `make check-docs`: PASS.
- `go test ./cmd/gc -run 'TestCityRuntimeBeadReconcileTickStrandedRoutedDemand|TestDetectStrandedRoutedDemand|TestFilterAssignedWorkBeadsForPoolDemandKeepsDirectAssigneeAfterTemplateFallback' -count=1 -v`: PASS.
- `go test ./internal/api -run '^TestRoutedDemandStrandedEventIsKnownAndTyped$' -count=1 -v`: PASS.

## Fast Baseline Note

`make test-fast-parallel` was run and all fast jobs passed except
`unit-cmd-gc-5-of-6`. The failing test was
`TestErrorReturningSessionProviderFactoriesPreserveSuccessBehavior/default`,
with the ambient warning:

`WARN native_store_unavailable gate=native_open reason="schema version mismatch: database is at v58, binary knows up to v53" scope=/home/jaword/projects/gc-management`

This is the documented live-city worktree failure tracked by the
`prepush-fast-suite-fails-from-live-city-worktree` memory. I verified the same
focused test passes outside the live city tree in clean `/var/tmp` worktrees for
both the reviewed SHA and current `origin/main`.
