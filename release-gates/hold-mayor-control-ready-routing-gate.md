# Release Gate: hold:mayor control-ready pool routing

Bead: ga-d0kr8l
Source bead: ga-amdg7f
Review bead: ga-gscpan
Reviewed commit: `2f3ab72964c81922734b4815badc143c5d3bcf1a`
Main tip checked: `08bba7a3a63faaece48cf88976e11c51727fb4e6`
Decision: **PASS**

`docs/PROJECT_MANIFEST.md` is absent from both the reviewed tree and current
`origin/main`. This gate therefore applies the deployer's explicit seven
release criteria and the source bead's seven-part `exit_contract`.

## Gate checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Reviewer handoff `gm-wisp-hbjbavc` states that ga-amdg7f passed review; deploy bead ga-d0kr8l records the reviewed commit and PASS evidence; review bead ga-gscpan is closed with reason `pass`. |
| 2 | Acceptance criteria met | PASS | `filterReadyByRoute` excludes exact label `hold:mayor` from unassigned `gc.run_target` and `gc.routed_to` discovery. Both cached and fallback ready sets flow through `evaluateControlReady`; deliberately assigned work continues through `filterReadyByAssignee` unchanged. The change adds no `--exclude-label` dependency and does not alter `internal/config/workquery.go`. Cache, fallback, and direct-filter tests cover the behavior. |
| 3 | Tests pass | PASS | `make test-cmd-gc-process`: 15,161 PASS, 0 FAIL, 5 SKIP; its required product-metrics profile: 12 PASS, 0 FAIL, 0 SKIP. `make test`: 34,138 PASS, 0 FAIL, 170 SKIP. The targeted three-test smoke, `go build ./cmd/gc/`, and `go vet ./...` also pass. Skip rationale is recorded below. |
| 4 | No high-severity review findings open | PASS | Reviewer evidence records no blocker, major, minor, or other unresolved high-severity finding. |
| 5 | Final branch is clean | PASS | `git status --porcelain=v1` was empty at the reviewed commit after all build, vet, smoke, and test commands. This checklist is the only deploy-only delta to commit. |
| 6 | Branch diverges cleanly from main | PASS | After `git fetch origin main`, `git merge-tree --write-tree origin/main 2f3ab72964c81922734b4815badc143c5d3bcf1a` exited 0 and produced tree `9f661950b1d6517a7dae775dfed3b82f2b956573`. The feature is two commits ahead of its merge base while current main is four commits ahead; no self-rebase was required. |
| 7 | Single feature theme | PASS | The reviewed commit set modifies only `cmd/gc/dispatch_control_ready.go` and `cmd/gc/dispatch_control_ready_test.go`, implementing and testing one control-ready routing rule. |

## Acceptance evidence

- Routed, unassigned beads labeled `hold:mayor` are excluded by exact label
  equality through the existing `beadLabelsContain` helper.
- The filter applies to both ambient route keys because
  `evaluateControlReady` invokes `filterReadyByRoute` for both
  `gc.run_target` and `gc.routed_to`.
- Cache and fallback data sources both feed the same `evaluateControlReady`
  path, so the rule cannot drift between those paths.
- Direct assignee discovery is intentionally unchanged because assignment is
  an explicit dispatch decision, unlike ambient pool discovery.
- The feature diff contains no bare `human` label, `--exclude-label`,
  `BD_PREV_VERSION`, or `internal/config/workquery.go` change.
- The three focused tests cover the direct filter, cached store path, and
  fallback `bd ready` path.

## Test evidence

- `go test ./cmd/gc -run 'Test(FilterReadyByRouteExcludesHoldMayorLabel|TryControlReadyFromCacheOrFallbackExcludesHoldMayorRoutedBead(OnFallbackPath)?)$' -count=1`
  — 3 named tests PASS, 0 FAIL, 0 SKIP.
- `make test-cmd-gc-process`
  - ordinary process lane — 15,161 PASS, 0 FAIL, 5 SKIP;
  - product-metrics lane — 12 PASS, 0 FAIL, 0 SKIP.
- The five process-lane skips are safe for this change:
  - two are helper-process entrypoints that only run when invoked by their
    parent tests;
  - one is an opt-in live pack-registry canary with no registry source set;
  - one only applies outside Linux and macOS;
  - one depends on an absent example-pack prompt fixture and does not exercise
    control-ready routing.
- `make test` — 34,138 PASS, 0 FAIL, 170 SKIP. This target intentionally sets
  `GC_FAST_UNIT=1`; its slow process-backed cases are covered by the separate
  successful `make test-cmd-gc-process` run. The remaining skips are
  helper-, platform-, fixture-, or external-service-gated tests unrelated to
  this two-file routing change.
- `go build ./cmd/gc/` — PASS.
- `go vet ./...` — PASS.
- `git diff --check 21146d906ff70a9915d9f2ef26a5124ba9e7cc24..2f3ab72964c81922734b4815badc143c5d3bcf1a`
  — PASS.
