# Plan: workflow snapshot complexity followup

Root: `ga-9h6i8mgy`  
Class: refactor  
Planning base: `f5302d1dcfd5c0f903c05188c4be679435cfbdff`  
Generated: 2026-11-19

The workflow snapshot optimization in #8164 has landed. This plan addresses
the remaining complexity regression while preserving its lookup behavior.

## Evidence and scope

The original review's `complexity-gate/post.txt` reported
`buildWorkflowSnapshot` at CCN 20 versus 17 at its base, and
`tryFullWorkflowSQL` at 24 versus 22. Its verdict attributed those changes to
the original PR, separately from the maintainer fix. Evidence lives under
`/home/jaword/projects/gc-management/.gc/maintainer-pr-review/gastownhall-gascity/pr-8164/runs/20261118T004957Z/`.

Current code includes the graph-shortcut extraction suggested by the root
bead: `tryGraphBindingSnapshot` landed with `17d7d77ce29` (#8164). Direct
`gocyclo` measurements at the planning base are:

| Function | Current CCN | Acceptance limit |
|---|---:|---:|
| `(*Server).tryFullWorkflowSQL` | 24 | 22 |
| `(*Server).buildWorkflowSnapshot` | 18 | 18 |
| `(*Server).tryGraphBindingSnapshot` | 5 | No new threshold offender |

The snapshot function is now below the reporting threshold of 20. Avoid
repeating its extraction. The remaining work is one SQL refactor scope;
splitting it into multiple build or test beads would duplicate ownership.

`COMPLEXITY_BASE_REF=f5302d1dcfd5c0f903c05188c4be679435cfbdff make complexity-diff`
measured the unchanged planning tree and printed `complexity: no threshold
changes`. This establishes the planning reference, not completion of the
requested reduction. The command's advisory exit status is intentional.

## Work package

| Bead | Outcome | Label | Target | Workflow |
|---|---|---|---|---|
| `ga-d4wdwx13` | Simplify SQL snapshot selection with lookup behavior unchanged | `ready-to-build` | `gascity/builder` | `mol-build-from-spec` |

Use the available non-TDD build path, which retains tests and reviewer
handoff, for this behavior-preserving refactor. Existing behavior tests
should already pass. Add coverage only for a demonstrated gap; a fabricated
behavior failure or a coverage-only flag would misstate this scope.

## Acceptance

1. Direct measurements show `tryFullWorkflowSQL` at CCN <=22 and
   `buildWorkflowSnapshot` at <=18. Extracted functions remain in scanned
   production paths and introduce no new threshold offender. The builder
   chooses the implementation decomposition.
2. Preserve root and scope selection, partial flags, provenance, and snapshot
   indices. An authoritative graph-binding root reads no work stores;
   binding misses, refusals, and rejected scopes retain the existing sweep.
3. Preserve SQL candidate handling: skip endpoint-less proxied-local scopes;
   return `errNoSQLWorkflowStores` when all candidates are skipped; retain
   actionable first probe errors, clean misses, root uniqueness, scope
   selection, and eligibility of real direct endpoints in mixed sets.
4. Demonstrate zero complexity growth against the review/deploy gate's
   pinned base under the full `complexity-zero-growth.md.tmpl` rule in
   `packs/actual/all/template-fragments/` of the city checkout. If the base
   check is green, the merge check passes. Otherwise retain
   `make complexity-check 2>&1` from base and merge, with no row worse, and
   measure `COMPLEXITY_BASE_REF=<pinned-base> make complexity-diff` on the
   merge. It must print no `new:` or `regressed:` line and no `deleted:` line
   for a function still living outside the scan. Inspect the lines, not
   merely the exit code. Keep `engdocs/complexity-baseline.json` unchanged.
5. Run the existing workflow and SQL regression cases with Bazel. Preserve
   the split-strict ratchet and satisfy the repository/API quality gates.
   A function move may require an accurate call-site baseline update; it
   does not authorize new debt, weaker assertions, or ignored failures.

## Dependencies and verification

The sole lineage edge is
`ga-d4wdwx13 --discovered-from--> ga-9h6i8mgy`. No blocking edge is needed:
#8164 and its split-strict input repair, `45b05505ba9` (#9283), are already
on main. The store permits one dependency type per ordered bead pair, so
the required `discovered-from` edge carries lineage and this plan records
the child membership. `bd dep cycles` reports no cycles. There is no
test-before-build successor and no rollup ship bead for this one-off scope.

The builder must read `AGENTS.md`, `internal/api/AGENTS.md`, and `TESTING.md`.
Relevant existing tests include:

- `TestWorkflowGetReadsGraphNamespaceRootFromTheBindingOnly`
- `TestWorkflowGetServesTheBindingCopyOverAWorkStoreTwin`
- `TestWorkflowGetSweepsWorkStoresWhenTheBindingMissesAGraphNamespaceID`
- `TestWorkflowGetDoesNotProbeARefusedGraphBinding`
- `TestTryFullWorkflowSQLTreatsProxiedLocalScopesAsNoSQLStores`
- `TestTryFullWorkflowSQLSurfacesDirectEndpointFailureBesideProxiedLocal`

Run `bazel test //internal/api:api_test
//internal/api:api_split_strict_test` and the repository/API gates, including
`make check`, `make check-hooks`, `make dashboard-check`, and local dashboard
preview. A filtered run used as evidence must print actual named PASS lines
with `--test_env=GO_TEST_WRAP_TESTV=1 --test_output=all`. The known
split-strict fixture failures are governed by the existing classified list;
no new list entry is authorized by this plan.

## Limits and handoff

Keep wire/schema behavior, the existing graph helper, the complexity tool's
exit contract, and the complexity baseline unchanged. TierIssues work
(`ga-212sl`) and per-bead DepList optimization remain separate scopes.

Tracker import was a no-op: no tracker-to-beads import script is installed.
No new architecture decision or public documentation is required. Commit
and push this plan on its own `docs/ga-9h6i8mgy-workflow-complexity` branch,
verify the exact artifact is tracked and clean, close the planning root,
then sling the child with the explicit workflow and verify its route and
claimability before recording a handoff receipt.
