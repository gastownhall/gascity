# Plan — Post-init database-visibility retry scope recovery

**Root bead:** `ga-lvrcyp.3.1`
**Rig:** `gascity`
**Rejected deploy source:** `56c803ec4dc5c58834164d5d8cc20f322867e27b`
**Scope-gate evidence:** `396cf1893db505a3ba2b890b21c38ed17b80f434`

## Goal

Publish a reviewable, single-theme candidate for the retry that covers the
brief database-visibility gap after a successful `bd init`. The replacement
must retain the exact-error retry behavior and the evidence-based 20-second
budget without bundling independently shippable Dolt deadlines, test timing,
environment-golden, or test-isolation changes.

The deploy gate rejected `56c803ec4` before testing or opening a pull request.
Its diff contained thirteen source-side commits across several subsystems, and
criterion 7 requires one feature theme per deploy candidate. The rejected SHA
and its builder branch remain provenance only.

No tracker integration skill is materialized in this PM worktree, so external
tracker import is a no-op for this package.

## Scope contract

The clean candidate recreates only the behavior reviewed in these commits:

| Commit | Required behavior |
| --- | --- |
| `e6d86865d` | Regression coverage for the post-init database-not-ready retry helper |
| `b3828e491` | Exact-signature retry, direct `backoff/v4` dependency, and the two first-command call sites |
| `82e4fec3f` | Retry budget widened from 10 seconds to 20 seconds using measured fleet contention |

The candidate excludes every independent theme identified by the scope gate:

| Excluded change | Independent disposition |
| --- | --- |
| `e669302f4`, `7c8adf8ba` Dolt deadline changes | `ga-l4xwgh`, `ga-biy3ae` |
| `b581872da`, `56c803ec4` environment-golden changes | Verify current `origin/main`; do not duplicate an existing landing |
| `94d0cc0ec` `internal/beads` HOME isolation | `ga-0l3teb` |
| `6009bddd8` `cmd/gc` HOME isolation | `ga-us7c35` |
| `ec6e67595` stop-test timing | `ga-bm7k79` |
| Integration subprocess environment isolation | `ga-ykejub` |

The reconciliation package must confirm each disposition against live Git and
bead state. A missing owner becomes a separate bead; it never broadens the
retry candidate.

## Work packages

| Bead | Outcome | Acceptance focus |
| --- | --- | --- |
| `ga-lvrcyp.3.1.1` | Publish the isolated retry candidate | Fresh `origin/main` base; retry-only semantic diff; 20-second budget; exact error classification; repeated real-infrastructure tests; pushed branch and verified SHA |
| `ga-lvrcyp.3.1.2` | Reconcile every excluded side change | Current ancestry, live owner/route, landed-versus-active disposition, and no duplicate branch or release |
| `ga-lvrcyp.3.1.3` | Route the candidate through review and deploy | Runs only after `.1` and `.2`; creates and verifies a review handoff for the new SHA; forbids all mixed or superseded sources |

Each child bead carries its complete measurable acceptance criteria and the
`ready-to-build` plus `source:actual-pm` labels. Parent labels were not
inherited.

## Dependency graph

```text
ga-lvrcyp.3.1.1  isolate retry candidate ----+
                                                +--> ga-lvrcyp.3.1.3  review/deploy handoff
ga-lvrcyp.3.1.2  reconcile excluded work -----+
```

The first two packages can proceed in parallel. The handoff package depends on
both so no review bead is created until the candidate is single-theme and all
excluded work has a durable independent disposition.

## Release guardrails

- Build the replacement from current `origin/main`; do not deploy a builder
  branch tip or reuse the scope-failed deploy branch.
- Treat `56c803ec4`, `b3828e491`, and `3ae940a5d` as invalid deploy sources.
  `b3828e491` remains a semantic input but has the superseded 10-second budget.
- The intended diff may touch `test/integration/dolt_config_test.go` only for
  the post-init `bd create` retry. It must not carry the independent startup or
  readiness deadline changes in that same file.
- Require three independent passes each for `TestBdStoreMailWispInsert` and
  `TestDoltConfigWiringExternalHost`, plus the retry helper tests and repository
  push gates.
- A reviewer PASS creates a fresh deploy bead for the exact reviewed SHA. No
  rig agent self-merges; merge authority remains with mayor, mpr, or an
  operator.

## Completion evidence

This recovery is complete when the retry-only child records a pushed and
independently verified SHA, the reconciliation child accounts for every omitted
theme, and the gated handoff child records a review bead whose route and exact
SHA were read back successfully. The original root can then remain closed as
the planning artifact while its children drive the release chain.
