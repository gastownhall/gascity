# Release Gate: Pool-Step Slot Affinity

- Deploy bead: `ga-39tsop`
- Source bead: `ga-85rmnq`
- Reviewed commit: `1e0dd4cce17bb02eaaf78a43061bd47739022b7c`
- Clean deploy branch: `builder/ga-0ht5r6`
- PR: https://github.com/gastownhall/gascity/pull/3405
- Gate date: 2026-06-12

## Scope Correction

The deploy bead named `builder/ga-frmdxd.3` as the source branch, but that
branch carried eight unrelated emergency, doctor, API, and dashboard commits in
addition to the reviewed graphroute fix. The reviewed graphroute patch was
re-cut as `builder/ga-0ht5r6` and opened as PR #3405. The patch in PR #3405 is
byte-identical to the reviewed delta from `1e0dd4cce17bb02eaaf78a43061bd47739022b7c`
for:

- `internal/graphroute/graphroute.go`
- `internal/graphroute/graphroute_test.go`
- `test/docsync/docsync_test.go`

The release gate therefore evaluates the clean branch `builder/ga-0ht5r6`
instead of the contaminated hook branch.

## Checklist

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 1 | Review PASS present | PASS | `ga-85rmnq` records reviewer PASS for `1e0dd4cce17bb02eaaf78a43061bd47739022b7c`; PR #3405 carries the same reviewed patch on a clean branch. |
| 2 | Acceptance criteria met | PASS | `ApplyGraphRouteBinding` stamps `gc.continuation_group=pool-workflow` and `gc.session_affinity=require` only in the `MetadataOnly` pool-route path. Tests cover pool-routed metadata, single-session non-stamping, and absence of session-name assignment on pool steps. No formula changes or new metadata keys were introduced. |
| 3 | Tests pass | PASS | Focused local checks passed: `go test ./internal/graphroute/... ./test/docsync/...` and `go vet ./internal/graphroute/...`. GitHub PR #3405 is mergeable with green `CI / required`, `CI / preflight`, `CI / integration`, process shards, integration shards, Dashboard SPA, and CodeQL on head `5def1f235410e1f885a7de9d621df56831019822`. Local `make test` was run and failed only in `cmd/gc` no-city/env-isolation tests because an active `/tmp/.gc` Dolt process makes `/tmp` resolve as a city without `/tmp/city.toml`; no graphroute or docsync tests failed. |
| 4 | No high-severity review findings open | PASS | Reviewer notes on `ga-85rmnq` report no security concerns or blocking findings. No HIGH findings are recorded on the deploy bead. |
| 5 | Final branch is clean | PASS | `git status --short --branch` was clean on `builder/ga-0ht5r6` before adding this gate file. |
| 6 | Branch diverges cleanly from main | PASS | PR #3405 reports `MERGEABLE` / `CLEAN`; `git merge-tree origin/main HEAD` completed without conflicts. |
| 7 | Single feature theme | PASS | The clean branch contains one commit touching only graphroute route binding/tests plus a docsync ignore entry needed for the repository's worktree layout. The contaminated branch was not used for deploy. |

## Local Test Details

Passed:

```text
go test ./internal/graphroute/... ./test/docsync/...
ok github.com/gastownhall/gascity/internal/graphroute 0.045s
ok github.com/gastownhall/gascity/test/docsync 3.497s
```

Passed:

```text
go vet ./internal/graphroute/...
```

Broad local run:

```text
make test
observable go test: FAIL status=1 log=/tmp/gascity-test.jsonl.uWrc4S
FAIL github.com/gastownhall/gascity/cmd/gc
```

Failed tests were limited to `cmd/gc` cases that require no city to be
discoverable from `/tmp`, including `TestFindCity/not_found`,
`TestDoPrimeStrictNoCity`, `TestRunDashboardServeAllowsNoCityWithSupervisor`,
`TestRunDashboardServeAllowsNoCityWithAPIOverride`, and related import/events
no-city checks. `lsof +D /tmp/.gc` showed an active Dolt process holding
`/tmp/.gc/runtime/packs/dolt/dolt.log`, so the deployer did not remove or move
that temp city state.

## CI Evidence

PR #3405 head `5def1f235410e1f885a7de9d621df56831019822` had these completed
green checks before the gate file was added:

- `CI / required`
- `CI / preflight`
- `CI / integration`
- `Preflight / static checks`
- `Preflight / acceptance A`
- `Dashboard SPA`
- `cmd/gc process / shard 1 of 12` through `cmd/gc process / shard 12 of 12`
- `Integration / packages-core-*`, `packages-cmd-gc-*`, `packages-runtime-tmux-*`
- `Integration / bdstore`
- `Integration / rest-smoke-*`
- `Integration / rest-full-*`
- `Integration / SQLite coordination store`
- `CodeQL`

## Gate Verdict

PASS. The deployable branch is the clean PR #3405 branch, not the stale
contaminated branch named in the original deploy bead.
